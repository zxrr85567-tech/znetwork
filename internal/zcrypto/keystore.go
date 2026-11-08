package zcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Encrypted keystore (ZKS1).
//
// On-disk JSON:
//
//	{
//	  "version": 1,
//	  "address": "z1...",
//	  "publicKey": "<64 hex>",
//	  "createdAt": "RFC3339",
//	  "kdf":    {"algo": "pbkdf2-hmac-sha256", "iterations": 600000, "salt": "<hex>"},
//	  "cipher": {"algo": "aes-256-gcm", "nonce": "<hex>", "ciphertext": "<hex>"}
//	}
//
// The GCM authentication tag makes a wrong password indistinguishable from a
// corrupted file, and both are reported as ErrDecrypt: no partial information is
// leaked about the plaintext or the key.
const (
	// KeystoreVersion is the only accepted version.
	KeystoreVersion = 1
	// MinKDFIterations is the floor for PBKDF2. Anything lower is rejected, so a
	// downgrade attack via a crafted keystore file cannot weaken a key.
	MinKDFIterations = 600000
	// KeystoreAlgoPBKDF2 and KeystoreAlgoAESGCM are the only supported algorithms.
	KeystoreAlgoPBKDF2 = "pbkdf2-hmac-sha256"
	KeystoreAlgoAESGCM = "aes-256-gcm"
	// MinPasswordLen is a hard floor; callers should enforce more (see wallet docs).
	MinPasswordLen = 10
)

// KDFParams describes the password-based key derivation.
type KDFParams struct {
	Algo       string `json:"algo"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
}

// CipherParams describes the authenticated encryption.
type CipherParams struct {
	Algo       string `json:"algo"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Keystore is an encrypted private key file.
type Keystore struct {
	Version   int          `json:"version"`
	Address   string       `json:"address"`
	PublicKey string       `json:"publicKey"`
	CreatedAt string       `json:"createdAt"`
	KDF       KDFParams    `json:"kdf"`
	Cipher    CipherParams `json:"cipher"`
}

// DefaultKDFIterations is the iteration count used when a caller passes 0. It is
// above MinKDFIterations on purpose: raising the floor later must never weaken
// keys created today.
const DefaultKDFIterations = 600_000

// EncryptKey wraps k in an authenticated, password-encrypted keystore.
//
// A zero iterations value means "use DefaultKDFIterations". Any explicitly
// requested value below MinKDFIterations is rejected, so a caller cannot silently
// weaken key protection.
func EncryptKey(k *PrivateKey, password string, iterations int) (*Keystore, error) {
	if k == nil {
		return nil, ErrBadSeed
	}
	if len(password) < MinPasswordLen {
		return nil, fmt.Errorf("%w: password shorter than %d characters", ErrWeakParams, MinPasswordLen)
	}
	if iterations == 0 {
		iterations = DefaultKDFIterations
	}
	if iterations < MinKDFIterations {
		return nil, fmt.Errorf("%w: KDF iterations below %d", ErrWeakParams, MinKDFIterations)
	}
	salt := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoEntropy, err)
	}
	key := PBKDF2([]byte(password), salt, iterations, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoEntropy, err)
	}
	pub := k.PublicKey()
	// Additional authenticated data binds the ciphertext to the address, so a
	// keystore's identity cannot be swapped without detection.
	aad := []byte(k.Address().String())
	ct := gcm.Seal(nil, nonce, k.Seed(), aad)
	return &Keystore{
		Version:   KeystoreVersion,
		Address:   k.Address().String(),
		PublicKey: pub.String(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		KDF: KDFParams{
			Algo:       KeystoreAlgoPBKDF2,
			Iterations: iterations,
			Salt:       hex.EncodeToString(salt),
		},
		Cipher: CipherParams{
			Algo:       KeystoreAlgoAESGCM,
			Nonce:      hex.EncodeToString(nonce),
			Ciphertext: hex.EncodeToString(ct),
		},
	}, nil
}

// Decrypt recovers the private key. It validates every parameter before use.
func (ks *Keystore) Decrypt(password string) (*PrivateKey, error) {
	if ks == nil {
		return nil, ErrBadKeystore
	}
	if ks.Version != KeystoreVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedKE, ks.Version)
	}
	if ks.KDF.Algo != KeystoreAlgoPBKDF2 || ks.Cipher.Algo != KeystoreAlgoAESGCM {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnsupportedKE, ks.KDF.Algo, ks.Cipher.Algo)
	}
	if ks.KDF.Iterations < MinKDFIterations {
		return nil, fmt.Errorf("%w: KDF iterations below %d", ErrWeakParams, ks.KDF.Iterations)
	}
	salt, err := hex.DecodeString(ks.KDF.Salt)
	if err != nil || len(salt) < 16 {
		return nil, ErrBadKeystore
	}
	nonce, err := hex.DecodeString(ks.Cipher.Nonce)
	if err != nil {
		return nil, ErrBadKeystore
	}
	ct, err := hex.DecodeString(ks.Cipher.Ciphertext)
	if err != nil {
		return nil, ErrBadKeystore
	}
	key := PBKDF2([]byte(password), salt, ks.KDF.Iterations, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, ErrBadKeystore
	}
	aad := []byte(ks.Address)
	seed, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	k, err := KeyFromSeed(seed)
	if err != nil {
		return nil, err
	}
	// Defence in depth: the recovered key must match the recorded identity.
	if subtle.ConstantTimeCompare([]byte(k.Address().String()), []byte(ks.Address)) != 1 {
		return nil, ErrMismatchKey
	}
	return k, nil
}

// MarshalJSONBytes returns the canonical JSON encoding of the keystore.
func (ks *Keystore) MarshalJSONBytes() ([]byte, error) { return json.MarshalIndent(ks, "", "  ") }

// ParseKeystore decodes keystore JSON, rejecting unknown or missing fields.
func ParseKeystore(b []byte) (*Keystore, error) {
	var ks Keystore
	dec := json.NewDecoder(newByteReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ks); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKeystore, err)
	}
	if ks.Address == "" || ks.PublicKey == "" {
		return nil, ErrBadKeystore
	}
	return &ks, nil
}

// SaveKeystore writes the keystore with 0600 permissions, atomically.
func SaveKeystore(path string, ks *Keystore) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := ks.MarshalJSONBytes()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
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

// LoadKeystore reads and parses a keystore file.
func LoadKeystore(path string) (*Keystore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKeystore(b)
}

// byteReader avoids importing bytes just for json.NewDecoder.
type byteReader struct {
	b []byte
	i int
}

func newByteReader(b []byte) *byteReader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
