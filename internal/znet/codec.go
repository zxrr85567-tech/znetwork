package znet

import (
	"fmt"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
	"znetwork/internal/ztypes"
)

// Wire codecs for the protocol messages.
//
// The ZENC reader is error-accumulating: after the first error every accessor
// returns zero and the error is reported once by Done(). Every decoder therefore
// ends with `if err := r.Done(); err != nil { ... }`, which rejects both truncated
// input and trailing bytes (serialization ambiguity, threat T-15).

// Status announces the sender's chain identity and height.
type Status struct {
	ChainID     string
	Height      uint64
	HeadHash    zcrypto.Hash
	GenesisHash zcrypto.Hash
}

// EncodeStatus serialises a Status.
func EncodeStatus(s *Status) []byte {
	w := zencoding.NewWriterSize(96)
	w.Str(s.ChainID)
	w.U64(s.Height)
	w.Raw(s.HeadHash[:])
	w.Raw(s.GenesisHash[:])
	return w.Done()
}

// DecodeStatus parses a Status.
func DecodeStatus(b []byte) (*Status, error) {
	r := zencoding.NewReader(b)
	s := &Status{}
	s.ChainID = r.Str()
	s.Height = r.U64()
	if raw := r.Raw(32); raw != nil {
		copy(s.HeadHash[:], raw)
	}
	if raw := r.Raw(32); raw != nil {
		copy(s.GenesisHash[:], raw)
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	return s, nil
}

// EncodeTx serialises a transaction for gossip.
func EncodeTx(tx *ztypes.Tx) []byte { return tx.Encode() }

// DecodeTx parses a gossiped transaction. Signature and static limits are
// re-validated by the receiving node before the transaction is pooled.
func DecodeTx(b []byte) (*ztypes.Tx, error) { return ztypes.DecodeTx(b) }

// EncodeProposal serialises a proposal, including the full block.
func EncodeProposal(p *zconsensus.Proposal) ([]byte, error) {
	blockBytes := p.Block.Encode()
	if len(blockBytes) > MaxFrame-512 {
		return nil, ErrFrameTooBig
	}
	w := zencoding.NewWriterSize(len(blockBytes) + 256)
	w.Str(p.ChainID)
	w.U64(p.Height)
	w.U32(p.Round)
	w.I64(p.TimeMs)
	w.Bytes(blockBytes)
	w.Raw(p.Proposer[:])
	w.Raw(p.PubKey[:])
	w.Raw(p.Signature[:])
	return w.Done(), nil
}

// DecodeProposal parses a proposal.
func DecodeProposal(b []byte) (*zconsensus.Proposal, error) {
	r := zencoding.NewReader(b)
	p := &zconsensus.Proposal{}
	p.ChainID = r.Str()
	p.Height = r.U64()
	p.Round = r.U32()
	p.TimeMs = r.I64()
	blockBytes := r.Bytes(MaxFrame)
	if raw := r.Raw(20); raw != nil {
		copy(p.Proposer[:], raw)
	}
	if raw := r.Raw(32); raw != nil {
		copy(p.PubKey[:], raw)
	}
	if raw := r.Raw(64); raw != nil {
		copy(p.Signature[:], raw)
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	blk, err := ztypes.DecodeBlock(blockBytes)
	if err != nil {
		return nil, fmt.Errorf("znet: proposal block: %w", err)
	}
	p.Block = blk
	return p, nil
}

// EncodeVote serialises a consensus vote.
func EncodeVote(v *zconsensus.Vote) []byte {
	w := zencoding.NewWriterSize(192)
	w.Str(v.ChainID)
	w.U64(v.Height)
	w.U32(v.Round)
	w.U8(v.Step)
	w.Raw(v.BlockID[:])
	w.Raw(v.Voter[:])
	w.Raw(v.PubKey[:])
	w.I64(v.TimeMs)
	w.Raw(v.Signature[:])
	return w.Done()
}

// DecodeVote parses a consensus vote.
func DecodeVote(b []byte) (*zconsensus.Vote, error) {
	r := zencoding.NewReader(b)
	v := &zconsensus.Vote{}
	v.ChainID = r.Str()
	v.Height = r.U64()
	v.Round = r.U32()
	v.Step = r.U8()
	if raw := r.Raw(32); raw != nil {
		copy(v.BlockID[:], raw)
	}
	if raw := r.Raw(20); raw != nil {
		copy(v.Voter[:], raw)
	}
	if raw := r.Raw(32); raw != nil {
		copy(v.PubKey[:], raw)
	}
	v.TimeMs = r.I64()
	if raw := r.Raw(64); raw != nil {
		copy(v.Signature[:], raw)
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	return v, nil
}

// MaxBlocksPerMessage bounds catch-up batches.
const MaxBlocksPerMessage = 128

// EncodeBlocks carries a batch of blocks (for catch-up sync).
func EncodeBlocks(blocks []*ztypes.Block) ([]byte, error) {
	if len(blocks) > MaxBlocksPerMessage {
		return nil, fmt.Errorf("znet: too many blocks in one message (%d)", len(blocks))
	}
	w := zencoding.NewWriterSize(4096)
	w.U32(uint32(len(blocks)))
	for _, b := range blocks {
		raw := b.Encode()
		if len(raw) > MaxFrame-64 {
			return nil, ErrFrameTooBig
		}
		w.Bytes(raw)
	}
	out := w.Done()
	if len(out) > MaxFrame {
		return nil, ErrFrameTooBig
	}
	return out, nil
}

// DecodeBlocks parses a block batch.
func DecodeBlocks(b []byte) ([]*ztypes.Block, error) {
	r := zencoding.NewReader(b)
	n := r.U32()
	if r.Err() != nil {
		return nil, r.Err()
	}
	if n > MaxBlocksPerMessage {
		return nil, fmt.Errorf("znet: too many blocks in one message (%d)", n)
	}
	out := make([]*ztypes.Block, 0, n)
	for i := uint32(0); i < n; i++ {
		raw := r.Bytes(MaxFrame)
		if r.Err() != nil {
			return nil, r.Err()
		}
		blk, err := ztypes.DecodeBlock(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, blk)
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	return out, nil
}

// EncodeHeightRange requests blocks from a height.
func EncodeHeightRange(from uint64, limit uint32) []byte {
	w := zencoding.NewWriterSize(16)
	w.U64(from)
	w.U32(limit)
	return w.Done()
}

// DecodeHeightRange parses a block request.
func DecodeHeightRange(b []byte) (uint64, uint32, error) {
	r := zencoding.NewReader(b)
	from := r.U64()
	limit := r.U32()
	if err := r.Done(); err != nil {
		return 0, 0, err
	}
	if limit == 0 || limit > MaxBlocksPerMessage {
		limit = 32
	}
	return from, limit, nil
}
