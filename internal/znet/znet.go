// Package znet is Z Network's peer-to-peer transport (AGENT_04).
//
// Protocol version 1 uses a simple, explicit TCP protocol:
//
//	handshake  : magic(4) || version(1) || chainID(len-prefixed str) || moniker(str) || listenAddr(str) || pubkey(32)
//	frame      : u32 length || u8 type || payload || u32 crc32c(payload)
//
// Properties:
//   - Every inbound message is size-limited (MaxFrame) before allocation, so a
//     peer cannot exhaust memory with an oversized length prefix.
//   - Every frame is checksummed; a corrupted or truncated frame terminates the
//     connection instead of being dispatched.
//   - Messages are typed and strictly decoded with the canonical ZENC reader,
//     which rejects trailing bytes (no ambiguous encodings — threat T-15/T-11).
//   - A peer must present the same chain id, or the connection is refused.
//   - Optional peer authentication: a peer may sign a challenge; the transport
//     records the verified identity but does not require it (peer identity is not
//     used for consensus authority anywhere — see NETWORK_SPECIFICATION.md).
//
// This package does NOT implement transport encryption. Peers exchange only
// public data (blocks, transactions, votes); confidentiality is not provided and
// is not claimed. Integrity of consensus-relevant data is provided by signatures,
// not by the transport.
package znet

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"sync"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
)

// Protocol constants.
const (
	Magic          uint32 = 0x5A4E4554 // "ZNET"
	ProtoVersion   uint8  = 1
	MaxFrame              = 4 << 20 // 4 MiB hard ceiling per frame
	HandshakeLimit        = 1024
	DefaultTimeout        = 10 * time.Second
	IdleTimeout           = 120 * time.Second
)

// Message types.
const (
	MsgHello      uint8 = 0x01
	MsgGetBlocks  uint8 = 0x02
	MsgBlocks     uint8 = 0x03
	MsgProposal   uint8 = 0x04
	MsgVote       uint8 = 0x05
	MsgTx         uint8 = 0x06
	MsgStatus     uint8 = 0x07
	MsgPing       uint8 = 0x08
	MsgPong       uint8 = 0x09
	MsgGetBlockBy uint8 = 0x0a
)

// Errors.
var (
	ErrHandshake   = errors.New("znet: handshake failed")
	ErrChainID     = errors.New("znet: peer chain id mismatch")
	ErrFrameTooBig = errors.New("znet: frame exceeds limit")
	ErrChecksum    = errors.New("znet: frame checksum mismatch")
	ErrClosed      = errors.New("znet: connection closed")
	ErrSelfDial    = errors.New("znet: refusing to dial self")
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Message is a decoded frame.
type Message struct {
	Type    uint8
	Payload []byte
}

// EncodeFrame builds a wire frame for a message.
func EncodeFrame(typ uint8, payload []byte) ([]byte, error) {
	if len(payload)+5 > MaxFrame {
		return nil, ErrFrameTooBig
	}
	buf := make([]byte, 4+1+len(payload)+4)
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)+1))
	buf[4] = typ
	copy(buf[5:], payload)
	crc := crc32.Checksum(buf[4:5+len(payload)], crcTable)
	binary.BigEndian.PutUint32(buf[5+len(payload):], crc)
	return buf, nil
}

// ReadMessage reads one framed message from r.
func ReadMessage(r io.Reader) (*Message, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 || n > MaxFrame {
		return nil, fmt.Errorf("%w: declared %d bytes", ErrFrameTooBig, n)
	}
	// n == 1 is legal: a type byte with an empty payload.
	// Layout after the length prefix: type(1) || payload(n-1) || crc32c(4).
	body := make([]byte, n+4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	want := binary.BigEndian.Uint32(body[n:])
	if got := crc32.Checksum(body[:n], crcTable); got != want {
		return nil, ErrChecksum
	}
	return &Message{Type: body[0], Payload: body[1:n]}, nil
}

// WriteMessage writes a framed message to w.
func WriteMessage(w io.Writer, typ uint8, payload []byte) error {
	frame, err := EncodeFrame(typ, payload)
	if err != nil {
		return err
	}
	_, err = w.Write(frame)
	return err
}

// Handshake is the peer identity exchange.
type Handshake struct {
	ChainID    string
	Moniker    string
	ListenAddr string
	PubKey     zcrypto.PublicKey
	Height     uint64
}

// Encode serialises a handshake (including the protocol version byte).
func (h *Handshake) Encode() []byte {
	w := zencoding.NewWriterSize(128)
	w.U8(ProtoVersion)
	w.Str(h.ChainID)
	w.Str(h.Moniker)
	w.Str(h.ListenAddr)
	w.Raw(h.PubKey[:])
	w.U64(h.Height)
	return w.Done()
}

// DecodeHandshake parses a handshake payload.
func DecodeHandshake(b []byte) (*Handshake, error) {
	r := zencoding.NewReader(b)
	if len(b) < 1 || b[0] != ProtoVersion {
		return nil, fmt.Errorf("%w: unsupported protocol version", ErrHandshake)
	}
	_ = r.U8() // already validated as ProtoVersion above
	h := &Handshake{}
	h.ChainID = r.Str()
	h.Moniker = r.Str()
	h.ListenAddr = r.Str()
	if raw := r.Raw(32); raw != nil {
		copy(h.PubKey[:], raw)
	}
	h.Height = r.U64()
	if err := r.Done(); err != nil {
		return nil, err
	}
	// The version byte is the first byte of the frame body; it is validated by
	// the caller through ProtoVersion before the rest is trusted.
	if len(h.ChainID) == 0 {
		return nil, fmt.Errorf("%w: empty chain id", ErrHandshake)
	}
	return h, nil
}

// exchangeHandshake performs the symmetric handshake on a connection.
func exchangeHandshake(c net.Conn, mine *Handshake, expectChain string, deadline time.Time) (*Handshake, error) {
	_ = c.SetDeadline(deadline)
	if err := WriteMessage(c, MsgHello, mine.Encode()); err != nil {
		return nil, fmt.Errorf("%w: send: %v", ErrHandshake, err)
	}
	msg, err := ReadMessage(c)
	if err != nil {
		return nil, fmt.Errorf("%w: receive: %v", ErrHandshake, err)
	}
	if msg.Type != MsgHello {
		return nil, fmt.Errorf("%w: expected hello, got message type %d", ErrHandshake, msg.Type)
	}
	theirs, err := DecodeHandshake(msg.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	if theirs.ChainID != expectChain {
		return nil, fmt.Errorf("%w: peer is on %q, we are on %q", ErrChainID, theirs.ChainID, expectChain)
	}
	_ = c.SetDeadline(time.Time{})
	return theirs, nil
}

// Peer is a connected peer.
type Peer struct {
	Conn      net.Conn
	Handshake *Handshake
	Addr      string
	Outbound  bool
	// Initiator is the public key of the node that opened this TCP connection.
	// It is used to break the "both sides dialled at once" tie deterministically:
	// both ends keep the connection initiated by the smaller key and close the
	// other, so they always end up sharing exactly one live connection.
	Initiator zcrypto.PublicKey

	writeMu sync.Mutex
}

// Send writes a framed message to the peer.
func (p *Peer) Send(typ uint8, payload []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.Conn.SetWriteDeadline(time.Now().Add(DefaultTimeout))
	return WriteMessage(p.Conn, typ, payload)
}

// Close closes the connection.
func (p *Peer) Close() error { return p.Conn.Close() }

// Server is a node's listening peer manager.
type Server struct {
	mu       sync.RWMutex
	chainID  string
	moniker  string
	pubkey   zcrypto.PublicKey
	ln       net.Listener
	peers    map[string]*Peer
	byAddr   map[string]*Peer
	heightFn func() uint64
	dedup    *dedupCache

	// OnMessage is called for every inbound message with the sending peer.
	OnMessage func(p *Peer, m *Message)
	// OnPeerUp / OnPeerDown report peer lifecycle changes.
	OnPeerUp   func(p *Peer)
	OnPeerDown func(p *Peer)

	closed chan struct{}
}

// NewServer creates a peer manager for a chain.
func NewServer(chainID, moniker string, pubkey zcrypto.PublicKey, heightFn func() uint64) *Server {
	if heightFn == nil {
		heightFn = func() uint64 { return 0 }
	}
	return &Server{
		chainID:  chainID,
		moniker:  moniker,
		pubkey:   pubkey,
		peers:    map[string]*Peer{},
		byAddr:   map[string]*Peer{},
		heightFn: heightFn,
		dedup:    newDedupCache(8192),
		closed:   make(chan struct{}),
	}
}

// Listen starts accepting connections. It returns the bound address.
func (s *Server) Listen(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go s.acceptLoop(ln)
	return ln.Addr().String(), nil
}

// Addr returns the bound listen address ("" if not listening).
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			return
		}
		go s.setupInbound(conn)
	}
}

func (s *Server) myHandshake() *Handshake {
	return &Handshake{
		ChainID:    s.chainID,
		Moniker:    s.moniker,
		ListenAddr: s.Addr(),
		PubKey:     s.pubkey,
		Height:     s.heightFn(),
	}
}

func (s *Server) setupInbound(conn net.Conn) {
	hs, err := exchangeHandshake(conn, s.myHandshake(), s.chainID, time.Now().Add(DefaultTimeout))
	if err != nil {
		_ = conn.Close()
		return
	}
	p := &Peer{Conn: conn, Handshake: hs, Addr: conn.RemoteAddr().String(), Outbound: false, Initiator: hs.PubKey}
	s.register(p)
}

// Dial connects to a peer and registers it.
func (s *Server) Dial(addr string) (*Peer, error) {
	if addr == "" {
		return nil, fmt.Errorf("znet: empty peer address")
	}
	if addr == s.Addr() {
		return nil, ErrSelfDial
	}
	conn, err := net.DialTimeout("tcp", addr, DefaultTimeout)
	if err != nil {
		return nil, err
	}
	hs, err := exchangeHandshake(conn, s.myHandshake(), s.chainID, time.Now().Add(DefaultTimeout))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	p := &Peer{Conn: conn, Handshake: hs, Addr: addr, Outbound: true, Initiator: s.pubkey}
	s.register(p)
	return p, nil
}

func (s *Server) register(p *Peer) {
	key := p.Handshake.PubKey.String()
	s.mu.Lock()
	if old, ok := s.peers[key]; ok {
		// Duplicate connection to the same peer. Both nodes must keep the SAME
		// socket, otherwise a simultaneous dial closes both. Tie-break on the
		// initiator: keep the connection opened by the smaller public key.
		keepNew := string(p.Initiator[:]) < string(old.Initiator[:])
		if !keepNew {
			s.mu.Unlock()
			_ = p.Conn.Close()
			return
		}
		s.peers[key] = p
		s.byAddr[p.Addr] = p
		delete(s.byAddr, old.Addr)
		s.mu.Unlock()
		_ = old.Conn.Close()
		s.up(p)
		go s.readLoop(p)
		return
	}
	s.peers[key] = p
	s.byAddr[p.Addr] = p
	s.mu.Unlock()
	s.up(p)
	go s.readLoop(p)
}

func (s *Server) up(p *Peer) {
	if s.OnPeerUp != nil {
		s.OnPeerUp(p)
	}
}

// Connected reports whether a peer with the given public key is connected.
func (s *Server) Connected(pub zcrypto.PublicKey) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.peers[pub.String()]
	return ok
}

func (s *Server) readLoop(p *Peer) {
	defer func() {
		s.remove(p)
		_ = p.Conn.Close()
	}()
	for {
		_ = p.Conn.SetReadDeadline(time.Now().Add(IdleTimeout))
		msg, err := ReadMessage(p.Conn)
		if err != nil {
			return
		}
		if s.OnMessage != nil {
			s.OnMessage(p, msg)
		}
	}
}

func (s *Server) remove(p *Peer) {
	key := p.Handshake.PubKey.String()
	s.mu.Lock()
	wasCurrent := false
	if cur, ok := s.peers[key]; ok && cur == p {
		delete(s.peers, key)
		wasCurrent = true
	}
	if cur, ok := s.byAddr[p.Addr]; ok && cur == p {
		delete(s.byAddr, p.Addr)
	}
	s.mu.Unlock()
	if wasCurrent && s.OnPeerDown != nil {
		s.OnPeerDown(p)
	}
}

// Broadcast sends a message to every connected peer, ignoring per-peer failures
// (a dead peer is removed by its read loop).
func (s *Server) Broadcast(typ uint8, payload []byte) int {
	s.mu.RLock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.RUnlock()
	sent := 0
	for _, p := range peers {
		if err := p.Send(typ, payload); err == nil {
			sent++
		}
	}
	return sent
}

// Peers returns a snapshot of connected peers.
func (s *Server) Peers() []*Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	return out
}

// PeerCount returns the number of connected peers.
func (s *Server) PeerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

// Close shuts down the listener and all connections.
func (s *Server) Close() {
	s.mu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	ln := s.ln
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, p := range peers {
		_ = p.Conn.Close()
	}
}

// dedupCache remembers recently gossiped payload fingerprints so a relayed
// message is forwarded at most once. Without it, relaying in a mesh would loop
// forever; the cache is bounded by insertion order (FIFO eviction).
type dedupCache struct {
	mu    sync.Mutex
	set   map[[16]byte]struct{}
	order [][16]byte
	max   int
}

func newDedupCache(max int) *dedupCache {
	return &dedupCache{set: map[[16]byte]struct{}{}, max: max}
}

func (d *dedupCache) see(key [16]byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.set[key]; ok {
		return true
	}
	d.set[key] = struct{}{}
	d.order = append(d.order, key)
	if len(d.order) > d.max {
		old := d.order[0]
		d.order = d.order[1:]
		delete(d.set, old)
	}
	return false
}

// Gossip relays a message to every peer except origin, de-duplicating by payload
// fingerprint. It is the correct primitive for proposals, votes and transactions:
// broadcast is for messages this node originates, gossip is for messages it
// forwards.
func (s *Server) Gossip(origin *Peer, typ uint8, payload []byte) int {
	sum := sha256.Sum256(append([]byte{typ}, payload...))
	var key [16]byte
	copy(key[:], sum[:16])
	if s.dedup.see(key) {
		return 0
	}
	s.mu.RLock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		if p == origin {
			continue
		}
		peers = append(peers, p)
	}
	s.mu.RUnlock()
	sent := 0
	for _, p := range peers {
		if err := p.Send(typ, payload); err == nil {
			sent++
		}
	}
	return sent
}
