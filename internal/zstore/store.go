// Package zstore persists blocks and state snapshots on disk.
//
// Three files live in the data directory:
//
//	blocks.log        append-only record log: u32 len | block bytes | sha256(block bytes)
//	receipts.log      append-only receipt log: u32 len | height | receipts | sha256
//	state/NNNNN.snap  full state snapshots, one per height
//
// Both formats are checksummed, so torn writes are detected rather than silently
// accepted (threat T-28, database corruption). A partial trailing record in
// blocks.log is treated as a crash artefact: it is reported and truncated, never
// interpreted. Every write is fsync'd before it is considered durable.
package zstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// File and format constants.
const (
	blocksFile    = "blocks.log"
	stateDir      = "state"
	snapshotMagic = "ZSNAP1\x00"
	// MaxRecordLen bounds a single block record (8 MiB) to prevent a corrupt
	// length prefix from causing a huge allocation.
	MaxRecordLen = 8 << 20
)

// ErrCorrupt is returned when stored data fails its integrity check.
var ErrCorrupt = errors.New("zstore: data integrity check failed")

// ErrTruncatedTail indicates a partial final record (crash during write).
var ErrTruncatedTail = errors.New("zstore: truncated tail record")

// Store is the on-disk persistence layer.
type Store struct {
	dir string
}

// Open creates (if needed) and returns a store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("zstore: empty data directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, stateDir), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store root.
func (s *Store) Dir() string { return s.dir }

func (s *Store) blocksPath() string { return filepath.Join(s.dir, blocksFile) }

func (s *Store) snapshotPath(height uint64) string {
	return filepath.Join(s.dir, stateDir, fmt.Sprintf("%016d.snap", height))
}

// ---- block log -------------------------------------------------------------

// AppendBlock appends one block record and fsyncs it.
func (s *Store) AppendBlock(b *ztypes.Block) error {
	body := b.Encode()
	if len(body) > MaxRecordLen {
		return fmt.Errorf("zstore: block record too large (%d bytes)", len(body))
	}
	sum := zcrypto.HashBytes(body)
	rec := make([]byte, 0, 4+len(body)+32)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
	rec = append(rec, lb[:]...)
	rec = append(rec, body...)
	rec = append(rec, sum[:]...)

	f, err := os.OpenFile(s.blocksPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(rec); err != nil {
		return err
	}
	return f.Sync()
}

// ReadBlocksFile parses every complete record. It returns the blocks in file
// order and, if the file ends with a partial record, the byte offset at which the
// valid region ends.
func (s *Store) ReadBlocksFile() ([]*ztypes.Block, int64, error) {
	data, err := os.ReadFile(s.blocksPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	var blocks []*ztypes.Block
	off := 0
	for off < len(data) {
		if len(data)-off < 4 {
			return blocks, int64(off), ErrTruncatedTail
		}
		n := int(binary.BigEndian.Uint32(data[off : off+4]))
		if n <= 0 || n > MaxRecordLen {
			return blocks, int64(off), fmt.Errorf("%w: implausible record length %d at offset %d", ErrCorrupt, n, off)
		}
		end := off + 4 + n + 32
		if end > len(data) {
			return blocks, int64(off), ErrTruncatedTail
		}
		body := data[off+4 : off+4+n]
		want := data[off+4+n : end]
		got := zcrypto.HashBytes(body)
		if !bytes.Equal(want, got[:]) {
			return blocks, int64(off), fmt.Errorf("%w: record checksum mismatch at offset %d", ErrCorrupt, off)
		}
		b, err := ztypes.DecodeBlock(body)
		if err != nil {
			return blocks, int64(off), fmt.Errorf("%w: record %d: %v", ErrCorrupt, len(blocks), err)
		}
		blocks = append(blocks, b)
		off = end
	}
	return blocks, int64(off), nil
}

// TruncateTail cuts the block log back to validLen bytes.
func (s *Store) TruncateTail(validLen int64) error {
	return os.Truncate(s.blocksPath(), validLen)
}

// RewriteBlocks replaces the log with exactly the given blocks (used on reorg).
func (s *Store) RewriteBlocks(blocks []*ztypes.Block) error {
	tmp := s.blocksPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		body := b.Encode()
		sum := zcrypto.HashBytes(body)
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
		if _, err := f.Write(lb[:]); err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(sum[:]); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.blocksPath())
}

// ---- state snapshots -------------------------------------------------------

// SaveState writes a checksummed snapshot for st.Height() and fsyncs it.
func (s *Store) SaveState(st *zstate.State) error {
	entries := st.Entries()
	w := zencoding.NewWriterSize(64 + len(entries)*64)
	w.Raw([]byte(snapshotMagic))
	w.U64(st.Height())
	bh := st.BlockHash()
	w.Raw(bh[:])
	root := st.Root()
	w.Raw(root[:])
	w.U32(uint32(len(entries)))
	for _, e := range entries {
		w.Bytes(e.Key)
		w.Bytes(e.Value)
	}
	body := w.Done()
	sum := zcrypto.HashBytes(body)
	final := make([]byte, 0, len(body)+32)
	final = append(final, body...)
	final = append(final, sum[:]...)

	path := s.snapshotPath(st.Height())
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(final); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState reads and verifies a snapshot, recomputing the state root.
func (s *Store) LoadState(height uint64) (*zstate.State, error) {
	data, err := os.ReadFile(s.snapshotPath(height))
	if err != nil {
		return nil, err
	}
	if len(data) < len(snapshotMagic)+8+32+32+4+32 {
		return nil, fmt.Errorf("%w: snapshot too short", ErrCorrupt)
	}
	body := data[:len(data)-32]
	want := data[len(data)-32:]
	got := zcrypto.HashBytes(body)
	if !bytes.Equal(want, got[:]) {
		return nil, fmt.Errorf("%w: snapshot checksum", ErrCorrupt)
	}
	r := zencoding.NewReader(body)
	if string(r.Raw(len(snapshotMagic))) != snapshotMagic {
		return nil, fmt.Errorf("%w: snapshot magic", ErrCorrupt)
	}
	h := r.U64()
	if h != height {
		return nil, fmt.Errorf("%w: snapshot height %d != filename %d", ErrCorrupt, h, height)
	}
	var bh, root ztypes.Hash
	copy(bh[:], r.Raw(32))
	copy(root[:], r.Raw(32))
	n := r.Len32(8)
	if r.Err() != nil {
		return nil, fmt.Errorf("%w: entry count", ErrCorrupt)
	}
	st := zstate.New()
	for i := 0; i < n; i++ {
		k := append([]byte(nil), r.Bytes(1<<16)...)
		v := append([]byte(nil), r.Bytes(zencoding.MaxBytesLen)...)
		if r.Err() != nil {
			return nil, fmt.Errorf("%w: entry %d", ErrCorrupt, i)
		}
		st.SetRaw(k, v)
	}
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if gotRoot := st.Root(); gotRoot != root {
		return nil, fmt.Errorf("%w: recomputed state root %s != recorded %s", ErrCorrupt, gotRoot.Short(), root.Short())
	}
	st.SetHeight(h)
	st.SetBlockHash(bh)
	return st, nil
}

// stateHeights lists snapshot heights present on disk, ascending.
func (s *Store) stateHeights() ([]uint64, error) {
	ents, err := os.ReadDir(filepath.Join(s.dir, stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var hs []uint64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".snap") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".snap")
		h, err := strconv.ParseUint(base, 10, 64)
		if err != nil {
			continue
		}
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
	return hs, nil
}

// LatestStateHeight returns the highest snapshot height on disk.
func (s *Store) LatestStateHeight() (uint64, bool, error) {
	hs, err := s.stateHeights()
	if err != nil || len(hs) == 0 {
		return 0, false, err
	}
	return hs[len(hs)-1], true, nil
}

// PruneStates keeps the highest `keep` snapshots and deletes older ones.
func (s *Store) PruneStates(keep int) error {
	hs, err := s.stateHeights()
	if err != nil {
		return err
	}
	if keep < 1 || len(hs) <= keep {
		return nil
	}
	for _, h := range hs[:len(hs)-keep] {
		if err := os.Remove(s.snapshotPath(h)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// DeleteStatesAbove removes snapshots above height (used when rolling back).
func (s *Store) DeleteStatesAbove(height uint64) error {
	hs, err := s.stateHeights()
	if err != nil {
		return err
	}
	for _, h := range hs {
		if h > height {
			if err := os.Remove(s.snapshotPath(h)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// ---- receipt log -----------------------------------------------------------
//
// Receipts are derived data, not consensus data: a node can always recompute them
// by re-executing a block. They are nevertheless persisted because indexers,
// explorers and wallets need them without holding full execution state. The file
// is append-only and self-checksummed, exactly like blocks.log, and a truncated
// tail is repaired by recomputation (see znode.Chain.RepairReceipts).

const receiptsFile = "receipts.log"

// AppendReceipts appends the receipts of one height.
func (s *Store) AppendReceipts(height uint64, receipts []*ztypes.Receipt) error {
	w := zencoding.NewWriterSize(256)
	w.U64(height)
	w.U32(uint32(len(receipts)))
	for _, r := range receipts {
		w.Bytes(r.Encode())
	}
	body := w.Done()
	rec := make([]byte, 0, 4+len(body)+32)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
	rec = append(rec, lb[:]...)
	rec = append(rec, body...)
	sum := sha256.Sum256(body)
	rec = append(rec, sum[:]...)
	f, err := os.OpenFile(filepath.Join(s.dir, receiptsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(rec); err != nil {
		return err
	}
	return f.Sync()
}

// ReadReceipts returns all persisted receipts keyed by height. A truncated tail is
// ignored (the height is simply absent and can be recomputed).
func (s *Store) ReadReceipts() (map[uint64][]*ztypes.Receipt, error) {
	out := map[uint64][]*ztypes.Receipt{}
	raw, err := os.ReadFile(filepath.Join(s.dir, receiptsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	off := 0
	for off+4 <= len(raw) {
		n := int(binary.BigEndian.Uint32(raw[off:]))
		if n <= 0 || n > MaxRecordLen || off+4+n+32 > len(raw) {
			break // truncated or corrupt tail
		}
		body := raw[off+4 : off+4+n]
		want := raw[off+4+n : off+4+n+32]
		sum := sha256.Sum256(body)
		if !bytes.Equal(sum[:], want) {
			break
		}
		r := zencoding.NewReader(body)
		height := r.U64()
		count := r.U32()
		if err := r.Err(); err != nil || count > ztypes.MaxTxPerBlock+8 {
			break
		}
		list := make([]*ztypes.Receipt, 0, count)
		bad := false
		for i := uint32(0); i < count; i++ {
			rb := r.Bytes(MaxRecordLen)
			if r.Err() != nil {
				bad = true
				break
			}
			rc, err := ztypes.DecodeReceipt(rb)
			if err != nil {
				bad = true
				break
			}
			list = append(list, rc)
		}
		if bad || r.Done() != nil {
			break
		}
		out[height] = list
		off += 4 + n + 32
	}
	return out, nil
}

// RewriteReceipts replaces the receipt log with the given set (used after a repair
// or a reorg). The write is atomic.
func (s *Store) RewriteReceipts(all map[uint64][]*ztypes.Receipt) error {
	heights := make([]uint64, 0, len(all))
	for h := range all {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	path := filepath.Join(s.dir, receiptsFile)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	for _, h := range heights {
		w := zencoding.NewWriterSize(256)
		w.U64(h)
		w.U32(uint32(len(all[h])))
		for _, r := range all[h] {
			w.Bytes(r.Encode())
		}
		body := w.Done()
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
		if _, err := f.Write(lb[:]); err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			return err
		}
		sum := sha256.Sum256(body)
		if _, err := f.Write(sum[:]); err != nil {
			f.Close()
			return err
		}
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
