package znode

import (
	"fmt"
	"sort"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/zexec"
	"znetwork/internal/zmerkle"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// BuildBlock assembles a candidate block on top of the current head. It returns
// the block and the receipts of the executed transactions; state is NOT mutated:
// the block is built against a copy and only committed when applied.
//
// included reports the transactions that were accepted.
func (c *Chain) BuildBlock(height uint64, timeMs int64, proposer ztypes.Address, txs []*ztypes.Tx, commit *ztypes.Commit) (*ztypes.Block, []*ztypes.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state.Clone()
	params, err := state.Params()
	if err != nil {
		return nil, nil, err
	}
	prev := c.blocks[len(c.blocks)-1]
	if height != prev.Header.Height+1 {
		return nil, nil, fmt.Errorf("%w: building height %d on head %d", ErrHeightMismatch, height, prev.Header.Height)
	}
	set, err := ActiveValidatorSet(state)
	if err != nil {
		return nil, nil, fmt.Errorf("znode: active validator set: %w", err)
	}
	if proposer.IsZero() {
		return nil, nil, fmt.Errorf("znode: proposer address required")
	}

	ctx := &zexec.BlockContext{Height: height, TimeMs: timeMs, Proposer: proposer, Params: params}
	if err := c.eng.BeginBlock(ctx, state); err != nil {
		return nil, nil, err
	}
	// Recompute the set: BeginBlock may have changed statuses.
	set, err = ActiveValidatorSet(state)
	if err != nil {
		return nil, nil, fmt.Errorf("znode: active validator set after BeginBlock: %w", err)
	}

	header := &ztypes.Header{
		Version:        ztypes.ProtocolVersion,
		ChainID:        c.cfg.ChainID,
		Height:         height,
		TimeMs:         timeMs,
		PrevHash:       prev.Header.Hash(),
		Proposer:       proposer,
		GasLimit:       params.BlockGasLimit,
		ValidatorsHash: set.ValidatorKey,
	}

	var included []*ztypes.Tx
	var receipts []*ztypes.Receipt
	var gasUsed uint64
	var txBytes int
	for _, tx := range txs {
		if len(included) >= c.cfg.MaxTxsPerBlock {
			break
		}
		size := len(tx.Encode())
		if txBytes+size > ztypes.MaxBlockBytes {
			break
		}
		if gasUsed > params.BlockGasLimit {
			break
		}
		rc := c.eng.ExecuteTx(ctx, state, tx)
		if rc.Status != ztypes.ReceiptSuccess && rc.GasUsed == 0 && rc.Fee == 0 {
			// Statically invalid: not includable at all.
			continue
		}
		if gasUsed+rc.GasUsed > params.BlockGasLimit {
			// Would exceed the block gas limit: stop including (deterministic
			// order, so every node makes the same decision).
			break
		}
		gasUsed += rc.GasUsed
		txBytes += size
		included = append(included, tx)
		receipts = append(receipts, rc)
	}
	minted, err := c.eng.EndBlock(ctx, state)
	if err != nil {
		return nil, nil, fmt.Errorf("znode: EndBlock: %w", err)
	}
	if minted > 0 {
		receipts = append(receipts, &ztypes.Receipt{
			Height: height, Type: 0, Status: ztypes.ReceiptSuccess, GasUsed: 0, Fee: 0,
			Events: []ztypes.Event{{Type: "block_reward", Attrs: [][2]string{{"minted_uz", fmt.Sprintf("%d", minted)},
				{"proposer", proposer.Bech32()}}}},
		})
	}
	if err := zexec.VerifySupply(state); err != nil {
		return nil, nil, fmt.Errorf("znode: refusing to build a block that breaks accounting: %w", err)
	}

	header.TxRoot = txRoot(included)
	header.ReceiptRoot = receiptRoot(receipts)
	header.TxCount = uint32(len(included))
	header.GasUsed = gasUsed
	header.StateRoot = state.Root()

	b := &ztypes.Block{Header: *header, Txs: included, Commit: commit}
	return b, receipts, nil
}

// ApplyBlock validates a block against the current head and commits it: state is
// updated, persisted, and the block appended to the log.
//
// This is the single validation path used both for proposals from other nodes and
// for local production, so a node can never accept a block it could not have
// produced.
func (c *Chain) ApplyBlock(b *ztypes.Block) ([]*ztypes.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next, receipts, err := c.applyTo(c.state, b, true)
	if err != nil {
		return nil, err
	}
	// Persist state before the block becomes visible in the log, then the block,
	// then the receipts. A crash at any point leaves a state that Open() can
	// repair by replaying from the last good snapshot.
	if err := c.store.SaveState(next); err != nil {
		return nil, err
	}
	if err := c.store.AppendBlock(b); err != nil {
		return nil, err
	}
	if err := c.store.AppendReceipts(b.Header.Height, receipts); err != nil {
		return nil, err
	}
	c.receipts[b.Header.Height] = receipts
	c.state = next
	c.blocks = append(c.blocks, b)
	c.ids[b.Header.Hash()] = b.Header.Height
	for _, tx := range b.Txs {
		c.txs[tx.ID()] = b.Header.Height
	}
	go c.notify(b, receipts)
	return receipts, nil
}

// applyTo executes and validates a block on top of `parent`, returning the new
// state. Callers must hold c.mu (or own the chain exclusively, as Open does). When checkCommit is true the commit certificate and the previous hash are
// verified as well (remote blocks); when false the block is replayed from local
// storage and only the state transition is re-derived.
func (c *Chain) applyTo(parent *zstate.State, b *ztypes.Block, checkCommit bool) (*zstate.State, []*ztypes.Receipt, error) {
	if b == nil {
		return nil, nil, fmt.Errorf("%w: nil block", ErrBlockInvalid)
	}
	h := &b.Header
	if h.ChainID != c.cfg.ChainID {
		return nil, nil, fmt.Errorf("%w: header chain id %q", ztypes.ErrChainIDMismatch, h.ChainID)
	}
	if h.Version != ztypes.ProtocolVersion {
		return nil, nil, fmt.Errorf("%w: unsupported block version %d", ErrBlockInvalid, h.Version)
	}
	if h.Height != parent.Height()+1 {
		return nil, nil, fmt.Errorf("%w: height %d does not follow %d", ErrHeightMismatch, h.Height, parent.Height())
	}
	if h.PrevHash != parent.BlockHash() {
		return nil, nil, fmt.Errorf("%w: previous hash %s != state hash %s", ErrBlockInvalid,
			h.PrevHash.Short(), parent.BlockHash().Short())
	}
	if len(b.Txs) > ztypes.MaxTxPerBlock {
		return nil, nil, fmt.Errorf("%w: %d transactions exceed the limit %d", ErrBlockInvalid, len(b.Txs), ztypes.MaxTxPerBlock)
	}
	if h.TxCount != uint32(len(b.Txs)) {
		return nil, nil, fmt.Errorf("%w: header tx count %d != %d", ErrBlockInvalid, h.TxCount, len(b.Txs))
	}
	if h.TxRoot != txRoot(b.Txs) {
		return nil, nil, fmt.Errorf("%w: transaction root mismatch", ErrBlockInvalid)
	}
	if h.TimeMs <= 0 {
		return nil, nil, fmt.Errorf("%w: missing block time", ErrBlockInvalid)
	}
	// Block time must be strictly increasing: timestamps are consensus data.
	// (c.blocks is read directly: applyTo runs with c.mu held.)
	if h.Height >= 2 {
		prev := c.blocks[h.Height-2]
		if h.TimeMs <= prev.Header.TimeMs {
			return nil, nil, fmt.Errorf("%w: block time %d is not after parent %d", ErrBlockInvalid, h.TimeMs, prev.Header.TimeMs)
		}
	}

	state := parent.Clone()
	params, err := state.Params()
	if err != nil {
		return nil, nil, err
	}
	// The validator set that governs this block is derived from the parent state,
	// which is exactly what the proposer used, so both sides always agree.
	set, err := ActiveValidatorSet(state)
	if err != nil {
		return nil, nil, fmt.Errorf("znode: validator set: %w", err)
	}
	if h.ValidatorsHash != set.ValidatorKey {
		return nil, nil, fmt.Errorf("%w: validators hash mismatch", ErrBlockInvalid)
	}
	// The proposer must always be a member of the set that governs this height.
	if _, ok := set.Lookup(h.Proposer); !ok {
		return nil, nil, fmt.Errorf("%w: proposer %s is not in the validator set", ErrBlockInvalid, h.Proposer.Bech32())
	}
	// A commit certificate only exists after consensus. Proposal validation
	// (checkCommit=false) therefore cannot and must not require one; committing a
	// block does.
	if checkCommit {
		commit := b.Commit
		if commit == nil {
			return nil, nil, fmt.Errorf("%w: block has no commit certificate", ErrBlockInvalid)
		}
		if err := zconsensus.VerifyCommit(c.cfg.ChainID, h.Height, h.Hash(), commit, set); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrBlockInvalid, err)
		}
	}

	ctx := &zexec.BlockContext{Height: h.Height, TimeMs: h.TimeMs, Proposer: h.Proposer, Params: params}
	if err := c.eng.BeginBlock(ctx, state); err != nil {
		return nil, nil, err
	}
	var receipts []*ztypes.Receipt
	var gasUsed uint64
	for _, tx := range b.Txs {
		rc := c.eng.ExecuteTx(ctx, state, tx)
		if rc.Status != ztypes.ReceiptSuccess && rc.GasUsed == 0 && rc.Fee == 0 {
			// A statically invalid transaction must never appear in a valid block.
			return nil, nil, fmt.Errorf("%w: block contains invalid transaction %s: %s",
				ErrBlockInvalid, tx.ID().Short(), rc.Error)
		}
		gasUsed += rc.GasUsed
		if gasUsed > params.BlockGasLimit {
			return nil, nil, fmt.Errorf("%w: block exceeds the gas limit", ErrBlockInvalid)
		}
		receipts = append(receipts, rc)
	}
	minted, err := c.eng.EndBlock(ctx, state)
	if err != nil {
		return nil, nil, err
	}
	if minted > 0 {
		receipts = append(receipts, &ztypes.Receipt{
			Height: h.Height, Type: 0, Status: ztypes.ReceiptSuccess,
			Events: []ztypes.Event{{Type: "block_reward", Attrs: [][2]string{{"minted_uz", fmt.Sprintf("%d", minted)},
				{"proposer", h.Proposer.Bech32()}}}},
		})
	}
	if gasUsed != h.GasUsed {
		return nil, nil, fmt.Errorf("%w: header gas used %d != executed %d", ErrBlockInvalid, h.GasUsed, gasUsed)
	}
	if h.ReceiptRoot != receiptRoot(receipts) {
		return nil, nil, fmt.Errorf("%w: receipt root mismatch", ErrBlockInvalid)
	}
	if c.cfg.CheckInvariants {
		if err := zexec.VerifySupply(state); err != nil {
			return nil, nil, fmt.Errorf("%w: accounting invariant: %v", ErrBlockInvalid, err)
		}
	}
	if h.StateRoot != state.Root() {
		return nil, nil, fmt.Errorf("%w: state root %s != computed %s", ErrBlockInvalid,
			h.StateRoot.Short(), state.Root().Short())
	}
	state.SetHeight(h.Height)
	state.SetBlockHash(h.Hash())
	// The root must be re-checked after the height/hash meta entries change.
	if h.StateRoot != state.Root() {
		return nil, nil, fmt.Errorf("%w: state root does not cover height/hash metadata", ErrBlockInvalid)
	}
	return state, receipts, nil
}

// txRoot is the Merkle root over transaction ids, in block order.
func txRoot(txs []*ztypes.Tx) zcrypto.Hash {
	leaves := make([]zmerkle.Hash, len(txs))
	for i, tx := range txs {
		id := tx.ID()
		leaves[i] = zcrypto.Digest(zcrypto.DomainTxRoot+"-leaf", id[:])
	}
	return zmerkle.Root(zcrypto.DomainTxRoot, leaves)
}

// receiptRoot is the Merkle root over receipt encodings, in block order.
func receiptRoot(receipts []*ztypes.Receipt) zcrypto.Hash {
	leaves := make([]zmerkle.Hash, len(receipts))
	for i, r := range receipts {
		leaves[i] = zcrypto.Digest(zcrypto.DomainReceiptRoot+"-leaf", r.Encode())
	}
	return zmerkle.Root(zcrypto.DomainReceiptRoot, leaves)
}

// sortedTxs returns transactions ordered canonically (by sender then nonce) so a
// block builder produces one deterministic order regardless of arrival order.
func sortedTxs(txs []*ztypes.Tx) []*ztypes.Tx {
	out := append([]*ztypes.Tx(nil), txs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sender != out[j].Sender {
			return string(out[i].Sender[:]) < string(out[j].Sender[:])
		}
		if out[i].Nonce != out[j].Nonce {
			return out[i].Nonce < out[j].Nonce
		}
		return out[i].ID().String() < out[j].ID().String()
	})
	return out
}

// SortTxs exposes the canonical ordering for the mempool and tests.
func SortTxs(txs []*ztypes.Tx) []*ztypes.Tx { return sortedTxs(txs) }

// ValidateOnly runs the full stateless + state-transition validation of a
// candidate block against the current head, without a commit certificate and
// without committing anything. It is what consensus uses to decide whether a
// proposal is votable, and it is the same code path that later commits the block,
// so a node can never accept a block it could not have validated.
func (c *Chain) ValidateOnly(b *ztypes.Block) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _, err := c.applyTo(c.state, b, false)
	return err
}

// Receipts returns the receipts of a height.
func (c *Chain) Receipts(height uint64) ([]*ztypes.Receipt, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.receipts[height]
	return r, ok
}

// TxReceipt returns the receipt of one transaction.
func (c *Chain) TxReceipt(id ztypes.Hash) (*ztypes.Receipt, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.txs[id]
	if !ok {
		return nil, false
	}
	for _, rc := range c.receipts[h] {
		if rc.TxID == id {
			return rc, true
		}
	}
	return nil, false
}

// ensureReceipts verifies that receipts exist for the whole chain and recomputes
// any that are missing (crash between block append and receipt append, or a
// receipt log written by an older build). Re-execution is safe because receipts
// are derived data and the state root commits to the block's effects.
func (c *Chain) ensureReceipts() error {
	missing := false
	for h := uint64(1); h <= c.Height(); h++ {
		if _, ok := c.receipts[h]; !ok {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}
	rebuilt := map[uint64][]*ztypes.Receipt{}
	// Re-execute from the earliest available snapshot that is <= the first gap.
	start := uint64(0)
	if latest, ok, err := c.store.LatestStateHeight(); err == nil && ok {
		start = latest
	}
	state, err := c.store.LoadState(start)
	if err != nil {
		return fmt.Errorf("znode: rebuilding receipts: %w", err)
	}
	for h := start + 1; h <= c.Height(); h++ {
		next, receipts, err := c.applyTo(state, c.blocks[h], false)
		if err != nil {
			return fmt.Errorf("znode: rebuilding receipts at height %d: %w", h, err)
		}
		rebuilt[h] = receipts
		state = next
	}
	for h, r := range rebuilt {
		if _, ok := c.receipts[h]; !ok {
			c.receipts[h] = r
		}
	}
	// Merge heights that were already valid with the recomputed ones.
	if err := c.store.RewriteReceipts(c.receipts); err != nil {
		return err
	}
	return nil
}
