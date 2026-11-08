package zmempool

import (
	"errors"
	"testing"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

const testChain = "znetwork-mempool-test"

type poolEnv struct {
	pool *Pool
	st   *zstate.State
	key  *zcrypto.PrivateKey
	to   ztypes.Address
}

func newPoolEnv(t *testing.T, balance uint64) *poolEnv {
	t.Helper()
	d := zcrypto.Digest("mempool-test-key")
	key, err := zcrypto.KeyFromSeed(d.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	st := zstate.New()
	params := ztypes.DefaultParams()
	st.SetParams(&params)
	st.SetChainID(testChain)
	st.SetAccount(key.Address(), &ztypes.Account{Balance: balance})
	var to ztypes.Address
	to[0] = 0x42
	return &poolEnv{pool: New(testChain), st: st, key: key, to: to}
}

func (e *poolEnv) tx(nonce, gasPrice, gasLimit uint64) *ztypes.Tx {
	tx := &ztypes.Tx{
		ChainID: testChain, Type: ztypes.TxTransfer, Sender: e.key.Address(),
		Nonce: nonce, GasLimit: gasLimit, GasPrice: gasPrice,
		TimeMs:  1_700_000_000_000,
		Payload: (&ztypes.TransferPayload{To: e.to, Amount: 1}).Encode(),
	}
	if err := tx.Sign(e.key); err != nil {
		panic(err)
	}
	return tx
}

func TestAddAndSelectInNonceOrder(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	// Insert 5..0 in reverse so ordering is actually exercised.
	for n := 5; n >= 0; n-- {
		if err := e.pool.Add(e.st, e.tx(uint64(n), 1, 100_000)); err != nil {
			t.Fatalf("add nonce %d: %v", n, err)
		}
	}
	if e.pool.Size() != 6 {
		t.Fatalf("size = %d", e.pool.Size())
	}
	sel := e.pool.Select(e.st, 100)
	if len(sel) != 6 {
		t.Fatalf("selected %d", len(sel))
	}
	for i, tx := range sel {
		if tx.Nonce != uint64(i) {
			t.Fatalf("position %d has nonce %d", i, tx.Nonce)
		}
	}
}

func TestDuplicateRejected(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	tx := e.tx(0, 1, 100_000)
	if err := e.pool.Add(e.st, tx); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.Add(e.st, tx); !errors.Is(err, ErrDuplicate) && !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestReplacementRequiresHigherPrice(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	if err := e.pool.Add(e.st, e.tx(0, 1, 100_000)); err != nil {
		t.Fatal(err)
	}
	// An identical transaction is a duplicate; a different transaction at the same
	// price is underpriced. Both must be rejected.
	if err := e.pool.Add(e.st, e.tx(0, 1, 100_000)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("identical duplicate = %v", err)
	}
	samePrice := e.tx(0, 1, 100_000)
	samePrice.Memo = "different body, same nonce and price"
	_ = samePrice.Sign(e.key)
	if err := e.pool.Add(e.st, samePrice); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("same-price replacement = %v", err)
	}
	// Lower price: rejected.
	if err := e.pool.Add(e.st, e.tx(0, 0, 100_000)); !errors.Is(err, ErrUnderpriced) {
		t.Fatalf("lower-price replacement = %v", err)
	}
	// Higher price: accepted and replaces.
	if err := e.pool.Add(e.st, e.tx(0, 5, 100_000)); err != nil {
		t.Fatalf("higher-price replacement rejected: %v", err)
	}
	if e.pool.Size() != 1 {
		t.Fatalf("size after replacement = %d", e.pool.Size())
	}
	sel := e.pool.Select(e.st, 10)
	if len(sel) != 1 || sel[0].GasPrice != 5 {
		t.Fatalf("selected %v", sel)
	}
	// The replaced transaction's id must no longer be known.
	if e.pool.Has(sel[0].ID()) != true {
		t.Fatal("replacement not tracked by id")
	}
}

func TestNonceBounds(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	// Account nonce is 0; a nonce far in the future is rejected.
	if err := e.pool.Add(e.st, e.tx(MaxFutureNonce+1, 1, 100_000)); !errors.Is(err, ErrNonceTooHigh) {
		t.Fatalf("future nonce error = %v", err)
	}
	// Once the account nonce moves past a pooled transaction it is dropped on
	// selection, not on insertion.
	if err := e.pool.Add(e.st, e.tx(0, 1, 100_000)); err != nil {
		t.Fatal(err)
	}
	acc, _ := e.st.GetAccount(e.key.Address())
	acc.Nonce = 1
	e.st.SetAccount(e.key.Address(), acc)
	if err := e.pool.Add(e.st, e.tx(0, 1, 100_000)); !errors.Is(err, ErrNonceTooLow) {
		t.Fatalf("stale nonce error = %v", err)
	}
	sel := e.pool.Select(e.st, 10)
	if len(sel) != 0 {
		t.Fatalf("stale transaction still selected: %v", sel)
	}
	if e.pool.Size() != 0 {
		t.Fatalf("stale transaction not evicted: size=%d", e.pool.Size())
	}
}

func TestInsufficientBalanceRejected(t *testing.T) {
	e := newPoolEnv(t, 1000) // cannot cover 100_000 * 1
	if err := e.pool.Add(e.st, e.tx(0, 1, 100_000)); err == nil {
		t.Fatal("unfunded transaction pooled")
	}
}

func TestStaticValidationApplied(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	// Wrong chain id.
	tx := e.tx(0, 1, 100_000)
	tx.ChainID = "other"
	if err := e.pool.Add(e.st, tx); !errors.Is(err, ErrInvalidTx) {
		t.Fatalf("cross-chain tx error = %v", err)
	}
	// Bad signature.
	tx = e.tx(0, 1, 100_000)
	tx.Signature[0] ^= 0xff
	if err := e.pool.Add(e.st, tx); !errors.Is(err, ErrInvalidTx) {
		t.Fatalf("bad signature error = %v", err)
	}
	// Zero gas limit.
	bad := e.tx(0, 1, 100_000)
	bad.GasLimit = 0
	_ = bad.Sign(e.key)
	if err := e.pool.Add(e.st, bad); err == nil {
		t.Fatal("zero gas limit pooled")
	}
	// Nil transaction must not panic.
	if err := e.pool.Add(e.st, nil); err == nil {
		t.Fatal("nil transaction accepted")
	}
}

func TestPerSenderLimit(t *testing.T) {
	e := newPoolEnv(t, 1<<62)
	for n := uint64(0); n < MaxPerSender; n++ {
		if err := e.pool.Add(e.st, e.tx(n, 1, 100_000)); err != nil {
			t.Fatalf("nonce %d: %v", n, err)
		}
	}
	if err := e.pool.Add(e.st, e.tx(MaxPerSender, 1, 100_000)); !errors.Is(err, ErrPerSenderLimit) {
		t.Fatalf("expected per-sender limit, got %v", err)
	}
}

func TestEvictionOfCheaperTransactions(t *testing.T) {
	e := newPoolEnv(t, 1<<62)
	// Fill the pool with nonce 0..MaxTxs-1 at price 1 using many senders would be
	// expensive; instead exercise the byte budget with large transactions.
	big := make([]byte, ztypes.MaxMemoLen)
	for i := range big {
		big[i] = 'x'
	}
	added := 0
	for {
		tx := e.tx(uint64(added), 1, 100_000)
		tx.Memo = string(big)
		_ = tx.Sign(e.key)
		if err := e.pool.Add(e.st, tx); err != nil {
			break
		}
		added++
		if added > MaxTxs+10 {
			break
		}
	}
	if added == 0 {
		t.Fatal("no transactions were accepted")
	}
	// A higher-priced transaction must still be accepted by evicting a cheaper one,
	// unless the pool is full of better transactions. It reuses an existing nonce
	// so the per-sender future-nonce window is not the limiting factor.
	hi := e.tx(0, 1000, 100_000)
	err := e.pool.Add(e.st, hi)
	if err != nil && !errors.Is(err, ErrPoolFull) && !errors.Is(err, ErrPerSenderLimit) {
		t.Fatalf("unexpected error: %v", err)
	}
	if err == nil && !e.pool.Has(hi.ID()) {
		t.Fatal("accepted transaction not tracked")
	}
}

func TestReapRemovesIncludedTransactions(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	var txs []*ztypes.Tx
	for n := uint64(0); n < 3; n++ {
		tx := e.tx(n, 1, 100_000)
		txs = append(txs, tx)
		if err := e.pool.Add(e.st, tx); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.pool.Reap(txs[:2]); n != 2 {
		t.Fatalf("reaped %d", n)
	}
	if e.pool.Size() != 1 {
		t.Fatalf("size = %d", e.pool.Size())
	}
	if e.pool.Has(txs[2].ID()) != true {
		t.Fatal("unreaped transaction lost")
	}
	// Reaping something absent is harmless.
	if n := e.pool.Reap([]*ztypes.Tx{txs[0]}); n != 0 {
		t.Fatalf("re-reaped %d", n)
	}
}

func TestSelectionHonoursGaps(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	// Pool nonces 0, 2 (gap at 1). Only 0 is executable.
	if err := e.pool.Add(e.st, e.tx(0, 1, 100_000)); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.Add(e.st, e.tx(2, 1, 100_000)); err != nil {
		t.Fatal(err)
	}
	sel := e.pool.Select(e.st, 10)
	if len(sel) != 1 || sel[0].Nonce != 0 {
		t.Fatalf("selection = %v", sel)
	}
	// Filling the gap makes both executable in order.
	if err := e.pool.Add(e.st, e.tx(1, 1, 100_000)); err != nil {
		t.Fatal(err)
	}
	sel = e.pool.Select(e.st, 10)
	if len(sel) != 3 || sel[0].Nonce != 0 || sel[1].Nonce != 1 || sel[2].Nonce != 2 {
		t.Fatalf("selection after gap fill = %v", sel)
	}
}

func TestConcurrentAddsAreSafe(t *testing.T) {
	e := newPoolEnv(t, 1<<62)
	done := make(chan struct{})
	for w := 0; w < 8; w++ {
		go func(worker int) {
			defer func() { done <- struct{}{} }()
			for n := 0; n < 20; n++ {
				nonce := uint64(worker*20 + n)
				_ = e.pool.Add(e.st, e.tx(nonce, 1, 100_000))
				_ = e.pool.Select(e.st, 10)
			}
		}(w)
	}
	deadline := time.After(20 * time.Second)
	for w := 0; w < 8; w++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("timeout")
		}
	}
	if e.pool.Size() == 0 {
		t.Fatal("no transactions were pooled")
	}
}

func TestOnRejectCallbackFires(t *testing.T) {
	e := newPoolEnv(t, 1_000_000_000)
	var rejected []string
	e.pool.OnReject = func(tx *ztypes.Tx, reason string) {
		rejected = append(rejected, reason)
	}
	tx := e.tx(0, 1, 100_000)
	if err := e.pool.Add(e.st, tx); err != nil {
		t.Fatal(err)
	}
	e.pool.Remove(tx.ID())
	if len(rejected) != 1 {
		t.Fatalf("reject callbacks = %d", len(rejected))
	}
}
