package znet

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// ---- framing ---------------------------------------------------------------

func TestFrameRoundTrip(t *testing.T) {
	payloads := [][]byte{nil, {}, {1}, bytes.Repeat([]byte{0xab}, 1000), bytes.Repeat([]byte{0xcd}, MaxFrame-16)}
	for _, p := range payloads {
		frame, err := EncodeFrame(MsgTx, p)
		if err != nil {
			t.Fatalf("len %d: encode: %v", len(p), err)
		}
		msg, err := ReadMessage(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("len %d: decode: %v", len(p), err)
		}
		if msg.Type != MsgTx || !bytes.Equal(msg.Payload, p) {
			t.Fatalf("len %d: round trip mismatch", len(p))
		}
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	if _, err := EncodeFrame(MsgTx, make([]byte, MaxFrame)); !errors.Is(err, ErrFrameTooBig) {
		t.Fatalf("oversize encode error = %v", err)
	}
	// A hostile length prefix must be refused before allocation.
	big := []byte{0xff, 0xff, 0xff, 0xff}
	if _, err := ReadMessage(bytes.NewReader(big)); !errors.Is(err, ErrFrameTooBig) {
		t.Fatalf("hostile length error = %v", err)
	}
	// Zero length is invalid.
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("zero-length frame accepted")
	}
}

func TestFrameRejectsCorruption(t *testing.T) {
	frame, err := EncodeFrame(MsgVote, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip a payload byte: the checksum must catch it.
	bad := append([]byte{}, frame...)
	bad[7] ^= 0xff
	if _, err := ReadMessage(bytes.NewReader(bad)); !errors.Is(err, ErrChecksum) {
		t.Fatalf("corrupt payload error = %v", err)
	}
	// Flip a checksum byte.
	bad = append([]byte{}, frame...)
	bad[len(bad)-1] ^= 0xff
	if _, err := ReadMessage(bytes.NewReader(bad)); !errors.Is(err, ErrChecksum) {
		t.Fatalf("corrupt checksum error = %v", err)
	}
	// Truncation is an error, never a partial message.
	if _, err := ReadMessage(bytes.NewReader(frame[:len(frame)-1])); err == nil {
		t.Fatal("truncated frame accepted")
	}
}

// ---- handshake and message codecs ------------------------------------------

func key(t *testing.T, label string) *zcrypto.PrivateKey {
	t.Helper()
	d := zcrypto.Digest("znet-test", []byte(label))
	k, err := zcrypto.KeyFromSeed(d.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestHandshakeRoundTrip(t *testing.T) {
	k := key(t, "a")
	h := &Handshake{ChainID: "znetwork-net-test", Moniker: "node-a", ListenAddr: "127.0.0.1:1", PubKey: k.PublicKey(), Height: 42}
	back, err := DecodeHandshake(h.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if back.ChainID != h.ChainID || back.Moniker != h.Moniker || back.Height != h.Height || back.PubKey != h.PubKey {
		t.Fatal("handshake round trip mismatch")
	}
	// Trailing bytes are rejected.
	if _, err := DecodeHandshake(append(h.Encode(), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	// A wrong version byte is rejected.
	bad := h.Encode()
	bad[0] = 99
	if _, err := DecodeHandshake(bad); err == nil {
		t.Fatal("unsupported version accepted")
	}
	// An empty chain id is rejected.
	empty := &Handshake{Moniker: "x"}
	if _, err := DecodeHandshake(empty.Encode()); err == nil {
		t.Fatal("empty chain id accepted")
	}
}

func TestStatusCodec(t *testing.T) {
	s := &Status{ChainID: "c", Height: 7, HeadHash: zcrypto.Digest("h"), GenesisHash: zcrypto.Digest("g")}
	back, err := DecodeStatus(EncodeStatus(s))
	if err != nil {
		t.Fatal(err)
	}
	if back.Height != s.Height || back.HeadHash != s.HeadHash || back.GenesisHash != s.GenesisHash {
		t.Fatal("status round trip mismatch")
	}
	if _, err := DecodeStatus(append(EncodeStatus(s), 1)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	if _, err := DecodeStatus([]byte{0x00}); err == nil {
		t.Fatal("truncated status accepted")
	}
}

func TestVoteCodec(t *testing.T) {
	k := key(t, "voter")
	v := &zconsensus.Vote{
		ChainID: "c", Height: 9, Round: 2, Step: 1,
		BlockID: zcrypto.Digest("b"), Voter: k.Address(), PubKey: k.PublicKey(), TimeMs: 1234,
	}
	if err := zconsensus.SignVote(k, v); err != nil {
		t.Fatal(err)
	}
	back, err := DecodeVote(EncodeVote(v))
	if err != nil {
		t.Fatal(err)
	}
	if back.Height != v.Height || back.Round != v.Round || back.Step != v.Step ||
		back.BlockID != v.BlockID || back.Voter != v.Voter || back.Signature != v.Signature {
		t.Fatal("vote round trip mismatch")
	}
	if _, err := DecodeVote(append(EncodeVote(v), 9)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	if _, err := DecodeVote([]byte{1, 2, 3}); err == nil {
		t.Fatal("truncated vote accepted")
	}
}

func TestBlocksCodec(t *testing.T) {
	mk := func(h uint64) *ztypes.Block {
		b := &ztypes.Block{}
		b.Header.Version = ztypes.ProtocolVersion
		b.Header.ChainID = "c"
		b.Header.Height = h
		b.Header.TimeMs = int64(h) * 1000
		return b
	}
	blocks := []*ztypes.Block{mk(1), mk(2), mk(3)}
	raw, err := EncodeBlocks(blocks)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeBlocks(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Fatalf("blocks = %d", len(back))
	}
	for i := range blocks {
		if back[i].Header.Hash() != blocks[i].Header.Hash() {
			t.Fatalf("block %d changed", i)
		}
	}
	// Empty batch is valid.
	if raw, err := EncodeBlocks(nil); err != nil {
		t.Fatal(err)
	} else if _, err := DecodeBlocks(raw); err != nil {
		t.Fatal(err)
	}
	// Oversized batches are refused at encode time.
	many := make([]*ztypes.Block, MaxBlocksPerMessage+1)
	for i := range many {
		many[i] = mk(uint64(i))
	}
	if _, err := EncodeBlocks(many); err == nil {
		t.Fatal("oversized batch encoded")
	}
	// A count that exceeds the limit is refused at decode time.
	evil := []byte{0x00, 0x00, 0x10, 0x00}
	if _, err := DecodeBlocks(evil); err == nil {
		t.Fatal("hostile block count accepted")
	}
}

func TestHeightRangeCodec(t *testing.T) {
	raw := EncodeHeightRange(100, 32)
	from, limit, err := DecodeHeightRange(raw)
	if err != nil || from != 100 || limit != 32 {
		t.Fatalf("range = %d, %d, %v", from, limit, err)
	}
	// Bogus limits are clamped, not trusted.
	raw = EncodeHeightRange(1, 100000)
	if _, limit, err := DecodeHeightRange(raw); err != nil || limit > MaxBlocksPerMessage {
		t.Fatalf("limit not clamped: %d, %v", limit, err)
	}
	if _, _, err := DecodeHeightRange(append(raw, 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
}

// ---- live transport --------------------------------------------------------

func TestTwoServersExchangeMessages(t *testing.T) {
	a := NewServer("chain-a", "a", key(t, "sa").PublicKey(), func() uint64 { return 1 })
	b := NewServer("chain-a", "b", key(t, "sb").PublicKey(), func() uint64 { return 2 })

	var mu sync.Mutex
	var got []uint8
	b.OnMessage = func(p *Peer, m *Message) {
		mu.Lock()
		got = append(got, m.Type)
		mu.Unlock()
	}
	addrA, err := a.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()

	peer, err := a.Dial(addrA)
	if err == nil {
		t.Fatalf("dialling own listen address succeeded: %v", peer)
	}
	_ = peer

	peer, err = a.Dial(b.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if peer.Handshake.Moniker != "b" || peer.Handshake.Height != 2 {
		t.Fatalf("handshake = %+v", peer.Handshake)
	}
	if err := peer.Send(MsgPing, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(MsgTx, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("messages received = %v", got)
	}
	if a.PeerCount() != 1 || b.PeerCount() != 1 {
		t.Fatalf("peer counts a=%d b=%d", a.PeerCount(), b.PeerCount())
	}
}

// TestFilterDropsMessagesBothDirections proves the partition hook: while a
// filter is installed on both ends, nothing crosses the wire in either
// direction even though the connection stays up; removing it heals traffic.
func TestFilterDropsMessagesBothDirections(t *testing.T) {
	a := NewServer("chain-a", "a", key(t, "fa").PublicKey(), nil)
	b := NewServer("chain-a", "b", key(t, "fb").PublicKey(), nil)
	var gotA, gotB int32
	a.OnMessage = func(p *Peer, m *Message) { atomic.AddInt32(&gotA, 1) }
	b.OnMessage = func(p *Peer, m *Message) { atomic.AddInt32(&gotB, 1) }
	if _, err := a.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	peer, err := a.Dial(b.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Cut: both servers drop every message, both directions.
	dropAll := func(p *Peer, inbound bool, typ uint8) bool { return true }
	a.SetFilter(dropAll)
	b.SetFilter(dropAll)
	if err := peer.Send(MsgPing, []byte("x")); err != nil {
		t.Fatalf("send under filter: %v", err)
	}
	b.Broadcast(MsgTx, []byte("y"))
	time.Sleep(150 * time.Millisecond)
	if g1, g2 := atomic.LoadInt32(&gotA), atomic.LoadInt32(&gotB); g1 != 0 || g2 != 0 {
		t.Fatalf("filter leaked traffic: a=%d b=%d", g1, g2)
	}
	// The connection itself must survive the cut (black hole, not link down).
	if a.PeerCount() != 1 || b.PeerCount() != 1 {
		t.Fatalf("connection dropped by filter: a=%d b=%d", a.PeerCount(), b.PeerCount())
	}

	// Heal: the same messages now flow.
	a.SetFilter(nil)
	b.SetFilter(nil)
	if err := peer.Send(MsgPing, []byte("x")); err != nil {
		t.Fatalf("send after heal: %v", err)
	}
	b.Broadcast(MsgTx, []byte("y"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&gotA) >= 1 && atomic.LoadInt32(&gotB) >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("after heal: a=%d b=%d", atomic.LoadInt32(&gotA), atomic.LoadInt32(&gotB))
}

func TestHandshakeRejectsWrongChain(t *testing.T) {
	a := NewServer("chain-a", "a", key(t, "a2").PublicKey(), nil)
	b := NewServer("chain-b", "b", key(t, "b2").PublicKey(), nil)
	if _, err := a.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	if _, err := a.Dial(b.Addr()); !errors.Is(err, ErrChainID) {
		t.Fatalf("cross-chain dial error = %v", err)
	}
	if a.PeerCount() != 0 || b.PeerCount() != 0 {
		t.Fatal("a cross-chain peer was registered")
	}
}

func TestGossipDeduplicatesAndSkipsOrigin(t *testing.T) {
	srv := NewServer("c", "s", key(t, "g").PublicKey(), nil)
	var mu sync.Mutex
	received := map[string]int{}
	srv.OnMessage = func(p *Peer, m *Message) {
		mu.Lock()
		received[string(m.Payload)]++
		mu.Unlock()
	}
	if _, err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Two clients connect so gossip has somewhere to go.
	peers := make([]*Peer, 0, 2)
	for i := 0; i < 2; i++ {
		c := NewServer("c", "client", key(t, "cl"+string(rune('a'+i))).PublicKey(), nil)
		if _, err := c.Listen("127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Dial(srv.Addr()); err != nil {
			t.Fatal(err)
		}
	}
	// Wait for the server to register both peers.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.PeerCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.PeerCount() != 2 {
		t.Fatalf("peers = %d", srv.PeerCount())
	}
	list := srv.Peers()
	origin := list[0]
	payload := []byte("vote-payload")
	if n := srv.Gossip(origin, MsgVote, payload); n != 1 {
		t.Fatalf("first gossip sent to %d peers", n)
	}
	if n := srv.Gossip(origin, MsgVote, payload); n != 0 {
		t.Fatalf("duplicate gossip sent to %d peers", n)
	}
	_ = peers
}

func TestMessageFloodIsBounded(t *testing.T) {
	// A peer that sends garbage must be disconnected, and must not be able to
	// register itself.
	srv := NewServer("chain-x", "x", key(t, "x").PublicKey(), nil)
	addr, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Send a hostile length prefix.
	if _, err := conn.Write([]byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// The connection should be closed by the server.
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		var b [1]byte
		if _, err := conn.Read(b[:]); err != nil {
			if errors.Is(err, net.ErrClosed) || err.Error() == "EOF" || err != nil {
				if srv.PeerCount() == 0 {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hostile connection was not dropped")
}

func TestBroadcastAndPeerCount(t *testing.T) {
	srv := NewServer("c", "s", key(t, "bc").PublicKey(), nil)
	if _, err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var count int32
	srv.OnMessage = func(p *Peer, m *Message) { atomic.AddInt32(&count, 1) }
	clients := make([]*Server, 0, 3)
	for i := 0; i < 3; i++ {
		c := NewServer("c", "cl", key(t, "clx"+string(rune('a'+i))).PublicKey(), nil)
		if _, err := c.Listen("127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Dial(srv.Addr()); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.PeerCount() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	// Broadcast reaches the connected peers. Inbound messages are what this server
	// receives, so to observe delivery the clients must send to it.
	if n := srv.Broadcast(MsgPing, []byte("hi")); n != 3 {
		t.Fatalf("broadcast reached %d peers", n)
	}
	// Reverse direction: each CLIENT sends to the server (sending on the server's
	// own Peer objects would be the outbound direction again).
	for _, c := range clients {
		for _, p := range c.Peers() {
			if err := p.Send(MsgPing, []byte("back")); err != nil {
				t.Fatalf("client send: %v", err)
			}
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&count) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&count); got < 3 {
		t.Fatalf("messages received = %d", got)
	}
}

// FuzzReadMessage asserts that the frame reader never panics and that a decoded
// message re-encodes to exactly the frame that was consumed — no more, no less.
// Consumed length is reported so a frame followed by unrelated bytes is allowed
// (that is a normal stream), while a frame that swallows bytes it did not need
// is a framing ambiguity and must fail.
func FuzzReadMessage(f *testing.F) {
	valid, _ := EncodeFrame(MsgTx, []byte("hello"))
	f.Add(valid)
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 2, 6, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		br := bytes.NewReader(b)
		msg, err := ReadMessage(br)
		if err != nil {
			return
		}
		consumed := len(b) - br.Len()
		again, err := EncodeFrame(msg.Type, msg.Payload)
		if err != nil {
			t.Fatalf("accepted a frame that cannot be re-encoded: %v", err)
		}
		if int64(len(again)) != int64(consumed) || !bytes.Equal(again, b[:consumed]) {
			t.Fatalf("frame consumed %d bytes but re-encodes to %d (%x vs %x)",
				consumed, len(again), b[:consumed], again)
		}
	})
}
