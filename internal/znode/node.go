package znode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/zmempool"
	"znetwork/internal/zmetrics"
	"znetwork/internal/znet"
	"znetwork/internal/ztypes"
)

// Node is a runnable Z Network node: chain + mempool + consensus driver.
//
// The driver is deliberately small and message-driven: consensus decisions are
// made by zconsensus, and this type only moves proposals and votes in and out.
// Both the in-process single-node case and the networked multi-node case use the
// same code path, which is why the unit tests and the live testnet exercise the
// same logic.
type Node struct {
	mu    sync.Mutex
	cfg   Config
	chain *Chain
	pool  *zmempool.Pool
	eng   *zconsensus.Engine

	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// driveMu serialises engine access between the driver loop and the network.
	driveMu sync.Mutex
	net     *znet.Server
	voteCh  chan zconsensus.Vote
	propCh  chan *zconsensus.Proposal

	// OnCommit is called for every committed block, in order.
	OnCommit func(b *ztypes.Block, receipts []*ztypes.Receipt)
	// OnTx is called after a transaction is accepted into the pool.
	OnTx func(tx *ztypes.Tx)
	// Metrics receives node-local counters. It may be nil; every call site tolerates
	// a nil sink so the node runs identically with and without monitoring.
	Metrics *zmetrics.Registry

	// OnVote/OnProposal are called for every consensus message this node emits,
	// so the network layer can gossip it. Nil means "local-only network".
	OnVote     func(v zconsensus.Vote)
	OnProposal func(p *zconsensus.Proposal)

	started bool
	errCh   chan error
}

// NewNode creates a node. It opens (or initialises) the chain and installs the
// current validator set in the consensus engine.
func NewNode(cfg Config, log *slog.Logger) (*Node, error) {
	if log == nil {
		log = slog.Default()
	}
	chain, err := Open(cfg)
	if err != nil {
		return nil, err
	}
	n := &Node{
		cfg:    cfg,
		chain:  chain,
		pool:   zmempool.New(cfg.ChainID),
		log:    log.With("moniker", cfg.Moniker),
		voteCh: make(chan zconsensus.Vote, 1024),
		propCh: make(chan *zconsensus.Proposal, 64),
		errCh:  make(chan error, 1),
	}
	var key *zcrypto.PrivateKey
	if cfg.Validator != nil {
		key = cfg.Validator
	}
	eng, err := zconsensus.NewEngine(zconsensus.Config{
		ChainID: cfg.ChainID,
		Key:     key,
		Now:     func() int64 { return time.Now().UnixMilli() },
		BuildProposal: func(height uint64, round uint32, timeMs int64, proposer ztypes.Address) (*ztypes.Block, error) {
			return n.buildProposal(height, timeMs, proposer)
		},
		ValidateProposal: func(b *ztypes.Block) error {
			return n.chain.ValidateOnly(b)
		},
	})
	if err != nil {
		return nil, err
	}
	set, err := ActiveValidatorSet(chain.State())
	if err != nil {
		return nil, fmt.Errorf("znode: genesis must contain at least one bonded validator: %w", err)
	}
	if err := eng.SetValidatorSet(set, chain.Height()+1); err != nil {
		return nil, err
	}
	n.eng = eng
	n.ctx, n.cancel = context.WithCancel(context.Background())
	return n, nil
}

// buildProposal selects from the mempool and builds a block.
func (n *Node) buildProposal(height uint64, timeMs int64, proposer ztypes.Address) (*ztypes.Block, error) {
	txs := n.pool.Select(n.chain.State(), n.cfg.MaxTxsPerBlock)
	b, _, err := n.chain.BuildBlock(height, timeMs, proposer, txs, nil)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Chain returns the chain.
func (n *Node) Chain() *Chain { return n.chain }

// Pool returns the mempool.
func (n *Node) Pool() *zmempool.Pool { return n.pool }

// Engine returns the consensus engine.
func (n *Node) Engine() *zconsensus.Engine { return n.eng }

// ValidatorAddress returns the node's validator address (zero for full nodes).
func (n *Node) ValidatorAddress() ztypes.Address {
	if n.cfg.Validator == nil {
		return ztypes.Address{}
	}
	return n.cfg.Validator.Address()
}

// SubmitTx validates a transaction against committed state and pools it.
func (n *Node) SubmitTx(tx *ztypes.Tx) (ztypes.Hash, error) {
	if tx == nil {
		return ztypes.Hash{}, fmt.Errorf("%w: nil transaction", zmempool.ErrInvalidTx)
	}
	if err := n.pool.Add(n.chain.State(), tx); err != nil {
		return ztypes.Hash{}, err
	}
	if n.OnTx != nil {
		n.OnTx(tx)
	}
	return tx.ID(), nil
}

// Start begins the consensus/block production loop.
func (n *Node) Start() {
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return
	}
	n.started = true
	n.mu.Unlock()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.run()
	}()
	n.log.Info("node started",
		"chain_id", n.cfg.ChainID,
		"height", n.chain.Height(),
		"validator", n.ValidatorAddress().Bech32(),
		"is_validator", n.ValidatorAddress() != (ztypes.Address{}),
		"listen", n.cfg.ListenAddr)
}

// Moniker is the node's human-readable name from its configuration.
func (n *Node) Moniker() string { return n.cfg.Moniker }

// Stop halts the node and waits for the driver to exit.
func (n *Node) Stop() {
	n.cancel()
	n.wg.Wait()
}

// Errors returns a channel reporting fatal driver errors (buffered, size 1).
func (n *Node) Errors() <-chan error { return n.errCh }

// run is the height loop.
func (n *Node) run() {
	for {
		select {
		case <-n.ctx.Done():
			return
		default:
		}
		if err := n.runHeight(); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			n.log.Error("height failed", "height", n.chain.Height()+1, "err", err)
			select {
			case <-n.ctx.Done():
				return
			case n.errCh <- err:
			default:
			}
			select {
			case <-n.ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			// Re-synchronise the engine onto the chain height; consensus state for
			// a failed height is discarded.
			_ = n.advance(n.chain.Height() + 1)
			continue
		}
		if n.Metrics != nil {
			n.Metrics.IncHeightCompleted()
		}
		if n.cfg.BlockTimeMs > 0 {
			select {
			case <-n.ctx.Done():
				return
			case <-time.After(time.Duration(n.cfg.BlockTimeMs) * time.Millisecond):
			}
		}
	}
}

// runHeight drives one height until a block is committed.
func (n *Node) runHeight() error {
	height := n.chain.Height() + 1
	// The chain is the authority on height. Positioning the engine here (instead
	// of trusting an internal counter) makes synced blocks, failed heights and
	// restarts converge on chainHeight+1.
	if err := n.advance(height); err != nil {
		return err
	}
	if err := n.startRound(); err != nil {
		return err
	}
	for {
		// The chain may have advanced past this height while we were waiting (a
		// synced block from a peer, or a duplicate commit certificate). Heights are
		// then already finalised and this round is moot.
		if n.chain.Height() >= height {
			return nil
		}
		select {
		case <-n.ctx.Done():
			return context.Canceled
		case v := <-n.voteCh:
			if err := n.handleVote(v); err != nil && !errors.Is(err, zconsensus.ErrDuplicateVote) {
				n.log.Debug("vote not applied", "err", err)
			}
		case p := <-n.propCh:
			if err := n.handleProposal(p); err != nil {
				n.log.Debug("proposal not applied", "err", err)
			}
		case <-time.After(zconsensus.RoundTimeout(n.eng.Round())):
			if n.eng.Committed() != nil {
				continue
			}
			round := n.eng.Round()
			if err := n.eng.AdvanceRound(); err != nil {
				return err
			}
			n.log.Info("round timeout, advancing", "height", height, "round", round+1)
			// Replay votes that arrived for this new round while we were behind.
			for _, v := range n.eng.DrainPendingVotes() {
				n.enqueueVote(v)
			}
			if err := n.startRound(); err != nil {
				return err
			}
		}
		if c := n.eng.Committed(); c != nil && c.Height == height {
			if err := n.commit(c); err != nil {
				return err
			}
			return n.advance(height + 1)
		}
	}
}

// startRound asks the engine to start the current round and dispatches whatever
// it produced.
func (n *Node) startRound() error {
	n.driveMu.Lock()
	res, err := n.eng.StartRound()
	n.driveMu.Unlock()
	if err != nil {
		return err
	}
	if res.Proposal != nil {
		// We are the proposer: the engine already holds the proposal, so it is
		// only broadcast.
		if n.OnProposal != nil {
			n.OnProposal(res.Proposal)
		} else {
			n.log.Debug("proposal built (local-only network)", "height", res.Proposal.Height)
		}
	}
	for _, v := range res.Votes {
		if n.Metrics != nil {
			n.Metrics.IncVoteCast()
		}
		if n.OnVote != nil {
			n.OnVote(v)
		}
		n.enqueueVote(v)
	}
	return nil
}

func (n *Node) enqueueVote(v zconsensus.Vote) {
	select {
	case n.voteCh <- v:
	default:
		n.log.Warn("vote queue full, dropping consensus vote", "voter", v.Voter.Bech32())
	}
}

// InjectProposal feeds a proposal received from the network into the node.
func (n *Node) InjectProposal(p *zconsensus.Proposal) {
	if p == nil {
		return
	}
	select {
	case n.propCh <- p:
	default:
		n.log.Warn("proposal queue full, dropping proposal", "height", p.Height)
	}
}

// InjectVote feeds a vote received from the network into the node.
func (n *Node) InjectVote(v zconsensus.Vote) {
	select {
	case n.voteCh <- v:
	default:
		n.log.Warn("vote queue full, dropping vote", "voter", v.Voter.Bech32())
	}
}

// handleProposal applies a remote proposal and dispatches the resulting votes.
func (n *Node) handleProposal(p *zconsensus.Proposal) error {
	n.driveMu.Lock()
	res, err := n.eng.HandleProposal(p)
	n.driveMu.Unlock()
	if err != nil {
		return err
	}
	for _, v := range res.Votes {
		if n.Metrics != nil {
			n.Metrics.IncVoteCast()
		}
		if n.OnVote != nil {
			n.OnVote(v)
		}
		n.enqueueVote(v)
	}
	return nil
}

// handleVote applies a vote (local or remote) and dispatches the resulting votes.
func (n *Node) handleVote(v zconsensus.Vote) error {
	if n.Metrics != nil {
		n.Metrics.IncVoteReceived()
	}
	n.driveMu.Lock()
	res, err := n.eng.HandleVote(v)
	n.driveMu.Unlock()
	if err != nil {
		return err
	}
	for _, out := range res.Votes {
		if n.OnVote != nil {
			n.OnVote(out)
		}
		n.enqueueVote(out)
	}
	return nil
}

// commit applies the committed block and advances.
func (n *Node) commit(c *zconsensus.CommitResult) error {
	if c.Block == nil {
		return fmt.Errorf("znode: commit without a block at height %d (proposal not received)", c.Height)
	}
	if c.Height <= n.chain.Height() {
		// Already applied (idempotent commit: a certificate can be observed twice).
		return nil
	}
	b := *c.Block
	b.Commit = c.Commit
	receipts, err := n.chain.ApplyBlock(&b)
	if err != nil {
		return err
	}
	n.pool.Reap(b.Txs)
	if n.OnCommit != nil {
		n.OnCommit(&b, receipts)
	}
	// Tell peers where we are: a validator whose expected height is behind the
	// committed chain can then resynchronise instead of waiting for a proposal
	// that will never come. Without this, a node that commits via sync can stall.
	go n.RefreshStatus()
	n.log.Info("block committed",
		"height", b.Header.Height,
		"txs", len(b.Txs),
		"gas", b.Header.GasUsed,
		"hash", b.Header.Hash().Short())
	return nil
}

// advance positions the consensus engine at an absolute height using the
// validator set that the chain state at that point requires.
func (n *Node) advance(height uint64) error {
	set, err := ActiveValidatorSet(n.chain.State())
	if err != nil {
		return err
	}
	return n.eng.SetHeight(height, set)
}
