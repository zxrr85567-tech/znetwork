// Package zcrypto is Z Network's authoritative cryptographic module (AGENT_08).
//
// Frozen public surface (see specifications/cryptography/CRYPTOGRAPHY_SPECIFICATION.md):
//
//	Hash / Digest        SHA-256, domain-separated, unambiguous framing
//	PrivateKey, PublicKey, Signature, Address
//	GenerateKey / KeyFromSeed
//	Bech32 Encode/Decode (BIP-173) with HRP "z"
//	PBKDF2-HMAC-SHA256 (RFC 8018)
//	Keystore (encrypted private key files)
//
// Algorithms are chosen from the Go standard library only:
//   - Signature: Ed25519 (RFC 8032), deterministic, non-malleable.
//   - Hash: SHA-256 (FIPS 180-4).
//   - No third-party dependency exists anywhere in this module.
//
// Every exported operation that touches untrusted input validates it and returns
// an error rather than panicking.
package zcrypto

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash is a 32-byte SHA-256 digest.
type Hash [32]byte

// HashBytes hashes the concatenation of the parts. This form is only safe when
// the caller's framing is itself unambiguous; prefer Digest.
func HashBytes(parts ...[]byte) Hash {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out Hash
	h.Sum(out[:0])
	return out
}

// Digest is the domain-separated, length-prefixed hash used throughout the
// protocol:
//
//	Digest(domain, a, b) = SHA-256( u32(len(domain)) || domain || u32(len(a)) || a || u32(len(b)) || b )
//
// Length prefixes remove any concatenation ambiguity, so
// Digest(d, "ab", "c") != Digest(d, "a", "bc"). The domain string separates
// every use of the hash function (transactions, headers, state leaves, votes,
// signatures) from every other use (threat T-21 / cross-protocol reuse).
func Digest(domain string, parts ...[]byte) Hash {
	h := sha256.New()
	var lb [4]byte
	putLen := func(n int) {
		lb[0] = byte(n >> 24)
		lb[1] = byte(n >> 16)
		lb[2] = byte(n >> 8)
		lb[3] = byte(n)
		h.Write(lb[:])
	}
	putLen(len(domain))
	h.Write([]byte(domain))
	for _, p := range parts {
		putLen(len(p))
		h.Write(p)
	}
	var out Hash
	h.Sum(out[:0])
	return out
}

// Bytes returns the digest as a byte slice (allocating copy).
func (h Hash) Bytes() []byte {
	b := make([]byte, 32)
	copy(b, h[:])
	return b
}

// String returns the lowercase hex encoding.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// Short returns the first 8 hex characters, for logs only.
func (h Hash) Short() string { return hex.EncodeToString(h[:4]) }

// IsZero reports whether the digest is all zeroes.
func (h Hash) IsZero() bool { return h == Hash{} }

// HashFromBytes copies b into a Hash, returning false unless len(b) == 32.
func HashFromBytes(b []byte) (Hash, bool) {
	var h Hash
	if len(b) != 32 {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// ParseHash decodes a 64-character hex string.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 64 {
		return h, ErrBadHash
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, ErrBadHash
	}
	copy(h[:], b)
	return h, nil
}
