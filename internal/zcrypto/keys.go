package zcrypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Domain separation strings. Adding a new one requires a protocol version bump
// (see specifications/VERSIONING_POLICY.md) because it changes signature preimages.
const (
	// DomainTx separates transaction signatures.
	DomainTx = "ZNET/v1/transaction"
	// DomainVote separates consensus vote signatures.
	DomainVote = "ZNET/v1/consensus-vote"
	// DomainP2P separates peer handshake signatures.
	DomainP2P = "ZNET/v1/peer-handshake"
	// DomainAddress separates address derivation from every other hash use.
	DomainAddress = "ZNET/v1/address"
	// DomainGenesis separates the genesis hash.
	DomainGenesis = "ZNET/v1/genesis"
	// DomainBlock separates block header hashes.
	DomainBlock = "ZNET/v1/block-header"
	// DomainStateLeaf separates state tree leaves.
	DomainStateLeaf = "ZNET/v1/state-leaf"
	// DomainStateNode separates state tree internal nodes.
	DomainStateNode = "ZNET/v1/state-node"
	// DomainTxRoot / DomainReceiptRoot separate the per-block Merkle roots.
	DomainTxRoot      = "ZNET/v1/tx-root"
	DomainReceiptRoot = "ZNET/v1/receipt-root"
	// DomainContractAddr separates contract address derivation.
	DomainContractAddr = "ZNET/v1/contract-address"
	// DomainKeystoreCheck separates the keystore password check value.
	DomainKeystoreCheck = "ZNET/v1/keystore-check"
)

// Errors returned by this package. They are stable identifiers: callers may
// compare with errors.Is.
var (
	ErrBadHash       = errors.New("zcrypto: malformed hash")
	ErrBadSignature  = errors.New("zcrypto: invalid signature")
	ErrBadPublicKey  = errors.New("zcrypto: malformed public key")
	ErrBadSeed       = errors.New("zcrypto: malformed private seed")
	ErrBadAddress    = errors.New("zcrypto: malformed address")
	ErrWeakParams    = errors.New("zcrypto: insecure KDF parameters")
	ErrDecrypt       = errors.New("zcrypto: decryption failed (wrong password or corrupted keystore)")
	ErrBadKeystore   = errors.New("zcrypto: malformed keystore")
	ErrMismatchKey   = errors.New("zcrypto: public key does not match address")
	ErrNoEntropy     = errors.New("zcrypto: entropy source failed")
	ErrUnsupportedKE = errors.New("zcrypto: unsupported keystore version or algorithm")
)

// PublicKey is an Ed25519 public key (32 bytes).
type PublicKey [32]byte

// Bytes returns a copy of the key bytes.
func (p PublicKey) Bytes() []byte {
	b := make([]byte, 32)
	copy(b, p[:])
	return b
}

// String returns hex.
func (p PublicKey) String() string { return hex.EncodeToString(p[:]) }

// IsZero reports whether the key is the zero value (never valid as an identity).
func (p PublicKey) IsZero() bool { return p == PublicKey{} }

// ParsePublicKey decodes a 64-char hex public key.
func ParsePublicKey(s string) (PublicKey, error) {
	var p PublicKey
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return p, ErrBadPublicKey
	}
	copy(p[:], b)
	return p, nil
}

// Address returns the account address derived from the public key.
func (p PublicKey) Address() Address { return AddressFromPub(p) }

// Signature is an Ed25519 signature (64 bytes).
type Signature [64]byte

// String returns hex.
func (s Signature) String() string { return hex.EncodeToString(s[:]) }

// IsZero reports whether the signature is the zero value.
func (s Signature) IsZero() bool { return s == Signature{} }

// ParseSignature decodes a 128-char hex signature.
func ParseSignature(str string) (Signature, error) {
	var s Signature
	b, err := hex.DecodeString(str)
	if err != nil || len(b) != 64 {
		return s, ErrBadSignature
	}
	copy(s[:], b)
	return s, nil
}

// Address is a 20-byte Z Network account address. Its canonical textual form is
// Bech32 with human-readable part "z" (e.g. "z1...").
type Address [20]byte

// Bytes returns a copy of the address bytes.
func (a Address) Bytes() []byte {
	b := make([]byte, 20)
	copy(b, a[:])
	return b
}

// Hex returns 40 lowercase hex characters (used in JSON for compactness).
func (a Address) Hex() string { return hex.EncodeToString(a[:]) }

// String returns the canonical Bech32 form.
func (a Address) String() string { return a.Bech32() }

// IsZero reports whether the address is empty.
func (a Address) IsZero() bool { return a == Address{} }

// AddressFromPub derives addr = SHA-256(digest(DomainAddress, pubkey))[12:32].
func AddressFromPub(p PublicKey) Address {
	d := Digest(DomainAddress, p[:])
	var a Address
	copy(a[:], d[12:32])
	return a
}

// AddressFromBytes copies 20 bytes into an Address.
func AddressFromBytes(b []byte) (Address, error) {
	var a Address
	if len(b) != 20 {
		return a, ErrBadAddress
	}
	copy(a[:], b)
	return a, nil
}

// ParseAddressHex decodes a 40-character hex address.
func ParseAddressHex(s string) (Address, error) {
	var a Address
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return a, ErrBadAddress
	}
	copy(a[:], b)
	return a, nil
}

// ParseAddress accepts either the canonical Bech32 form or 40-char hex.
func ParseAddress(s string) (Address, error) { return ParseAddressText(s) }

// PrivateKey is an Ed25519 private key with its seed retained for deterministic
// re-derivation (keystore encryption operates on the seed).
type PrivateKey struct {
	seed [32]byte
	priv ed25519.PrivateKey
	pub  PublicKey
}

// GenerateKey creates a new key from crypto/rand.
func GenerateKey() (*PrivateKey, error) {
	seed := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoEntropy, err)
	}
	return KeyFromSeed(seed)
}

// KeyFromSeed derives a private key from a 32-byte seed (RFC 8032).
func KeyFromSeed(seed []byte) (*PrivateKey, error) {
	if len(seed) != 32 {
		return nil, ErrBadSeed
	}
	k := &PrivateKey{}
	copy(k.seed[:], seed)
	k.priv = ed25519.NewKeyFromSeed(k.seed[:])
	copy(k.pub[:], k.priv.Public().(ed25519.PublicKey))
	return k, nil
}

// Seed returns a copy of the 32-byte seed. Treat as secret.
func (k *PrivateKey) Seed() []byte {
	b := make([]byte, 32)
	copy(b, k.seed[:])
	return b
}

// PublicKey returns the public key. A *PrivateKey must not be nil.
func (k *PrivateKey) PublicKey() PublicKey { return k.pub }

// Address returns the address of the key.
func (k *PrivateKey) Address() Address { return AddressFromPub(k.pub) }

// Sign signs msg under the given domain:
//
//	sig = Ed25519_sign(priv, Digest(domain, msg))
//
// Ed25519 is deterministic, so identical inputs always produce identical
// signatures (required for consensus-safe transaction hashing and for
// reproducible test fixtures).
func (k *PrivateKey) Sign(domain string, msg []byte) Signature {
	pre := Digest(domain, msg)
	raw := ed25519.Sign(k.priv, pre[:])
	var s Signature
	copy(s[:], raw)
	return s
}

// VerifySignature checks a domain-separated signature. It never panics: malformed
// keys or signatures yield ErrBadSignature.
func VerifySignature(pub PublicKey, domain string, msg []byte, sig Signature) error {
	if pub.IsZero() {
		return ErrBadPublicKey
	}
	pre := Digest(domain, msg)
	if len(sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	if !ed25519.Verify(ed25519.PublicKey(pub[:]), pre[:], sig[:]) {
		return ErrBadSignature
	}
	return nil
}

// VerifyAddressSignature verifies that sig over msg (in domain) was produced by
// the private key owning addr. This is the check used for transactions.
func VerifyAddressSignature(addr Address, pub PublicKey, domain string, msg []byte, sig Signature) error {
	if subtle.ConstantTimeCompare(AddressFromPub(pub).Bytes(), addr.Bytes()) != 1 {
		return ErrMismatchKey
	}
	return VerifySignature(pub, domain, msg, sig)
}
