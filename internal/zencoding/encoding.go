// Package zencoding implements Z Network's canonical binary encoding ("ZENC").
//
// Design goals, in priority order:
//  1. Unambiguous: exactly one byte string decodes to a given value, and every
//     byte string has exactly one interpretation. No optional fields, no
//     variable-width integers for fixed-width values, no self-describing types.
//  2. Strict: a decoder never reads past the end of its buffer, never allocates
//     based on an unvalidated length, and rejects trailing bytes.
//  3. Cheap to audit: fixed big-endian widths, one length-prefixed form for
//     variable-length data.
//
// Wire forms:
//
//	u8, u16, u32, u64  -> fixed-width big-endian
//	i64                -> two's complement, big-endian
//	bool               -> exactly 0x00 or 0x01 (any other byte is an error)
//	bytes / string     -> u32 big-endian length, then the raw bytes
//
// There is no float type in the protocol.
//
// Security note (threat T-15, serialization ambiguity): callers must consume the
// whole buffer. Use Reader.Done() to enforce it; hashing code in this repository
// always does.
package zencoding

import (
	"encoding/binary"
	"errors"
)

// Hard limits. They exist so that a hostile 4-byte length prefix cannot cause a
// multi-gigabyte allocation (threat T-12, resource exhaustion).
const (
	MaxBytesLen = 8 << 20 // 8 MiB for a single byte field (contract code, memo, ...)
	MaxStrLen   = 4096    // string fields (chain id, moniker, memo, ...)
	MaxItems    = 1 << 20 // elements in a single repeated field
)

var (
	// ErrShortBuffer means the input ended in the middle of a value.
	ErrShortBuffer = errors.New("zencoding: short buffer")
	// ErrLengthLimit means a declared length exceeded the documented limit.
	ErrLengthLimit = errors.New("zencoding: declared length exceeds limit")
	// ErrTrailingBytes means the input contained bytes beyond the encoded value.
	ErrTrailingBytes = errors.New("zencoding: trailing bytes after value")
	// ErrNonCanonicalBool means a bool was encoded with a byte other than 0/1.
	ErrNonCanonicalBool = errors.New("zencoding: non-canonical bool")
	// ErrEncoding is a generic wrap for decoder failures.
	ErrEncoding = errors.New("zencoding: invalid encoding")
)

// Writer builds a canonical byte string.
type Writer struct {
	b []byte
}

// NewWriter returns an empty Writer with modest pre-allocation.
func NewWriter() *Writer { return &Writer{b: make([]byte, 0, 128)} }

// NewWriterSize returns a Writer pre-allocated to n bytes.
func NewWriterSize(n int) *Writer { return &Writer{b: make([]byte, 0, n)} }

// U8 appends one byte.
func (w *Writer) U8(v uint8) { w.b = append(w.b, v) }

// U16 appends a big-endian uint16.
func (w *Writer) U16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }

// U32 appends a big-endian uint32.
func (w *Writer) U32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }

// U64 appends a big-endian uint64.
func (w *Writer) U64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }

// I64 appends a big-endian two's-complement int64.
func (w *Writer) I64(v int64) { w.b = binary.BigEndian.AppendUint64(w.b, uint64(v)) }

// Bool appends 0x01 for true, 0x00 for false.
func (w *Writer) Bool(v bool) {
	if v {
		w.b = append(w.b, 1)
		return
	}
	w.b = append(w.b, 0)
}

// Raw appends bytes with no length prefix. Only for use inside a hand-built
// fixed-layout structure whose width the reader already knows.
func (w *Writer) Raw(b []byte) { w.b = append(w.b, b...) }

// Bytes appends a u32 length prefix followed by the bytes.
func (w *Writer) Bytes(b []byte) {
	w.U32(uint32(len(b)))
	w.b = append(w.b, b...)
}

// Str appends a u32 length prefix followed by the UTF-8 bytes.
func (w *Writer) Str(s string) {
	w.U32(uint32(len(s)))
	w.b = append(w.b, s...)
}

// Len reports how many bytes have been written.
func (w *Writer) Len() int { return len(w.b) }

// Done returns the encoded bytes. The Writer must not be used afterwards.
func (w *Writer) Done() []byte { return w.b }

// Reader parses canonical bytes. On the first error every subsequent call is a
// no-op returning the zero value, so callers may check Err once at the end.
type Reader struct {
	b   []byte
	off int
	err error
}

// NewReader wraps b. The Reader keeps a reference; it does not copy.
func NewReader(b []byte) *Reader { return &Reader{b: b} }

func (r *Reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// Err returns the first error encountered, or nil.
func (r *Reader) Err() error { return r.err }

// Remaining reports how many unread bytes are left.
func (r *Reader) Remaining() int {
	if r.err != nil {
		return 0
	}
	return len(r.b) - r.off
}

// Offset reports the current read position.
func (r *Reader) Offset() int { return r.off }

// Done asserts the buffer has been fully consumed. Call it after parsing.
func (r *Reader) Done() error {
	if r.err != nil {
		return r.err
	}
	if r.off != len(r.b) {
		r.fail(ErrTrailingBytes)
		return r.err
	}
	return nil
}

func (r *Reader) take(n int) ([]byte, bool) {
	if r.err != nil {
		return nil, false
	}
	if n < 0 || len(r.b)-r.off < n {
		r.fail(ErrShortBuffer)
		return nil, false
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, true
}

// U8 reads one byte.
func (r *Reader) U8() uint8 {
	s, ok := r.take(1)
	if !ok {
		return 0
	}
	return s[0]
}

// U16 reads a big-endian uint16.
func (r *Reader) U16() uint16 {
	s, ok := r.take(2)
	if !ok {
		return 0
	}
	return binary.BigEndian.Uint16(s)
}

// U32 reads a big-endian uint32.
func (r *Reader) U32() uint32 {
	s, ok := r.take(4)
	if !ok {
		return 0
	}
	return binary.BigEndian.Uint32(s)
}

// U64 reads a big-endian uint64.
func (r *Reader) U64() uint64 {
	s, ok := r.take(8)
	if !ok {
		return 0
	}
	return binary.BigEndian.Uint64(s)
}

// I64 reads a big-endian two's-complement int64.
func (r *Reader) I64() int64 {
	s, ok := r.take(8)
	if !ok {
		return 0
	}
	return int64(binary.BigEndian.Uint64(s))
}

// Bool reads a strictly canonical boolean.
func (r *Reader) Bool() bool {
	v := r.U8()
	switch v {
	case 0:
		return false
	case 1:
		return true
	default:
		r.fail(ErrNonCanonicalBool)
		return false
	}
}

// Raw reads exactly n bytes as a sub-slice of the input (no copy).
func (r *Reader) Raw(n int) []byte {
	s, ok := r.take(n)
	if !ok {
		return nil
	}
	return s
}

// Bytes reads a u32 length prefix and that many bytes. The result is a sub-slice
// of the input buffer; callers that retain it must copy.
func (r *Reader) Bytes(limit int) []byte {
	n := r.U32()
	if r.err != nil {
		return nil
	}
	if limit <= 0 || limit > MaxBytesLen {
		limit = MaxBytesLen
	}
	if int(n) > limit {
		r.fail(ErrLengthLimit)
		return nil
	}
	return r.Raw(int(n))
}

// Str reads a length-prefixed UTF-8 string.
func (r *Reader) Str() string {
	b := r.Bytes(MaxStrLen)
	if r.err != nil {
		return ""
	}
	return string(b)
}

// Len32 reads a repeat-count and validates it against both the configured cap
// and the remaining buffer, so a bogus count cannot drive a huge allocation.
func (r *Reader) Len32(minElementSize int) int {
	n := r.U32()
	if r.err != nil {
		return 0
	}
	if int(n) > MaxItems {
		r.fail(ErrLengthLimit)
		return 0
	}
	if minElementSize > 0 && int(n)*minElementSize > r.Remaining() {
		// Cheap structural sanity check: cannot possibly be true.
		r.fail(ErrShortBuffer)
		return 0
	}
	return int(n)
}
