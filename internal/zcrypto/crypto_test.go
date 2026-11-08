package zcrypto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// ---- BIP-173 Bech32 test vectors ------------------------------------------

func TestBech32ValidVectors(t *testing.T) {
	valid := []string{
		"A12UEL5L",
		"a12uel5l",
		"an83characterlonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1tt5tgs",
		"abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw",
		"split1checkupstagehandshakeupstreamerranterredcaperred2y9e3w",
		"?1ezyfcl",
	}
	for _, v := range valid {
		if _, _, err := DecodeBech32(v); err != nil {
			t.Errorf("DecodeBech32(%q) = %v, want ok", v, err)
		}
	}
}

func TestBech32InvalidVectors(t *testing.T) {
	invalid := []string{
		"an84characterslonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1569pvx", // > 90 chars
		"pzry9x0s0muk",  // no separator
		"1pzry9x0s0muk", // empty HRP
		"x1b4n0q5v",     // invalid data character
		"li1dgmt3",      // too short
		"A1G7SGD8",      // checksum failure
		"10a06t8",       // too short
		"1qzzfhee",      // empty HRP
		"aBc1qpzry9x8",  // mixed case
	}
	for _, v := range invalid {
		if _, _, err := DecodeBech32(v); err == nil {
			t.Errorf("DecodeBech32(%q) succeeded, want error", v)
		}
	}
}

func TestAddressBech32RoundTrip(t *testing.T) {
	for i := 0; i < 200; i++ {
		k, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		a := k.Address()
		s := a.Bech32()
		if !strings.HasPrefix(s, "z1") {
			t.Fatalf("address %q lacks z1 prefix", s)
		}
		back, err := ParseAddressText(s)
		if err != nil {
			t.Fatalf("ParseAddressText(%q) = %v", s, err)
		}
		if back != a {
			t.Fatalf("roundtrip mismatch %v != %v", back, a)
		}
		if _, err := ParseAddressText(strings.ToUpper(s)); err != nil {
			t.Fatalf("uppercase must decode: %v", err)
		}
		// Single-character tampering must always be detected.
		b := []byte(s)
		b[len(b)-1] ^= 1
		if _, err := ParseAddressText(string(b)); err == nil {
			t.Fatalf("tampered address %q accepted", string(b))
		}
	}
}

func TestAddressRejectsWrongHRPAndLength(t *testing.T) {
	enc, err := encodeBech32("zt", make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAddressText(enc); err == nil {
		t.Fatal("wrong HRP accepted")
	}
	short, err := encodeBech32(AddressHRP, make([]byte, 19))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAddressText(short); err == nil {
		t.Fatal("19-byte payload accepted")
	}
}

// ---- Ed25519 (RFC 8032) ----------------------------------------------------

func TestEd25519RFC8032Vector1(t *testing.T) {
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	wantPub, _ := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	wantSig, _ := hex.DecodeString("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b")
	msg := []byte{}

	k, err := KeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(k.PublicKey().Bytes()) != hex.EncodeToString(wantPub) {
		t.Fatal("public key derivation mismatch")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(priv, msg)
	if hex.EncodeToString(sig) != hex.EncodeToString(wantSig) {
		t.Fatal("RFC 8032 signature mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(wantPub), msg, wantSig) {
		t.Fatal("RFC 8032 verification failed")
	}
}

func TestSignVerifyDomainSeparation(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("transfer 10 uz to z1xyz")
	sig := k.Sign(DomainTx, msg)
	if err := VerifySignature(k.PublicKey(), DomainTx, msg, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// A signature made in one domain must never verify in another.
	if err := VerifySignature(k.PublicKey(), DomainVote, msg, sig); err == nil {
		t.Fatal("cross-domain signature accepted")
	}
	// Different message must fail.
	if err := VerifySignature(k.PublicKey(), DomainTx, []byte("transfer 11 uz to z1xyz"), sig); err == nil {
		t.Fatal("signature valid for a different message")
	}
	// Deterministic: Ed25519 signatures must be byte-identical on repeat.
	if k.Sign(DomainTx, msg) != sig {
		t.Fatal("signatures are not deterministic")
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	k, _ := GenerateKey()
	sig := k.Sign(DomainTx, []byte("x"))
	for i := 0; i < 64; i += 17 {
		bad := sig
		bad[i] ^= 0x80
		if err := VerifySignature(k.PublicKey(), DomainTx, []byte("x"), bad); err == nil {
			t.Fatalf("tampered signature byte %d accepted", i)
		}
	}
}

func TestAddressSignatureBinding(t *testing.T) {
	k, _ := GenerateKey()
	other, _ := GenerateKey()
	msg := []byte("hello")
	sig := k.Sign(DomainTx, msg)
	if err := VerifyAddressSignature(k.Address(), k.PublicKey(), DomainTx, msg, sig); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	if err := VerifyAddressSignature(other.Address(), k.PublicKey(), DomainTx, msg, sig); err == nil {
		t.Fatal("address/public-key mismatch accepted")
	}
}

func TestDigestFramingIsUnambiguous(t *testing.T) {
	a := Digest(DomainTx, []byte("ab"), []byte("c"))
	b := Digest(DomainTx, []byte("a"), []byte("bc"))
	if a == b {
		t.Fatal("concatenation ambiguity: 'ab'+'c' collides with 'a'+'bc'")
	}
	if Digest(DomainTx, []byte("x")) == Digest(DomainVote, []byte("x")) {
		t.Fatal("domain separation not applied")
	}
}

func TestPublicKeySerializationRejectsGarbage(t *testing.T) {
	if _, err := ParsePublicKey("zz"); err == nil {
		t.Fatal("short hex accepted")
	}
	short := strings.Repeat("00", 31)
	if _, err := ParsePublicKey(short); err == nil {
		t.Fatal("31-byte key accepted")
	}
	if _, err := ParseHash(strings.Repeat("g", 64)); err == nil {
		t.Fatal("non-hex hash accepted")
	}
}

// ---- PBKDF2 ---------------------------------------------------------------

func TestPBKDF2RFC7914Vector(t *testing.T) {
	// RFC 7914 §11: PBKDF2-HMAC-SHA256(P="passwd", S="salt", c=1, dkLen=64)
	want := "55ac046e56e3089f" + "ec1691c22544b605" + "f94185216dde0465" + "e68b9d57c20dacbc" +
		"49ca9cccf179b645" + "991664b39d77ef31" + "7c71b845b1e30bd5" + "09112041d3a19783"
	got := hex.EncodeToString(PBKDF2([]byte("passwd"), []byte("salt"), 1, 64))
	if got != want {
		t.Fatalf("pbkdf2 = %s want %s", got, want)
	}
}

func TestPBKDF2IterationSensitivity(t *testing.T) {
	a := PBKDF2([]byte("p"), []byte("s"), 1000, 32)
	b := PBKDF2([]byte("p"), []byte("s"), 1001, 32)
	if hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Fatal("iteration count has no effect")
	}
	if len(PBKDF2([]byte("p"), []byte("s"), 10, 17)) != 17 {
		t.Fatal("dkLen not honoured")
	}
}

// ---- accessor and parsing coverage -----------------------------------------
//
// These exercise the string/parse helpers that every other package uses when
// moving keys and hashes to and from JSON, hex and bech32.

func TestHashAccessorsAndParsing(t *testing.T) {
	h := HashBytes([]byte("hello"))
	if h.IsZero() {
		t.Fatal("non-zero hash reported as zero")
	}
	if len(h.String()) != 64 {
		t.Fatalf("hash string length = %d", len(h.String()))
	}
	if len(h.Short()) != 8 || !strings.HasPrefix(h.String(), h.Short()) {
		t.Fatalf("short form = %q", h.Short())
	}
	if !bytes.Equal(h.Bytes(), h[:]) {
		t.Fatal("Bytes() differs from the array value")
	}
	if (Hash{}).IsZero() != true {
		t.Fatal("zero hash not reported as zero")
	}
	// Round trip through hex.
	parsed, err := ParseHash(h.String())
	if err != nil || parsed != h {
		t.Fatalf("ParseHash round trip: %v", err)
	}
	back, ok := HashFromBytes(h.Bytes())
	if !ok || back != h {
		t.Fatal("HashFromBytes failed on a 32-byte slice")
	}
	// Bad inputs.
	for _, bad := range []string{"", "zz", h.String()[:62], h.String() + "00"} {
		if _, err := ParseHash(bad); err == nil {
			t.Fatalf("ParseHash accepted %q", bad)
		}
	}
	if _, ok := HashFromBytes([]byte{1, 2, 3}); ok {
		t.Fatal("HashFromBytes accepted a short slice")
	}
}

func TestKeyAccessorsAndParsing(t *testing.T) {
	k, err := KeyFromSeed(Digest("accessor-test").Bytes())
	if err != nil {
		t.Fatal(err)
	}
	pub := k.PublicKey()
	if pub.IsZero() {
		t.Fatal("public key reported as zero")
	}
	if len(pub.String()) != 64 || len(pub.Bytes()) != 32 {
		t.Fatalf("public key string/bytes = %q/%d", pub.String(), len(pub.Bytes()))
	}
	parsedPub, err := ParsePublicKey(pub.String())
	if err != nil || parsedPub != pub {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	for _, bad := range []string{"", "00", strings.Repeat("zz", 32), strings.Repeat("00", 31)} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("ParsePublicKey accepted %q", bad)
		}
	}
	if k.Address().IsZero() {
		t.Fatal("address reported as zero")
	}
	// Address parsing in all three forms.
	addr := k.Address()
	byBech, err := ParseAddress(addr.String())
	if err != nil || byBech != addr {
		t.Fatalf("ParseAddress(bech32): %v", err)
	}
	byHex, err := ParseAddressHex(hex.EncodeToString(addr[:]))
	if err != nil || byHex != addr {
		t.Fatalf("ParseAddressHex: %v", err)
	}
	fromBytes, err := AddressFromBytes(addr[:])
	if err != nil || fromBytes != addr {
		t.Fatalf("AddressFromBytes: %v", err)
	}
	if _, err := AddressFromBytes([]byte{1, 2}); err == nil {
		t.Fatal("AddressFromBytes accepted a short slice")
	}
	if _, err := ParseAddress("not-an-address"); err == nil {
		t.Fatal("ParseAddress accepted garbage")
	}
	// Signatures round trip through hex.
	sig := k.Sign(DomainTx, []byte("message"))
	parsedSig, err := ParseSignature(hex.EncodeToString(sig[:]))
	if err != nil || parsedSig != sig {
		t.Fatalf("ParseSignature: %v", err)
	}
	if _, err := ParseSignature("00"); err == nil {
		t.Fatal("ParseSignature accepted a short signature")
	}
	if (Signature{}).IsZero() != true {
		t.Fatal("zero signature not reported as zero")
	}
}

func TestGenerateKeyIsRandom(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if a.Address() == b.Address() {
		t.Fatal("two generated keys share an address")
	}
	// A generated key must sign and verify.
	msg := []byte("generated key test")
	if err := VerifySignature(a.PublicKey(), DomainTx, msg, a.Sign(DomainTx, msg)); err != nil {
		t.Fatalf("generated key cannot verify: %v", err)
	}
	if err := VerifySignature(b.PublicKey(), DomainTx, msg, a.Sign(DomainTx, msg)); err == nil {
		t.Fatal("signature verified under the wrong key")
	}
}
