// Package znode assembles the pieces into a running node: chain state, block
// production, block application (the full validation path), mempool interaction
// and persistence. It is the layer the consensus engine, the network, the RPC
// server and the CLI all talk to.
package znode

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/zexec"
	"znetwork/internal/zstate"
	"znetwork/internal/zstore"
	"znetwork/internal/ztypes"
)

// Errors surfaced by the chain.
var (
	ErrNotSynced      = errors.New("znode: chain state not initialised")
	ErrTxRejected     = errors.New("znode: transaction rejected by mempool")
	ErrBlockInvalid   = errors.New("znode: block failed validation")
	ErrHeightMismatch = errors.New("znode: block height does not extend the chain")
)

// Config is the node configuration.
type Config struct {
	ChainID    string
	DataDir    string
	Genesis    *ztypes.Genesis
	Validator  *zcrypto.PrivateKey // nil for a full (non-validating) node
	Moniker    string
	ListenAddr string
	Seeds      []string

	// BlockTimeMs is the target interval between blocks used by the node's driver.
	BlockTimeMs int64
	// MaxTxsPerBlock bounds block size in transactions.
	MaxTxsPerBlock int
	// CheckInvariants enables the supply invariant check after every block. It is
	// on by default in this implementation because the cost is acceptable at
	// development scale and a silent accounting bug is worse than a slow block.
	CheckInvariants bool
	// ScrubMemPool drops transactions that fail validation on execution.
	ScrubMemPool bool
}

// Chain is a running node's view of the blockchain.
type Chain struct {
	mu       sync.Mutex
	cfg      Config
	eng      *zexec.Engine
	store    *zstore.Store
	state    *zstate.State
	blocks   []*ztypes.Block
	ids      map[zcrypto.Hash]uint64 // block hash -> height
	txs      map[ztypes.Hash]uint64  // tx id -> height
	receipts map[uint64][]*ztypes.Receipt

	subs []func(b *ztypes.Block, receipts []*ztypes.Receipt)
}

// Open initialises a chain: it loads existing data if present, otherwise it
// initialises genesis state and stores block 0 (the genesis block).
func Open(cfg Config) (*Chain, error) {
	if cfg.ChainID == "" {
		return nil, errors.New("znode: chain id required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("znode: data dir required")
	}
	if cfg.MaxTxsPerBlock <= 0 {
		cfg.MaxTxsPerBlock = 2000
	}
	st, err := zstore.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	c := &Chain{cfg: cfg, eng: zexec.NewEngine(cfg.ChainID), store: st,
		ids: map[zcrypto.Hash]uint64{}, txs: map[ztypes.Hash]uint64{},
		receipts: map[uint64][]*ztypes.Receipt{}}

	blocks, _, err := st.ReadBlocksFile()
	if err != nil && !errors.Is(err, zstore.ErrTruncatedTail) {
		return nil, err
	}
	if len(blocks) > 0 {
		if blocks[0].Header.ChainID != cfg.ChainID {
			return nil, fmt.Errorf("%w: stored chain id %q != configured %q",
				ztypes.ErrChainIDMismatch, blocks[0].Header.ChainID, cfg.ChainID)
		}
	}
	if len(blocks) == 0 {
		if err := c.bootstrap(cfg.Genesis); err != nil {
			return nil, err
		}
	} else {
		latest, ok, err := st.LatestStateHeight()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("znode: blocks present but no state snapshot (data dir corrupted or interrupted initialisation)")
		}
		state, err := st.LoadState(latest)
		if err != nil {
			return nil, fmt.Errorf("znode: loading state at height %d: %w", latest, err)
		}
		c.state = state
		for _, b := range blocks {
			h := b.Header.Hash()
			c.blocks = append(c.blocks, b)
			c.ids[h] = b.Header.Height
			for _, tx := range b.Txs {
				c.txs[tx.ID()] = b.Header.Height
			}
		}
		// If state is behind the block log (crash between state save and block
		// append), replay the missing blocks.
		loaded, err := st.ReadReceipts()
		if err != nil {
			return nil, fmt.Errorf("znode: reading receipts: %w", err)
		}
		c.receipts = loaded
		if latest < c.Height() {
			if err := c.replayFrom(latest); err != nil {
				return nil, err
			}
		} else if err := c.ensureReceipts(); err != nil {
			return nil, err
		}
	}
	if c.state == nil {
		return nil, ErrNotSynced
	}
	return c, nil
}

// bootstrap initialises genesis state.
func (c *Chain) bootstrap(g *ztypes.Genesis) error {
	if g == nil {
		return fmt.Errorf("%w: no genesis and no stored chain", ztypes.ErrGenesisInvalid)
	}
	if g.ChainID != c.cfg.ChainID {
		return fmt.Errorf("%w: genesis chain id %q != configured %q",
			ztypes.ErrChainIDMismatch, g.ChainID, c.cfg.ChainID)
	}
	st, err := c.eng.InitChain(g)
	if err != nil {
		return fmt.Errorf("znode: genesis: %w", err)
	}
	if err := zexec.VerifySupply(st); err != nil {
		return fmt.Errorf("znode: genesis accounting: %w", err)
	}
	gh, err := g.Hash()
	if err != nil {
		return err
	}
	gb := &ztypes.Block{}
	gb.Header.Version = ztypes.ProtocolVersion
	gb.Header.ChainID = g.ChainID
	gb.Header.Height = 0
	gb.Header.TimeMs = g.TimeMs
	gb.Header.Proposer = ztypes.Address{}
	gb.Header.ValidatorsHash = validatorSetHashFromState(st)
	gb.Header.StateRoot = st.Root()
	rg, err := g.Resolve()
	if err != nil {
		return err
	}
	gb.Header.GasLimit = rg.Params.BlockGasLimit
	st.SetBlockHash(gb.Header.Hash())
	st.SetHeight(0)
	// Persist state before the block so a crash can never leave a block that has
	// no state behind it (the replay path above handles the reverse order).
	if err := c.store.SaveState(st); err != nil {
		return err
	}
	if err := c.store.AppendBlock(gb); err != nil {
		return err
	}
	c.state = st
	c.blocks = []*ztypes.Block{gb}
	c.ids[gb.Header.Hash()] = 0
	c.receipts[0] = nil
	_ = gh
	return nil
}

// replayFrom re-executes blocks after height h to rebuild state.
func (c *Chain) replayFrom(h uint64) error {
	state, err := c.store.LoadState(h)
	if err != nil {
		return err
	}
	for i := int(h) + 1; i < len(c.blocks); i++ {
		next, receipts, err := c.applyTo(state, c.blocks[i], false)
		if err != nil {
			return fmt.Errorf("znode: replaying block %d: %w", i, err)
		}
		state = next
		_ = receipts
	}
	if err := c.store.SaveState(state); err != nil {
		return err
	}
	c.state = state
	return nil
}

// validatorSetHashFromState computes the committed validator-set hash.
func validatorSetHashFromState(st *zstate.State) zcrypto.Hash {
	set, err := ActiveValidatorSet(st)
	if err != nil || set == nil {
		return zcrypto.Hash{}
	}
	return set.ValidatorKey
}

// ActiveValidatorSet builds the consensus validator set from state: bonded
// validators with at least the minimum stake, capped at max_active_validators,
// ordered by descending voting power (ties broken by address) so the cap is
// applied deterministically.
func ActiveValidatorSet(st *zstate.State) (*zconsensus.ValidatorSet, error) {
	params, err := st.Params()
	if err != nil {
		return nil, err
	}
	vals, err := st.Validators()
	if err != nil {
		return nil, err
	}
	type cand struct {
		v *ztypes.Validator
	}
	var candidates []*ztypes.Validator
	for _, v := range vals {
		if v.Status != ztypes.ValidatorBonded {
			continue
		}
		if v.VotingPower() < params.MinValidatorStakeUZ {
			continue
		}
		candidates = append(candidates, v)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].VotingPower() != candidates[j].VotingPower() {
			return candidates[i].VotingPower() > candidates[j].VotingPower()
		}
		return string(candidates[i].Operator[:]) < string(candidates[j].Operator[:])
	})
	var out []zconsensus.Validator
	for i, v := range candidates {
		if uint64(i) >= params.MaxActiveValidators {
			break
		}
		out = append(out, zconsensus.Validator{
			Address: v.Operator,
			PubKey:  v.PubKey,
			Power:   v.VotingPower(),
		})
	}
	if len(out) == 0 {
		return nil, zcrypto.ErrBadPublicKey
	}
	return zconsensus.NewValidatorSet(validatorSetChainID(st), out)
}

// validatorSetChainID reads the chain id committed in state.
func validatorSetChainID(st *zstate.State) string { return st.ChainID() }

// Height returns the current chain height.
func (c *Chain) Height() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.blocks)) - 1
}

// State returns the committed state. Callers must not mutate it.
func (c *Chain) State() *zstate.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Params returns the current parameter set.
func (c *Chain) Params() (*ztypes.Params, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Params()
}

// Block returns a block by height.
func (c *Chain) Block(height uint64) (*ztypes.Block, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if height >= uint64(len(c.blocks)) {
		return nil, false
	}
	return c.blocks[height], true
}

// BlockByHash returns a block by hash.
func (c *Chain) BlockByHash(h zcrypto.Hash) (*ztypes.Block, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	height, ok := c.ids[h]
	if !ok {
		return nil, false
	}
	return c.blocks[height], true
}

// TxHeight returns the height containing a transaction.
func (c *Chain) TxHeight(id ztypes.Hash) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.txs[id]
	return h, ok
}

// Subscribe registers a block listener. Listeners are called without the chain
// lock held, in registration order.
func (c *Chain) Subscribe(fn func(b *ztypes.Block, receipts []*ztypes.Receipt)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs = append(c.subs, fn)
}

func (c *Chain) notify(b *ztypes.Block, receipts []*ztypes.Receipt) {
	c.mu.Lock()
	subs := append([]func(*ztypes.Block, []*ztypes.Receipt){}, c.subs...)
	c.mu.Unlock()
	for _, fn := range subs {
		fn(b, receipts)
	}
}

// BlockContext builds the execution context for a height.
func (c *Chain) blockContext(height uint64, timeMs int64, proposer ztypes.Address, params *ztypes.Params) *zexec.BlockContext {
	return &zexec.BlockContext{Height: height, TimeMs: timeMs, Proposer: proposer, Params: params}
}

// LastBlock returns the head block.
func (c *Chain) LastBlock() *ztypes.Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocks[len(c.blocks)-1]
}

// Blocks returns all blocks (used by the indexer at bootstrap).
func (c *Chain) Blocks() []*ztypes.Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*ztypes.Block(nil), c.blocks...)
}
