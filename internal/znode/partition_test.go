package znode

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/znet"
	"znetwork/internal/ztypes"
)

// This file is the adversarial-network harness (NEXT_ACTIONS N-01). It runs a
// live four-validator network and then attacks it at the application layer
// through znet.Server.SetFilter:
//
//	scenario 1 — safety under partition: cut the network into {0,1}|{2,3};
//	             neither group holds 2/3 power, so no height may advance and no
//	             two nodes may hold different blocks at a common height.
//	scenario 3 — Byzantine equivocation during the stall: a test peer feeds
//	             conflicting prevote pairs signed by one validator over the
//	             real wire path; every node must record verifiable evidence
//	             (power counted once, second vote rejected) and no chain may
//	             ever contain a forged block.
//	scenario 2 — liveness after heal: remove the cut; all four converge past
//	             the stall on one consistent chain.
//
// The filter drops messages while keeping connections up: a black-hole
// partition, the same capability an on-path attacker has (BLOCKERS B-003).

// lineHarness is a four-validator line network (0-1-2-3), as in
// TestFourNodeNetworkReachesConsensus, plus the raw addresses and servers the
// partition hook needs.
type lineHarness struct {
	net     *zdevnet.Network
	nodes   []*Node
	servers []*znet.Server
	addrs   []string
}

func startLineHarness(t *testing.T, blockTimeMs int64) *lineHarness {
	t.Helper()
	dir := t.TempDir()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	const n = 4
	h := &lineHarness{
		net:     net,
		nodes:   make([]*Node, n),
		servers: make([]*znet.Server, n),
		addrs:   make([]string, n),
	}
	for i := 0; i < n; i++ {
		cfg := Config{
			ChainID:         net.ChainID,
			DataDir:         filepath.Join(dir, net.Nodes[i].Name),
			Genesis:         net.Genesis,
			Validator:       net.Nodes[i].Key,
			Moniker:         net.Nodes[i].Name,
			BlockTimeMs:     blockTimeMs,
			MaxTxsPerBlock:  100,
			CheckInvariants: true,
		}
		node, err := NewNode(cfg, quietLogger())
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		h.nodes[i] = node
		srv := znet.NewServer(net.ChainID, net.Nodes[i].Name, net.Nodes[i].Key.PublicKey(), func() uint64 {
			return node.Chain().Height()
		})
		h.servers[i] = srv
		addr, err := srv.Listen("127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen %d: %v", i, err)
		}
		h.addrs[i] = addr
		node.AttachNetwork(srv)
	}
	seeds := map[int][]string{
		0: {h.addrs[1]},
		1: {h.addrs[0], h.addrs[2]},
		2: {h.addrs[1], h.addrs[3]},
		3: {h.addrs[2]},
	}
	for i, node := range h.nodes {
		node.DialSeeds(seeds[i])
	}
	for _, node := range h.nodes {
		node.Start()
	}
	t.Cleanup(func() {
		for _, node := range h.nodes {
			node.Stop()
		}
		for _, srv := range h.servers {
			srv.Close()
		}
	})
	return h
}

// cutGroups installs a symmetric partition: messages whose remote peer sits in
// a different group are dropped in both directions by both endpoints. Unknown
// peers (the byzantine test peer) are never cut. The returned counter proves
// the cut engaged; the returned function heals it.
func (h *lineHarness) cutGroups(t *testing.T, groups []int) (*atomic.Int64, func()) {
	t.Helper()
	if len(groups) != len(h.servers) {
		t.Fatalf("groups=%d servers=%d", len(groups), len(h.servers))
	}
	idxByPeer := make(map[string]int, len(h.net.Nodes))
	for i := range h.net.Nodes {
		idxByPeer[h.net.Nodes[i].Key.PublicKey().String()] = i
	}
	var dropped atomic.Int64
	for i, srv := range h.servers {
		i := i
		srv.SetFilter(func(p *znet.Peer, inbound bool, typ uint8) bool {
			j, ok := idxByPeer[p.Handshake.PubKey.String()]
			if !ok {
				return false // not part of the topology under test (e.g. the byzantine peer)
			}
			if groups[i] != groups[j] {
				dropped.Add(1)
				return true
			}
			return false
		})
	}
	heal := func() {
		for _, srv := range h.servers {
			srv.SetFilter(nil)
		}
	}
	return &dropped, heal
}

// minHeight is the lowest chain height across all nodes.
func (h *lineHarness) minHeight() uint64 {
	min := h.nodes[0].Chain().Height()
	for _, n := range h.nodes[1:] {
		if got := n.Chain().Height(); got < min {
			min = got
		}
	}
	return min
}

// assertNoForks is the safety property: for every height every node has, all
// nodes hold the identical block. A single divergent block fails the test.
func (h *lineHarness) assertNoForks(t *testing.T, phase string) {
	t.Helper()
	upTo := h.minHeight()
	if upTo == 0 {
		t.Fatalf("[%s] no committed block to compare", phase)
	}
	ref := make(map[uint64]zcrypto.Hash, upTo)
	for height := uint64(1); height <= upTo; height++ {
		b, ok := h.nodes[0].Chain().Block(height)
		if !ok {
			t.Fatalf("[%s] node 0 missing block %d", phase, height)
		}
		ref[height] = b.Header.Hash()
	}
	for i, n := range h.nodes {
		for height := uint64(1); height <= upTo; height++ {
			b, ok := n.Chain().Block(height)
			if !ok {
				t.Fatalf("[%s] node %d missing block %d (min height %d)", phase, i, height, upTo)
			}
			if got := b.Header.Hash(); got != ref[height] {
				t.Fatalf("[%s] fork at height %d: node %d has %s, node 0 has %s",
					phase, height, i, got.Short(), ref[height].Short())
			}
		}
	}
}

// forgePrevote builds a valid prevote signed by key for (height, round).
func forgePrevote(t *testing.T, chainID string, key *zcrypto.PrivateKey, height uint64, round uint32, blockID zcrypto.Hash) zconsensus.Vote {
	t.Helper()
	v := zconsensus.Vote{
		ChainID: chainID, Height: height, Round: round, Step: zconsensus.StepPrevote,
		BlockID: blockID, Voter: key.Address(), TimeMs: time.Now().UnixMilli(),
	}
	if err := zconsensus.SignVote(key, &v); err != nil {
		t.Fatalf("sign forged vote: %v", err)
	}
	return v
}

// evidenceCount counts equivocation entries for one voter.
func evidenceCount(evs []zconsensus.Equivocation, voter ztypes.Address) int {
	c := 0
	for _, ev := range evs {
		if ev.Voter == voter {
			c++
		}
	}
	return c
}

// verifyEvidence checks that every evidence entry for voter is self-contained:
// two distinct block ids, both votes carrying a signature that verifies under
// the voter's address.
func verifyEvidence(t *testing.T, phase string, nodeIdx int, evs []zconsensus.Equivocation, voter ztypes.Address) {
	t.Helper()
	for _, ev := range evs {
		if ev.Voter != voter {
			continue
		}
		if ev.First.BlockID == ev.Second.BlockID {
			t.Fatalf("[%s] node %d: evidence for equal block ids", phase, nodeIdx)
		}
		for _, v := range []zconsensus.Vote{ev.First, ev.Second} {
			if err := zcrypto.VerifyAddressSignature(v.Voter, v.PubKey, zconsensus.VoteDomain, zconsensus.VoteMsg(&v), v.Signature); err != nil {
				t.Fatalf("[%s] node %d: evidence vote not independently verifiable: %v", phase, nodeIdx, err)
			}
		}
	}
}

// TestAdversarialPartitionHarness runs the three N-01 scenarios against one
// live network: partition (safety), Byzantine equivocation during the stall,
// heal (liveness).
func TestAdversarialPartitionHarness(t *testing.T) {
	h := startLineHarness(t, 20)

	// ---- baseline: a healthy network commits consistently ------------------
	waitAllHeights(t, h.nodes, 5, 45*time.Second)
	h.assertNoForks(t, "baseline")

	// ---- scenario 1: partition {0,1} | {2,3} ------------------------------
	dropped, heal := h.cutGroups(t, []int{0, 0, 1, 1})
	// Force immediate cross-group traffic so the counter proves the cut is
	// engaged rather than waiting for the next gossip round.
	for _, n := range h.nodes {
		n.RefreshStatus()
	}
	deadline := time.Now().Add(15 * time.Second)
	for dropped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dropped.Load() == 0 {
		t.Fatal("partition never engaged: no cross-group message was dropped")
	}
	// Let any commit that completed locally just before the cut land, then
	// freeze the view.
	time.Sleep(300 * time.Millisecond)
	startH := make([]uint64, len(h.nodes))
	for i, n := range h.nodes {
		startH[i] = n.Chain().Height()
	}
	// Stall window: ~35 block times. Neither group holds 2/3 of the power
	// (2 of 4 validators), so no height may advance on either side.
	time.Sleep(750 * time.Millisecond)
	for i, n := range h.nodes {
		if got := n.Chain().Height(); got != startH[i] {
			t.Fatalf("scenario 1 (safety): node %d advanced %d -> %d during partition", i, startH[i], got)
		}
	}
	h.assertNoForks(t, "during partition")

	// ---- scenario 3: Byzantine validator feeds conflicting votes ----------
	// The engines are pinned at chainHeight+1 on both sides of the cut, which
	// makes the attack deterministic. A test peer speaks the real wire path
	// (MsgVote -> decode -> InjectVote -> gossip -> engine).
	byzKey, err := zdevnet.DeterministicKey("n01-byzantine-attacker")
	if err != nil {
		t.Fatal(err)
	}
	byz := znet.NewServer(h.net.ChainID, "byzantine", byzKey.PublicKey(), func() uint64 { return 0 })
	if _, err := byz.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer byz.Close()
	byzPeers := make([]*znet.Peer, len(h.nodes))
	for i, addr := range h.addrs {
		p, err := byz.Dial(addr)
		if err != nil {
			t.Fatalf("byzantine dial to node %d: %v", i, err)
		}
		byzPeers[i] = p
	}

	rawA := zcrypto.Digest("znet-test/fork", []byte("forged-block-A"))
	rawB := zcrypto.Digest("znet-test/fork", []byte("forged-block-B"))
	rawC := zcrypto.Digest("znet-test/fork", []byte("forged-block-C"))
	rawD := zcrypto.Digest("znet-test/fork", []byte("forged-block-D"))
	val0 := h.net.Nodes[0].Address
	val0Key := h.net.Nodes[0].Key

	// Feed each node conflicting prevote pairs for ITS current height/round —
	// signed by validator 0's key, transported by the byzantine peer — until
	// every engine records evidence (or the deadline expires).
	attackDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(attackDeadline) {
		pending := false
		for i, n := range h.nodes {
			if evidenceCount(n.Engine().Evidence(), val0) > 0 {
				continue
			}
			pending = true
			eng := n.Engine()
			height := n.Chain().Height() + 1
			round := eng.Round()
			v1 := forgePrevote(t, h.net.ChainID, val0Key, height, round, zconsensus.SetBlockID(height, rawA))
			v2 := forgePrevote(t, h.net.ChainID, val0Key, height, round, zconsensus.SetBlockID(height, rawB))
			_ = byzPeers[i].Send(znet.MsgVote, znet.EncodeVote(&v1))
			_ = byzPeers[i].Send(znet.MsgVote, znet.EncodeVote(&v2))
		}
		if !pending {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i, n := range h.nodes {
		if evidenceCount(n.Engine().Evidence(), val0) == 0 {
			t.Fatalf("scenario 3: node %d recorded no equivocation evidence for validator 0", i)
		}
		verifyEvidence(t, "equivocation", i, n.Engine().Evidence(), val0)
	}

	// Power counted once: feed a FRESH conflicting pair straight into one
	// engine. The first vote may be counted or rejected, but the second —
	// a different block id from the same validator at the same
	// (height, round, step) — must be rejected with ErrEquivocation, which
	// happens before the vote is added to the set, so the validator's power
	// can never be counted twice.
	target := h.nodes[1]
	eng := target.Engine()
	height := target.Chain().Height() + 1
	round := eng.Round()
	vC := forgePrevote(t, h.net.ChainID, val0Key, height, round, zconsensus.SetBlockID(height, rawC))
	vD := forgePrevote(t, h.net.ChainID, val0Key, height, round, zconsensus.SetBlockID(height, rawD))
	if _, err := eng.HandleVote(vC); err != nil && !errors.Is(err, zconsensus.ErrEquivocation) {
		t.Fatalf("scenario 3: first fresh vote: %v", err)
	}
	if _, err := eng.HandleVote(vD); !errors.Is(err, zconsensus.ErrEquivocation) {
		t.Fatalf("scenario 3: second conflicting vote must be rejected as equivocation, got: %v", err)
	}
	if evidenceCount(eng.Evidence(), val0) == 0 {
		t.Fatal("scenario 3: synchronous equivocation left no evidence")
	}

	// ---- scenario 2: heal and prove liveness ------------------------------
	heal()
	stallTop := startH[0]
	for _, v := range startH {
		if v > stallTop {
			stallTop = v
		}
	}
	targetHeight := stallTop + 5
	waitAllHeights(t, h.nodes, targetHeight, 60*time.Second)
	h.assertNoForks(t, "after heal")

	// The conflicting (forged) blocks were never adopted by any chain, at any
	// height, on any node.
	for i, n := range h.nodes {
		for height := uint64(1); height <= n.Chain().Height(); height++ {
			b, ok := n.Chain().Block(height)
			if !ok {
				t.Fatalf("node %d missing block %d", i, height)
			}
			got := b.Header.Hash()
			if got == rawA || got == rawB || got == rawC || got == rawD {
				t.Fatalf("scenario 3: node %d adopted the forged block %s at height %d", i, got.Short(), height)
			}
		}
	}

	// Connections survived the cut (black hole, not link down).
	for i, s := range h.servers {
		if s.PeerCount() == 0 {
			t.Fatalf("node %d lost all peers across the partition", i)
		}
	}
}
