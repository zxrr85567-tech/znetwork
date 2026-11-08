package ztypes

import (
	"fmt"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
)

// Header is a block header. Its hash commits to every consensus-relevant field.
//
// StateRoot is the state root *after* executing the block's transactions, so a
// header is a commitment to the resulting state (the proposer computes it by
// speculative execution before signing).
type Header struct {
	Version        uint16
	ChainID        string
	Height         uint64
	TimeMs         int64
	PrevHash       Hash
	TxRoot         Hash
	ReceiptRoot    Hash
	StateRoot      Hash
	Proposer       Address
	GasUsed        uint64
	GasLimit       uint64
	ValidatorsHash Hash
	TxCount        uint32
}

// Encode returns the canonical header encoding.
func (h *Header) Encode() []byte {
	w := zencoding.NewWriterSize(256)
	w.U8(0x01) // header envelope version
	w.U16(h.Version)
	w.Str(h.ChainID)
	w.U64(h.Height)
	w.I64(h.TimeMs)
	w.Raw(h.PrevHash[:])
	w.Raw(h.TxRoot[:])
	w.Raw(h.ReceiptRoot[:])
	w.Raw(h.StateRoot[:])
	w.Raw(h.Proposer[:])
	w.U64(h.GasUsed)
	w.U64(h.GasLimit)
	w.Raw(h.ValidatorsHash[:])
	w.U32(h.TxCount)
	return w.Done()
}

// Hash returns the block hash: Digest("ZNET/v1/block-header", encode(header)).
func (h *Header) Hash() Hash { return zcrypto.Digest(domainHeader, h.Encode()) }

// DecodeHeader parses a canonical header.
func DecodeHeader(b []byte) (*Header, error) {
	r := zencoding.NewReader(b)
	if v := r.U8(); v != 0x01 {
		return nil, fmt.Errorf("%w: header envelope %d", ErrBlockInvalid, v)
	}
	h := &Header{}
	h.Version = r.U16()
	h.ChainID = r.Str()
	h.Height = r.U64()
	h.TimeMs = r.I64()
	copy(h.PrevHash[:], r.Raw(32))
	copy(h.TxRoot[:], r.Raw(32))
	copy(h.ReceiptRoot[:], r.Raw(32))
	copy(h.StateRoot[:], r.Raw(32))
	copy(h.Proposer[:], r.Raw(20))
	h.GasUsed = r.U64()
	h.GasLimit = r.U64()
	copy(h.ValidatorsHash[:], r.Raw(32))
	h.TxCount = r.U32()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlockInvalid, err)
	}
	return h, nil
}

// CommitSig is one validator's precommit for a block.
type CommitSig struct {
	Validator Address
	PubKey    PublicKey
	Signature Signature
	Signed    bool // false = explicit nil vote (precommit for <nil>)
}

// Commit is the aggregated precommit set that finalised a block.
type Commit struct {
	Height    uint64
	Round     uint32
	BlockHash Hash
	Sigs      []CommitSig
}

// Encode returns the canonical commit encoding.
func (c *Commit) Encode() []byte {
	w := zencoding.NewWriterSize(64 * (len(c.Sigs) + 1))
	w.U8(0x01)
	w.U64(c.Height)
	w.U32(c.Round)
	w.Raw(c.BlockHash[:])
	w.U32(uint32(len(c.Sigs)))
	for _, s := range c.Sigs {
		w.Raw(s.Validator[:])
		w.Raw(s.PubKey[:])
		w.Bool(s.Signed)
		w.Raw(s.Signature[:])
	}
	return w.Done()
}

// DecodeCommit parses a canonical commit.
func DecodeCommit(b []byte) (*Commit, error) {
	r := zencoding.NewReader(b)
	if v := r.U8(); v != 0x01 {
		return nil, fmt.Errorf("%w: commit envelope %d", ErrBlockInvalid, v)
	}
	c := &Commit{}
	c.Height = r.U64()
	c.Round = r.U32()
	copy(c.BlockHash[:], r.Raw(32))
	n := r.Len32(117)
	if r.Err() != nil || n > MaxValidators {
		return nil, fmt.Errorf("%w: commit sig count", ErrBlockInvalid)
	}
	c.Sigs = make([]CommitSig, n)
	for i := 0; i < n; i++ {
		copy(c.Sigs[i].Validator[:], r.Raw(20))
		copy(c.Sigs[i].PubKey[:], r.Raw(32))
		c.Sigs[i].Signed = r.Bool()
		copy(c.Sigs[i].Signature[:], r.Raw(64))
	}
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlockInvalid, err)
	}
	return c, nil
}

// Block is a block: header, transactions, and the commit that finalised it.
// The commit is not part of the block hash (it is produced after the header is
// fixed); it is stored alongside so any observer can verify finality offline.
type Block struct {
	Header Header
	Txs    []*Tx
	Commit *Commit
}

// Hash returns the block hash (header hash).
func (b *Block) Hash() Hash {
	h := b.Header
	return h.Hash()
}

// Encode returns the canonical block encoding (header, txs, commit).
func (b *Block) Encode() []byte {
	hdr := b.Header.Encode()
	w := zencoding.NewWriterSize(len(hdr) + 128*(len(b.Txs)+1))
	w.U8(0x01)
	w.Bytes(hdr)
	w.U32(uint32(len(b.Txs)))
	for _, tx := range b.Txs {
		w.Bytes(tx.Encode())
	}
	if b.Commit == nil {
		w.U8(0)
	} else {
		w.U8(1)
		w.Bytes(b.Commit.Encode())
	}
	return w.Done()
}

// DecodeBlock parses a canonical block.
func DecodeBlock(b []byte) (*Block, error) {
	r := zencoding.NewReader(b)
	if v := r.U8(); v != 0x01 {
		return nil, fmt.Errorf("%w: block envelope %d", ErrBlockInvalid, v)
	}
	hdrBytes := r.Bytes(4096)
	hdr, err := DecodeHeader(hdrBytes)
	if err != nil {
		return nil, err
	}
	n := r.Len32(4)
	if r.Err() != nil || n > MaxTxPerBlock {
		return nil, fmt.Errorf("%w: tx count", ErrBlockInvalid)
	}
	blk := &Block{Header: *hdr, Txs: make([]*Tx, 0, n)}
	for i := 0; i < n; i++ {
		txb := r.Bytes(MaxPayloadLen + 1024)
		tx, err := DecodeTx(txb)
		if err != nil {
			return nil, fmt.Errorf("%w: tx %d: %v", ErrBlockInvalid, i, err)
		}
		blk.Txs = append(blk.Txs, tx)
	}
	hasCommit := r.U8()
	if r.Err() != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlockInvalid, r.Err())
	}
	if hasCommit == 1 {
		cb := r.Bytes(1 << 20)
		c, err := DecodeCommit(cb)
		if err != nil {
			return nil, err
		}
		blk.Commit = c
	}
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlockInvalid, err)
	}
	return blk, nil
}

// TxIDs returns the transaction identifiers in block order.
func (b *Block) TxIDs() []Hash {
	out := make([]Hash, len(b.Txs))
	for i, tx := range b.Txs {
		out[i] = tx.ID()
	}
	return out
}

// Receipt status values.
const (
	// ReceiptFailed marks a transaction that was charged gas but rejected.
	ReceiptFailed uint8 = 0
	// ReceiptSuccess marks a transaction whose state changes were applied.
	ReceiptSuccess uint8 = 1
)

// Event is an emitted log entry. Attributes are ordered pairs.
type Event struct {
	Type  string      `json:"type"`
	Attrs [][2]string `json:"attrs"`
}

// Receipt is the result of executing one transaction.
type Receipt struct {
	TxID            Hash
	Height          uint64
	Index           uint32
	Sender          Address
	Type            TxType
	Status          uint8
	GasWanted       uint64
	GasUsed         uint64
	Fee             uint64
	ContractAddress Address
	Events          []Event
	Output          []byte
	Error           string
}

// Encode returns the canonical receipt encoding.
func (r *Receipt) Encode() []byte {
	w := zencoding.NewWriterSize(256)
	w.U8(0x01)
	w.Raw(r.TxID[:])
	w.U64(r.Height)
	w.U32(r.Index)
	w.Raw(r.Sender[:])
	w.U8(uint8(r.Type))
	w.U8(r.Status)
	w.U64(r.GasWanted)
	w.U64(r.GasUsed)
	w.U64(r.Fee)
	w.Raw(r.ContractAddress[:])
	w.U32(uint32(len(r.Events)))
	for _, e := range r.Events {
		w.Str(e.Type)
		w.U32(uint32(len(e.Attrs)))
		for _, a := range e.Attrs {
			w.Str(a[0])
			w.Str(a[1])
		}
	}
	w.Bytes(r.Output)
	w.Str(r.Error)
	return w.Done()
}

// DecodeReceipt parses a canonical receipt.
func DecodeReceipt(b []byte) (*Receipt, error) {
	r := zencoding.NewReader(b)
	if v := r.U8(); v != 0x01 {
		return nil, fmt.Errorf("%w: receipt envelope %d", ErrStateInvalid, v)
	}
	rc := &Receipt{}
	copy(rc.TxID[:], r.Raw(32))
	rc.Height = r.U64()
	rc.Index = r.U32()
	copy(rc.Sender[:], r.Raw(20))
	rc.Type = TxType(r.U8())
	rc.Status = r.U8()
	rc.GasWanted = r.U64()
	rc.GasUsed = r.U64()
	rc.Fee = r.U64()
	copy(rc.ContractAddress[:], r.Raw(20))
	n := r.Len32(8)
	if r.Err() != nil || n > 1024 {
		return nil, fmt.Errorf("%w: event count", ErrStateInvalid)
	}
	rc.Events = make([]Event, 0, n)
	for i := 0; i < n; i++ {
		e := Event{Type: r.Str()}
		na := r.Len32(4)
		if r.Err() != nil || na > 64 {
			return nil, fmt.Errorf("%w: attribute count", ErrStateInvalid)
		}
		for j := 0; j < na; j++ {
			k := r.Str()
			v := r.Str()
			e.Attrs = append(e.Attrs, [2]string{k, v})
		}
		rc.Events = append(rc.Events, e)
	}
	rc.Output = append([]byte(nil), r.Bytes(MaxPayloadLen)...)
	rc.Error = r.Str()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateInvalid, err)
	}
	return rc, nil
}
