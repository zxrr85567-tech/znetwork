package zcrypto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The keystore is the only thing standing between a stolen file and a stolen
// key, so these tests attack it: wrong passwords, weakened KDF parameters,
// swapped identities, truncated ciphertext and unknown JSON fields.

func testKey(t *testing.T) *PrivateKey {
	t.Helper()
	k, err := KeyFromSeed(Digest("keystore-test").Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const goodPassword = "correct horse battery staple"

func TestKeystoreRoundTrip(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ks.KDF.Iterations != DefaultKDFIterations {
		t.Fatalf("iterations = %d, want default %d", ks.KDF.Iterations, DefaultKDFIterations)
	}
	if ks.Version != KeystoreVersion || ks.KDF.Algo != KeystoreAlgoPBKDF2 || ks.Cipher.Algo != KeystoreAlgoAESGCM {
		t.Fatalf("keystore params: %+v", ks)
	}
	if ks.Address != k.Address().String() || ks.PublicKey != k.PublicKey().String() {
		t.Fatal("identity not recorded")
	}
	// The plaintext seed must not appear anywhere in the file.
	raw, err := ks.MarshalJSONBytes()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), hex.EncodeToString(k.Seed())) {
		t.Fatal("seed appears in the keystore file")
	}
	back, err := ks.Decrypt(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Seed(), k.Seed()) || back.Address() != k.Address() {
		t.Fatal("recovered key differs")
	}
}

func TestKeystoreWrongPasswordIsIndistinguishable(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range []string{"wrong password!!", "correct horse battery stapl", ""} {
		if _, err := ks.Decrypt(pw); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("password %q: error = %v, want ErrDecrypt", pw, err)
		}
	}
}

func TestKeystoreRejectsWeakParameters(t *testing.T) {
	k := testKey(t)
	// Short password.
	if _, err := EncryptKey(k, "short", 0); !errors.Is(err, ErrWeakParams) {
		t.Fatalf("short password error = %v", err)
	}
	// Explicit weak KDF below the floor.
	if _, err := EncryptKey(k, goodPassword, MinKDFIterations-1); !errors.Is(err, ErrWeakParams) {
		t.Fatalf("weak iterations error = %v", err)
	}
	// Nil key.
	if _, err := EncryptKey(nil, goodPassword, 0); err == nil {
		t.Fatal("nil key encrypted")
	}
}

func TestKeystoreRejectsDowngradedFile(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	// An attacker who edits the file to a weak iteration count must be rejected
	// before any decryption is attempted.
	ks.KDF.Iterations = 1000
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrWeakParams) {
		t.Fatalf("downgraded iterations error = %v", err)
	}
	// Wrong version, algorithms and short salt are all rejected.
	ks.KDF.Iterations = DefaultKDFIterations
	ks.Version = 2
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrUnsupportedKE) {
		t.Fatalf("version error = %v", err)
	}
	ks.Version = KeystoreVersion
	ks.KDF.Algo = "scrypt"
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrUnsupportedKE) {
		t.Fatal("unknown KDF accepted")
	}
	ks.KDF.Algo = KeystoreAlgoPBKDF2
	ks.KDF.Salt = "00"
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrBadKeystore) {
		t.Fatal("short salt accepted")
	}
}

func TestKeystoreIdentityCannotBeSwapped(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The address is authenticated as AAD; changing it must fail the tag.
	other := testKey2(t)
	ks.Address = other.Address().String()
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("swapped address error = %v", err)
	}
}

func testKey2(t *testing.T) *PrivateKey {
	t.Helper()
	k, err := KeyFromSeed(Digest("keystore-test-2").Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeystoreRejectsTamperedCiphertext(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := hex.DecodeString(ks.Cipher.Ciphertext)
	for _, idx := range []int{0, len(ct) / 2, len(ct) - 1} {
		corrupt := append([]byte{}, ct...)
		corrupt[idx] ^= 0x01
		ks.Cipher.Ciphertext = hex.EncodeToString(corrupt)
		if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("byte %d: error = %v, want ErrDecrypt", idx, err)
		}
	}
	// Truncated ciphertext (shorter than the GCM tag) is also rejected.
	ks.Cipher.Ciphertext = hex.EncodeToString(ct[:4])
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("truncated ciphertext error = %v", err)
	}
}

func TestKeystoreJSONIsStrict(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ks)
	parsed, err := ParseKeystore(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Address != ks.Address {
		t.Fatal("address lost in JSON round trip")
	}
	// Unknown fields are rejected: a future or hostile schema cannot smuggle
	// parameters past the parser.
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	generic["extra"] = true
	withExtra, _ := json.Marshal(generic)
	if _, err := ParseKeystore(withExtra); err == nil {
		t.Fatal("unknown field accepted")
	}
	// Missing identity fields are rejected.
	var noAddr map[string]any
	_ = json.Unmarshal(raw, &noAddr)
	delete(noAddr, "address")
	rawNoAddr, _ := json.Marshal(noAddr)
	if _, err := ParseKeystore(rawNoAddr); err == nil {
		t.Fatal("keystore without an address accepted")
	}
	if _, err := ParseKeystore([]byte("{")); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestKeystoreFilePermissions(t *testing.T) {
	k := testKey(t)
	ks, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "keys", "test.key.json")
	if err := SaveKeystore(path, ks); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A keystore must never be world- or group-readable.
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("keystore mode = %o, want 0600", st.Mode().Perm())
	}
	loaded, err := LoadKeystore(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Address != ks.Address {
		t.Fatal("address lost on disk round trip")
	}
	if _, err := LoadKeystore(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing keystore returned no error")
	}
}

func TestKeystoreIsNonDeterministic(t *testing.T) {
	k := testKey(t)
	a, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptKey(k, goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh salt and nonce every time: two encryptions of one key must differ,
	// otherwise identical keys are linkable across backups.
	if a.KDF.Salt == b.KDF.Salt || a.Cipher.Nonce == b.Cipher.Nonce || a.Cipher.Ciphertext == b.Cipher.Ciphertext {
		t.Fatal("keystores are deterministic")
	}
	if _, err := a.Decrypt(goodPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decrypt(goodPassword); err != nil {
		t.Fatal(err)
	}
}

func TestNilKeystoreIsRejected(t *testing.T) {
	var ks *Keystore
	if _, err := ks.Decrypt(goodPassword); !errors.Is(err, ErrBadKeystore) {
		t.Fatalf("nil keystore error = %v", err)
	}
}
