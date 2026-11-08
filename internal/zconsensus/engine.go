package zconsensus

import (
	"fmt"
	"sync"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// ProposalDomain is the signature domain for block proposals.
const ProposalDomain = "ZNET/v1/consensus-proposal"

// Proposal is a signed block proposal for (height, round).
type Proposal struct {
	ChainID   string            `json:"chain_id"`
	Height    uint64            `json:"height"`
	Round     uint32            `json:"round"`
	TimeMs    int64             `json:"time_ms"`
	Block     *ztypes.Block     `json:"block"`
	Proposer  ztypes.Address    `json:"proposer"`
	PubKey    zcrypto.PublicKey `json:"pub_key"`
	Signature zcrypto.Signature `json:"signature"`
}

// ProposalMsg is the canonical signed message for a proposal.
func ProposalMsg(p *Proposal) []byte {
	b := []byte(p.ChainID)
	b = append(b, 0)
	b = appendU64(b, p.Height)
	b = appendU32(b, p.Round)
	b = appendU64(b, uint64(p.TimeMs))
	h := p.Block.Header.Hash()
	b = append(b, h[:]...)
	b = append(b, p.Proposer[:]...)
	return b
}

// SignProposal signs p with the proposer key.
func SignProposal(k *zcrypto.PrivateKey, p *Proposal) error {
	if k.Address() != p.Proposer {
		return fmt.Errorf("%w: key does not own proposer address", ErrInvalidSignature)
	}
	p.PubKey = k.PublicKey()
	p.Signature = k.Sign(ProposalDomain, ProposalMsg(p))
	return nil
}

// Equivocation is recorded evidence that a validator signed two different block
// ids for the same (height, round, step). Both votes are retained so the evidence
// is self-contained and independently verifiable.
type Equivocation struct {
	Height uint64         `json:"height"`
	Round  uint32         `json:"round"`
	Step   uint8          `json:"step"`
	Voter  ztypes.Address `json:"voter"`
	First  Vote           `json:"first"`
	Second Vote           `json:"second"`
}

// CommitResult is the outcome of reaching a commit decision.
type CommitResult struct {
	Height  uint64         `json:"height"`
	Round   uint32         `json:"round"`
	Block   *ztypes.Block  `json:"block"`
	Commit  *ztypes.Commit `json:"commit"`
	BlockID zcrypto.Hash   `json:"block_id"`
}

// Config wires the engine to the rest of the node.
type Config struct {
	ChainID string
	// Key is the validator signing key. A node with a nil key is a full node: it
	// validates and commits but never proposes or votes.
	Key *zcrypto.PrivateKey
	// BuildProposal assembles the candidate block for (height, round). It is
	// called only when this node is the proposer.
	BuildProposal func(height uint64, round uint32, timeMs int64, proposer ztypes.Address) (*ztypes.Block, error)
	// ValidateProposal performs full block validation (stateless + state
	// transition) using the node's own view of state.
	ValidateProposal func(b *ztypes.Block) error
	// Now returns the current time in milliseconds (injectable for tests).
	Now func() int64
}

// Engine is the consensus state machine for one node.
type Engine struct {
	mu  sync.Mutex
	cfg Config
	set *ValidatorSet

	height uint64
	round  uint32
	step   uint8

	proposal   *Proposal
	prevotes   *VoteSet
	precommits *VoteSet
	lockedID   zcrypto.Hash
	locked     bool
	// polkaID/polkaRound record the last block that gathered a 2/3 prevote
	// majority. A lock may only be released by a polka for a NEWER round, which is
	// what keeps the "locked value" safe across rounds.
	polkaID    zcrypto.Hash
	polkaRound uint32
	polkaSeen  bool
	// proposals remembers accepted proposals per round so a node that advances
	// past a round can still commit a block whose certificate arrives later.
	proposals map[uint32]*Proposal
	// pendingVotes holds votes for future rounds so a round that starts late can
	// catch up instead of waiting for the next timeout.
	pendingVotes []Vote

	signed map[sigKey]zcrypto.Hash
	evid   []Equivocation

	// committed tracks the last commit this engine produced.
	committed *CommitResult
}

type sigKey struct {
	height uint64
	round  uint32
	step   uint8
	voter  ztypes.Address
}

// NewEngine creates a consensus engine.
func NewEngine(cfg Config) (*Engine, error) {
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("zconsensus: chain id required")
	}
	if cfg.BuildProposal == nil || cfg.ValidateProposal == nil {
		return nil, fmt.Errorf("zconsensus: build/validate hooks required")
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("zconsensus: Now hook required")
	}
	return &Engine{cfg: cfg, signed: map[sigKey]zcrypto.Hash{}}, nil
}

// SetValidatorSet installs the active set and starts a height.
func (e *Engine) SetValidatorSet(set *ValidatorSet, height uint64) error {
	if set == nil {
		return fmt.Errorf("zconsensus: nil validator set")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.set = set
	e.height = height
	e.round = 0
	e.step = StepPropose
	e.proposal = nil
	e.prevotes = NewVoteSet(height, 0, StepPrevote)
	e.precommits = NewVoteSet(height, 0, StepPrecommit)
	e.locked = false
	e.lockedID = zcrypto.Hash{}
	e.polkaSeen = false
	e.polkaID = zcrypto.Hash{}
	e.polkaRound = 0
	e.proposals = map[uint32]*Proposal{}
	e.pendingVotes = nil
	return nil
}

// Height returns the current height.
func (e *Engine) Height() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.height
}

// Round returns the current round.
func (e *Engine) Round() uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.round
}

// Step returns the current step.
func (e *Engine) Step() uint8 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.step
}

// IsValidator reports whether this node's key is in the active set.
func (e *Engine) IsValidator() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.isValidatorLocked()
}

// isValidatorLocked is the lock-held form; sync.Mutex is not reentrant, so every
// locked code path must use this instead of IsValidator.
func (e *Engine) isValidatorLocked() bool {
	if e.cfg.Key == nil || e.set == nil {
		return false
	}
	_, ok := e.set.Lookup(e.cfg.Key.Address())
	return ok
}

// Evidence returns recorded equivocations.
func (e *Engine) Evidence() []Equivocation {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Equivocation(nil), e.evid...)
}

// StepResult is what a handler asks the node to broadcast.
type StepResult struct {
	Proposal *Proposal
	Votes    []Vote
	Commit   *CommitResult
}

// CurrentProposer returns the proposer for the current round.
func (e *Engine) CurrentProposer() (Validator, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set == nil {
		return Validator{}, fmt.Errorf("zconsensus: no validator set installed")
	}
	return e.set.Proposer(e.height, e.round), nil
}

// StartRound drives the local step for the current round. If this node is the
// proposer it builds and signs a proposal. If it is not, it waits for a proposal;
// a timeout must be signalled with AdvanceRound.
func (e *Engine) StartRound() (StepResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startRoundLocked()
}

func (e *Engine) startRoundLocked() (StepResult, error) {
	if e.set == nil {
		return StepResult{}, fmt.Errorf("zconsensus: no validator set installed")
	}
	if e.step != StepPropose {
		// Round already in progress or finished; nothing to start.
		return StepResult{}, nil
	}
	if !e.isValidatorLocked() {
		return StepResult{}, nil
	}
	proposer := e.set.Proposer(e.height, e.round)
	if proposer.Address != e.cfg.Key.Address() {
		return StepResult{}, nil
	}
	timeMs := e.cfg.Now()
	block, err := e.cfg.BuildProposal(e.height, e.round, timeMs, proposer.Address)
	if err != nil {
		return StepResult{}, fmt.Errorf("zconsensus: build proposal: %w", err)
	}
	if block.Header.Height != e.height {
		return StepResult{}, fmt.Errorf("%w: builder returned height %d", errInternalInvariant, block.Header.Height)
	}
	p := &Proposal{
		ChainID:  e.cfg.ChainID,
		Height:   e.height,
		Round:    e.round,
		TimeMs:   timeMs,
		Block:    block,
		Proposer: proposer.Address,
	}
	if err := SignProposal(e.cfg.Key, p); err != nil {
		return StepResult{}, err
	}
	e.proposal = p
	h := block.Header.Hash()
	e.step = StepPrevote
	vote := e.makeVoteLocked(StepPrevote, SetBlockID(e.height, h))
	return StepResult{Proposal: p, Votes: []Vote{vote}}, nil
}

func (e *Engine) makeVoteLocked(step uint8, id zcrypto.Hash) Vote {
	v := Vote{
		ChainID: e.cfg.ChainID,
		Height:  e.height,
		Round:   e.round,
		Step:    step,
		BlockID: id,
		Voter:   e.cfg.Key.Address(),
		TimeMs:  e.cfg.Now(),
	}
	_ = SignVote(e.cfg.Key, &v)
	return v
}

// HandleProposal validates an incoming proposal and, if accepted, produces this
// node's prevote.
func (e *Engine) HandleProposal(p *Proposal) (StepResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set == nil {
		return StepResult{}, fmt.Errorf("zconsensus: no validator set installed")
	}
	if p == nil || p.Block == nil {
		return StepResult{}, fmt.Errorf("zconsensus: nil proposal")
	}
	if p.Height != e.height {
		return StepResult{}, fmt.Errorf("%w: proposal height %d, current %d", ErrWrongHeight, p.Height, e.height)
	}
	if p.Round != e.round {
		return StepResult{}, fmt.Errorf("%w: proposal round %d, current %d", ErrWrongRound, p.Round, e.round)
	}
	if p.ChainID != e.cfg.ChainID {
		return StepResult{}, fmt.Errorf("%w: proposal chain id %q", ztypes.ErrChainIDMismatch, p.ChainID)
	}
	if !okLookup(e.set, p.Proposer) {
		return StepResult{}, fmt.Errorf("%w: proposer not in validator set", ErrNotValidator)
	}
	want := e.set.Proposer(e.height, e.round)
	if want.Address != p.Proposer {
		return StepResult{}, fmt.Errorf("%w: %s is not the round proposer (%s)", ErrInvalidSignature, p.Proposer.Bech32(), want.Address.Bech32())
	}
	if err := zcrypto.VerifyAddressSignature(p.Proposer, p.PubKey, ProposalDomain, ProposalMsg(p), p.Signature); err != nil {
		return StepResult{}, fmt.Errorf("%w: proposal signature: %v", ErrInvalidSignature, err)
	}
	if p.Block.Header.Height != e.height {
		return StepResult{}, fmt.Errorf("%w: block height %d", ErrWrongHeight, p.Block.Header.Height)
	}
	if p.Block.Header.Proposer != p.Proposer {
		return StepResult{}, fmt.Errorf("%w: header proposer mismatch", ErrInvalidBlockID)
	}
	if e.set.ValidatorKey != p.Block.Header.ValidatorsHash {
		return StepResult{}, fmt.Errorf("%w: validators hash mismatch", ErrInvalidBlockID)
	}
	if err := e.cfg.ValidateProposal(p.Block); err != nil {
		// An invalid proposal is ignored (never voted for). The round will time
		// out; that is the protocol's correct behaviour.
		return StepResult{}, fmt.Errorf("zconsensus: proposal failed validation: %w", err)
	}
	if e.proposals == nil {
		e.proposals = map[uint32]*Proposal{}
	}
	e.proposals[e.round] = p
	e.proposal = p
	id := SetBlockID(e.height, p.Block.Header.Hash())

	// Lock rule (safety). A validator that has seen a 2/3 prevote majority ("polka")
	// for block B in round R is locked on B and may only prevote for a different
	// block if it has since seen a polka for that block in a LATER round. Because
	// two conflicting polkas in the same round would require >1/3 Byzantine power,
	// locked validators cannot help commit two different blocks at one height.
	if e.locked && e.lockedID != id {
		canUnlock := e.polkaSeen && e.polkaID == id && e.round > e.polkaRound
		if !canUnlock {
			if !e.isValidatorLocked() {
				e.step = StepPrevote
				return StepResult{}, nil
			}
			v := e.makeVoteLocked(StepPrevote, e.lockedID)
			e.step = StepPrevote
			return StepResult{Votes: []Vote{v}}, nil
		}
	}
	if !e.isValidatorLocked() {
		e.step = StepPrevote
		return StepResult{}, nil
	}
	e.step = StepPrevote
	v := e.makeVoteLocked(StepPrevote, id)
	return StepResult{Votes: []Vote{v}}, nil
}

func okLookup(set *ValidatorSet, a ztypes.Address) bool {
	_, ok := set.Lookup(a)
	return ok
}

// HandleVote processes an incoming vote and returns any votes this node should
// broadcast as a consequence, plus a commit when the block is decided.
func (e *Engine) HandleVote(v Vote) (StepResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out StepResult
	if e.set == nil {
		return out, fmt.Errorf("zconsensus: no validator set installed")
	}
	if v.Height != e.height {
		// Votes for other heights are not applicable here.
		return out, nil
	}
	if v.Round > e.round {
		// Keep votes for rounds we have not reached yet: a node that started a
		// round late can then catch up immediately instead of waiting for another
		// timeout, which is what allowed the cluster to stall.
		if len(e.pendingVotes) < 4096 {
			e.pendingVotes = append(e.pendingVotes, v)
		}
		return out, nil
	}
	if v.Round < e.round {
		// A commit can still arrive for a round we already left behind: it is
		// validated against the votes we kept for that round only if we still hold
		// them, so stale-round votes are ignored.
		return out, nil
	}
	// Equivocation detection happens before the vote is counted.
	k := sigKey{v.Height, v.Round, v.Step, v.Voter}
	if prev, ok := e.signed[k]; ok && prev != v.BlockID {
		e.evid = append(e.evid, Equivocation{
			Height: v.Height, Round: v.Round, Step: v.Step, Voter: v.Voter,
			First:  Vote{Height: v.Height, Round: v.Round, Step: v.Step, BlockID: prev, Voter: v.Voter},
			Second: v,
		})
		return out, fmt.Errorf("%w: validator %s", ErrEquivocation, v.Voter.Bech32())
	}
	var set *VoteSet
	switch v.Step {
	case StepPrevote:
		set = e.prevotes
	case StepPrecommit:
		set = e.precommits
	default:
		return out, fmt.Errorf("zconsensus: vote with bad step %d", v.Step)
	}
	power, err := set.Add(v, e.set)
	if err != nil {
		return out, err
	}
	e.signed[k] = v.BlockID

	if !HasQuorum(power, e.set.TotalPower()) {
		return out, nil
	}
	// Quorum reached for this step.
	if e.step < v.Step+1 {
		e.step = v.Step + 1
	}
	sum := set.Sum(v.BlockID)
	if !HasQuorum(sum, e.set.TotalPower()) {
		return out, nil
	}
	switch v.Step {
	case StepPrevote:
		// Record the polka. A polka for a newer round than the current lock is
		// what authorises re-locking on a different block.
		if !e.polkaSeen || v.Round > e.polkaRound || (v.Round == e.polkaRound && e.polkaID == v.BlockID) {
			e.polkaSeen = true
			e.polkaID = v.BlockID
			e.polkaRound = v.Round
		}
		if e.isValidatorLocked() {
			e.locked = true
			e.lockedID = v.BlockID
			e.step = StepPrecommit
			pv := e.makeVoteLocked(StepPrecommit, v.BlockID)
			out.Votes = append(out.Votes, pv)
		}
	case StepPrecommit:
		if e.committed != nil && e.committed.Height == e.height {
			return out, nil // already committed
		}
		// Build the commit certificate from the precommit votes.
		commit, err := BuildCommit(e.cfg.ChainID, e.height, e.round, e.precommits, e.set, v.BlockID)
		if err != nil {
			return out, err
		}
		// The committed block may have been proposed in an earlier round than the
		// one in which the certificate completes.
		var block *ztypes.Block
		for r, prop := range e.proposals {
			_ = r
			if prop != nil && SetBlockID(e.height, prop.Block.Header.Hash()) == v.BlockID {
				block = prop.Block
				break
			}
		}
		if block == nil && e.proposal != nil && SetBlockID(e.height, e.proposal.Block.Header.Hash()) == v.BlockID {
			block = e.proposal.Block
		}
		if block == nil {
			out.Commit = &CommitResult{Height: e.height, Round: v.Round, Commit: commit, BlockID: v.BlockID}
			return out, nil
		}
		out.Commit = &CommitResult{
			Height: e.height, Round: v.Round, Block: block,
			Commit: commit, BlockID: v.BlockID,
		}
		e.committed = out.Commit
	}
	return out, nil
}

// BuildCommit gathers the precommit votes for blockID into a serialisable commit.
func BuildCommit(chainID string, height uint64, round uint32, precommits *VoteSet, set *ValidatorSet, blockID zcrypto.Hash) (*ztypes.Commit, error) {
	c := &ztypes.Commit{Height: height, Round: round, BlockHash: blockID}
	votes := precommits.Sorted()
	for _, v := range votes {
		if v.BlockID != blockID {
			continue
		}
		c.Sigs = append(c.Sigs, ztypes.CommitSig{
			Validator: v.Voter,
			PubKey:    v.PubKey,
			Signature: v.Signature,
			Signed:    true,
		})
	}
	if len(c.Sigs) == 0 {
		return nil, fmt.Errorf("zconsensus: commit with no signatures")
	}
	return c, nil
}

// VerifyCommit checks a commit certificate against a validator set.
func VerifyCommit(chainID string, height uint64, blockHash zcrypto.Hash, c *ztypes.Commit, set *ValidatorSet) error {
	if c == nil {
		return fmt.Errorf("zconsensus: nil commit")
	}
	if c.Height != height {
		return fmt.Errorf("%w: commit height %d", ErrWrongHeight, c.Height)
	}
	id := SetBlockID(height, blockHash)
	if c.BlockHash != id {
		return fmt.Errorf("%w: commit block id mismatch", ErrInvalidBlockID)
	}
	seen := map[ztypes.Address]bool{}
	var power uint64
	for _, s := range c.Sigs {
		if !s.Signed {
			continue
		}
		val, ok := set.Lookup(s.Validator)
		if !ok {
			return fmt.Errorf("%w: signer %s not in validator set", ErrNotValidator, s.Validator.Bech32())
		}
		if seen[s.Validator] {
			return fmt.Errorf("%w: duplicate signer %s", ErrDuplicateVote, s.Validator.Bech32())
		}
		seen[s.Validator] = true
		v := Vote{
			ChainID: chainID, Height: height, Round: c.Round, Step: StepPrecommit,
			BlockID: id, Voter: s.Validator, PubKey: s.PubKey, Signature: s.Signature,
		}
		if err := zcrypto.VerifyAddressSignature(s.Validator, s.PubKey, VoteDomain, VoteMsg(&v), s.Signature); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
		}
		power += val.Power
	}
	if !HasQuorum(power, set.TotalPower()) {
		return fmt.Errorf("zconsensus: commit power %d does not reach quorum of %d", power, set.TotalPower())
	}
	return nil
}

// AdvanceRound moves to the next round after a timeout.
func (e *Engine) AdvanceRound() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.committed != nil && e.committed.Height == e.height {
		return nil
	}
	if e.round+1 >= MaxRounds {
		return fmt.Errorf("zconsensus: exceeded %d rounds at height %d", MaxRounds, e.height)
	}
	e.round++
	e.step = StepPropose
	e.proposal = nil
	e.prevotes = NewVoteSet(e.height, e.round, StepPrevote)
	e.precommits = NewVoteSet(e.height, e.round, StepPrecommit)
	// The lock survives a timeout: that is the whole point of locking. It is only
	// released by a polka for a newer round (see HandleVote) or by a new height.
	return nil
}

// DrainPendingVotes returns and clears the votes buffered for future rounds. The
// node calls this after advancing a round.
func (e *Engine) DrainPendingVotes() []Vote {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.pendingVotes
	e.pendingVotes = nil
	return out
}

// PendingVotesCount reports how many future-round votes are buffered.
func (e *Engine) PendingVotesCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pendingVotes)
}

// Committed returns the commit produced at the current height, if any.
func (e *Engine) Committed() *CommitResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.committed
}

// SetHeight positions the engine at an explicit height with the validator set that
// validates it. The chain is the authority on height: a node that synced blocks,
// failed a height, or restarted must be able to place the engine at exactly
// chainHeight+1. Incrementing instead would let the engine drift above the chain
// and then reject its own proposals ("block height does not extend the chain").
func (e *Engine) SetHeight(height uint64, set *ValidatorSet) error {
	if set == nil {
		return fmt.Errorf("zconsensus: nil validator set")
	}
	if height == 0 {
		return fmt.Errorf("zconsensus: height must be at least 1")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetForHeightLocked(height, set)
	return nil
}

// AdvanceHeight moves the engine to the next height with a new validator set.
// It is only correct when the engine is known to sit exactly one height below the
// chain; callers that can observe the chain should use SetHeight instead.
func (e *Engine) AdvanceHeight(set *ValidatorSet) error {
	if set == nil {
		return fmt.Errorf("zconsensus: nil validator set")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetForHeightLocked(e.height+1, set)
	return nil
}

func (e *Engine) resetForHeightLocked(height uint64, set *ValidatorSet) {
	e.height = height
	e.round = 0
	e.step = StepPropose
	e.set = set
	e.proposal = nil
	e.prevotes = NewVoteSet(e.height, 0, StepPrevote)
	e.precommits = NewVoteSet(e.height, 0, StepPrecommit)
	e.locked = false
	e.lockedID = zcrypto.Hash{}
	e.committed = nil
	e.evid = nil
	e.polkaID = zcrypto.Hash{}
	e.polkaRound = 0
	e.polkaSeen = false
	e.proposals = make(map[uint32]*Proposal)
	// Pending votes are for heights already finished; dropping them prevents a
	// stale vote from a previous height being replayed into this one.
	e.pendingVotes = nil
}
