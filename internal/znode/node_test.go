package znode

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"znetwork/internal/zconsensus"
	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/zexec"
	"znetwork/internal/ztypes"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func devnetCfg(t *testing.T, dir string, node int) (Config, *zdevnet.Network) {
	t.Helper()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		ChainID:         net.ChainID,
		DataDir:         filepath.Join(dir, net.Nodes[node].Name),
		Genesis:         net.Genesis,
		Validator:       net.Nodes[node].Key,
		Moniker:         net.Nodes[node].Name,
		BlockTimeMs:     20,
		MaxTxsPerBlock:  200,
		CheckInvariants: true,
	}, net
}

func waitHeight(t *testing.T, n *Node, want uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.Chain().Height() >= want {
			return
		}
		select {
		case err := <-n.Errors():
			t.Fatalf("node error while waiting for height %d: %v", want, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("timeout waiting for height %d (at %d)", want, n.Chain().Height())
}

func TestSingleNodeProducesBlocks(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	// A single-validator network: only node 0 is a validator in this test.
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	n.Start()
	defer n.Stop()
	waitHeight(t, n, 3, 10*time.Second)

	// Every block must be internally consistent.
	for h := uint64(1); h <= n.Chain().Height(); h++ {
		b, ok := n.Chain().Block(h)
		if !ok {
			t.Fatalf("missing block %d", h)
		}
		if err := n.Chain().ValidateOnly(b); err == nil {
			// ValidateOnly is relative to the head, so replaying an old block on a
			// higher head must fail. That is expected; we check the linkage here.
			t.Fatalf("block %d unexpectedly validated against a later head", h)
		}
		prev, _ := n.Chain().Block(h - 1)
		if b.Header.PrevHash != prev.Header.Hash() {
			t.Fatalf("block %d does not link to its parent", h)
		}
	}
	if err := zexec.VerifySupply(n.Chain().State()); err != nil {
		t.Fatalf("accounting invariant after blocks: %v", err)
	}
}

// minimalGenesis builds a genesis with a single validator (node index i) so a
// one-node network has quorum.
func minimalGenesis(t *testing.T, net *zdevnet.Network, i int) *ztypes.Genesis {
	t.Helper()
	g := &ztypes.Genesis{
		Version:    ztypes.ProtocolVersion,
		ChainID:    net.ChainID,
		TimeMs:     net.Genesis.TimeMs,
		ParamsJSON: map[string]uint64{"min_validator_stake_uz": 1_000 * ztypes.CoinScale},
	}
	v := net.Nodes[i]
	g.Alloc = append(g.Alloc, ztypes.GenesisAccount{Address: v.Address.Bech32(), Balance: 10_000 * ztypes.CoinScale})
	g.Validators = append(g.Validators, ztypes.GenesisValidator{
		PublicKey:     v.Key.PublicKey().String(),
		Moniker:       v.Name,
		CommissionBps: 1000,
		SelfBond:      1_000 * ztypes.CoinScale,
	})
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestTransactionIncludedAndExecuted(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Add a funded user to the minimal genesis so there is someone to pay.
	// (Rebuild the node with an extra allocation.)
	user, err := zcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.DataDir = filepath.Join(dir, "node-with-user")
	gen := minimalGenesis(t, net, 0)
	gen.Alloc = append(gen.Alloc, ztypes.GenesisAccount{Address: user.Address().Bech32(), Balance: 1_000 * ztypes.CoinScale})
	cfg2.Genesis = gen
	n2, err := NewNode(cfg2, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	n2.Start()
	defer n2.Stop()
	waitHeight(t, n2, 1, 10*time.Second)

	recipient, _ := zcrypto.GenerateKey()
	amount := uint64(25 * ztypes.CoinScale)
	tx := &ztypes.Tx{
		ChainID: net.ChainID, Type: ztypes.TxTransfer, Sender: user.Address(),
		Nonce: 0, GasLimit: 200_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
		Payload: (&ztypes.TransferPayload{To: recipient.Address(), Amount: amount}).Encode(),
	}
	if err := tx.Sign(user); err != nil {
		t.Fatal(err)
	}
	id, err := n2.SubmitTx(tx)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Wait until the transaction is in a block.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h, ok := n2.Chain().TxHeight(id); ok {
			if h == 0 {
				t.Fatal("transaction reported at genesis height")
			}
			acc, _ := n2.Chain().State().GetAccount(recipient.Address())
			if acc.Balance != amount {
				t.Fatalf("recipient balance = %d, want %d", acc.Balance, amount)
			}
			if err := zexec.VerifySupply(n2.Chain().State()); err != nil {
				t.Fatalf("invariant: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("transaction was never included")
	_ = n
}

func TestInvalidTransactionRejectedByPool(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := zcrypto.GenerateKey()
	tx := &ztypes.Tx{
		ChainID: net.ChainID, Type: ztypes.TxTransfer, Sender: stranger.Address(),
		Nonce: 0, GasLimit: 200_000, GasPrice: 1,
		Payload: (&ztypes.TransferPayload{To: net.Nodes[0].Address, Amount: 1}).Encode(),
	}
	if err := tx.Sign(stranger); err != nil {
		t.Fatal(err)
	}
	if _, err := n.SubmitTx(tx); err == nil {
		t.Fatal("unfunded transaction was pooled")
	}
	// Wrong chain id must be rejected too.
	tx.ChainID = "elsewhere"
	if err := tx.Sign(stranger); err != nil {
		t.Fatal(err)
	}
	if _, err := n.SubmitTx(tx); err == nil {
		t.Fatal("cross-chain transaction was pooled")
	}
}

func TestRestartRecoversState(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	cfg.CheckInvariants = true
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	n.Start()
	waitHeight(t, n, 3, 10*time.Second)
	height := n.Chain().Height()
	root := n.Chain().State().Root()
	supply := n.Chain().State().Supply()
	n.Stop()

	n2, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n2.Chain().Height() != height {
		t.Fatalf("height after restart = %d, want %d", n2.Chain().Height(), height)
	}
	if n2.Chain().State().Root() != root {
		t.Fatal("state root changed across restart")
	}
	if n2.Chain().State().Supply() != supply {
		t.Fatal("supply changed across restart")
	}
	// The restarted node must keep producing valid blocks.
	n2.Start()
	defer n2.Stop()
	waitHeight(t, n2, height+2, 10*time.Second)
	if err := zexec.VerifySupply(n2.Chain().State()); err != nil {
		t.Fatalf("invariant after restart: %v", err)
	}
}

func TestRejectsBlockWithBadStateRoot(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := n.Chain().BuildBlock(1, time.Now().UnixMilli(), net.Nodes[0].Address, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Header.StateRoot[0] ^= 0xff
	if _, err := n.Chain().ApplyBlock(b); err == nil {
		t.Fatal("block with corrupted state root was accepted")
	}
	if n.Chain().Height() != 0 {
		t.Fatal("chain height advanced on a rejected block")
	}
}

func TestRejectsBlockWithWrongProposer(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	outsider, _ := zcrypto.GenerateKey()
	b, _, err := n.Chain().BuildBlock(1, time.Now().UnixMilli(), outsider.Address(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A block must carry a commit from the validator set; without one it is invalid.
	if _, err := n.Chain().ApplyBlock(b); err == nil {
		t.Fatal("block without a commit was accepted")
	}
}

func TestRejectsBlockWithBrokenTransactionRoot(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := n.Chain().BuildBlock(1, time.Now().UnixMilli(), net.Nodes[0].Address, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Header.TxRoot[0] ^= 1
	if _, err := n.Chain().ApplyBlock(b); err == nil {
		t.Fatal("block with broken tx root was accepted")
	}
}

func TestBuildProposalRejectsFutureHeight(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.Chain().BuildBlock(5, time.Now().UnixMilli(), net.Nodes[0].Address, nil, nil); err == nil {
		t.Fatal("built a block at a non-sequential height")
	}
}

func TestMempoolRespectsNonceSequence(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	st := n.Chain().State()
	k, _ := zcrypto.GenerateKey()
	// Fund the account directly through state to keep this test focused on the pool.
	acc, _ := st.GetAccount(k.Address())
	acc.Balance = 100 * ztypes.CoinScale
	st.SetAccount(k.Address(), acc)

	to := net.Nodes[0].Address
	var txs []*ztypes.Tx
	for nonce := uint64(0); nonce < 3; nonce++ {
		tx := &ztypes.Tx{
			ChainID: net.ChainID, Type: ztypes.TxTransfer, Sender: k.Address(),
			Nonce: nonce, GasLimit: 100_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
			Payload: (&ztypes.TransferPayload{To: to, Amount: 1}).Encode(),
		}
		if err := tx.Sign(k); err != nil {
			t.Fatal(err)
		}
		if err := n.Pool().Add(st, tx); err != nil {
			t.Fatalf("pool add nonce %d: %v", nonce, err)
		}
		txs = append(txs, tx)
	}
	selected := n.Pool().Select(st, 10)
	if len(selected) != 3 {
		t.Fatalf("selected %d transactions", len(selected))
	}
	for i, tx := range selected {
		if tx.Nonce != uint64(i) {
			t.Fatalf("selection order broken at %d: nonce %d", i, tx.Nonce)
		}
	}
	// Duplicate must be rejected.
	if err := n.Pool().Add(st, txs[0]); err == nil {
		t.Fatal("duplicate accepted")
	}
	// Consumed nonce must be dropped at selection time.
	acc, _ = st.GetAccount(k.Address())
	acc.Nonce = 1
	st.SetAccount(k.Address(), acc)
	selected = n.Pool().Select(st, 10)
	if len(selected) != 2 || selected[0].Nonce != 1 {
		t.Fatalf("after nonce bump selection = %v", nonces(selected))
	}
}

func nonces(txs []*ztypes.Tx) []uint64 {
	out := make([]uint64, len(txs))
	for i, tx := range txs {
		out[i] = tx.Nonce
	}
	return out
}

func TestConsensusMessagesAreDeliveredThroughNode(t *testing.T) {
	dir := t.TempDir()
	cfg, net := devnetCfg(t, dir, 0)
	cfg.Genesis = minimalGenesis(t, net, 0)
	n, err := NewNode(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	var votes, proposals int
	n.OnVote = func(v zconsensus.Vote) { votes++ }
	n.OnProposal = func(p *zconsensus.Proposal) { proposals++ }
	n.Start()
	defer n.Stop()
	waitHeight(t, n, 2, 10*time.Second)
	if proposals == 0 || votes == 0 {
		t.Fatalf("no consensus messages emitted (proposals=%d votes=%d)", proposals, votes)
	}
}
