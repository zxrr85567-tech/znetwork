package zrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/zexec"
	"znetwork/internal/zmempool"
	"znetwork/internal/zmetrics"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
	"znetwork/internal/zvm"
)

// rpcChain is a minimal Backend used to test the RPC layer in isolation from the
// node driver. It applies real transactions through the real engine.
type rpcChain struct {
	chainID  string
	eng      *zexec.Engine
	st       *zstate.State
	blocks   []*ztypes.Block
	receipts []*ztypes.Receipt
	net      *zdevnet.Network
}

func newRPCChain(t *testing.T) *rpcChain {
	t.Helper()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	eng := zexec.NewEngine(net.ChainID)
	st, err := eng.InitChain(net.Genesis)
	if err != nil {
		t.Fatal(err)
	}
	gb := &ztypes.Block{}
	gb.Header.Version = ztypes.ProtocolVersion
	gb.Header.ChainID = net.ChainID
	gb.Header.Height = 0
	gb.Header.TimeMs = net.Genesis.TimeMs
	gb.Header.StateRoot = st.Root()
	st.SetHeight(0)
	st.SetBlockHash(gb.Header.Hash())
	return &rpcChain{chainID: net.ChainID, eng: eng, st: st, net: net, blocks: []*ztypes.Block{gb}}
}

func (c *rpcChain) advance(txsPerBlock int) {
	p, _ := c.st.Params()
	h := c.st.Height() + 1
	ctx := &zexec.BlockContext{Height: h, TimeMs: c.net.Genesis.TimeMs + int64(h)*1000,
		Proposer: c.net.Nodes[0].Address, Params: p}
	_ = c.eng.BeginBlock(ctx, c.st)
	var included []*ztypes.Tx
	for i := 0; i < txsPerBlock; i++ {
		kp := c.net.Users[i%len(c.net.Users)].Key
		acc, _ := c.st.GetAccount(kp.Address())
		tx := &ztypes.Tx{ChainID: c.chainID, Type: ztypes.TxTransfer, Sender: kp.Address(),
			Nonce: acc.Nonce, GasLimit: 200_000, GasPrice: 1, TimeMs: ctx.TimeMs,
			Payload: (&ztypes.TransferPayload{To: c.net.Nodes[0].Address, Amount: ztypes.CoinScale}).Encode()}
		_ = tx.Sign(kp)
		if rc := c.eng.ExecuteTx(ctx, c.st, tx); rc.Status == ztypes.ReceiptSuccess {
			included = append(included, tx)
			rc.Height, rc.Index = h, uint32(len(c.receipts))
			c.receipts = append(c.receipts, rc)
		}
	}
	_, _ = c.eng.EndBlock(ctx, c.st)
	prev := c.blocks[len(c.blocks)-1]
	blk := &ztypes.Block{}
	blk.Header.Version = ztypes.ProtocolVersion
	blk.Header.ChainID = c.chainID
	blk.Header.Height = h
	blk.Header.TimeMs = ctx.TimeMs
	blk.Header.PrevHash = prev.Header.Hash()
	blk.Header.Proposer = c.net.Nodes[0].Address
	blk.Header.TxCount = uint32(len(included))
	blk.Header.StateRoot = c.st.Root()
	blk.Txs = included
	c.st.SetHeight(h)
	c.st.SetBlockHash(blk.Header.Hash())
	c.blocks = append(c.blocks, blk)
}

func (c *rpcChain) Height() uint64 { return uint64(len(c.blocks)) - 1 }
func (c *rpcChain) Block(h uint64) (*ztypes.Block, bool) {
	if h >= uint64(len(c.blocks)) {
		return nil, false
	}
	return c.blocks[h], true
}
func (c *rpcChain) BlockByHash(h zcrypto.Hash) (*ztypes.Block, bool) {
	for _, b := range c.blocks {
		if b.Header.Hash() == h {
			return b, true
		}
	}
	return nil, false
}
func (c *rpcChain) LastBlock() *ztypes.Block { return c.blocks[len(c.blocks)-1] }

// Receipts implements ReceiptBackend.
func (c *rpcChain) Receipts(height uint64) ([]*ztypes.Receipt, bool) {
	var out []*ztypes.Receipt
	for _, r := range c.receipts {
		if r.Height == height {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
func (c *rpcChain) State() *zstate.State { return c.st }
func (c *rpcChain) TxHeight(id zcrypto.Hash) (uint64, bool) {
	for _, b := range c.blocks {
		for _, tx := range b.Txs {
			if tx.ID() == id {
				return b.Header.Height, true
			}
		}
	}
	return 0, false
}

type env struct {
	srv     *httptest.Server
	chain   *rpcChain
	pool    *zmempool.Pool
	service *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	chain := newRPCChain(t)
	chain.advance(3)
	chain.advance(2)
	pool := zmempool.New(chain.chainID)
	svc := NewService(chain.chainID, chain, pool, func(tx *ztypes.Tx) (ztypes.Hash, error) {
		if err := pool.Add(chain.st, tx); err != nil {
			return ztypes.Hash{}, err
		}
		return tx.ID(), nil
	}, func() int { return 3 })
	mux := http.NewServeMux()
	mux.Handle("/", svc)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{srv: srv, chain: chain, pool: pool, service: svc}
}

// call performs a JSON-RPC request and returns the raw result/error.
func (e *env) call(t *testing.T, method string, params any) (json.RawMessage, *RPCError) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode response %s: %v", data, err)
	}
	return out.Result, out.Error
}

func (e *env) mustCall(t *testing.T, method string, params any, dst any) {
	t.Helper()
	res, rerr := e.call(t, method, params)
	if rerr != nil {
		t.Fatalf("%s: %+v", method, rerr)
	}
	if dst != nil {
		if err := json.Unmarshal(res, dst); err != nil {
			t.Fatalf("%s result %s: %v", method, res, err)
		}
	}
}

func TestChainInfoAndNodeInfo(t *testing.T) {
	e := newEnv(t)
	var info map[string]any
	e.mustCall(t, "z_chainInfo", nil, &info)
	if info["chain_id"] != e.chain.chainID {
		t.Fatalf("chain id = %v", info["chain_id"])
	}
	if info["height"].(float64) != 2 {
		t.Fatalf("height = %v", info["height"])
	}
	if info["genesis_hash"] == "" {
		t.Fatal("missing genesis hash")
	}
	var node map[string]any
	e.mustCall(t, "z_nodeInfo", nil, &node)
	if node["peer_count"].(float64) != 3 {
		t.Fatalf("peer count = %v", node["peer_count"])
	}
	if node["version"] != Version {
		t.Fatalf("version = %v", node["version"])
	}
}

func TestBlockQueries(t *testing.T) {
	e := newEnv(t)
	var blk blockDTO
	e.mustCall(t, "z_getBlock", map[string]any{"height": 1}, &blk)
	if blk.Height != 1 || blk.Hash == "" || len(blk.TxIDs) == 0 {
		t.Fatalf("block = %+v", blk)
	}
	var byHash blockDTO
	e.mustCall(t, "z_getBlockByHash", map[string]any{"hash": blk.Hash}, &byHash)
	if byHash.Hash != blk.Hash {
		t.Fatal("block by hash mismatch")
	}
	var list []blockDTO
	e.mustCall(t, "z_getBlocks", map[string]any{"from": 0, "limit": 10}, &list)
	if len(list) != 3 {
		t.Fatalf("blocks = %d", len(list))
	}
	// Unknown height -> not found.
	if _, rerr := e.call(t, "z_getBlock", map[string]any{"height": 99}); rerr == nil || rerr.Code != CodeNotFound {
		t.Fatalf("expected not found, got %+v", rerr)
	}
}

func TestAccountQueries(t *testing.T) {
	e := newEnv(t)
	addr := e.chain.net.Users[0].Address.Bech32()
	var acc map[string]any
	e.mustCall(t, "z_getAccount", map[string]any{"address": addr}, &acc)
	if acc["address"] != addr {
		t.Fatalf("address = %v", acc["address"])
	}
	if acc["balance_uz"].(float64) <= 0 {
		t.Fatal("balance not reported")
	}
	var bal map[string]any
	e.mustCall(t, "z_getBalance", map[string]any{"address": addr}, &bal)
	if bal["balance_z"] == "" {
		t.Fatal("formatted balance missing")
	}
	// The test chain includes user-0 transactions, so the committed nonce is > 0;
	// the pending nonce must equal it when the pool is empty.
	var nonce map[string]any
	e.mustCall(t, "z_pendingNonce", map[string]any{"address": addr}, &nonce)
	if nonce["nonce"].(float64) != acc["nonce"].(float64) {
		t.Fatalf("pending nonce = %v, committed = %v", nonce["nonce"], acc["nonce"])
	}
	// Invalid address must be rejected as invalid params, not internal error.
	if _, rerr := e.call(t, "z_getAccount", map[string]any{"address": "not-an-address"}); rerr == nil || rerr.Code != CodeInvalidParams {
		t.Fatalf("expected invalid params, got %+v", rerr)
	}
	// Unknown field must be rejected (strict decoding).
	if _, rerr := e.call(t, "z_getAccount", map[string]any{"addr": addr}); rerr == nil {
		t.Fatal("unknown parameter field accepted")
	}
}

func TestSupplyIsBalanced(t *testing.T) {
	e := newEnv(t)
	var supplies map[string]any
	e.mustCall(t, "z_getSupply", nil, &supplies)
	if supplies["balanced"] != true {
		t.Fatalf("supply not balanced: %+v", supplies)
	}
	supply := supplies["supply_uz"].(float64)
	liquid := supplies["liquid_uz"].(float64)
	bonded := supplies["bonded_uz"].(float64)
	unbonding := supplies["unbonding_uz"].(float64)
	if supply != liquid+bonded+unbonding {
		t.Fatalf("accounting mismatch: %v != %v+%v+%v", supply, liquid, bonded, unbonding)
	}
}

func TestValidatorAndParamsQueries(t *testing.T) {
	e := newEnv(t)
	var vals map[string]any
	e.mustCall(t, "z_getValidators", nil, &vals)
	if vals["count"].(float64) != 4 {
		t.Fatalf("validator count = %v", vals["count"])
	}
	op := e.chain.net.Nodes[0].Address.Bech32()
	var one map[string]any
	e.mustCall(t, "z_getValidator", map[string]any{"operator": op}, &one)
	if one["status"] != "bonded" {
		t.Fatalf("status = %v", one["status"])
	}
	var params map[string]any
	e.mustCall(t, "z_getParams", nil, &params)
	if params["note"] != ztypes.EconomicsNote {
		t.Fatalf("economics note missing: %v", params["note"])
	}
}

func TestTransactionSubmission(t *testing.T) {
	e := newEnv(t)
	user := e.chain.net.Users[0]
	acc, _ := e.chain.st.GetAccount(user.Address)
	tx := &ztypes.Tx{ChainID: e.chain.chainID, Type: ztypes.TxTransfer, Sender: user.Address,
		Nonce: acc.Nonce, GasLimit: 200_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
		Payload: (&ztypes.TransferPayload{To: e.chain.net.Nodes[1].Address, Amount: ztypes.CoinScale}).Encode()}
	if err := tx.Sign(user.Key); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	e.mustCall(t, "z_sendRawTransaction", map[string]any{"tx": hexEncode(tx.Encode())}, &res)
	if res["status"] != "accepted" {
		t.Fatalf("submit = %+v", res)
	}
	// It must be visible in the mempool and as pending by id.
	var mem map[string]any
	e.mustCall(t, "z_getMempool", nil, &mem)
	if mem["size"].(float64) != 1 {
		t.Fatalf("mempool size = %v", mem["size"])
	}
	var pending map[string]any
	e.mustCall(t, "z_getTransaction", map[string]any{"id": tx.ID().String()}, &pending)
	if pending["status"] != "pending" {
		t.Fatalf("status = %v", pending["status"])
	}
	// A transaction from an unfunded account must be rejected.
	stranger, _ := zcrypto.GenerateKey()
	bad := &ztypes.Tx{ChainID: e.chain.chainID, Type: ztypes.TxTransfer, Sender: stranger.Address(),
		GasLimit: 200_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
		Payload: (&ztypes.TransferPayload{To: e.chain.net.Nodes[1].Address, Amount: 1}).Encode()}
	_ = bad.Sign(stranger)
	if _, rerr := e.call(t, "z_sendRawTransaction", map[string]any{"tx": hexEncode(bad.Encode())}); rerr == nil || rerr.Code != CodeTxRejected {
		t.Fatalf("expected tx rejected, got %+v", rerr)
	}
	// Garbage hex must be invalid params.
	if _, rerr := e.call(t, "z_sendRawTransaction", map[string]any{"tx": "zzzz"}); rerr == nil {
		t.Fatal("garbage hex accepted")
	}
}

func TestCommittedTransactionLookup(t *testing.T) {
	e := newEnv(t)
	blk, _ := e.chain.Block(1)
	if len(blk.Txs) == 0 {
		t.Fatal("no transactions in block 1")
	}
	var res map[string]any
	e.mustCall(t, "z_getTransaction", map[string]any{"id": blk.Txs[0].ID().String()}, &res)
	if res["status"] != "committed" || res["height"].(float64) != 1 {
		t.Fatalf("lookup = %+v", res)
	}
}

func TestContractQueries(t *testing.T) {
	e := newEnv(t)
	user := e.chain.net.Users[0]
	asm, err := zvm.Assemble("PUSH 7\nPUSH 0\nSSTORE")
	if err != nil {
		t.Fatal(err)
	}
	// Deploy through the engine directly so the state contains a real contract.
	p, _ := e.chain.st.Params()
	h := e.chain.st.Height() + 1
	ctx := &zexec.BlockContext{Height: h, TimeMs: time.Now().UnixMilli(), Proposer: e.chain.net.Nodes[0].Address, Params: p}
	acc, _ := e.chain.st.GetAccount(user.Address)
	tx := &ztypes.Tx{ChainID: e.chain.chainID, Type: ztypes.TxContractDeploy, Sender: user.Address,
		Nonce: acc.Nonce, GasLimit: 2_000_000, GasPrice: 1, TimeMs: ctx.TimeMs,
		Payload: (&ztypes.ContractDeployPayload{Code: asm.Code}).Encode()}
	_ = tx.Sign(user.Key)
	rc := e.chain.eng.ExecuteTx(ctx, e.chain.st, tx)
	if rc.Status != ztypes.ReceiptSuccess {
		t.Fatalf("deploy failed: %s", rc.Error)
	}
	addr := rc.ContractAddress.Bech32()
	var code map[string]any
	e.mustCall(t, "z_getContractCode", map[string]any{"address": addr}, &code)
	if code["size"].(float64) == 0 {
		t.Fatal("no code returned")
	}
	var slot map[string]any
	e.mustCall(t, "z_getContractStorage", map[string]any{"address": addr, "slot": 0}, &slot)
	if slot["value"].(float64) != 7 {
		t.Fatalf("storage slot = %v", slot["value"])
	}
	// A non-contract account must be reported as not found.
	if _, rerr := e.call(t, "z_getContractCode", map[string]any{"address": user.Address.Bech32()}); rerr == nil || rerr.Code != CodeNotFound {
		t.Fatalf("expected not found, got %+v", rerr)
	}
}

func TestUnknownMethodAndBadRequests(t *testing.T) {
	e := newEnv(t)
	if _, rerr := e.call(t, "z_doesNotExist", nil); rerr == nil || rerr.Code != CodeMethodNotFound {
		t.Fatalf("expected method not found, got %+v", rerr)
	}
	// Wrong jsonrpc version.
	raw := []byte(`{"jsonrpc":"1.0","id":1,"method":"z_getHeight"}`)
	resp, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error *RPCError `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &out)
	if out.Error == nil || out.Error.Code != CodeInvalidRequest {
		t.Fatalf("expected invalid request, got %s", body)
	}
	// Malformed JSON.
	resp2, err := http.Post(e.srv.URL, "application/json", strings.NewReader("{oops"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(body2, &out)
	if out.Error == nil || out.Error.Code != CodeParseError {
		t.Fatalf("expected parse error, got %s", body2)
	}
	// GET must be rejected.
	resp3, err := http.Get(e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", resp3.StatusCode)
	}
}

func TestBatchRequests(t *testing.T) {
	e := newEnv(t)
	batch := []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "z_getHeight"},
		{"jsonrpc": "2.0", "id": 2, "method": "z_chainInfo"},
		{"jsonrpc": "2.0", "id": 3, "method": "nope"},
	}
	raw, _ := json.Marshal(batch)
	resp, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("batch response %s: %v", body, err)
	}
	if len(out) != 3 {
		t.Fatalf("batch responses = %d", len(out))
	}
	if out[0].Error != nil || out[1].Error != nil {
		t.Fatal("valid batch entries failed")
	}
	if out[2].Error == nil || out[2].Error.Code != CodeMethodNotFound {
		t.Fatal("bad batch entry was not reported")
	}
	// Oversized batch is rejected.
	many := make([]map[string]any, MaxBatch+1)
	for i := range many {
		many[i] = map[string]any{"jsonrpc": "2.0", "id": i, "method": "z_getHeight"}
	}
	raw, _ = json.Marshal(many)
	resp2, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var single struct {
		Error *RPCError `json:"error"`
	}
	b2, _ := io.ReadAll(resp2.Body)
	_ = json.Unmarshal(b2, &single)
	if single.Error == nil || single.Error.Code != CodeInvalidRequest {
		t.Fatalf("oversized batch accepted: %s", b2)
	}
}

func TestAuthTokenProtectsWrites(t *testing.T) {
	e := newEnv(t)
	e.service.SetAuthToken("s3cret")
	user := e.chain.net.Users[0]
	acc, _ := e.chain.st.GetAccount(user.Address)
	tx := &ztypes.Tx{ChainID: e.chain.chainID, Type: ztypes.TxTransfer, Sender: user.Address,
		Nonce: acc.Nonce, GasLimit: 200_000, GasPrice: 1, TimeMs: time.Now().UnixMilli(),
		Payload: (&ztypes.TransferPayload{To: e.chain.net.Nodes[1].Address, Amount: 1}).Encode()}
	_ = tx.Sign(user.Key)
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "z_sendRawTransaction",
		"params": map[string]any{"tx": hexEncode(tx.Encode())}}
	raw, _ := json.Marshal(body)
	// Without a token.
	resp, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Error *RPCError `json:"error"`
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(b, &out)
	if out.Error == nil || out.Error.Code != CodeUnauthorized {
		t.Fatalf("unauthenticated write accepted: %s", b)
	}
	// With the token.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	out.Error = nil
	_ = json.Unmarshal(b2, &out)
	if out.Error != nil {
		t.Fatalf("authenticated write failed: %s", b2)
	}
	// Reads remain public.
	if _, rerr := e.call(t, "z_getHeight", nil); rerr != nil {
		t.Fatal("read method required auth")
	}
}

func TestOversizedRequestRejected(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("a", MaxBodyBytes+10)
	resp, err := http.Post(e.srv.URL, "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestMetricsMethod(t *testing.T) {
	e := newEnv(t)
	// Generate one successful and one failing call. (Unknown methods are rejected
	// before dispatch and are therefore not counted as RPC errors.)
	e.call(t, "z_getHeight", nil)
	e.call(t, "z_getAccount", map[string]any{"address": "invalid"})
	var m map[string]any
	e.mustCall(t, "z_metrics", nil, &m)
	if m["rpc_requests"].(float64) < 2 {
		t.Fatalf("requests = %v", m["rpc_requests"])
	}
	if m["rpc_errors"].(float64) < 1 {
		t.Fatalf("errors = %v", m["rpc_errors"])
	}
	if m["height"].(float64) != 2 {
		t.Fatalf("height = %v", m["height"])
	}
}

func TestGovernanceAndDelegationQueries(t *testing.T) {
	e := newEnv(t)
	var dels map[string]any
	e.mustCall(t, "z_getDelegations", nil, &dels)
	if dels["count"].(float64) != 4 { // one self-delegation per genesis validator
		t.Fatalf("delegations = %v", dels["count"])
	}
	var props map[string]any
	e.mustCall(t, "z_getProposals", nil, &props)
	if props["count"].(float64) != 0 {
		t.Fatalf("proposals = %v", props["count"])
	}
}

func TestHealthEndpoint(t *testing.T) {
	chain := newRPCChain(t)
	pool := zmempool.New(chain.chainID)
	svc := NewService(chain.chainID, chain, pool, func(tx *ztypes.Tx) (ztypes.Hash, error) {
		return tx.ID(), nil
	}, nil)
	srv, addr, err := svc.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	base := "http://" + addr.String()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"status":"ok"`) {
		t.Fatalf("health = %s", b)
	}
	// And a real RPC call over the network.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"z_getHeight"}`)
	resp2, err := http.Post(base, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(b2), `"height":0`) {
		t.Fatalf("rpc over network = %s", b2)
	}
	_ = filepath.Join
}

// ---- metrics endpoint -------------------------------------------------------

func TestMetricsEndpointServesPrometheus(t *testing.T) {
	e := newEnv(t)
	svc := e.service
	reg := zmetrics.New()
	reg.SetLabel("chain_id", "znetwork-test")
	reg.IncTxAccepted()
	svc.SetMetrics(reg)

	mux := http.NewServeMux()
	mux.Handle("/", svc)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(reg.WritePrometheus())
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "znet_tx_accepted_total{chain_id=\"znetwork-test\"} 1") {
		t.Fatalf("exposition:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestMetricsMethodIncludesRegistry(t *testing.T) {
	e := newEnv(t)
	reg := zmetrics.New()
	reg.IncVoteCast()
	e.service.SetMetrics(reg)
	res, rerr := e.call(t, "z_metrics", nil)
	if rerr != nil {
		t.Fatalf("z_metrics: %+v", rerr)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(res, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var counters zmetrics.Counters
	if err := json.Unmarshal(decoded["registry"], &counters); err != nil {
		t.Fatalf("registry field: %v (%s)", err, decoded["registry"])
	}
	if counters.VotesCast != 1 {
		t.Fatalf("votes cast = %d", counters.VotesCast)
	}
}

// ---- explorer ---------------------------------------------------------------

func TestExplorerServesUIAndSchema(t *testing.T) {
	svc := NewService("znetwork-test", &rpcChain{}, nil, nil, nil)
	h := svc.Explorer()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Z NETWORK", "DEVELOPMENT / TESTNET", "z_chainInfo", "z_getBlocks"} {
		if !strings.Contains(body, want) {
			t.Fatalf("explorer HTML missing %q", want)
		}
	}
	// No external resources: the UI must work on an air-gapped node.
	for _, banned := range []string{"https://", "http://", "//cdn", "<link"} {
		if strings.Contains(body, banned) {
			t.Fatalf("explorer references an external resource (%q)", banned)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explorer.sql", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "z_getTransaction") {
		t.Fatalf("schema endpoint: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", rec.Code)
	}
}

// ---- receipts ---------------------------------------------------------------

func TestGetTransactionIncludesReceipt(t *testing.T) {
	e := newEnv(t) // rpcChain implements ReceiptBackend
	// Use a transaction that is definitely committed: the tests advance blocks
	// with transactions, so read one back from the head block.
	blk := e.chain.LastBlock()
	if len(blk.Txs) == 0 {
		t.Fatal("test chain has no transactions to inspect")
	}
	id := blk.Txs[0].ID().String()

	var dto struct {
		ID      string         `json:"id"`
		Height  uint64         `json:"height"`
		Status  string         `json:"status"`
		Receipt map[string]any `json:"receipt"`
	}
	e.mustCall(t, "z_getTransaction", map[string]any{"id": id}, &dto)
	if dto.Receipt == nil {
		t.Fatalf("receipt missing for committed tx %s (height %d)", id, dto.Height)
	}
	for _, k := range []string{"gas_used", "fee_uz", "status", "events", "sender"} {
		if _, ok := dto.Receipt[k]; !ok {
			t.Fatalf("receipt missing field %q: %v", k, dto.Receipt)
		}
	}

	var rec map[string]any
	e.mustCall(t, "z_getReceipt", map[string]any{"id": id}, &rec)
	if rec["tx_id"] != id {
		t.Fatalf("z_getReceipt returned %v for %s", rec["tx_id"], id)
	}
	// Unknown ids are a not-found error, not an internal error.
	if _, rerr := e.call(t, "z_getReceipt", map[string]any{"id": strings.Repeat("00", 32)}); rerr == nil {
		t.Fatal("unknown receipt id accepted")
	} else if rerr.Code != -32001 {
		t.Fatalf("error code = %d, want -32001", rerr.Code)
	}
}
