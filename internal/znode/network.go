package znode

import (
	"errors"
	"fmt"
	"time"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/znet"
	"znetwork/internal/ztypes"
)

// AttachNetwork connects the node to a peer manager. After this call the node
// gossips proposals, votes and transactions and processes peers' messages.
//
// The wiring is intentionally explicit: every inbound message is decoded with the
// canonical codec, then validated by the same code paths that local production
// uses (engine.HandleProposal / engine.HandleVote for consensus, pool.Add for
// transactions, Chain.ApplyBlock for blocks), so a malicious peer can never take
// a shortcut into state.
func (n *Node) AttachNetwork(srv *znet.Server) {
	n.mu.Lock()
	n.net = srv
	n.mu.Unlock()

	n.OnVote = func(v zconsensus.Vote) {
		srv.Broadcast(znet.MsgVote, znet.EncodeVote(&v))
	}
	n.OnProposal = func(p *zconsensus.Proposal) {
		if raw, err := znet.EncodeProposal(p); err == nil {
			srv.Broadcast(znet.MsgProposal, raw)
		} else {
			n.log.Error("encoding proposal for broadcast", "err", err)
		}
	}
	n.OnTx = func(tx *ztypes.Tx) {
		srv.Broadcast(znet.MsgTx, znet.EncodeTx(tx))
	}

	srv.OnMessage = func(peer *znet.Peer, m *znet.Message) {
		n.handlePeerMessage(peer, m)
	}
	srv.OnPeerUp = func(peer *znet.Peer) {
		n.log.Info("peer connected", "peer", peer.Addr, "moniker", peer.Handshake.Moniker, "outbound", peer.Outbound)
		n.sendStatus(peer)
		// Catch the peer up: ask for everything after our head.
		from := n.chain.Height() + 1
		if peer.Handshake.Height > n.chain.Height() {
			_ = peer.Send(znet.MsgGetBlocks, znet.EncodeHeightRange(from, znet.MaxBlocksPerMessage))
		}
	}
	srv.OnPeerDown = func(peer *znet.Peer) {
		n.log.Info("peer disconnected", "peer", peer.Addr, "moniker", peer.Handshake.Moniker)
	}
}

func (n *Node) sendStatus(peer *znet.Peer) {
	st := &znet.Status{
		ChainID:     n.cfg.ChainID,
		Height:      n.chain.Height(),
		HeadHash:    n.chain.LastBlock().Header.Hash(),
		GenesisHash: n.chain.State().GenesisHash(),
	}
	_ = peer.Send(znet.MsgStatus, znet.EncodeStatus(st))
}

// handlePeerMessage dispatches one inbound message. It reports whether the
// message was accepted and, when not, a short reason (used by tests and debug
// logs).
func (n *Node) handlePeerMessage(peer *znet.Peer, m *znet.Message) (bool, string) {
	switch m.Type {
	case znet.MsgStatus:
		st, err := znet.DecodeStatus(m.Payload)
		if err != nil {
			n.log.Warn("bad status message", "peer", peer.Addr, "err", err)
			return false, "status decode: " + err.Error()
		}
		if st.ChainID != n.cfg.ChainID {
			n.log.Warn("peer announced a different chain", "peer", peer.Addr, "chain_id", st.ChainID)
			_ = peer.Close()
			return false, "chain id"
		}
		if gh := n.chain.State().GenesisHash(); !st.GenesisHash.IsZero() && st.GenesisHash != gh {
			n.log.Warn("peer has a different genesis", "peer", peer.Addr, "genesis", st.GenesisHash.Short())
			_ = peer.Close()
			return false, "genesis"
		}
		if st.Height > n.chain.Height() {
			_ = peer.Send(znet.MsgGetBlocks, znet.EncodeHeightRange(n.chain.Height()+1, znet.MaxBlocksPerMessage))
		}
		return true, ""
	case znet.MsgGetBlocks:
		from, limit, err := znet.DecodeHeightRange(m.Payload)
		if err != nil {
			n.log.Warn("bad block request", "peer", peer.Addr, "err", err)
			return false, "range decode: " + err.Error()
		}
		n.serveBlocks(peer, from, limit)
		return true, ""
	case znet.MsgBlocks:
		blocks, err := znet.DecodeBlocks(m.Payload)
		if err != nil {
			n.log.Warn("bad block batch", "peer", peer.Addr, "err", err)
			return false, "blocks decode: " + err.Error()
		}
		n.applyPeerBlocks(peer, blocks)
		return true, "blocks"
	case znet.MsgProposal:
		p, err := znet.DecodeProposal(m.Payload)
		if err != nil {
			n.log.Warn("bad proposal", "peer", peer.Addr, "err", err)
			return false, "proposal decode: " + err.Error()
		}
		if p.ChainID != n.cfg.ChainID {
			return false, "proposal chain id"
		}
		n.InjectProposal(p)
		// Relay to peers that have not seen it (de-duplicated).
		if n.net != nil {
			n.net.Gossip(peer, znet.MsgProposal, m.Payload)
		}
		return true, "proposal"
	case znet.MsgVote:
		v, err := znet.DecodeVote(m.Payload)
		if err != nil {
			n.log.Warn("bad vote", "peer", peer.Addr, "err", err)
			return false, "vote decode: " + err.Error()
		}
		if v.ChainID != n.cfg.ChainID {
			return false, "vote chain id"
		}
		n.InjectVote(*v)
		if n.net != nil {
			n.net.Gossip(peer, znet.MsgVote, m.Payload)
		}
		return true, "vote"
	case znet.MsgTx:
		tx, err := znet.DecodeTx(m.Payload)
		if err != nil {
			n.log.Debug("bad transaction from peer", "peer", peer.Addr, "err", err)
			return false, "tx decode: " + err.Error()
		}
		if tx.ChainID != n.cfg.ChainID {
			return false, "tx chain id"
		}
		if err := tx.VerifySignature(); err != nil {
			n.log.Debug("peer transaction failed signature check", "peer", peer.Addr, "err", err)
			return false, "tx signature"
		}
		if n.chain.TxKnown(tx.ID()) || n.pool.Has(tx.ID()) {
			return false, "tx known"
		}
		if err := n.pool.Add(n.chain.State(), tx); err != nil {
			n.log.Debug("peer transaction not pooled", "peer", peer.Addr, "err", err)
			return false, "tx pool: " + err.Error()
		}
		// Relay once (gossip de-duplicates, so a loop is not possible).
		if n.net != nil {
			n.net.Gossip(peer, znet.MsgTx, m.Payload)
		}
		return true, "tx"
	case znet.MsgPing:
		_ = peer.Send(znet.MsgPong, nil)
		return true, "ping"
	case znet.MsgPong:
		return true, "pong"
	default:
		n.log.Debug("unknown message type", "type", m.Type, "peer", peer.Addr)
		return false, "unknown type"
	}
}

func (n *Node) serveBlocks(peer *znet.Peer, from uint64, limit uint32) {
	head := n.chain.Height()
	if from > head {
		_ = peer.Send(znet.MsgBlocks, mustEncodeBlocks(nil))
		return
	}
	var out []*ztypes.Block
	for h := from; h <= head && uint32(len(out)) < limit; h++ {
		if b, ok := n.chain.Block(h); ok {
			out = append(out, b)
		}
	}
	raw, err := znet.EncodeBlocks(out)
	if err != nil {
		n.log.Warn("encoding block batch", "err", err)
		return
	}
	_ = peer.Send(znet.MsgBlocks, raw)
}

func mustEncodeBlocks(blocks []*ztypes.Block) []byte {
	raw, err := znet.EncodeBlocks(blocks)
	if err != nil {
		return nil
	}
	return raw
}

// applyPeerBlocks applies blocks received for catch-up, in order, stopping at the
// first gap. Blocks that are already known are skipped.
func (n *Node) applyPeerBlocks(peer *znet.Peer, blocks []*ztypes.Block) {
	for _, b := range blocks {
		if b.Header.Height <= n.chain.Height() {
			continue
		}
		if b.Header.Height != n.chain.Height()+1 {
			// Gap: ask again from our head.
			_ = peer.Send(znet.MsgGetBlocks, znet.EncodeHeightRange(n.chain.Height()+1, znet.MaxBlocksPerMessage))
			return
		}
		if _, err := n.chain.ApplyBlock(b); err != nil {
			if errors.Is(err, ErrBlockInvalid) || errors.Is(err, ErrHeightMismatch) {
				n.log.Warn("rejecting synced block", "height", b.Header.Height, "err", err, "peer", peer.Addr)
				return
			}
			n.log.Error("applying synced block", "height", b.Header.Height, "err", err)
			return
		}
		n.pool.Reap(b.Txs)
		if n.OnCommit != nil {
			n.OnCommit(b, nil)
		}
		n.log.Info("block synced", "height", b.Header.Height, "hash", b.Header.Hash().Short())
		// Keep the consensus engine's height in step with sync progress.
		n.resyncEngine()
	}
}

// resyncEngine positions the consensus engine at chainHeight+1 after a sync jump.
// It is a hard assignment, not a loop: after catching up ten blocks the engine
// must sit at the next height, never somewhere below it.
func (n *Node) resyncEngine() {
	n.driveMu.Lock()
	defer n.driveMu.Unlock()
	if n.eng.Height() == n.chain.Height()+1 {
		return
	}
	set, err := ActiveValidatorSet(n.chain.State())
	if err != nil {
		return
	}
	if err := n.eng.SetHeight(n.chain.Height()+1, set); err != nil {
		n.log.Error("engine resync failed", "err", err)
	}
}

// DialSeeds connects to the configured seed peers, retrying in the background so a
// node can start before its peers.
func (n *Node) DialSeeds(seeds []string) {
	if len(seeds) == 0 {
		return
	}
	// Maintain a connection to every seed for the lifetime of the node: a peer
	// that restarts or loses its link is re-dialled. Dialling an already connected
	// seed is a no-op, so the loop is safe to run forever.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-ticker.C:
			}
			n.mu.Lock()
			srv := n.net
			n.mu.Unlock()
			if srv == nil {
				continue
			}
			for _, seed := range seeds {
				if seed == srv.Addr() {
					continue // that is us
				}
				_, err := srv.Dial(seed)
				if err != nil && !errors.Is(err, znet.ErrSelfDial) {
					continue
				}
			}
		}
	}()
}

// PeerCount returns the number of connected peers.
func (n *Node) PeerCount() int {
	n.mu.Lock()
	srv := n.net
	n.mu.Unlock()
	if srv == nil {
		return 0
	}
	return srv.PeerCount()
}

// BroadcastTx gossips a transaction accepted via RPC.
func (n *Node) BroadcastTx(tx *ztypes.Tx) {
	n.mu.Lock()
	srv := n.net
	n.mu.Unlock()
	if srv != nil {
		srv.Broadcast(znet.MsgTx, znet.EncodeTx(tx))
	}
}

// GenesisHash returns the chain's genesis hash.
func (c *Chain) GenesisHash() zcrypto.Hash { return c.State().GenesisHash() }

// TxKnown reports whether a transaction is already on chain.
func (c *Chain) TxKnown(id zcrypto.Hash) bool {
	_, ok := c.TxHeight(id)
	return ok
}

var _ = fmt.Sprintf

// RefreshStatus announces this node's current height to all peers. It is called
// after a block is applied locally so a validator that was waiting for a proposal
// of a height it had already synced can resynchronise instead of stalling.
func (n *Node) RefreshStatus() {
	n.mu.Lock()
	srv := n.net
	n.mu.Unlock()
	if srv == nil {
		return
	}
	for _, p := range srv.Peers() {
		n.sendStatus(p)
	}
}
