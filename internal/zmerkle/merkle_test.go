package zmerkle

import (
	"testing"

	"znetwork/internal/zcrypto"
)

func h(b byte) Hash {
	var x Hash
	for i := range x {
		x[i] = b
	}
	return x
}

func TestRootDeterministicAndOrdered(t *testing.T) {
	a := []Hash{h(1), h(2), h(3)}
	b := []Hash{h(1), h(2), h(3)}
	if Root("d", a) != Root("d", b) {
		t.Fatal("root not deterministic")
	}
	c := []Hash{h(1), h(3), h(2)}
	if Root("d", a) == Root("d", c) {
		t.Fatal("root ignores leaf order")
	}
	if Root("d", a) == Root("e", a) {
		t.Fatal("root ignores domain")
	}
}

func TestEmptyRootIsDefined(t *testing.T) {
	if Root("d", nil).IsZero() {
		t.Fatal("empty root must be a domain-separated hash, not zero")
	}
}

// Promotion must prevent the classic duplicate-leaf ambiguity: [a,b,c] and
// [a,b,c,c] must not collide.
func TestPromotionPreventsDuplicateAmbiguity(t *testing.T) {
	x := Root("d", []Hash{h(1), h(2), h(3)})
	y := Root("d", []Hash{h(1), h(2), h(3), h(3)})
	if x == y {
		t.Fatal("duplicate-leaf collision detected")
	}
}

func TestProofsVerify(t *testing.T) {
	for n := 1; n <= 17; n++ {
		leaves := make([]Hash, n)
		dom := "ZNET/v1/state"
		kvs := make([]KV, n)
		for i := 0; i < n; i++ {
			kvs[i] = KV{Key: []byte{byte(i)}, Value: []byte{byte(i * 7)}}
			leaves[i] = zcrypto.Digest(dom+"-leaf", kvs[i].Key, kvs[i].Value)
		}
		root := RootOf(dom, kvs)
		for i := 0; i < n; i++ {
			p, ok := BuildProof(dom, leaves, i)
			if !ok {
				t.Fatalf("n=%d i=%d: BuildProof failed", n, i)
			}
			if err := VerifyProof(dom, leaves[i], p, root); err != nil {
				t.Fatalf("n=%d i=%d: proof does not verify: %v", n, i, err)
			}
			// A wrong leaf must fail.
			if err := VerifyProof(dom, h(0xff), p, root); err == nil {
				t.Fatalf("n=%d i=%d: proof accepted a wrong leaf", n, i)
			}
		}
	}
}

func TestProofEncodeDecode(t *testing.T) {
	dom := "d"
	leaves := []Hash{h(1), h(2), h(3), h(4), h(5)}
	p, _ := BuildProof(dom, leaves, 3)
	b := EncodeProof(p)
	back, err := DecodeProof(b)
	if err != nil {
		t.Fatalf("DecodeProof: %v", err)
	}
	if back.LeafIndex != p.LeafIndex || len(back.Steps) != len(p.Steps) {
		t.Fatal("proof roundtrip mismatch")
	}
	if err := VerifyProof(dom, leaves[3], back, Root(dom, leaves)); err != nil {
		t.Fatalf("decoded proof does not verify: %v", err)
	}
	if _, err := DecodeProof(b[:len(b)-1]); err == nil {
		t.Fatal("truncated proof accepted")
	}
	if _, err := DecodeProof(append(b, 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
}

func FuzzProofDecode(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(EncodeProof(Proof{LeafIndex: 1, LeafCount: 2, Steps: []Step{{Sibling: h(1)}}}))
	f.Fuzz(func(t *testing.T, in []byte) {
		_, _ = DecodeProof(in) // must never panic
	})
}
