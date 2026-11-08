package znode

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/zexec"
	"znetwork/internal/znet"
	"znetwork/internal/ztypes"
)

// TestFourNodeNetworkReachesConsensus is the end-to-end multi-node test: four
// independent nodes, each with its own data directory, its own TCP listener and its
// own consensus engine, connected in a line (0-1-2-3) so messages must be relayed.
// It asserts that all four nodes commit the same blocks and that a transaction
// submitted to one node reaches every node's state through the real pipeline:
// SubmitTx -> mempool -> gossip -> proposal -> consensus -> block -> state.
func TestFourNodeNetworkReachesConsensus(t *testing.T) {
	dir := t.TempDir()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	const nodeCount = 4
	servers := make([]*znet.Server, nodeCount)
	nodes := make([]*Node, nodeCount)
	addrs := make([]string, nodeCount)

	for i := 0; i < nodeCount; i++ {
		cfg := Config{
			ChainID:         net.ChainID,
			DataDir:         filepath.Join(dir, net.Nodes[i].Name),
			Genesis:         net.Genesis,
			Validator:       net.Nodes[i].Key,
			Moniker:         net.Nodes[i].Name,
			BlockTimeMs:     20,
			MaxTxsPerBlock:  100,
			CheckInvariants: true,
		}
		var lg = quietLogger()
		if os.Getenv("ZNODE_TEST_LOG") != "" {
			lg = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		}
		n, err := NewNode(cfg, lg)
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		nodes[i] = n
		servers[i] = znet.NewServer(net.ChainID, net.Nodes[i].Name, net.Nodes[i].Key.PublicKey(), func() uint64 {
			return n.Chain().Height()
		})
		addr, err := servers[i].Listen("127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen %d: %v", i, err)
		}
		addrs[i] = addr
		n.AttachNetwork(servers[i])
	}
	defer func() {
		for _, n := range nodes {
			n.Stop()
		}
		for _, s := range servers {
			s.Close()
		}
	}()

	// Line topology: 0 - 1 - 2 - 3. Proposals and votes must be relayed by 1 and 2,
	// which means each node maintains its links through the reconnect loop.
	seedLists := map[int][]string{
		0: {addrs[1]},
		1: {addrs[0], addrs[2]},
		2: {addrs[1], addrs[3]},
		3: {addrs[2]},
	}

	for i, n := range nodes {
		n.DialSeeds(seedLists[i])
	}
	for _, n := range nodes {
		n.Start()
	}

	const target = 4
	waitAllHeights(t, nodes, target, 45*time.Second)

	// All nodes must agree on the head hash.
	head := nodes[0].Chain().LastBlock().Header.Hash()
	for i, n := range nodes {
		if got := n.Chain().LastBlock().Header.Hash(); got != head {
			t.Fatalf("node %d head %s != node 0 head %s", i, got.Short(), head.Short())
		}
	}

	// Submit a transfer to node 0 and require it to appear on every node.
	user := net.Users[0]
	recipient := net.Users[1].Address
	acc, err := nodes[0].Chain().State().GetAccount(user.Address)
	if err != nil {
		t.Fatal(err)
	}
	amount := uint64(7 * ztypes.CoinScale)
	tx := &ztypes.Tx{
		ChainID: net.ChainID, Type: ztypes.TxTransfer, Sender: user.Address,
		Nonce: acc.Nonce, GasLimit: 200_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
		Payload: (&ztypes.TransferPayload{To: recipient, Amount: amount}).Encode(),
	}
	if err := tx.Sign(user.Key); err != nil {
		t.Fatal(err)
	}
	id, err := nodes[0].SubmitTx(tx)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	nodes[0].BroadcastTx(tx)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, n := range nodes {
			if _, ok := n.Chain().TxHeight(id); !ok {
				all = false
				break
			}
		}
		if all {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, n := range nodes {
		if _, ok := n.Chain().TxHeight(id); !ok {
			t.Fatalf("node %d never saw transaction %s (height %d)", i, id.Short(), n.Chain().Height())
		}
		a, _ := n.Chain().State().GetAccount(recipient)
		if a.Balance < amount {
			t.Fatalf("node %d recipient balance %d < %d", i, a.Balance, amount)
		}
		if err := zexec.VerifySupply(n.Chain().State()); err != nil {
			t.Fatalf("node %d accounting: %v", i, err)
		}
	}
	// Peers must actually be connected.
	for i, s := range servers {
		if s.PeerCount() == 0 {
			t.Fatalf("node %d has no peers", i)
		}
	}
}

func waitAllHeights(t *testing.T, nodes []*Node, target uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, n := range nodes {
			if n.Chain().Height() < target {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, n := range nodes {
		t.Logf("node %d at height %d (peer count %d)", i, n.Chain().Height(), n.PeerCount())
	}
	t.Fatalf("not all nodes reached height %d", target)
}

// TestNonValidatorNodeSyncsFromNetwork checks the sync path: a full node (no
// validator key) joins a running validator network and is caught up by block
// catch-up, never proposing or voting.
func TestNonValidatorNodeSyncsFromNetwork(t *testing.T) {
	dir := t.TempDir()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Single-validator network from node 0's perspective; node 1 runs as a full
	// node with the same genesis.
	cfg0 := Config{
		ChainID: net.ChainID, DataDir: filepath.Join(dir, "val"),
		Genesis: minimalGenesis(t, net, 0), Validator: net.Nodes[0].Key, Moniker: "val",
		BlockTimeMs: 20, MaxTxsPerBlock: 100, CheckInvariants: true,
	}
	val, err := NewNode(cfg0, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	srv0 := znet.NewServer(net.ChainID, "val", net.Nodes[0].Key.PublicKey(), func() uint64 { return val.Chain().Height() })
	addr0, err := srv0.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	val.AttachNetwork(srv0)
	val.Start()
	defer val.Stop()
	defer srv0.Close()
	waitHeight(t, val, 2, 15*time.Second)

	cfg1 := Config{
		ChainID: net.ChainID, DataDir: filepath.Join(dir, "full"),
		Genesis: minimalGenesis(t, net, 0), Validator: nil, Moniker: "full",
		BlockTimeMs: 20, MaxTxsPerBlock: 100, CheckInvariants: true,
	}
	full, err := NewNode(cfg1, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	srv1 := znet.NewServer(net.ChainID, "full", zcrypto.PublicKey{}, func() uint64 { return full.Chain().Height() })
	if _, err := srv1.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	full.AttachNetwork(srv1)
	full.Start()
	defer full.Stop()
	defer srv1.Close()

	if _, err := srv1.Dial(addr0); err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitHeight(t, full, 1, 30*time.Second)
	// The validator keeps producing blocks, so compare the state root AT A SHARED
	// HEIGHT rather than live heads (which move between the two reads).
	shared := full.Chain().Height()
	if shared > val.Chain().Height() {
		shared = val.Chain().Height()
	}
	if shared == 0 {
		t.Fatal("full node did not sync any block")
	}
	deadline := time.Now().Add(20 * time.Second)
	matched := false
	for time.Now().Before(deadline) {
		h := full.Chain().Height()
		if val.Chain().Height() < h {
			h = val.Chain().Height()
		}
		if h == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		fb, ok1 := full.Chain().Block(h)
		vb, ok2 := val.Chain().Block(h)
		if ok1 && ok2 && fb.Header.Hash() == vb.Header.Hash() &&
			full.Chain().State().Root() == val.Chain().State().Root() &&
			full.Chain().Height() == val.Chain().Height() {
			matched = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !matched {
		t.Fatalf("full node never matched the validator (full height %d, validator height %d)",
			full.Chain().Height(), val.Chain().Height())
	}
	if full.ValidatorAddress() != (ztypes.Address{}) {
		t.Fatal("full node claims a validator address")
	}
	if full.Engine().IsValidator() {
		t.Fatal("full node considers itself a validator")
	}
}
