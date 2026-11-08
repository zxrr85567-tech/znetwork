package zconsensus

import (
	"fmt"
	"testing"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/ztypes"
)

func testSet(t *testing.T, n int) (*ValidatorSet, []*zcrypto.PrivateKey) {
	t.Helper()
	var vals []Validator
	var keys []*zcrypto.PrivateKey
	for i := 0; i < n; i++ {
		k, err := zdevnet.DeterministicKey(fmt.Sprintf("consensus-test-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
		// Deliberately unequal power to exercise weighting.
		vals = append(vals, Validator{Address: k.Address(), PubKey: k.PublicKey(), Power: uint64(1000 + i*250)})
	}
	set, err := NewValidatorSet("consensus-test-1", vals)
	if err != nil {
		t.Fatal(err)
	}
	return set, keys
}

func TestValidatorSetRejectsBadInput(t *testing.T) {
	k, _ := zdevnet.DeterministicKey("bad")
	if _, err := NewValidatorSet("c", nil); err == nil {
		t.Fatal("empty set accepted")
	}
	if _, err := NewValidatorSet("c", []Validator{{Address: k.Address(), PubKey: k.PublicKey(), Power: 0}}); err == nil {
		t.Fatal("zero-power validator accepted")
	}
	dup := []Validator{
		{Address: k.Address(), PubKey: k.PublicKey(), Power: 1},
		{Address: k.Address(), PubKey: k.PublicKey(), Power: 1},
	}
	if _, err := NewValidatorSet("c", dup); err == nil {
		t.Fatal("duplicate validator accepted")
	}
}

func TestProposerScheduleIsDeterministicAndProportional(t *testing.T) {
	set, _ := testSet(t, 4)
	// Determinism: the same (height, round) always yields the same proposer.
	for h := uint64(1); h <= 50; h++ {
		if set.Proposer(h, 0).Address != set.Proposer(h, 0).Address {
			t.Fatal("proposer not deterministic")
		}
	}
	// Proportionality: over many heights the share of slots tracks voting power.
	counts := map[ztypes.Address]int{}
	const heights = 20_000
	for h := uint64(1); h <= heights; h++ {
		counts[set.Proposer(h, 0).Address]++
	}
	for _, v := range set.Validators {
		got := float64(counts[v.Address]) / float64(heights)
		want := float64(v.Power) / float64(set.TotalPower())
		if diff := got - want; diff > 0.03 || diff < -0.03 {
			t.Fatalf("validator %s share %.4f, want %.4f", v.Address.Bech32(), got, want)
		}
	}
}

func TestHashSetIsOrderIndependent(t *testing.T) {
	a, _ := testSet(t, 5)
	var rev []Validator
	for i := len(a.Validators) - 1; i >= 0; i-- {
		rev = append(rev, a.Validators[i])
	}
	b, err := NewValidatorSet("consensus-test-1", rev)
	if err != nil {
		t.Fatal(err)
	}
	if a.ValidatorKey != b.ValidatorKey {
		t.Fatal("validator set hash depends on input order")
	}
}

func TestQuorumBoundary(t *testing.T) {
	cases := []struct {
		power, total uint64
		want         bool
	}{
		{2, 3, false},   // exactly 2/3 is not enough
		{67, 100, true}, // > 2/3
		{66, 100, false},
		{1, 1, true},
		{0, 1, false},
		{999_999_999_999, 1_000_000_000_000, true},
	}
	for _, c := range cases {
		if got := HasQuorum(c.power, c.total); got != c.want {
			t.Fatalf("HasQuorum(%d,%d) = %v, want %v", c.power, c.total, got, c.want)
		}
	}
}

// buildBlock makes a minimally valid block for consensus tests. Full block
// construction (transactions, receipts, state root) is tested in znode.
func buildBlock(chainID string, height uint64, timeMs int64, proposer ztypes.Address, vh zcrypto.Hash, payload byte) *ztypes.Block {
	b := &ztypes.Block{}
	b.Header.Version = ztypes.ProtocolVersion
	b.Header.ChainID = chainID
	b.Header.Height = height
	b.Header.TimeMs = timeMs
	b.Header.Proposer = proposer
	b.Header.ValidatorsHash = vh
	b.Header.GasLimit = 1_000_000
	// payload varies the hash so distinct proposals are distinguishable
	b.Header.TxCount = uint32(payload)
	return b
}

type node struct {
	eng  *Engine
	key  *zcrypto.PrivateKey
	addr ztypes.Address
}

func newCluster(t *testing.T, n int) ([]*node, *ValidatorSet, string) {
	t.Helper()
	set, keys := testSet(t, n)
	chainID := "consensus-test-1"
	var nodes []*node
	for i, k := range keys {
		idx := i
		cfg := Config{
			ChainID: chainID,
			Key:     k,
			Now:     func() int64 { return 1_700_000_000_000 },
			BuildProposal: func(height uint64, round uint32, timeMs int64, proposer ztypes.Address) (*ztypes.Block, error) {
				return buildBlock(chainID, height, timeMs, proposer, set.ValidatorKey, byte(idx)), nil
			},
			ValidateProposal: func(b *ztypes.Block) error {
				if b.Header.ChainID != chainID {
					return fmt.Errorf("bad chain")
				}
				return nil
			},
		}
		eng, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.SetValidatorSet(set, 1); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, &node{eng: eng, key: k, addr: k.Address()})
	}
	return nodes, set, chainID
}

// runHeight drives one height to commit across the cluster, delivering messages
// until everyone has committed. It returns the number of messages exchanged.
func runHeight(t *testing.T, nodes []*node, set *ValidatorSet, height uint64) int {
	t.Helper()
	proposer := set.Proposer(height, 0)

	var proposal *Proposal
	var votes []Vote
	// Each node starts its round.
	for _, n := range nodes {
		res, err := n.eng.StartRound()
		if err != nil {
			t.Fatalf("StartRound: %v", err)
		}
		if res.Proposal != nil {
			if res.Proposal.Proposer != proposer.Address {
				t.Fatalf("proposer %s built a proposal but round proposer is %s",
					res.Proposal.Proposer.Bech32(), proposer.Address.Bech32())
			}
			proposal = res.Proposal
		}
		votes = append(votes, res.Votes...)
	}
	if proposal == nil {
		t.Fatal("no proposal was built")
	}
	// Deliver the proposal to everyone (including the proposer, which already
	// holds it; HandleProposal must be idempotent).
	for _, n := range nodes {
		res, err := n.eng.HandleProposal(proposal)
		if err != nil {
			t.Fatalf("HandleProposal at %s: %v", n.addr.Bech32(), err)
		}
		votes = append(votes, res.Votes...)
	}
	exchanged := 1
	delivered := map[string]bool{}
	// Gossip votes until everyone commits. Every vote is delivered to every node
	// exactly once, including the voter itself (a validator counts its own vote).
	for iter := 0; iter < 100; iter++ {
		if len(votes) == 0 {
			break
		}
		batch := votes
		votes = nil
		for _, v := range batch {
			key := fmt.Sprintf("%d/%d/%d/%s/%x", v.Height, v.Round, v.Step, v.Voter.Bech32(), v.BlockID[:4])
			if delivered[key] {
				continue
			}
			delivered[key] = true
			exchanged++
			for _, n := range nodes {
				res, err := n.eng.HandleVote(v)
				if err != nil {
					t.Fatalf("HandleVote at %s: %v", n.addr.Bech32(), err)
				}
				votes = append(votes, res.Votes...)
			}
		}
		done := true
		for _, n := range nodes {
			if n.eng.Committed() == nil {
				done = false
			}
		}
		if done {
			break
		}
	}
	for _, n := range nodes {
		c := n.eng.Committed()
		if c == nil {
			t.Fatalf("node %s did not commit height %d", n.addr.Bech32(), height)
		}
		if c.Height != height || c.Block == nil {
			t.Fatalf("bad commit at %s: %+v", n.addr.Bech32(), c)
		}
		if err := verifyResult(set, c); err != nil {
			t.Fatalf("commit from %s failed verification: %v", n.addr.Bech32(), err)
		}
	}
	return exchanged
}

// verifyResult checks a CommitResult the way a syncing node would.
func verifyResult(set *ValidatorSet, c *CommitResult) error {
	return VerifyCommit(c.Block.Header.ChainID, c.Height, c.Block.Header.Hash(), c.Commit, set)
}

func TestFourNodeConsensusCommits(t *testing.T) {
	nodes, set, _ := newCluster(t, 4)
	msgs := runHeight(t, nodes, set, 1)
	if msgs < 4 {
		t.Fatalf("suspiciously few messages exchanged: %d", msgs)
	}
	// Every node must agree on the committed block id.
	first := nodes[0].eng.Committed().BlockID
	for _, n := range nodes {
		if n.eng.Committed().BlockID != first {
			t.Fatalf("nodes disagree on the committed block id")
		}
	}
}

func TestSingleNodeConsensusCommits(t *testing.T) {
	nodes, set, _ := newCluster(t, 1)
	runHeight(t, nodes, set, 1)
}

func TestSevenNodeConsensusCommits(t *testing.T) {
	nodes, set, _ := newCluster(t, 7)
	runHeight(t, nodes, set, 1)
}

func TestNonValidatorDoesNotProposeOrVote(t *testing.T) {
	set, _ := testSet(t, 3)
	stranger, err := zcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(Config{
		ChainID: "c",
		Key:     stranger,
		Now:     func() int64 { return 0 },
		BuildProposal: func(h uint64, r uint32, t int64, p ztypes.Address) (*ztypes.Block, error) {
			return nil, fmt.Errorf("must not be called")
		},
		ValidateProposal: func(b *ztypes.Block) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SetValidatorSet(set, 1); err != nil {
		t.Fatal(err)
	}
	res, err := eng.StartRound()
	if err != nil {
		t.Fatal(err)
	}
	if res.Proposal != nil || len(res.Votes) != 0 {
		t.Fatal("non-validator produced consensus messages")
	}
	if eng.IsValidator() {
		t.Fatal("IsValidator true for outsider")
	}
}

func TestEquivocationIsDetected(t *testing.T) {
	nodes, set, chainID := newCluster(t, 4)
	n := nodes[0]
	id1 := SetBlockID(1, zcrypto.Digest("x", []byte("a")))
	id2 := SetBlockID(1, zcrypto.Digest("x", []byte("b")))
	// A Byzantine validator signs two different block ids at the same step.
	v1 := Vote{ChainID: chainID, Height: 1, Round: 0, Step: StepPrevote, BlockID: id1, Voter: n.key.Address()}
	v2 := Vote{ChainID: chainID, Height: 1, Round: 0, Step: StepPrevote, BlockID: id2, Voter: n.key.Address()}
	if err := SignVote(n.key, &v1); err != nil {
		t.Fatal(err)
	}
	if err := SignVote(n.key, &v2); err != nil {
		t.Fatal(err)
	}
	target := nodes[1]
	if _, err := target.eng.HandleVote(v1); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if _, err := target.eng.HandleVote(v2); err == nil {
		t.Fatal("equivocation not rejected")
	}
	ev := target.eng.Evidence()
	if len(ev) != 1 || ev[0].Voter != n.key.Address() {
		t.Fatalf("evidence = %+v", ev)
	}
	if ev[0].First.BlockID != id1 || ev[0].Second.BlockID != id2 {
		t.Fatal("evidence does not retain both votes")
	}
	_ = set
}

func TestDuplicateVoteRejected(t *testing.T) {
	nodes, _, chainID := newCluster(t, 4)
	id := SetBlockID(1, zcrypto.Digest("x", []byte("a")))
	v := Vote{ChainID: chainID, Height: 1, Round: 0, Step: StepPrevote, BlockID: id, Voter: nodes[0].key.Address()}
	if err := SignVote(nodes[0].key, &v); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes[1].eng.HandleVote(v); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes[1].eng.HandleVote(v); err == nil {
		t.Fatal("duplicate vote accepted")
	}
}

func TestForgedVoteRejected(t *testing.T) {
	nodes, _, chainID := newCluster(t, 4)
	id := SetBlockID(1, zcrypto.Digest("x", []byte("a")))
	// Validator 0's address, but signed by validator 1's key.
	v := Vote{ChainID: chainID, Height: 1, Round: 0, Step: StepPrevote, BlockID: id, Voter: nodes[0].key.Address()}
	v.PubKey = nodes[1].key.PublicKey()
	v.Signature = nodes[1].key.Sign(VoteDomain, VoteMsg(&v))
	if _, err := nodes[2].eng.HandleVote(v); err == nil {
		t.Fatal("forged vote accepted")
	}
}

func TestVoteForOtherChainRejected(t *testing.T) {
	nodes, _, _ := newCluster(t, 4)
	id := SetBlockID(1, zcrypto.Digest("x", []byte("a")))
	v := Vote{ChainID: "other-chain", Height: 1, Round: 0, Step: StepPrevote, BlockID: id, Voter: nodes[0].key.Address()}
	if err := SignVote(nodes[0].key, &v); err != nil {
		t.Fatal(err)
	}
	// The signature is valid for that other chain, but the vote must not count here:
	// HandleVote accepts it only if the vote passes our signature check with our
	// chain id, which recomputes VoteMsg and therefore fails.
	if _, err := nodes[1].eng.HandleVote(v); err == nil {
		t.Fatal("cross-chain vote accepted")
	}
}

func TestInsufficientQuorumCannotCommit(t *testing.T) {
	nodes, set, chainID := newCluster(t, 4)
	// Build a proposal for height 1 and collect only 1/4 of precommits.
	h := buildBlock(chainID, 1, 0, set.Proposer(1, 0).Address, set.ValidatorKey, 0)
	id := SetBlockID(1, h.Header.Hash())
	var sigs []ztypes.CommitSig
	k := nodes[0].key
	v := Vote{ChainID: chainID, Height: 1, Round: 0, Step: StepPrecommit, BlockID: id, Voter: k.Address()}
	if err := SignVote(k, &v); err != nil {
		t.Fatal(err)
	}
	sigs = append(sigs, ztypes.CommitSig{Validator: k.Address(), PubKey: k.PublicKey(), Signature: v.Signature, Signed: true})
	commit := &ztypes.Commit{Height: 1, Round: 0, BlockHash: id, Sigs: sigs}
	if err := VerifyCommit(chainID, 1, h.Header.Hash(), commit, set); err == nil {
		t.Fatal("commit with 1/4 power verified")
	}
	// Duplicate signers must not double-count.
	sigs2 := append([]ztypes.CommitSig{}, sigs...)
	sigs2 = append(sigs2, sigs[0], sigs[0])
	commit2 := &ztypes.Commit{Height: 1, Round: 0, BlockHash: id, Sigs: sigs2}
	if err := VerifyCommit(chainID, 1, h.Header.Hash(), commit2, set); err == nil {
		t.Fatal("duplicate signers were counted")
	}
}

func TestCommitForDifferentBlockRejected(t *testing.T) {
	nodes, set, chainID := newCluster(t, 4)
	_ = nodes
	h := buildBlock(chainID, 1, 0, set.Proposer(1, 0).Address, set.ValidatorKey, 0)
	other := buildBlock(chainID, 1, 0, set.Proposer(1, 0).Address, set.ValidatorKey, 9)
	id := SetBlockID(1, other.Header.Hash())
	commit := &ztypes.Commit{Height: 1, Round: 0, BlockHash: id}
	if err := VerifyCommit(chainID, 1, h.Header.Hash(), commit, set); err == nil {
		t.Fatal("commit for a different block id verified")
	}
}

func TestAdvanceRoundResetsState(t *testing.T) {
	nodes, _, _ := newCluster(t, 4)
	n := nodes[1]
	if err := n.eng.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if n.eng.Round() != 1 || n.eng.Step() != StepPropose {
		t.Fatalf("round=%d step=%d", n.eng.Round(), n.eng.Step())
	}
	// A proposal for round 0 must now be rejected as stale.
	stale := &Proposal{ChainID: "consensus-test-1", Height: 1, Round: 0, Block: buildBlock("consensus-test-1", 1, 0, ztypes.Address{}, zcrypto.Hash{}, 0)}
	if _, err := n.eng.HandleProposal(stale); err == nil {
		t.Fatal("stale proposal accepted")
	}
}

func TestWrongProposerRejected(t *testing.T) {
	nodes, set, chainID := newCluster(t, 4)
	// Sign a proposal as a validator that is not the round proposer.
	wrong := nodes[0].key
	real := set.Proposer(1, 0)
	if wrong.Address() == real.Address {
		wrong = nodes[1].key
	}
	b := buildBlock(chainID, 1, 0, wrong.Address(), set.ValidatorKey, 0)
	p := &Proposal{ChainID: chainID, Height: 1, Round: 0, TimeMs: 0, Block: b, Proposer: wrong.Address()}
	if err := SignProposal(wrong, p); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.addr == wrong.Address() {
			continue
		}
		if _, err := n.eng.HandleProposal(p); err == nil {
			t.Fatal("proposal from a non-proposer accepted")
		}
	}
}

func TestInvalidProposalNotVotedFor(t *testing.T) {
	nodes, set, chainID := newCluster(t, 4)
	prop := set.Proposer(1, 0)
	var proposerKey *zcrypto.PrivateKey
	for _, n := range nodes {
		if n.addr == prop.Address {
			proposerKey = n.key
		}
	}
	bad := buildBlock(chainID, 1, 0, prop.Address, set.ValidatorKey, 0)
	bad.Header.ChainID = "not-our-chain"
	p := &Proposal{ChainID: chainID, Height: 1, Round: 0, Block: bad, Proposer: prop.Address}
	if err := SignProposal(proposerKey, p); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.addr == prop.Address {
			continue
		}
		if _, err := n.eng.HandleProposal(p); err == nil {
			t.Fatal("invalid block was accepted")
		}
	}
}

func TestProposerMustMatchHeader(t *testing.T) {
	nodes, set, chainID := newCluster(t, 4)
	prop := set.Proposer(1, 0)
	var proposerKey *zcrypto.PrivateKey
	for _, n := range nodes {
		if n.addr == prop.Address {
			proposerKey = n.key
		}
	}
	b := buildBlock(chainID, 1, 0, ztypes.Address{}, set.ValidatorKey, 0) // zero proposer in header
	p := &Proposal{ChainID: chainID, Height: 1, Round: 0, Block: b, Proposer: prop.Address}
	if err := SignProposal(proposerKey, p); err != nil {
		t.Fatal(err)
	}
	var other *node
	for _, n := range nodes {
		if n.addr != prop.Address {
			other = n
			break
		}
	}
	if _, err := other.eng.HandleProposal(p); err == nil {
		t.Fatal("proposal whose header proposer differs was accepted")
	}
}

func TestMultiHeightSequence(t *testing.T) {
	nodes, set, _ := newCluster(t, 4)
	for h := uint64(1); h <= 5; h++ {
		if h > 1 {
			for _, n := range nodes {
				if err := n.eng.AdvanceHeight(set); err != nil {
					t.Fatal(err)
				}
			}
		}
		runHeight(t, nodes, set, h)
	}
	for _, n := range nodes {
		if n.eng.Committed().Height != 5 {
			t.Fatalf("node %s committed height %d", n.addr.Bech32(), n.eng.Committed().Height)
		}
	}
}
