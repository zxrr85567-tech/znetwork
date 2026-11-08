// Package zmempool is Z Network's transaction pool.
//
// Properties enforced here:
//
//   - Static validity only: the pool checks signatures, chain id, static limits
//     and nonce ordering. It never executes transactions, so a transaction in the
//     pool is "plausible", not "guaranteed".
//   - One transaction per (sender, nonce). A transaction that conflicts with one
//     already pooled replaces it only if it pays a strictly higher gas price and
//     is otherwise valid for the same nonce.
//   - Bounded resource use: total bytes and per-sender counts are capped, so a
//     flood from one account cannot evict the rest of the pool.
//   - Deterministic ordering: SelectedTxs returns transactions in canonical order
//     (sender bytes, then nonce), which is what block production consumes.
package zmempool

import (
	"container/list"
	"errors"
	"fmt"
	"sort"
	"sync"

	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// Pool limits.
const (
	MaxTxs         = 5000
	MaxBytes       = 8 << 20 // 8 MiB
	MaxPerSender   = 64
	MaxFutureNonce = 64 // max gap ahead of the account nonce accepted from one sender
)

// Errors.
var (
	ErrPoolFull       = errors.New("zmempool: pool is full")
	ErrDuplicate      = errors.New("zmempool: duplicate transaction")
	ErrNonceTooLow    = errors.New("zmempool: nonce below account nonce")
	ErrNonceTooHigh   = errors.New("zmempool: nonce too far in the future")
	ErrUnderpriced    = errors.New("zmempool: replacement underpriced")
	ErrPerSenderLimit = errors.New("zmempool: per-sender limit reached")
	ErrInvalidTx      = errors.New("zmempool: invalid transaction")
)

type entry struct {
	tx   *ztypes.Tx
	size int
}

// Pool is the transaction pool.
type Pool struct {
	mu      sync.Mutex
	chainID string

	bySender map[ztypes.Address]map[uint64]*list.Element
	order    *list.List // global FIFO of *entry, used for byte accounting and eviction
	bytes    int
	ids      map[ztypes.Hash]struct{}

	// OnReject is called (outside the lock) when a transaction is evicted.
	OnReject func(tx *ztypes.Tx, reason string)
}

// New creates a pool for a chain id.
func New(chainID string) *Pool {
	return &Pool{
		chainID:  chainID,
		bySender: map[ztypes.Address]map[uint64]*list.Element{},
		order:    list.New(),
		ids:      map[ztypes.Hash]struct{}{},
	}
}

// Size returns the number of pooled transactions.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.order.Len()
}

// Bytes returns the pooled byte count.
func (p *Pool) Bytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

// Add validates a transaction statically against committed state and inserts it.
//
// state is read-only here: the pool only looks at nonces and balances, never
// executes.
func (p *Pool) Add(st *zstate.State, tx *ztypes.Tx) error {
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", ErrInvalidTx)
	}
	if err := tx.ValidateStatic(p.chainID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTx, err)
	}
	id := tx.ID()
	size := len(tx.Encode())

	p.mu.Lock()
	defer p.mu.Unlock()

	acc, err := st.GetAccount(tx.Sender)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTx, err)
	}
	if tx.Nonce < acc.Nonce {
		return fmt.Errorf("%w: tx nonce %d < account nonce %d", ErrNonceTooLow, tx.Nonce, acc.Nonce)
	}
	if tx.Nonce > acc.Nonce+MaxFutureNonce {
		return fmt.Errorf("%w: tx nonce %d, account nonce %d", ErrNonceTooHigh, tx.Nonce, acc.Nonce)
	}
	// The sender must be able to pay the maximum fee right now.
	maxFee, ovf := ztypes.MulU64(tx.GasLimit, tx.GasPrice)
	if ovf {
		return fmt.Errorf("%w: fee overflow", ErrInvalidTx)
	}
	if acc.Balance < maxFee {
		return fmt.Errorf("%w: balance %d below max fee %d", ErrInvalidTx, acc.Balance, maxFee)
	}
	if _, dup := p.ids[id]; dup {
		return ErrDuplicate
	}
	senderTxs := p.bySender[tx.Sender]
	if senderTxs == nil {
		senderTxs = map[uint64]*list.Element{}
		p.bySender[tx.Sender] = senderTxs
	}
	if el, exists := senderTxs[tx.Nonce]; exists {
		old := el.Value.(*entry)
		if tx.GasPrice <= old.tx.GasPrice {
			return fmt.Errorf("%w: existing %d >= new %d", ErrUnderpriced, old.tx.GasPrice, tx.GasPrice)
		}
		// Replace in place.
		p.bytes += size - old.size
		p.ids[id] = struct{}{}
		delete(p.ids, old.tx.ID())
		el.Value = &entry{tx: tx, size: size}
		if p.bytes > MaxBytes {
			return p.trimLocked()
		}
		return nil
	}
	if len(senderTxs) >= MaxPerSender {
		return fmt.Errorf("%w: %d transactions from %s", ErrPerSenderLimit, len(senderTxs), tx.Sender.Bech32())
	}
	if p.order.Len() >= MaxTxs || p.bytes+size > MaxBytes {
		// Evict the largest low-priced transaction we can find; if none can be
		// evicted without dropping something better than the newcomer, reject.
		if !p.evictForLocked(tx) {
			return ErrPoolFull
		}
	}
	el := p.order.PushBack(&entry{tx: tx, size: size})
	senderTxs[tx.Nonce] = el
	p.ids[id] = struct{}{}
	p.bytes += size
	return nil
}

// evictForLocked removes one transaction that is strictly worse than the
// newcomer (lower gas price, or equal price and larger) to make room.
func (p *Pool) evictForLocked(newTx *ztypes.Tx) bool {
	var victim *list.Element
	for el := p.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry)
		if e.tx.GasPrice < newTx.GasPrice {
			victim = el
			break
		}
	}
	if victim == nil {
		return false
	}
	p.removeElementLocked(victim, "evicted for higher-priced transaction")
	return true
}

func (p *Pool) trimLocked() error {
	for p.bytes > MaxBytes && p.order.Len() > 0 {
		p.removeElementLocked(p.order.Front(), "byte budget exceeded")
	}
	return nil
}

func (p *Pool) removeElementLocked(el *list.Element, reason string) {
	e := el.Value.(*entry)
	p.order.Remove(el)
	if senderTxs := p.bySender[e.tx.Sender]; senderTxs != nil {
		if cur, ok := senderTxs[e.tx.Nonce]; ok && cur == el {
			delete(senderTxs, e.tx.Nonce)
			if len(senderTxs) == 0 {
				delete(p.bySender, e.tx.Sender)
			}
		}
	}
	delete(p.ids, e.tx.ID())
	p.bytes -= e.size
	if p.OnReject != nil {
		p.OnReject(e.tx, reason)
	}
}

// Remove deletes a transaction by id (used when a block includes it).
func (p *Pool) Remove(id ztypes.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el := p.findByTxIDLocked(id); el != nil {
		p.removeElementLocked(el, "included in a block")
		return true
	}
	return false
}

func (p *Pool) findByTxIDLocked(id ztypes.Hash) *list.Element {
	for el := p.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry)
		if e.tx.ID() == id {
			return el
		}
	}
	return nil
}

// Has reports whether a transaction id is pooled.
func (p *Pool) Has(id ztypes.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.ids[id]
	return ok
}

// Select returns up to max transactions that are currently executable against st,
// in canonical order, and drops any transaction that can never become executable
// (nonce already consumed) or that is no longer statically valid.
//
// It does not remove the returned transactions; the caller removes them once a
// block is committed so a failed height does not lose transactions.
func (p *Pool) Select(st *zstate.State, max int) []*ztypes.Tx {
	p.mu.Lock()
	defer p.mu.Unlock()
	type cand struct {
		tx *ztypes.Tx
	}
	var ready []*ztypes.Tx
	var drop []*list.Element
	for el := p.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry)
		acc, err := st.GetAccount(e.tx.Sender)
		if err != nil {
			continue
		}
		if e.tx.Nonce < acc.Nonce {
			drop = append(drop, el)
			continue
		}
		ready = append(ready, e.tx)
	}
	for _, el := range drop {
		p.removeElementLocked(el, "nonce consumed at execution time")
	}
	// Canonical ordering, then honour per-sender nonce sequencing: a sender's
	// transaction N is only included if N-1 is included or already committed.
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Sender != ready[j].Sender {
			return string(ready[i].Sender[:]) < string(ready[j].Sender[:])
		}
		return ready[i].Nonce < ready[j].Nonce
	})
	out := make([]*ztypes.Tx, 0, max)
	nextAllowed := map[ztypes.Address]uint64{}
	for _, tx := range ready {
		if len(out) >= max {
			break
		}
		acc, err := st.GetAccount(tx.Sender)
		if err != nil {
			continue
		}
		if tx.Sender == (ztypes.Address{}) && tx.Nonce == 0 && len(out) > 0 {
			continue
		}
		next, ok := nextAllowed[tx.Sender]
		if !ok {
			next = acc.Nonce
			nextAllowed[tx.Sender] = next
		}
		if tx.Nonce != next {
			continue // gap: later nonces wait
		}
		out = append(out, tx)
		nextAllowed[tx.Sender] = next + 1
	}
	return out
}

// All returns a snapshot of pooled transactions in canonical order (for RPC).
func (p *Pool) All(st *zstate.State) []*ztypes.Tx {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*ztypes.Tx, 0, p.order.Len())
	for el := p.order.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*entry).tx)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sender != out[j].Sender {
			return string(out[i].Sender[:]) < string(out[j].Sender[:])
		}
		return out[i].Nonce < out[j].Nonce
	})
	return out
}

// Reap removes transactions that were included in a block.
func (p *Pool) Reap(txs []*ztypes.Tx) int {
	n := 0
	for _, tx := range txs {
		if p.Remove(tx.ID()) {
			n++
		}
	}
	return n
}
