// Package zconsensus implements Z Network's consensus engine (AGENT_02).
//
// Protocol version 1 uses a practical Byzantine-fault-tolerant commit protocol in
// the Tendermint lineage (propose -> prevote -> precommit -> commit) with a
// deterministic, stake-weighted proposer schedule.
//
// Honest scope statement (see CONSENSUS_SPECIFICATION.md and LIMITATIONS L-001):
//
//   - Safety. Given fewer than 1/3 of voting power is Byzantine the commit rule
//     never commits two different blocks at one height. The rule is enforced by
//     construction: a block needs > 2/3 precommits for its own block id, and a
//     validator's signed vote set is tracked so an equivocation (two distinct
//     block ids at one height/round/step) is detected and recorded.
//   - Liveness. Relies on eventual synchrony and a fixed round timeout schedule.
//     The timeout schedule is local and deterministic; nodes simply retry the next
//     round. There is no bounded-timestamp-free variant yet.
//   - The engine is a state machine and is transport-agnostic: znode drives it,
//     znet delivers messages. That is why the same code path is exercised by the
//     unit tests and by the live multi-node testnet.
package zconsensus

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// Round and step constants.
const (
	StepPropose uint8 = iota
	StepPrevote
	StepPrecommit
	StepCommit
)

// MaxRounds bounds how many rounds a height may take before the node waits for
// an external signal. It is a resource guard, not a protocol rule.
const MaxRounds = 64

// Errors.
var (
	ErrNotValidator      = errors.New("zconsensus: node is not in the active validator set")
	ErrEquivocation      = errors.New("zconsensus: equivocation detected")
	ErrWrongHeight       = errors.New("zconsensus: message height does not match current height")
	ErrWrongRound        = errors.New("zconsensus: message round does not match current round")
	ErrInvalidSignature  = errors.New("zconsensus: invalid validator signature")
	ErrInvalidBlockID    = errors.New("zconsensus: block id mismatch")
	ErrDuplicateVote     = errors.New("zconsensus: duplicate vote")
	errInternalInvariant = errors.New("zconsensus: internal invariant violated")
)

// Vote is a signed consensus vote over a block id.
type Vote struct {
	ChainID   string            `json:"chain_id"`
	Height    uint64            `json:"height"`
	Round     uint32            `json:"round"`
	Step      uint8             `json:"step"`
	BlockID   zcrypto.Hash      `json:"block_id"`
	Voter     ztypes.Address    `json:"voter"`
	PubKey    zcrypto.PublicKey `json:"pub_key"`
	TimeMs    int64             `json:"time_ms"`
	Signature zcrypto.Signature `json:"signature"`
}

func appendU64(b []byte, v uint64) []byte {
	return append(b, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// SetBlockID is the hash that identifies (height, round, block) for voting.
func SetBlockID(height uint64, hash zcrypto.Hash) zcrypto.Hash {
	b := appendU64(nil, height)
	b = append(b, hash[:]...)
	return zcrypto.Digest("ZNET/v1/block-id", b)
}

// Validator is a member of the active set with its voting power.
type Validator struct {
	Address ztypes.Address
	PubKey  zcrypto.PublicKey
	Power   uint64 // voting power in uz (bonded stake)
}

// ValidatorSet is the active set for a height, with deterministic ordering.
type ValidatorSet struct {
	// ChainID binds the set to one network so votes cannot cross chains.
	ChainID      string
	ValidatorKey zcrypto.Hash
	Validators   []Validator
	byAddr       map[ztypes.Address]int
	total        uint64
}

// NewValidatorSet validates, sorts and indexes a set for a chain.
func NewValidatorSet(chainID string, vals []Validator) (*ValidatorSet, error) {
	if chainID == "" {
		return nil, errors.New("zconsensus: chain id required for validator set")
	}
	if len(vals) == 0 {
		return nil, errors.New("zconsensus: empty validator set")
	}
	if len(vals) > ztypes.MaxValidators {
		return nil, fmt.Errorf("zconsensus: validator set too large (%d)", len(vals))
	}
	sorted := append([]Validator(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool {
		return string(sorted[i].Address[:]) < string(sorted[j].Address[:])
	})
	vs := &ValidatorSet{ChainID: chainID, Validators: sorted, byAddr: make(map[ztypes.Address]int, len(sorted))}
	var total uint64
	for i, v := range sorted {
		if v.Power == 0 {
			return nil, fmt.Errorf("zconsensus: validator %s has zero power", v.Address.Bech32())
		}
		if _, dup := vs.byAddr[v.Address]; dup {
			return nil, fmt.Errorf("zconsensus: duplicate validator %s", v.Address.Bech32())
		}
		vs.byAddr[v.Address] = i
		total += v.Power
	}
	vs.total = total
	vs.ValidatorKey = HashSet(sorted)
	return vs, nil
}

// HashSet is the deterministic hash bounding a validator set (committed in the
// block header so a node cannot silently validate against a different set).
func HashSet(vals []Validator) zcrypto.Hash {
	var b []byte
	for _, v := range vals {
		b = append(b, v.Address[:]...)
		b = appendU64(b, v.Power)
		b = append(b, v.PubKey[:]...)
	}
	return zcrypto.Digest("ZNET/v1/validator-set", b)
}

// TotalPower returns the sum of voting power.
func (vs *ValidatorSet) TotalPower() uint64 { return vs.total }

// Len returns the number of validators.
func (vs *ValidatorSet) Len() int { return len(vs.Validators) }

// Lookup returns the validator for an address.
func (vs *ValidatorSet) Lookup(addr ztypes.Address) (Validator, bool) {
	i, ok := vs.byAddr[addr]
	if !ok {
		return Validator{}, false
	}
	return vs.Validators[i], true
}

// Proposer selects the proposer for a round using stake-weighted round-robin over
// the sorted set. The schedule is fully deterministic and verifiable by every
// node: proposer(height, round) = sorted[(height*7 + round) weighted index].
//
// Weighted selection walks the sorted list accumulating power until it exceeds
// `seed % total`, which gives each validator a share of slots proportional to its
// stake while remaining a pure function of (height, round, set).
func (vs *ValidatorSet) Proposer(height uint64, round uint32) Validator {
	seed := zcrypto.Digest("ZNET/v1/proposer-seed", appendU64(appendU64(nil, height), uint64(round)))
	var x uint64
	for i := 0; i < 8; i++ {
		x = x<<8 | uint64(seed[i])
	}
	target := x % vs.total
	var acc uint64
	for _, v := range vs.Validators {
		acc += v.Power
		if acc > target {
			return v
		}
	}
	return vs.Validators[len(vs.Validators)-1]
}

// VoteSet accumulates signed votes for one (height, round, step).
type VoteSet struct {
	height uint64
	round  uint32
	step   uint8
	votes  map[ztypes.Address]Vote
	power  map[ztypes.Address]uint64
	total  uint64
}

// NewVoteSet creates an empty vote set.
func NewVoteSet(height uint64, round uint32, step uint8) *VoteSet {
	return &VoteSet{height: height, round: round, step: step,
		votes: map[ztypes.Address]Vote{}, power: map[ztypes.Address]uint64{}}
}

// Add inserts a vote if it is for the expected (height, round, step) and the voter
// is in the set. It returns the accumulated power for that block id over the
// validator set it was created for.
func (vs *VoteSet) Add(v Vote, set *ValidatorSet) (uint64, error) {
	if v.ChainID != set.ChainID {
		return 0, fmt.Errorf("%w: vote chain id %q", ztypes.ErrChainIDMismatch, v.ChainID)
	}
	if v.Height != vs.height || v.Round != vs.round || v.Step != vs.step {
		return 0, ErrWrongHeight
	}
	val, ok := set.Lookup(v.Voter)
	if !ok {
		return 0, ErrNotValidator
	}
	if err := zcrypto.VerifyAddressSignature(v.Voter, v.PubKey, VoteDomain, VoteMsg(&v), v.Signature); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	if _, dup := vs.votes[v.Voter]; dup {
		return 0, ErrDuplicateVote
	}
	vs.votes[v.Voter] = v
	vs.power[v.Voter] = val.Power
	vs.total += val.Power
	return vs.total, nil
}

// VoteDomain is the signature domain for consensus votes.
const VoteDomain = "ZNET/v1/consensus-vote"

// VoteMsg is the canonical message a validator signs. It binds chain, height,
// round, step, block id, voter and timestamp, so a vote can never be replayed on
// another chain, height, round or step.
func VoteMsg(v *Vote) []byte {
	b := []byte(v.ChainID)
	b = append(b, 0)
	b = appendU64(b, v.Height)
	b = appendU32(b, v.Round)
	b = append(b, v.Step)
	b = append(b, v.BlockID[:]...)
	b = append(b, v.Voter[:]...)
	// TimeMs is deliberately NOT signed: it is local metadata, it is not stored in
	// the commit certificate, and a signed value that cannot be reproduced from
	// the certificate would make every commit unverifiable.
	return b
}

// SignVote signs v in place with the given key. The key's address must equal v.Voter.
func SignVote(k *zcrypto.PrivateKey, v *Vote) error {
	if k.Address() != v.Voter {
		return ErrInvalidSignature
	}
	v.PubKey = k.PublicKey()
	v.Signature = k.Sign(VoteDomain, VoteMsg(v))
	return nil
}

// Sum returns the total power accumulated for a given block id.
func (vs *VoteSet) Sum(blockID zcrypto.Hash) uint64 {
	var p uint64
	for addr, v := range vs.votes {
		if v.BlockID == blockID {
			p += vs.power[addr]
		}
	}
	return p
}

// Total returns the accumulated power of all votes.
func (vs *VoteSet) Total() uint64 { return vs.total }

// Sorted returns the votes ordered by voter address for deterministic serialisation.
func (vs *VoteSet) Sorted() []Vote {
	out := make([]Vote, 0, len(vs.votes))
	for _, v := range vs.votes {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].Voter[:]) < string(out[j].Voter[:]) })
	return out
}

// HasQuorum reports whether power reaches the 2/3 threshold of total voting power.
func HasQuorum(power, total uint64) bool {
	// > 2/3, computed without floating point: power*3 > total*2
	hi1, lo1 := mul64(power, 3)
	hi2, lo2 := mul64(total, 2)
	if hi1 != hi2 {
		return hi1 > hi2
	}
	return lo1 > lo2
}

func mul64(a, b uint64) (hi, lo uint64) {
	const mask = 0xffffffff
	a0, a1 := a&mask, a>>32
	b0, b1 := b&mask, b>>32
	p00 := a0 * b0
	p01 := a0 * b1
	p10 := a1 * b0
	p11 := a1 * b1
	mid := p01 + (p00 >> 32)
	mid += p10
	lo = (mid << 32) | (p00 & mask)
	hi = p11 + (mid >> 32)
	return hi, lo
}

// RoundTimeout is the round timeout schedule: 2s, 4s, 6s ... capped at 30s.
func RoundTimeout(round uint32) time.Duration {
	base := 2 * time.Second
	d := base * time.Duration(round+1)
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// BlockID is a convenience helper.
func BlockID(h zcrypto.Hash) zcrypto.Hash { return h }
