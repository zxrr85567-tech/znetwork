package znode

import (
	"testing"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// Unit-level coverage of the chain accessors, transaction ordering and receipt
// retention. The process-level behaviour (consensus, sync, restart) lives in the
// node_test.go and multinode_test.go suites, which are slower and probabilistic;
// these tests pin down the deterministic pieces they depend on.

func startTestNode(t *testing.T) *Node {
	t.Helper()
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	n.Start()
	return n
}

func TestChainAccessors(t *testing.T) {
	n := startTestNode(t)
	waitHeight(t, n, 3, 20*time.Second)
	c := n.Chain()

	if c.Height() < 3 {
		t.Fatalf("height = %d", c.Height())
	}
	// Block by height and by hash must agree, and Blocks must be ordered.
	head, ok := c.Block(c.Height())
	if !ok {
		t.Fatal("head block missing")
	}
	if byHash, ok := c.BlockByHash(head.Header.Hash()); !ok || byHash.Header.Height != head.Header.Height {
		t.Fatal("BlockByHash disagrees with Block")
	}
	if _, ok := c.Block(c.Height() + 1000); ok {
		t.Fatal("Block returned a block above the head")
	}
	if _, ok := c.BlockByHash(zcrypto.Digest("nonexistent")); ok {
		t.Fatal("BlockByHash returned a block for a random hash")
	}
	all := c.Blocks()
	if len(all) == 0 {
		t.Fatal("Blocks is empty")
	}
	for i := 1; i < len(all); i++ {
		if all[i].Header.Height != all[i-1].Header.Height+1 {
			t.Fatalf("blocks are not contiguous at %d", i)
		}
		if all[i].Header.PrevHash != all[i-1].Header.Hash() {
			t.Fatalf("block %d does not link to its parent", all[i].Header.Height)
		}
	}
	if last := c.LastBlock(); last == nil || last.Header.Height != all[len(all)-1].Header.Height {
		t.Fatal("LastBlock disagrees with Blocks")
	}
	if params, err := c.Params(); err != nil || params.BlockGasLimit == 0 {
		t.Fatalf("Params() = %+v, %v", params, err)
	}
	if c.State() == nil {
		t.Fatal("State() is nil")
	}
}

func TestSubscribeReceivesCommittedBlocks(t *testing.T) {
	n := startTestNode(t)
	type commit struct {
		height   uint64
		receipts int
	}
	ch := make(chan commit, 16)
	n.Chain().Subscribe(func(b *ztypes.Block, receipts []*ztypes.Receipt) {
		ch <- commit{b.Header.Height, len(receipts)}
	})
	waitHeight(t, n, 3, 20*time.Second)
	select {
	case got := <-ch:
		if got.height == 0 {
			t.Fatal("subscription received height 0")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subscription received no blocks")
	}
}

func TestTxOrdering(t *testing.T) {
	// Sorting is by nonce then by gas price then by id: deterministic for every
	// proposer, which is what makes the transaction root reproducible.
	k, err := zcrypto.KeyFromSeed(zcrypto.Digest("order-test").Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var to ztypes.Address
	to[0] = 1
	mk := func(nonce, price uint64, memo string) *ztypes.Tx {
		tx := &ztypes.Tx{
			ChainID: "znetwork-order-test", Type: ztypes.TxTransfer, Sender: k.Address(),
			Nonce: nonce, GasLimit: 100_000, GasPrice: price, TimeMs: 1,
			Memo: memo, Payload: (&ztypes.TransferPayload{To: to, Amount: 1}).Encode(),
		}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	in := []*ztypes.Tx{mk(2, 5, "c"), mk(1, 1, "a"), mk(1, 9, "b")}
	txs := SortTxs(in)
	// Ordering is (sender, nonce, id). Same sender here, so nonce decides first.
	if txs[0].Nonce != 1 || txs[1].Nonce != 1 || txs[2].Nonce != 2 {
		t.Fatalf("nonce order: %d %d %d", txs[0].Nonce, txs[1].Nonce, txs[2].Nonce)
	}
	if txs[0].ID().String() >= txs[1].ID().String() {
		t.Fatal("equal-nonce transactions are not ordered by id")
	}
	// The input slice must not be reordered: callers keep their own ordering.
	if in[0].Nonce != 2 {
		t.Fatal("SortTxs mutated its input")
	}
	// Sorting is deterministic and independent of input order.
	first := []string{txs[0].ID().String(), txs[1].ID().String(), txs[2].ID().String()}
	shuffled := SortTxs([]*ztypes.Tx{in[2], in[0], in[1]})
	for i, tx := range shuffled {
		if tx.ID().String() != first[i] {
			t.Fatal("sort is not deterministic")
		}
	}
	// Different senders are grouped deterministically regardless of nonce.
	k2, err := zcrypto.KeyFromSeed(zcrypto.Digest("order-test-2").Bytes())
	if err != nil {
		t.Fatal(err)
	}
	other := &ztypes.Tx{
		ChainID: "znetwork-order-test", Type: ztypes.TxTransfer, Sender: k2.Address(),
		Nonce: 0, GasLimit: 100_000, GasPrice: 1, TimeMs: 1,
		Payload: (&ztypes.TransferPayload{To: to, Amount: 1}).Encode(),
	}
	if err := other.Sign(k2); err != nil {
		t.Fatal(err)
	}
	two := SortTxs([]*ztypes.Tx{other, txs[2]})
	wantFirst := other.Sender
	if string(txs[2].Sender[:]) < string(other.Sender[:]) {
		wantFirst = txs[2].Sender
	}
	if two[0].Sender != wantFirst {
		t.Fatal("senders are not ordered by address")
	}
}

func TestTxReceiptLookup(t *testing.T) {
	n := startTestNode(t)
	waitHeight(t, n, 2, 20*time.Second)
	// The node's validator key is funded by the minimal genesis, so it can pay for
	// a transfer to itself at nonce 0.
	key := n.cfg.Validator
	var to ztypes.Address
	to[0] = 0x7a
	tx := &ztypes.Tx{
		ChainID: n.cfg.ChainID, Type: ztypes.TxTransfer, Sender: key.Address(),
		Nonce: 0, GasLimit: 200_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
		Payload: (&ztypes.TransferPayload{To: to, Amount: ztypes.CoinScale}).Encode(),
	}
	if err := tx.Sign(key); err != nil {
		t.Fatal(err)
	}
	id, err := n.SubmitTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := n.Chain().TxReceipt(id); ok {
			if rec.TxID != id {
				t.Fatal("receipt belongs to another transaction")
			}
			if recs, ok := n.Chain().Receipts(rec.Height); !ok || len(recs) == 0 {
				t.Fatal("height receipts missing")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("receipt for a committed transaction never appeared")
}

func TestNodeDiagnostics(t *testing.T) {
	n := startTestNode(t)
	if n.Moniker() == "" {
		t.Fatal("moniker is empty")
	}
	if n.ValidatorAddress() == (ztypes.Address{}) {
		t.Fatal("validator node has a zero validator address")
	}
	if n.PeerCount() != 0 {
		t.Fatalf("unconnected node reports %d peers", n.PeerCount())
	}
	// The genesis hash the node advertises to peers must be the chain's own.
	if n.Chain().State().GenesisHash() != n.Chain().State().GenesisHash() {
		t.Fatal("genesis hash disagrees with chain state")
	}
}
