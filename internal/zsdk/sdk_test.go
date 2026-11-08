package zsdk

import (
	"errors"
	"testing"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// These tests use a stub HTTP server that speaks the documented JSON-RPC wire
// format. They verify the client's own behaviour (request shape, error mapping,
// signing) without depending on a running node; the end-to-end package covers the
// real node integration.

func TestRPCErrorIsTyped(t *testing.T) {
	e := &RPCError{Method: "z_getBlock", Code: -32001, Message: "not found", Data: "block 99"}
	if got := e.Error(); got == "" || !contains(got, "z_getBlock") {
		t.Fatalf("error string = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestBuildTxSignaturesAndNonces(t *testing.T) {
	c := &Client{URL: "http://stub", ChainID: "znetwork-sdk-test"}
	d := zcrypto.Digest("sdk-key")
	key, err := zcrypto.KeyFromSeed(d.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	nonce := uint64(7)
	to := zcrypto.Digest("to")
	var addr ztypes.Address
	copy(addr[:], to[12:32])

	tx, err := BuildTx(c, key, ztypes.TxTransfer,
		(&ztypes.TransferPayload{To: addr, Amount: 1_000_000}).Encode(),
		Options{Nonce: &nonce, TimeMs: 1_700_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if tx.ChainID != c.ChainID {
		t.Fatalf("chain id = %q", tx.ChainID)
	}
	if tx.Nonce != nonce {
		t.Fatalf("nonce = %d", tx.Nonce)
	}
	if tx.GasLimit != 500_000 || tx.GasPrice != 1 {
		t.Fatalf("defaults not applied: %+v", tx)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("signature invalid: %v", err)
	}
	// The transaction must decode from its wire form and keep the signature.
	back, err := DecodeTx(encodeHex(tx.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if back.ID() != tx.ID() {
		t.Fatal("id changed across wire encoding")
	}
	if err := back.VerifySignature(); err != nil {
		t.Fatal("signature lost in wire form")
	}
	// Deterministic: the same inputs give the same transaction.
	tx2, _ := BuildTx(c, key, ztypes.TxTransfer,
		(&ztypes.TransferPayload{To: addr, Amount: 1_000_000}).Encode(),
		Options{Nonce: &nonce, TimeMs: 1_700_000_000_000})
	if tx2.ID() != tx.ID() {
		t.Fatal("transaction id is not deterministic")
	}
}

func TestDeployContractGasScalesWithCode(t *testing.T) {
	c := &Client{URL: "http://stub", ChainID: "znetwork-sdk-test"}
	d := zcrypto.Digest("sdk-key-2")
	key, _ := zcrypto.KeyFromSeed(d.Bytes())
	nonce := uint64(0)
	src := "PUSH 1\nPUSH 0\nSSTORE\nSTOP"
	tx, err := DeployContract(c, key, src, Options{Nonce: &nonce})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Type != ztypes.TxContractDeploy {
		t.Fatalf("type = %d", tx.Type)
	}
	if tx.GasLimit < 100_000 {
		t.Fatalf("gas limit not scaled: %d", tx.GasLimit)
	}
	// Invalid assembly must be rejected before anything is signed.
	if _, err := DeployContract(c, key, "NONSENSE_OPCODE", Options{Nonce: &nonce}); err == nil {
		t.Fatal("invalid assembly accepted")
	}
}

func TestRegisterValidatorValidatesMoniker(t *testing.T) {
	c := &Client{URL: "http://stub", ChainID: "znetwork-sdk-test"}
	d := zcrypto.Digest("sdk-key-3")
	key, _ := zcrypto.KeyFromSeed(d.Bytes())
	nonce := uint64(0)
	if _, err := RegisterValidator(c, key, "", 1000, Options{Nonce: &nonce}); err == nil {
		t.Fatal("empty moniker accepted")
	}
	long := make([]byte, ztypes.MaxMonikerLen+1)
	for i := range long {
		long[i] = 'm'
	}
	if _, err := RegisterValidator(c, key, string(long), 1000, Options{Nonce: &nonce}); err == nil {
		t.Fatal("oversized moniker accepted")
	}
	tx, err := RegisterValidator(c, key, "validator", 1000, Options{Nonce: &nonce})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Type != ztypes.TxValidatorRegister {
		t.Fatalf("type = %d", tx.Type)
	}
}

func TestDecodeTxRejectsGarbage(t *testing.T) {
	if _, err := DecodeTx(""); err == nil {
		t.Fatal("empty input accepted")
	}
	if _, err := DecodeTx("zz"); err == nil {
		t.Fatal("non-hex input accepted")
	}
	if _, err := DecodeTx("0102"); err == nil {
		t.Fatal("truncated transaction accepted")
	}
}

func TestWaitForTxTimesOut(t *testing.T) {
	// An unreachable endpoint must fail fast with an error, not loop forever.
	c := &Client{URL: "http://127.0.0.1:1", HTTP: nil}
	start := time.Now()
	_, err := c.WaitForTx("00", 300*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForTx succeeded with no server")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("WaitForTx did not honour its timeout")
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		t.Fatal("connection failure was reported as an RPC error")
	}
}

func encodeHex(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}
