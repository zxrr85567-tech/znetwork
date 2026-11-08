package zencoding

import (
	"bytes"
	"errors"
	"testing"
)

func TestRoundTripPrimitives(t *testing.T) {
	w := NewWriter()
	w.U8(0xab)
	w.U16(0xbeef)
	w.U32(0xdeadbeef)
	w.U64(0x0123456789abcdef)
	w.I64(-42)
	w.Bool(true)
	w.Bool(false)
	w.Bytes([]byte{1, 2, 3})
	w.Str("z-network")
	b := w.Done()

	r := NewReader(b)
	if got := r.U8(); got != 0xab {
		t.Fatalf("U8 = %x", got)
	}
	if got := r.U16(); got != 0xbeef {
		t.Fatalf("U16 = %x", got)
	}
	if got := r.U32(); got != 0xdeadbeef {
		t.Fatalf("U32 = %x", got)
	}
	if got := r.U64(); got != 0x0123456789abcdef {
		t.Fatalf("U64 = %x", got)
	}
	if got := r.I64(); got != -42 {
		t.Fatalf("I64 = %d", got)
	}
	if !r.Bool() {
		t.Fatal("Bool(0) = false")
	}
	if r.Bool() {
		t.Fatal("Bool(1) = true")
	}
	if got := r.Bytes(16); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("Bytes = %x", got)
	}
	if got := r.Str(); got != "z-network" {
		t.Fatalf("Str = %q", got)
	}
	if err := r.Done(); err != nil {
		t.Fatalf("Done = %v", err)
	}
}

func TestDeterministicOutput(t *testing.T) {
	build := func() []byte {
		w := NewWriter()
		w.U64(1)
		w.Bytes([]byte("abc"))
		return w.Done()
	}
	if !bytes.Equal(build(), build()) {
		t.Fatal("encoding is not deterministic")
	}
	// Known-answer: u64(1) || len(3) || "abc"
	want := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 3, 'a', 'b', 'c'}
	if !bytes.Equal(build(), want) {
		t.Fatalf("got %x want %x", build(), want)
	}
}

func TestTruncatedInputIsRejected(t *testing.T) {
	cases := [][]byte{
		{},                       // U64 on empty
		{0, 0, 0},                // partial u32
		{0, 0, 0, 5, 'a'},        // declares 5 bytes, has 1
		{0xff, 0xff, 0xff, 0xff}, // huge declared length
	}
	for i, c := range cases {
		r := NewReader(c)
		_ = r.U64()
		_ = r.Bytes(MaxBytesLen)
		_ = r.Str()
		if r.Err() == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestTrailingBytesRejected(t *testing.T) {
	r := NewReader([]byte{1, 2})
	if r.U8() != 1 {
		t.Fatal("U8")
	}
	if err := r.Done(); !errors.Is(err, ErrTrailingBytes) {
		t.Fatalf("Done = %v, want ErrTrailingBytes", err)
	}
}

func TestNonCanonicalBoolRejected(t *testing.T) {
	for _, b := range []byte{2, 0xff} {
		r := NewReader([]byte{b})
		_ = r.Bool()
		if !errors.Is(r.Err(), ErrNonCanonicalBool) {
			t.Fatalf("byte %x: got %v", b, r.Err())
		}
	}
}

func TestLengthLimitEnforced(t *testing.T) {
	w := NewWriter()
	w.U32(1000)
	w.Raw(make([]byte, 1000))
	r := NewReader(w.Done())
	_ = r.Bytes(10) // caller limit 10 < declared 1000
	if !errors.Is(r.Err(), ErrLengthLimit) {
		t.Fatalf("got %v, want ErrLengthLimit", r.Err())
	}
}

func TestLen32StructuralCheck(t *testing.T) {
	w := NewWriter()
	w.U32(1000000) // claims a million elements, buffer is empty
	r := NewReader(w.Done())
	if n := r.Len32(8); n != 0 || r.Err() == nil {
		t.Fatalf("expected rejection, got n=%d err=%v", n, r.Err())
	}
}

// FuzzDecode asserts that the decoder never panics and that any successfully
// decoded value re-encodes to the identical byte string (canonicality).
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 3, 'a', 'b', 'c'})
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(in)
		b := r.Bytes(MaxBytesLen)
		if r.Err() != nil {
			return
		}
		w := NewWriter()
		w.Bytes(b)
		if !bytes.Equal(w.Done(), in[:r.Offset()]) {
			t.Fatalf("non-canonical: %x decodes to %x", in[:r.Offset()], w.Done())
		}
	})
}
