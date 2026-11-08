package zstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

func testBlock(h uint64, txs int) *ztypes.Block {
	b := &ztypes.Block{}
	b.Header.Version = ztypes.ProtocolVersion
	b.Header.ChainID = "znetwork-store-test"
	b.Header.Height = h
	b.Header.TimeMs = 1_700_000_000_000 + int64(h)*1000
	b.Header.TxRoot = zcrypto.Digest("tx", []byte{byte(h)})
	b.Header.StateRoot = zcrypto.Digest("state", []byte{byte(h)})
	b.Header.GasLimit = 1_000_000
	b.Header.TxCount = uint32(txs)
	return b
}

func TestBlockLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for h := uint64(0); h < 10; h++ {
		if err := st.AppendBlock(testBlock(h, int(h))); err != nil {
			t.Fatal(err)
		}
	}
	blocks, validLen, err := st.ReadBlocksFile()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(blocks) != 10 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	if validLen <= 0 {
		t.Fatal("valid length not reported")
	}
	for i, b := range blocks {
		if b.Header.Height != uint64(i) {
			t.Fatalf("block %d has height %d", i, b.Header.Height)
		}
	}
	// A fresh open must see the same chain.
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	blocks2, _, err := st2.ReadBlocksFile()
	if err != nil || len(blocks2) != 10 {
		t.Fatalf("reopen: %d blocks, %v", len(blocks2), err)
	}
}

func TestTruncatedTailIsDetectedAndRepaired(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	for h := uint64(0); h < 5; h++ {
		if err := st.AppendBlock(testBlock(h, 1)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, blocksFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash that wrote only part of the last record.
	if err := os.Truncate(path, info.Size()-10); err != nil {
		t.Fatal(err)
	}
	blocks, validLen, err := st.ReadBlocksFile()
	if !errors.Is(err, ErrTruncatedTail) {
		t.Fatalf("expected ErrTruncatedTail, got %v", err)
	}
	if len(blocks) != 4 {
		t.Fatalf("blocks before truncation = %d, want 4", len(blocks))
	}
	if err := st.TruncateTail(validLen); err != nil {
		t.Fatal(err)
	}
	// After repair the log is clean and appendable again.
	blocks, _, err = st.ReadBlocksFile()
	if err != nil || len(blocks) != 4 {
		t.Fatalf("after repair: %d blocks, %v", len(blocks), err)
	}
	if err := st.AppendBlock(testBlock(4, 1)); err != nil {
		t.Fatal(err)
	}
	blocks, _, err = st.ReadBlocksFile()
	if err != nil || len(blocks) != 5 {
		t.Fatalf("after append: %d blocks, %v", len(blocks), err)
	}
}

func TestCorruptChecksumIsDetected(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	for h := uint64(0); h < 3; h++ {
		if err := st.AppendBlock(testBlock(h, 1)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, blocksFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the first record body (after the 4-byte length).
	data[8] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	blocks, _, err := st.ReadBlocksFile()
	if err == nil {
		t.Fatalf("corrupt record accepted (%d blocks read)", len(blocks))
	}
	if len(blocks) != 0 {
		t.Fatalf("blocks read past corruption: %d", len(blocks))
	}
}

func TestStateSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	state := zstate.New()
	params := ztypes.DefaultParams()
	state.SetParams(&params)
	state.SetChainID("znetwork-store-test")
	state.SetSupply(5_000_000)
	var a ztypes.Address
	a[0] = 7
	state.SetAccount(a, &ztypes.Account{Balance: 1234, Nonce: 3})
	state.SetHeight(4)
	state.SetBlockHash(zcrypto.Digest("head"))
	if err := st.SaveState(state); err != nil {
		t.Fatal(err)
	}
	height, ok, err := st.LatestStateHeight()
	if err != nil || !ok || height != 4 {
		t.Fatalf("latest = %d, %v, %v", height, ok, err)
	}
	loaded, err := st.LoadState(4)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Root() != state.Root() {
		t.Fatalf("state root changed across save/load")
	}
	if loaded.Height() != 4 || loaded.Supply() != 5_000_000 {
		t.Fatal("state metadata lost")
	}
	acc, _ := loaded.GetAccount(a)
	if acc.Balance != 1234 || acc.Nonce != 3 {
		t.Fatal("account lost in snapshot")
	}
	if loaded.ChainID() != "znetwork-store-test" {
		t.Fatal("chain id lost")
	}
	// A missing snapshot must be an error, not a zero state.
	if _, err := st.LoadState(99); err == nil {
		t.Fatal("missing snapshot returned no error")
	}
}

func TestStateSnapshotRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	state := zstate.New()
	params := ztypes.DefaultParams()
	state.SetParams(&params)
	state.SetHeight(2)
	state.SetBlockHash(zcrypto.Digest("h"))
	if err := st.SaveState(state); err != nil {
		t.Fatal(err)
	}
	path := st.snapshotPath(2)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadState(2); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}

func TestPruneAndDeleteStates(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	for h := uint64(1); h <= 5; h++ {
		state := zstate.New()
		params := ztypes.DefaultParams()
		state.SetParams(&params)
		state.SetHeight(h)
		state.SetBlockHash(zcrypto.Digest("h", []byte{byte(h)}))
		if err := st.SaveState(state); err != nil {
			t.Fatal(err)
		}
	}
	heights, err := st.stateHeights()
	if err != nil || len(heights) != 5 {
		t.Fatalf("snapshots = %v, %v", heights, err)
	}
	if err := st.PruneStates(2); err != nil {
		t.Fatal(err)
	}
	heights, _ = st.stateHeights()
	if len(heights) != 2 || heights[len(heights)-1] != 5 {
		t.Fatalf("after prune: %v", heights)
	}
	if err := st.DeleteStatesAbove(2); err != nil {
		t.Fatal(err)
	}
	heights, _ = st.stateHeights()
	for _, h := range heights {
		if h > 2 {
			t.Fatalf("snapshot above the fork point survived: %d", h)
		}
	}
}

func TestReceiptLog(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	mk := func(h uint64, n int) []*ztypes.Receipt {
		out := make([]*ztypes.Receipt, n)
		for i := 0; i < n; i++ {
			out[i] = &ztypes.Receipt{
				TxID: zcrypto.Digest("tx", []byte{byte(h), byte(i)}), Height: h,
				Type: ztypes.TxTransfer, Status: ztypes.ReceiptSuccess, GasUsed: uint64(i), Fee: uint64(i),
			}
		}
		return out
	}
	if err := st.AppendReceipts(1, mk(1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendReceipts(2, mk(2, 3)); err != nil {
		t.Fatal(err)
	}
	all, err := st.ReadReceipts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(all[1]) != 2 || len(all[2]) != 3 {
		t.Fatalf("receipts = %v", all)
	}
	// A truncated tail is ignored rather than failing the whole read.
	path := filepath.Join(dir, receiptsFile)
	info, _ := os.Stat(path)
	if err := os.Truncate(path, info.Size()-5); err != nil {
		t.Fatal(err)
	}
	all, err = st.ReadReceipts()
	if err != nil {
		t.Fatalf("truncated receipt log reported an error: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("receipts after truncation = %d heights", len(all))
	}
	// Rewrite makes the log canonical again.
	if err := st.RewriteReceipts(all); err != nil {
		t.Fatal(err)
	}
	all2, err := st.ReadReceipts()
	if err != nil || len(all2) != 1 {
		t.Fatalf("after rewrite: %v, %v", all2, err)
	}
}

func TestRewriteBlocks(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	for h := uint64(0); h < 6; h++ {
		if err := st.AppendBlock(testBlock(h, 1)); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a reorg: keep only blocks 0..2.
	if err := st.RewriteBlocks([]*ztypes.Block{testBlock(0, 1), testBlock(1, 1), testBlock(2, 1)}); err != nil {
		t.Fatal(err)
	}
	blocks, _, err := st.ReadBlocksFile()
	if err != nil || len(blocks) != 3 {
		t.Fatalf("after rewrite: %d blocks, %v", len(blocks), err)
	}
	// The log must still be appendable.
	if err := st.AppendBlock(testBlock(3, 1)); err != nil {
		t.Fatal(err)
	}
	blocks, _, err = st.ReadBlocksFile()
	if err != nil || len(blocks) != 4 {
		t.Fatalf("after append: %d blocks, %v", len(blocks), err)
	}
}

func TestOversizedRecordRejected(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	// Build a block that exceeds MaxRecordLen by filling it with big transactions.
	b := testBlock(1, 0)
	if err := st.AppendBlock(b); err != nil {
		t.Fatal(err)
	}
	// Directly appending an absurd length must be refused by the reader.
	path := filepath.Join(dir, blocksFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	blocks, _, err := st.ReadBlocksFile()
	if err == nil {
		t.Fatal("absurd record length accepted")
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks before the bad record = %d", len(blocks))
	}
}
