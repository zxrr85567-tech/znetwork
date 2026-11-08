// Package zindexer maintains a queryable projection of the chain (AGENT_17).
//
// The indexer is deliberately crash-safe and reorg-aware:
//
//   - Every indexed height records the block hash it indexed. On restart, or when
//     the chain head moves backwards, the indexer walks back to the last height
//     whose recorded hash still matches the canonical chain and re-indexes from
//     there. Stale entries above that point are removed.
//   - The checkpoint file is written atomically (tmp + fsync + rename), so a crash
//     never leaves a half-written index header.
//   - The index is a pure function of the chain: build(fresh) == build(incremental).
//     A property test asserts exactly that.
//
// Storage is an append-only NDJSON file plus an in-memory map. At development
// scale this is simpler and faster to verify than a database, and it avoids
// depending on sqlite3 (which is unavailable in this environment).
package zindexer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// Record is one indexed transaction.
type Record struct {
	Height    uint64         `json:"height"`
	BlockHash string         `json:"block_hash"`
	TxID      string         `json:"tx_id"`
	Type      uint8          `json:"type"`
	Sender    string         `json:"sender"`
	Nonce     uint64         `json:"nonce"`
	GasUsed   uint64         `json:"gas_used"`
	GasPrice  uint64         `json:"gas_price"`
	FeeUZ     uint64         `json:"fee_uz"`
	Status    uint8          `json:"status"`
	Error     string         `json:"error,omitempty"`
	Events    []ztypes.Event `json:"events,omitempty"`
	Contract  string         `json:"contract,omitempty"`
	TimeMs    int64          `json:"time_ms"`
}

// HeightEntry tracks the indexed result of one block.
type HeightEntry struct {
	Height    uint64 `json:"height"`
	BlockHash string `json:"block_hash"`
	TxCount   int    `json:"tx_count"`
}

// checkpoint is the persisted index header.
type checkpoint struct {
	Version  int           `json:"version"`
	ChainID  string        `json:"chain_id"`
	Height   uint64        `json:"height"`
	HeadHash string        `json:"head_hash"`
	Records  int           `json:"records"`
	Heights  []HeightEntry `json:"heights"`
}

// Index is the in-memory index plus its durable log.
type Index struct {
	mu      sync.RWMutex
	dir     string
	chainID string

	height  uint64
	head    string
	heights []HeightEntry
	byID    map[string]Record
	byAddr  map[string][]string
	byType  map[uint8][]string
	logFile *os.File
}

const (
	logName        = "tx-index.ndjson"
	checkpointName = "index-checkpoint.json"
	maxRecords     = 500_000
)

// Open loads or creates an index in dir.
func Open(dir, chainID string) (*Index, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	idx := &Index{
		dir:     dir,
		chainID: chainID,
		byID:    map[string]Record{},
		byAddr:  map[string][]string{},
		byType:  map[uint8][]string{},
	}
	cpPath := filepath.Join(dir, checkpointName)
	if raw, err := os.ReadFile(cpPath); err == nil {
		var cp checkpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, fmt.Errorf("zindexer: corrupt checkpoint: %w", err)
		}
		if cp.ChainID != chainID {
			return nil, fmt.Errorf("zindexer: index belongs to chain %q, not %q", cp.ChainID, chainID)
		}
		idx.height = cp.Height
		idx.head = cp.HeadHash
		idx.heights = cp.Heights
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := idx.loadLog(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, logName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	idx.logFile = f
	return idx, nil
}

// loadLog replays the record log. Records above the checkpointed height (from a
// crash between log append and checkpoint write) are simply kept: they are
// canonical unless a reorg is detected, and reorg handling removes them.
func (i *Index) loadLog() error {
	f, err := os.Open(filepath.Join(i.dir, logName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			// A truncated final line is the expected crash artifact; anything else
			// is corruption and must be reported, not silently ignored.
			if sc.Err() == nil {
				return fmt.Errorf("zindexer: corrupt index log at line %d: %w", line, err)
			}
			break
		}
		i.addRecord(r)
	}
	return sc.Err()
}

func (i *Index) addRecord(r Record) {
	if _, ok := i.byID[r.TxID]; ok {
		return
	}
	if len(i.byID) >= maxRecords {
		return
	}
	i.byID[r.TxID] = r
	i.byAddr[r.Sender] = append(i.byAddr[r.Sender], r.TxID)
	i.byType[r.Type] = append(i.byType[r.Type], r.TxID)
}

// Height returns the last indexed height.
func (i *Index) Height() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.height
}

// LastIndexedHash returns the hash of the last indexed block.
func (i *Index) LastIndexedHash() (zcrypto.Hash, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	h, err := zcrypto.ParseHash(i.head)
	if err != nil {
		return zcrypto.Hash{}, false
	}
	return h, true
}

// ChainSource is the minimal chain surface the indexer needs.
type ChainSource interface {
	Height() uint64
	Block(height uint64) (*ztypes.Block, bool)
}

// ReceiptSource is implemented by chains that can supply persisted receipts. When
// available, Sync indexes receipt data (gas used, fees, status, events, contract
// addresses); without it, receipt-derived fields stay zero and the index is still
// a correct record of what each block contained.
type ReceiptSource interface {
	Receipts(height uint64) ([]*ztypes.Receipt, bool)
}

// Sync indexes all blocks from the current index height to the chain head. When a
// reorg is detected (indexed hash no longer matches the chain at that height), the
// index is rolled back to the fork point first.
func (i *Index) Sync(chain ChainSource) error {
	head := chain.Height()
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.rollbackLocked(chain, head); err != nil {
		return err
	}
	for h := i.height + 1; h <= head; h++ {
		b, ok := chain.Block(h)
		if !ok {
			return fmt.Errorf("zindexer: chain reports height %d but block %d is missing", head, h)
		}
		var rcpts []*ztypes.Receipt
		if rs, ok := chain.(ReceiptSource); ok {
			if r, ok := rs.Receipts(h); ok {
				rcpts = r
			}
		}
		if err := i.indexBlockLocked(b, rcpts); err != nil {
			return err
		}
	}
	return i.flushLocked()
}

// IndexBlock indexes one block with its receipts (used by the live node path,
// where receipts are already computed and must not be recomputed).
func (i *Index) IndexBlock(b *ztypes.Block, receipts []*ztypes.Receipt) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.indexBlockLocked(b, receipts)
}

func (i *Index) indexBlockLocked(b *ztypes.Block, receipts []*ztypes.Receipt) error {
	h := b.Header.Height
	if h != i.height+1 {
		return fmt.Errorf("zindexer: cannot index height %d after %d", h, i.height)
	}
	byTx := map[string]*ztypes.Receipt{}
	for _, rc := range receipts {
		if rc.TxID != (ztypes.Hash{}) {
			byTx[rc.TxID.String()] = rc
		}
	}
	for _, tx := range b.Txs {
		id := tx.ID()
		rec := Record{
			Height:    h,
			BlockHash: b.Header.Hash().String(),
			TxID:      id.String(),
			Type:      uint8(tx.Type),
			Sender:    tx.Sender.Bech32(),
			Nonce:     tx.Nonce,
			GasPrice:  tx.GasPrice,
			TimeMs:    b.Header.TimeMs,
		}
		if rc, ok := byTx[id.String()]; ok {
			rec.GasUsed = rc.GasUsed
			rec.FeeUZ = rc.Fee
			rec.Status = rc.Status
			rec.Error = rc.Error
			rec.Events = rc.Events
			if !rc.ContractAddress.IsZero() {
				rec.Contract = rc.ContractAddress.Bech32()
			}
		}
		if i.logFile != nil {
			raw, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if _, err := i.logFile.Write(append(raw, '\n')); err != nil {
				return err
			}
		}
		i.addRecord(rec)
	}
	i.height = h
	i.head = b.Header.Hash().String()
	i.heights = append(i.heights, HeightEntry{Height: h, BlockHash: i.head, TxCount: len(b.Txs)})
	return nil
}

// rollbackLocked removes index entries above the fork point, if any.
func (i *Index) rollbackLocked(chain ChainSource, chainHead uint64) error {
	forkAt := i.height + 1 // exclusive upper bound of entries to keep
	if i.height <= chainHead {
		if b, ok := chain.Block(i.height); ok {
			if b.Header.Hash().String() == i.head {
				return nil // no reorg
			}
		}
	}
	// Walk back to the last height whose hash still matches.
	for h := i.height; ; h-- {
		b, ok := chain.Block(h)
		if ok {
			entry, have := i.entryAt(h)
			if have && entry.BlockHash == b.Header.Hash().String() {
				forkAt = h + 1
				break
			}
		}
		if h == 0 {
			forkAt = 0
			break
		}
	}
	if forkAt > i.height {
		return nil
	}
	// Drop records at or above forkAt.
	for id, rec := range i.byID {
		if rec.Height >= forkAt {
			delete(i.byID, id)
		}
	}
	i.rebuildSecondaryLocked()
	kept := i.heights[:0]
	for _, e := range i.heights {
		if e.Height < forkAt {
			kept = append(kept, e)
		}
	}
	i.heights = kept
	if forkAt == 0 {
		i.height = 0
		i.head = ""
	} else {
		i.height = forkAt - 1
		b, _ := chain.Block(i.height)
		i.head = b.Header.Hash().String()
	}
	// Rewrite the log so stale records cannot come back on restart.
	return i.rewriteLogLocked()
}

func (i *Index) entryAt(h uint64) (HeightEntry, bool) {
	if h == 0 {
		return HeightEntry{}, false
	}
	for idx := len(i.heights) - 1; idx >= 0; idx-- {
		if i.heights[idx].Height == h {
			return i.heights[idx], true
		}
	}
	return HeightEntry{}, false
}

func (i *Index) rebuildSecondaryLocked() {
	i.byAddr = map[string][]string{}
	i.byType = map[uint8][]string{}
	ids := make([]string, 0, len(i.byID))
	for id := range i.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := i.byID[id]
		i.byAddr[rec.Sender] = append(i.byAddr[rec.Sender], id)
		i.byType[rec.Type] = append(i.byType[rec.Type], id)
	}
}

func (i *Index) rewriteLogLocked() error {
	if i.logFile != nil {
		_ = i.logFile.Close()
		i.logFile = nil
	}
	path := filepath.Join(i.dir, logName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(i.byID))
	for id := range i.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	w := bufio.NewWriterSize(f, 64*1024)
	for _, id := range ids {
		raw, err := json.Marshal(i.byID[id])
		if err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	nf, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	i.logFile = nf
	return nil
}

// flushLocked persists the checkpoint and fsyncs the log.
func (i *Index) flushLocked() error {
	if i.logFile != nil {
		if err := i.logFile.Sync(); err != nil {
			return err
		}
	}
	cp := checkpoint{
		Version:  1,
		ChainID:  i.chainID,
		Height:   i.height,
		HeadHash: i.head,
		Records:  len(i.byID),
		Heights:  i.heights,
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(i.dir, checkpointName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Close flushes and closes the index.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.flushLocked(); err != nil {
		return err
	}
	if i.logFile != nil {
		err := i.logFile.Close()
		i.logFile = nil
		return err
	}
	return nil
}

// ---- queries ---------------------------------------------------------------

// Transaction returns an indexed transaction.
func (i *Index) Transaction(id string) (Record, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	r, ok := i.byID[id]
	return r, ok
}

// ByAddress returns the transaction ids of an address, newest first.
func (i *Index) ByAddress(addr string) []Record {
	i.mu.RLock()
	defer i.mu.RUnlock()
	ids := append([]string(nil), i.byAddr[addr]...)
	sort.Slice(ids, func(a, b int) bool {
		ra, rb := i.byID[ids[a]], i.byID[ids[b]]
		if ra.Height != rb.Height {
			return ra.Height > rb.Height
		}
		return ra.Nonce > rb.Nonce
	})
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		out = append(out, i.byID[id])
	}
	return out
}

// Recent returns the most recent n transactions across the whole chain.
func (i *Index) Recent(n int) []Record {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]Record, 0, n)
	for idx := len(i.heights) - 1; idx >= 0 && len(out) < n; idx-- {
		e := i.heights[idx]
		var recs []Record
		for _, rec := range i.byID {
			if rec.Height == e.Height {
				recs = append(recs, rec)
			}
		}
		sort.Slice(recs, func(a, b int) bool { return recs[a].TxID < recs[b].TxID })
		for _, r := range recs {
			if len(out) >= n {
				break
			}
			out = append(out, r)
		}
	}
	return out
}

// Count returns the number of indexed transactions.
func (i *Index) Count() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.byID)
}

// Stats summarises the index for the explorer/RPC.
func (i *Index) Stats() map[string]any {
	i.mu.RLock()
	defer i.mu.RUnlock()
	perType := map[string]int{}
	for t, ids := range i.byType {
		perType[fmt.Sprintf("%d", t)] = len(ids)
	}
	return map[string]any{
		"indexed_height":       i.height,
		"records":              len(i.byID),
		"addresses":            len(i.byAddr),
		"transactions_by_type": perType,
	}
}
