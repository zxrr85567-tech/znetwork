package zindexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/zexec"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// builder produces a small real chain by executing real transactions through the
// real execution engine — no synthetic blocks.
type builder struct {
	t     *testing.T
	eng   *zexec.Engine
	state *zstate.State
	chain []*ztypes.Block
	net   *zdevnet.Network
	rcpts [][]*ztypes.Receipt
	h     uint64
}

func newBuilder(t *testing.T) *builder {
	t.Helper()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	eng := zexec.NewEngine(net.ChainID)
	st, err := eng.InitChain(net.Genesis)
	if err != nil {
		t.Fatal(err)
	}
	b := &builder{t: t, eng: eng, state: st, net: net, h: 0}
	gb := &ztypes.Block{}
	gb.Header.Version = ztypes.ProtocolVersion
	gb.Header.ChainID = net.ChainID
	gb.Header.Height = 0
	gb.Header.TimeMs = net.Genesis.TimeMs
	gb.Header.StateRoot = st.Root()
	st.SetHeight(0)
	st.SetBlockHash(gb.Header.Hash())
	b.chain = append(b.chain, gb)
	b.rcpts = append(b.rcpts, nil)
	return b
}

// advance executes n transactions and returns the new blocks.
func (b *builder) advance(txsPerBlock int) *ztypes.Block {
	b.t.Helper()
	b.h++
	params, err := b.state.Params()
	if err != nil {
		b.t.Fatal(err)
	}
	ctx := &zexec.BlockContext{Height: b.h, TimeMs: b.net.Genesis.TimeMs + int64(b.h)*1000,
		Proposer: b.net.Nodes[0].Address, Params: params}
	if err := b.eng.BeginBlock(ctx, b.state); err != nil {
		b.t.Fatal(err)
	}
	var included []*ztypes.Tx
	var receipts []*ztypes.Receipt
	for i := 0; i < txsPerBlock; i++ {
		kp := b.net.Users[i%len(b.net.Users)].Key
		to := b.net.Nodes[i%len(b.net.Nodes)].Address
		acc, _ := b.state.GetAccount(kp.Address())
		tx := &ztypes.Tx{
			ChainID: b.net.ChainID, Type: ztypes.TxTransfer, Sender: kp.Address(),
			Nonce: acc.Nonce, GasLimit: 200_000, GasPrice: 1,
			TimeMs:  b.net.Genesis.TimeMs + int64(b.h)*1000,
			Payload: (&ztypes.TransferPayload{To: to, Amount: 1 * ztypes.CoinScale}).Encode(),
		}
		if err := tx.Sign(kp); err != nil {
			b.t.Fatal(err)
		}
		rc := b.eng.ExecuteTx(ctx, b.state, tx)
		if rc.Status != ztypes.ReceiptSuccess {
			b.t.Fatalf("tx failed: %s", rc.Error)
		}
		included = append(included, tx)
		receipts = append(receipts, rc)
	}
	if _, err := b.eng.EndBlock(ctx, b.state); err != nil {
		b.t.Fatal(err)
	}
	prev := b.chain[len(b.chain)-1]
	blk := &ztypes.Block{}
	blk.Header.Version = ztypes.ProtocolVersion
	blk.Header.ChainID = b.net.ChainID
	blk.Header.Height = b.h
	blk.Header.TimeMs = ctx.TimeMs
	blk.Header.PrevHash = prev.Header.Hash()
	blk.Header.Proposer = b.net.Nodes[0].Address
	blk.Header.TxCount = uint32(len(included))
	blk.Header.StateRoot = b.state.Root()
	blk.Txs = included
	b.state.SetHeight(b.h)
	b.state.SetBlockHash(blk.Header.Hash())
	b.chain = append(b.chain, blk)
	b.rcpts = append(b.rcpts, receipts)
	return blk
}

func (b *builder) source() ChainSource { return &sliceChain{blocks: b.chain, rcpts: b.rcpts} }

type sliceChain struct {
	blocks []*ztypes.Block
	rcpts  [][]*ztypes.Receipt
}

func (s *sliceChain) Height() uint64 { return uint64(len(s.blocks)) - 1 }
func (s *sliceChain) Block(h uint64) (*ztypes.Block, bool) {
	if h >= uint64(len(s.blocks)) {
		return nil, false
	}
	return s.blocks[h], true
}

// Receipts lets the indexer index gas/fees/status the way a real node does.
func (s *sliceChain) Receipts(h uint64) ([]*ztypes.Receipt, bool) {
	if int(h) >= len(s.rcpts) {
		return nil, false
	}
	return s.rcpts[h], true
}

func TestIndexMatchesChain(t *testing.T) {
	b := newBuilder(t)
	for i := 0; i < 5; i++ {
		b.advance(3)
	}
	idx, err := Open(t.TempDir(), b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	if idx.Height() != 5 {
		t.Fatalf("indexed height %d", idx.Height())
	}
	if idx.Count() != 15 {
		t.Fatalf("indexed %d transactions, want 15", idx.Count())
	}
	// Every indexed transaction must be findable by id and carry receipt data.
	last := b.chain[5]
	if len(last.Txs) == 0 {
		t.Fatal("no transactions in block 5")
	}
	rec, ok := idx.Transaction(last.Txs[0].ID().String())
	if !ok {
		t.Fatal("transaction not indexed")
	}
	if rec.Height != 5 || rec.FeeUZ == 0 || rec.Status != ztypes.ReceiptSuccess {
		t.Fatalf("record = %+v", rec)
	}
	// Address lookup.
	addr := b.net.Users[0].Address.Bech32()
	if got := idx.ByAddress(addr); len(got) == 0 {
		t.Fatalf("no records for %s", addr)
	}
	if got := idx.Recent(5); len(got) != 5 {
		t.Fatalf("recent = %d", len(got))
	}
}

func TestIndexSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	b := newBuilder(t)
	for i := 0; i < 4; i++ {
		b.advance(2)
	}
	idx, err := Open(dir, b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	count := idx.Count()
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx2, err := Open(dir, b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	if idx2.Count() != count {
		t.Fatalf("records after restart = %d, want %d", idx2.Count(), count)
	}
	if idx2.Height() != 4 {
		t.Fatalf("height after restart = %d", idx2.Height())
	}
	// Incremental sync of nothing must be a no-op.
	if err := idx2.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	if idx2.Count() != count {
		t.Fatal("duplicate records after re-sync")
	}
}

func TestIndexHandlesReorg(t *testing.T) {
	dir := t.TempDir()
	b := newBuilder(t)
	for i := 0; i < 3; i++ {
		b.advance(2)
	}
	idx, err := Open(dir, b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	beforeCount := idx.Count()
	if beforeCount != 6 || idx.Height() != 3 {
		t.Fatalf("pre-reorg index = %d records at height %d", beforeCount, idx.Height())
	}

	// Build a competing chain: identical blocks up to height 1, then a DIFFERENT
	// block 2 (4 transactions instead of 2) and a block 3.
	alt := newBuilderWithKeys(t, b.net)
	alt.advance(2) // height 1: same content as b
	alt.advance(4) // height 2: different content
	alt.advance(1) // height 3: different content

	if err := idx.Sync(alt.source()); err != nil {
		t.Fatalf("reorg sync: %v", err)
	}
	if idx.Height() != 3 {
		t.Fatalf("height after reorg = %d, want 3", idx.Height())
	}
	want := len(alt.chain[1].Txs) + len(alt.chain[2].Txs) + len(alt.chain[3].Txs)
	if idx.Count() != want {
		t.Fatalf("records after reorg = %d, want %d", idx.Count(), want)
	}
	// Every transaction in the canonical chain must be present...
	for h := uint64(1); h <= 3; h++ {
		for _, tx := range alt.chain[h].Txs {
			rec, ok := idx.Transaction(tx.ID().String())
			if !ok {
				t.Fatalf("canonical transaction at height %d missing after reorg", h)
			}
			if rec.BlockHash != alt.chain[h].Header.Hash().String() {
				t.Fatalf("transaction indexed against the wrong block hash")
			}
		}
	}
	// ...and transactions from the orphaned blocks that are not in the new chain
	// must be gone.
	canonical := map[string]bool{}
	for h := uint64(1); h <= 3; h++ {
		for _, tx := range alt.chain[h].Txs {
			canonical[tx.ID().String()] = true
		}
	}
	for h := uint64(2); h <= 3; h++ {
		for _, tx := range b.chain[h].Txs {
			id := tx.ID().String()
			if canonical[id] {
				continue
			}
			if _, ok := idx.Transaction(id); ok {
				t.Fatalf("orphaned transaction %s still indexed after reorg", id)
			}
		}
	}
}

// newBuilderWithKeys builds a chain from the same genesis and keys as net, so two
// builders can produce competing (or identical) blocks.
func newBuilderWithKeys(t *testing.T, net *zdevnet.Network) *builder {
	t.Helper()
	eng := zexec.NewEngine(net.ChainID)
	st, err := eng.InitChain(net.Genesis)
	if err != nil {
		t.Fatal(err)
	}
	b := &builder{t: t, eng: eng, state: st, net: net}
	gb := &ztypes.Block{}
	gb.Header.Version = ztypes.ProtocolVersion
	gb.Header.ChainID = net.ChainID
	gb.Header.Height = 0
	gb.Header.TimeMs = net.Genesis.TimeMs
	gb.Header.StateRoot = st.Root()
	st.SetHeight(0)
	st.SetBlockHash(gb.Header.Hash())
	b.chain = append(b.chain, gb)
	b.rcpts = append(b.rcpts, nil)
	return b
}

func TestIndexLogCorruptionIsReported(t *testing.T) {
	dir := t.TempDir()
	b := newBuilder(t)
	b.advance(2)
	idx, err := Open(dir, b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	idx.Close()
	// Corrupt a complete line (not just the tail).
	path := filepath.Join(dir, logName)
	data, err := osRead(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 10 {
		t.Fatal("log too short for this test")
	}
	// Corrupt a byte inside the FIRST complete record's JSON body.
	data[12] = '\x00'
	if err := osWrite(path, data); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, b.net.ChainID); err == nil {
		t.Fatal("corrupt index log was accepted")
	}
}

func TestIndexRejectsForeignChain(t *testing.T) {
	dir := t.TempDir()
	idx, err := Open(dir, "chain-a")
	if err != nil {
		t.Fatal(err)
	}
	idx.Close()
	if _, err := Open(dir, "chain-b"); err == nil {
		t.Fatal("index for a different chain was accepted")
	}
}

// TestBuildIsAFunctionOfChain asserts build(fresh) == build(incremental).
func TestBuildIsAFunctionOfChain(t *testing.T) {
	b := newBuilder(t)
	for i := 0; i < 4; i++ {
		b.advance(3)
	}
	fresh, err := Open(t.TempDir(), b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	incr, err := Open(t.TempDir(), b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	defer incr.Close()
	// incremental: index height by height
	for h := uint64(1); h <= b.source().Height(); h++ {
		blk, _ := b.source().Block(h)
		var rcpts []*ztypes.Receipt
		_ = rcpts
		if err := incr.IndexBlock(blk, b.rcpts[h]); err != nil {
			t.Fatal(err)
		}
	}
	if fresh.Count() != incr.Count() {
		t.Fatalf("counts differ: %d vs %d", fresh.Count(), incr.Count())
	}
	for id, rec := range fresh.byID {
		other, ok := incr.Transaction(id)
		if !ok {
			t.Fatalf("record %s missing in incremental index", id)
		}
		if other.Height != rec.Height || other.FeeUZ != rec.FeeUZ || other.BlockHash != rec.BlockHash {
			t.Fatalf("record mismatch for %s: %+v vs %+v", id, rec, other)
		}
	}
}

func TestConcurrentReadsAreSafe(t *testing.T) {
	b := newBuilder(t)
	for i := 0; i < 3; i++ {
		b.advance(4)
	}
	idx, err := Open(t.TempDir(), b.net.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Sync(b.source()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_ = idx.Recent(10)
				_ = idx.ByAddress(b.net.Users[0].Address.Bech32())
				_ = idx.Stats()
				_ = idx.Height()
			}
		}()
	}
	deadline := time.After(10 * time.Second)
	for i := 0; i < 8; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("timeout in concurrent reader test")
		}
	}
}

func osRead(p string) ([]byte, error) { return os.ReadFile(p) }

func osWrite(p string, b []byte) error { return os.WriteFile(p, b, 0o644) }

var _ = zcrypto.Hash{}
