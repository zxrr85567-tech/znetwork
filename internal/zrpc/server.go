// Package zrpc implements Z Network's JSON-RPC 2.0 API (AGENT_18).
//
// Design rules:
//
//   - Exactly one method table. Every method validates its parameters and returns
//     a typed error code; nothing reaches the chain without validation.
//   - Read methods never mutate state. Write methods only submit signed
//     transactions: the RPC server holds no keys and can never sign for a user.
//   - Requests are size-limited (MaxBodyBytes) and batched requests are bounded
//     (MaxBatch), so an unauthenticated client cannot force unbounded work.
//   - Errors use stable codes documented in API_STANDARDS.md.
//
// Transport: HTTP POST with a JSON-RPC 2.0 body. WebSocket subscriptions are not
// implemented in protocol version 1 (documented limitation L-007); clients poll
// blocks instead.
package zrpc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zexec"
	"znetwork/internal/zmempool"
	"znetwork/internal/zmetrics"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// Limits and error codes.
const (
	MaxBodyBytes = 1 << 20 // 1 MiB
	MaxBatch     = 20
	MaxResults   = 1000

	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// Server-defined range.
	CodeTxRejected   = -32000
	CodeNotFound     = -32001
	CodeUnavailable  = -32002
	CodeUnauthorized = -32003
)

// Version is the RPC API version reported by z_nodeInfo.
const Version = "1.0.0"

// Backend is the chain surface the RPC uses. It is satisfied by *znode.Chain; the
// interface is declared here so the dependency direction stays one-way (RPC does
// not import the node implementation) and so the API can be tested standalone.
type Backend interface {
	Height() uint64
	Block(height uint64) (*ztypes.Block, bool)
	BlockByHash(h zcrypto.Hash) (*ztypes.Block, bool)
	LastBlock() *ztypes.Block
	State() *zstate.State
	TxHeight(id zcrypto.Hash) (uint64, bool)
}

// ReceiptBackend is an optional capability: backends that retain execution
// receipts expose them so z_getTransaction can return execution status and
// z_getReceipt can serve them directly. Without it the methods degrade to
// "receipt not available" rather than failing.
type ReceiptBackend interface {
	Receipts(height uint64) ([]*ztypes.Receipt, bool)
}

// Service is the RPC service.
type Service struct {
	chainID string
	backend Backend
	pool    *zmempool.Pool
	submit  func(tx *ztypes.Tx) (ztypes.Hash, error)
	peerCnt func() int

	started time.Time
	reg     *zmetrics.Registry
	mu      sync.Mutex
	methods map[string]method
	auth    string // optional bearer token; empty disables auth
	metrics MetricsSnapshot
}

// MetricsSnapshot is a point-in-time view of node counters.
type MetricsSnapshot struct {
	RPCRequests   uint64 `json:"rpc_requests"`
	RPCErrors     uint64 `json:"rpc_errors"`
	TxAccepted    uint64 `json:"tx_accepted"`
	TxRejected    uint64 `json:"tx_rejected"`
	BlocksApplied uint64 `json:"blocks_applied"`
}

type method struct {
	handler func(ctx context.Context, params json.RawMessage) (any, *RPCError)
	// write marks methods that submit transactions and therefore may require a
	// bearer token when authentication is enabled.
	write bool
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%d: %s", e.Code, e.Message) }

func errParams(format string, a ...any) *RPCError {
	return &RPCError{Code: CodeInvalidParams, Message: "invalid params", Data: fmt.Sprintf(format, a...)}
}

func errInternal(err error) *RPCError {
	return &RPCError{Code: CodeInternalError, Message: "internal error", Data: err.Error()}
}

func errNotFound(what string) *RPCError {
	return &RPCError{Code: CodeNotFound, Message: "not found", Data: what}
}

// NewService creates the RPC service. submit is the node's transaction entry
// point; peerCnt reports the current peer count.
func NewService(chainID string, backend Backend, pool *zmempool.Pool,
	submit func(tx *ztypes.Tx) (ztypes.Hash, error), peerCnt func() int) *Service {
	s := &Service{
		chainID: chainID,
		backend: backend,
		pool:    pool,
		submit:  submit,
		peerCnt: peerCnt,
		started: time.Now(),
		methods: map[string]method{},
	}
	if peerCnt == nil {
		s.peerCnt = func() int { return 0 }
	}
	for name, h := range map[string]func(context.Context, json.RawMessage) (any, *RPCError){
		"z_chainInfo":          s.chainInfo,
		"z_nodeInfo":           s.nodeInfo,
		"z_getHeight":          s.getHeight,
		"z_getBlock":           s.getBlock,
		"z_getBlockByHash":     s.getBlockByHash,
		"z_getBlocks":          s.getBlocks,
		"z_getAccount":         s.getAccount,
		"z_getBalance":         s.getBalance,
		"z_getNonce":           s.getNonce,
		"z_getSupply":          s.getSupply,
		"z_getValidators":      s.getValidators,
		"z_getValidator":       s.getValidator,
		"z_getDelegations":     s.getDelegations,
		"z_getProposals":       s.getProposals,
		"z_getProposal":        s.getProposal,
		"z_getParams":          s.getParams,
		"z_getTransaction":     s.getTransaction,
		"z_getReceipt":         s.getReceipt,
		"z_getMempool":         s.getMempool,
		"z_getContractStorage": s.getContractStorage,
		"z_getContractCode":    s.getContractCode,
		"z_pendingNonce":       s.pendingNonce,
		"z_metrics":            s.metricsMethod,
	} {
		s.methods[name] = method{handler: h}
	}
	s.methods["z_sendRawTransaction"] = method{handler: s.sendRawTransaction, write: true}
	return s
}

// SetMetrics attaches a registry; when set, /metrics serves the Prometheus text
// exposition format and z_metrics includes the registry counters.
func (s *Service) SetMetrics(r *zmetrics.Registry) { s.reg = r }

// Registry returns the attached metrics registry (may be nil).
func (s *Service) Registry() *zmetrics.Registry { return s.reg }

// SetAuthToken enables bearer-token authentication for write methods.
func (s *Service) SetAuthToken(token string) { s.auth = token }

// ---- request/response plumbing ---------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ServeHTTP implements the JSON-RPC transport.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.metrics.RPCRequests++
	s.mu.Unlock()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &RPCError{Code: CodeParseError, Message: "read error"}})
		return
	}
	if len(body) > MaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &RPCError{Code: CodeInvalidRequest, Message: "request too large"}})
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var batch []request
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSON(w, http.StatusOK, response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &RPCError{Code: CodeParseError, Message: "parse error"}})
			return
		}
		if len(batch) == 0 || len(batch) > MaxBatch {
			writeJSON(w, http.StatusOK, response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &RPCError{Code: CodeInvalidRequest, Message: "bad batch size"}})
			return
		}
		out := make([]response, 0, len(batch))
		for _, req := range batch {
			out = append(out, s.handle(r, &req))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &RPCError{Code: CodeParseError, Message: "parse error"}})
		return
	}
	writeJSON(w, http.StatusOK, s.handle(r, &req))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Service) handle(httpReq *http.Request, req *request) response {
	id := req.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := response{JSONRPC: "2.0", ID: id}
	if req.JSONRPC != "2.0" {
		resp.Error = &RPCError{Code: CodeInvalidRequest, Message: "jsonrpc must be \"2.0\""}
		return resp
	}
	m, ok := s.methods[req.Method]
	if !ok {
		resp.Error = &RPCError{Code: CodeMethodNotFound, Message: "method not found", Data: req.Method}
		return resp
	}
	if m.write && s.auth != "" {
		auth := httpReq.Header.Get("Authorization")
		want := "Bearer " + s.auth
		if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
			resp.Error = &RPCError{Code: CodeUnauthorized, Message: "unauthorized"}
			return resp
		}
	}
	result, rerr := m.handler(httpReq.Context(), req.Params)
	if rerr != nil {
		s.mu.Lock()
		s.metrics.RPCErrors++
		s.mu.Unlock()
		resp.Error = rerr
		return resp
	}
	resp.Result = result
	return resp
}

// ---- helpers ---------------------------------------------------------------

func decodeParams(raw json.RawMessage, dst any) *RPCError {
	if len(raw) == 0 || string(raw) == "null" {
		return &RPCError{Code: CodeInvalidParams, Message: "params required"}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errParams("%v", err)
	}
	return nil
}

func parseAddress(s string) (ztypes.Address, *RPCError) {
	a, err := zcrypto.ParseAddress(s)
	if err != nil {
		return ztypes.Address{}, errParams("address %q: %v", s, err)
	}
	return a, nil
}

func parseHash(s string) (zcrypto.Hash, *RPCError) {
	h, err := zcrypto.ParseHash(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return zcrypto.Hash{}, errParams("hash %q: %v", s, err)
	}
	return h, nil
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0xf]
	}
	return string(out)
}

// ---- chain info ------------------------------------------------------------

func (s *Service) chainInfo(_ context.Context, params json.RawMessage) (any, *RPCError) {
	if len(params) != 0 && string(params) != "null" && string(params) != "[]" && string(params) != "{}" {
		return nil, errParams("no parameters accepted")
	}
	head := s.backend.LastBlock()
	paramsState, err := s.backend.State().Params()
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{
		"chain_id":        s.chainID,
		"protocol":        ztypes.ProtocolVersion,
		"height":          s.backend.Height(),
		"head_hash":       head.Header.Hash().String(),
		"genesis_hash":    s.backend.State().GenesisHash().String(),
		"state_root":      s.backend.State().Root().String(),
		"supply_uz":       s.backend.State().Supply(),
		"validator_count": len(boundedValidators(s.backend)),
		"peer_count":      s.peerCnt(),
		"block_gas_limit": paramsState.BlockGasLimit,
		"base_tx_gas":     paramsState.BaseTxGas,
	}, nil
}

func boundedValidators(b Backend) []*ztypes.Validator {
	vals, err := b.State().Validators()
	if err != nil {
		return nil
	}
	if len(vals) > MaxResults {
		return vals[:MaxResults]
	}
	return vals
}

func (s *Service) nodeInfo(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	return map[string]any{
		"version":      Version,
		"protocol":     ztypes.ProtocolVersion,
		"chain_id":     s.chainID,
		"uptime_sec":   int64(time.Since(s.started).Seconds()),
		"peer_count":   s.peerCnt(),
		"height":       s.backend.Height(),
		"mempool_size": s.pool.Size(),
		"time_ms":      time.Now().UnixMilli(),
	}, nil
}

func (s *Service) getHeight(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	return map[string]any{"height": s.backend.Height()}, nil
}

// ---- blocks ----------------------------------------------------------------

type blockDTO struct {
	Height         uint64   `json:"height"`
	Hash           string   `json:"hash"`
	PrevHash       string   `json:"prev_hash"`
	TimeMs         int64    `json:"time_ms"`
	Proposer       string   `json:"proposer"`
	TxCount        uint32   `json:"tx_count"`
	GasUsed        uint64   `json:"gas_used"`
	GasLimit       uint64   `json:"gas_limit"`
	StateRoot      string   `json:"state_root"`
	TxRoot         string   `json:"tx_root"`
	ReceiptRoot    string   `json:"receipt_root"`
	ValidatorsHash string   `json:"validators_hash"`
	TxIDs          []string `json:"tx_ids"`
	CommitSigs     int      `json:"commit_signatures"`
}

func blockToDTO(b *ztypes.Block) blockDTO {
	dto := blockDTO{
		Height:         b.Header.Height,
		Hash:           b.Header.Hash().String(),
		PrevHash:       b.Header.PrevHash.String(),
		TimeMs:         b.Header.TimeMs,
		Proposer:       b.Header.Proposer.Bech32(),
		TxCount:        b.Header.TxCount,
		GasUsed:        b.Header.GasUsed,
		GasLimit:       b.Header.GasLimit,
		StateRoot:      b.Header.StateRoot.String(),
		TxRoot:         b.Header.TxRoot.String(),
		ReceiptRoot:    b.Header.ReceiptRoot.String(),
		ValidatorsHash: b.Header.ValidatorsHash.String(),
	}
	for _, tx := range b.Txs {
		dto.TxIDs = append(dto.TxIDs, tx.ID().String())
	}
	if b.Commit != nil {
		dto.CommitSigs = len(b.Commit.Sigs)
	}
	return dto
}

func (s *Service) getBlock(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Height *uint64 `json:"height"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	if p.Height == nil {
		return nil, errParams("height is required")
	}
	b, ok := s.backend.Block(*p.Height)
	if !ok {
		return nil, errNotFound(fmt.Sprintf("block %d", *p.Height))
	}
	return blockToDTO(b), nil
}

func (s *Service) getBlockByHash(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Hash string `json:"hash"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	h, e := parseHash(p.Hash)
	if e != nil {
		return nil, e
	}
	b, ok := s.backend.BlockByHash(h)
	if !ok {
		return nil, errNotFound("block " + p.Hash)
	}
	return blockToDTO(b), nil
}

func (s *Service) getBlocks(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		From  uint64 `json:"from"`
		Limit uint64 `json:"limit"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	if p.Limit == 0 || p.Limit > MaxResults {
		p.Limit = 50
	}
	head := s.backend.Height()
	out := []blockDTO{}
	for h := p.From; h <= head && uint64(len(out)) < p.Limit; h++ {
		if b, ok := s.backend.Block(h); ok {
			out = append(out, blockToDTO(b))
		}
	}
	return out, nil
}

// ---- accounts --------------------------------------------------------------

func accountDTO(a ztypes.Address, acc *ztypes.Account) map[string]any {
	return map[string]any{
		"address":        a.Bech32(),
		"balance_uz":     acc.Balance,
		"balance_z":      ztypes.FormatUZ(acc.Balance),
		"nonce":          acc.Nonce,
		"is_contract":    acc.IsContract(),
		"code_hash":      acc.CodeHash.String(),
		"created_height": acc.CreatedHeight,
	}
}

func (s *Service) getAccount(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Address string `json:"address"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Address)
	if e != nil {
		return nil, e
	}
	acc, err := s.backend.State().GetAccount(a)
	if err != nil {
		return nil, errInternal(err)
	}
	return accountDTO(a, acc), nil
}

func (s *Service) getBalance(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Address string `json:"address"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Address)
	if e != nil {
		return nil, e
	}
	acc, err := s.backend.State().GetAccount(a)
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"address": a.Bech32(), "balance_uz": acc.Balance, "balance_z": ztypes.FormatUZ(acc.Balance)}, nil
}

func (s *Service) getNonce(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Address string `json:"address"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Address)
	if e != nil {
		return nil, e
	}
	acc, err := s.backend.State().GetAccount(a)
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"address": a.Bech32(), "nonce": acc.Nonce}, nil
}

func (s *Service) pendingNonce(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Address string `json:"address"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Address)
	if e != nil {
		return nil, e
	}
	acc, err := s.backend.State().GetAccount(a)
	if err != nil {
		return nil, errInternal(err)
	}
	next := acc.Nonce
	for _, tx := range s.pool.All(s.backend.State()) {
		if tx.Sender == a && tx.Nonce >= next {
			next = tx.Nonce + 1
		}
	}
	return map[string]any{"address": a.Bech32(), "nonce": next}, nil
}

func (s *Service) getSupply(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	acct, err := zexec.Account(s.backend.State())
	if err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{
		"supply_uz":    acct.Supply,
		"liquid_uz":    acct.Liquid,
		"bonded_uz":    acct.Bonded,
		"unbonding_uz": acct.Unbonding,
		"balanced":     acct.Diff() == 0,
		"max_supply":   ztypes.MaxSupply,
		"supply_z":     ztypes.FormatUZ(acct.Supply),
	}, nil
}

// ---- staking ---------------------------------------------------------------

func validatorDTO(v *ztypes.Validator) map[string]any {
	return map[string]any{
		"operator":          v.Operator.Bech32(),
		"moniker":           v.Moniker,
		"pubkey":            v.PubKey.String(),
		"commission_bps":    v.CommissionBps,
		"self_bond_uz":      v.SelfBond,
		"delegated_uz":      v.Delegated,
		"voting_power_uz":   v.VotingPower(),
		"status":            v.Status.String(),
		"registered_height": v.RegisteredHeight,
	}
}

func (s *Service) getValidators(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	vals, err := s.backend.State().Validators()
	if err != nil {
		return nil, errInternal(err)
	}
	if len(vals) > MaxResults {
		vals = vals[:MaxResults]
	}
	out := make([]map[string]any, 0, len(vals))
	for _, v := range vals {
		out = append(out, validatorDTO(v))
	}
	return map[string]any{"validators": out, "count": len(out)}, nil
}

func (s *Service) getValidator(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Operator string `json:"operator"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Operator)
	if e != nil {
		return nil, e
	}
	v, err := s.backend.State().GetValidator(a)
	if err != nil {
		return nil, errInternal(err)
	}
	if v == nil {
		return nil, errNotFound("validator " + p.Operator)
	}
	return validatorDTO(v), nil
}

func (s *Service) getDelegations(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Delegator string `json:"delegator"`
		Validator string `json:"validator"`
	}
	if len(raw) != 0 && string(raw) != "null" {
		if e := decodeParams(raw, &p); e != nil {
			return nil, e
		}
	}
	if p.Delegator != "" && p.Validator != "" {
		d, e := parseAddress(p.Delegator)
		if e != nil {
			return nil, e
		}
		v, e := parseAddress(p.Validator)
		if e != nil {
			return nil, e
		}
		rec, err := s.backend.State().GetDelegation(d, v)
		if err != nil {
			return nil, errInternal(err)
		}
		if rec == nil {
			return nil, errNotFound("delegation")
		}
		return delegationDTO(rec), nil
	}
	var filter *ztypes.Address
	if p.Delegator != "" {
		d, e := parseAddress(p.Delegator)
		if e != nil {
			return nil, e
		}
		filter = &d
	}
	all, err := s.backend.State().Delegations()
	if err != nil {
		return nil, errInternal(err)
	}
	out := []map[string]any{}
	for _, rec := range all {
		if filter != nil && rec.Delegator != *filter {
			continue
		}
		if len(out) >= MaxResults {
			break
		}
		out = append(out, delegationDTO(rec))
	}
	return map[string]any{"delegations": out, "count": len(out)}, nil
}

func delegationDTO(d *ztypes.DelegationRecord) map[string]any {
	unbonding := make([]map[string]any, 0, len(d.Unbonding))
	var total uint64
	for _, u := range d.Unbonding {
		unbonding = append(unbonding, map[string]any{"amount_uz": u.Amount, "release_height": u.ReleaseHeight})
		total += u.Amount
	}
	return map[string]any{
		"delegator":     d.Delegator.Bech32(),
		"validator":     d.Validator.Bech32(),
		"amount_uz":     d.Amount,
		"unbonding":     unbonding,
		"unbonding_uz":  total,
		"total_with_uz": d.Amount + total,
	}
}

// ---- governance ------------------------------------------------------------

func proposalDTO(p *ztypes.Proposal) map[string]any {
	return map[string]any{
		"id":                p.ID,
		"proposer":          p.Proposer.Bech32(),
		"title":             p.Title,
		"description":       p.Description,
		"kind":              uint8(p.Kind),
		"param_key":         p.ParamKey,
		"param_value":       p.ParamValue,
		"deposit_uz":        p.Deposit,
		"submit_height":     p.SubmitHeight,
		"voting_end_height": p.VotingEndHeight,
		"status":            p.Status.String(),
		"yes_uz":            p.YesPower,
		"no_uz":             p.NoPower,
		"abstain_uz":        p.AbstainPower,
		"total_voted_uz":    p.TotalVoted(),
	}
}

func (s *Service) getProposals(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	props, err := s.backend.State().Proposals()
	if err != nil {
		return nil, errInternal(err)
	}
	if len(props) > MaxResults {
		props = props[:MaxResults]
	}
	out := make([]map[string]any, 0, len(props))
	for _, p := range props {
		out = append(out, proposalDTO(p))
	}
	return map[string]any{"proposals": out, "count": len(out)}, nil
}

func (s *Service) getProposal(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		ID uint64 `json:"id"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	prop, err := s.backend.State().GetProposal(p.ID)
	if err != nil {
		return nil, errInternal(err)
	}
	if prop == nil {
		return nil, errNotFound("proposal")
	}
	return proposalDTO(prop), nil
}

func (s *Service) getParams(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	params, err := s.backend.State().Params()
	if err != nil {
		return nil, errInternal(err)
	}
	out := map[string]uint64{}
	for _, k := range ztypes.ParamKeys() {
		v, _ := params.Get(k)
		out[k] = v
	}
	return map[string]any{"params": out, "note": ztypes.EconomicsNote}, nil
}

// ---- transactions ----------------------------------------------------------

func (s *Service) getTransaction(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		ID string `json:"id"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	h, e := parseHash(p.ID)
	if e != nil {
		return nil, e
	}
	height, ok := s.backend.TxHeight(h)
	if !ok {
		if s.pool.Has(h) {
			return map[string]any{"id": p.ID, "status": "pending", "height": nil}, nil
		}
		return nil, errNotFound("transaction " + p.ID)
	}
	blk, _ := s.backend.Block(height)
	var found *ztypes.Tx
	if blk != nil {
		for _, tx := range blk.Txs {
			if tx.ID() == h {
				found = tx
				break
			}
		}
	}
	if found == nil {
		return nil, errInternal(errors.New("transaction index inconsistent"))
	}
	dto := txDTO(found, height)
	if rb, ok := s.backend.(ReceiptBackend); ok {
		if recs, ok := rb.Receipts(height); ok {
			for _, r := range recs {
				if r.TxID == found.ID() {
					dto["receipt"] = receiptDTO(r)
					break
				}
			}
		}
	}
	return dto, nil
}

// receiptDTO renders a receipt for JSON consumers. Gas and fee values are given
// in both micro-units and decimal Z so a client never has to scale by hand.
func receiptDTO(r *ztypes.Receipt) map[string]any {
	out := map[string]any{
		"tx_id":            r.TxID.String(),
		"height":           r.Height,
		"type":             uint8(r.Type),
		"status":           int(r.Status),
		"gas_used":         r.GasUsed,
		"gas_wanted":       r.GasWanted,
		"fee_uz":           r.Fee,
		"fee_z":            ztypes.FormatUZ(r.Fee),
		"sender":           r.Sender.Bech32(),
		"contract_address": r.ContractAddress.Bech32(),
		"output":           hexEncode(r.Output),
		"error":            r.Error,
	}
	events := make([]map[string]any, 0, len(r.Events))
	for _, ev := range r.Events {
		attrs := make([]map[string]string, 0, len(ev.Attrs))
		for _, a := range ev.Attrs {
			attrs = append(attrs, map[string]string{"key": a[0], "value": a[1]})
		}
		events = append(events, map[string]any{"type": ev.Type, "attributes": attrs})
	}
	out["events"] = events
	return out
}

// getReceipt returns the execution receipt for a committed transaction.
func (s *Service) getReceipt(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		ID string `json:"id"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	h, e := parseHash(p.ID)
	if e != nil {
		return nil, e
	}
	height, ok := s.backend.TxHeight(h)
	if !ok {
		return nil, errNotFound("transaction " + p.ID)
	}
	rb, ok := s.backend.(ReceiptBackend)
	if !ok {
		return nil, errInternal(errors.New("receipts are not retained by this node"))
	}
	recs, ok := rb.Receipts(height)
	if !ok {
		return nil, errNotFound("receipt for " + p.ID)
	}
	for _, r := range recs {
		if r.TxID == h {
			return receiptDTO(r), nil
		}
	}
	return nil, errNotFound("receipt for " + p.ID)
}

func txDTO(tx *ztypes.Tx, height uint64) map[string]any {
	fee, _ := tx.Fee()
	dto := map[string]any{
		"id":        tx.ID().String(),
		"status":    "committed",
		"height":    height,
		"type":      uint8(tx.Type),
		"sender":    tx.Sender.Bech32(),
		"nonce":     tx.Nonce,
		"gas_limit": tx.GasLimit,
		"gas_price": tx.GasPrice,
		"fee_uz":    fee,
		"memo":      tx.Memo,
		"time_ms":   tx.TimeMs,
		"payload":   hexEncode(tx.Payload),
	}
	return dto
}

func (s *Service) getMempool(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	txs := s.pool.All(s.backend.State())
	if len(txs) > 200 {
		txs = txs[:200]
	}
	out := make([]map[string]any, 0, len(txs))
	for _, tx := range txs {
		out = append(out, map[string]any{
			"id":        tx.ID().String(),
			"type":      uint8(tx.Type),
			"sender":    tx.Sender.Bech32(),
			"nonce":     tx.Nonce,
			"gas_price": tx.GasPrice,
			"size":      len(tx.Encode()),
		})
	}
	return map[string]any{"transactions": out, "size": s.pool.Size(), "bytes": s.pool.Bytes()}, nil
}

func (s *Service) sendRawTransaction(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Tx string `json:"tx"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	rawTx, err := hexDecode(p.Tx)
	if err != nil {
		return nil, errParams("tx hex: %v", err)
	}
	tx, err := ztypes.DecodeTx(rawTx)
	if err != nil {
		s.mu.Lock()
		s.metrics.TxRejected++
		s.mu.Unlock()
		return nil, &RPCError{Code: CodeTxRejected, Message: "invalid transaction", Data: err.Error()}
	}
	if tx.ChainID != s.chainID {
		s.mu.Lock()
		s.metrics.TxRejected++
		s.mu.Unlock()
		return nil, &RPCError{Code: CodeTxRejected, Message: "wrong chain id", Data: tx.ChainID}
	}
	id, err := s.submit(tx)
	if err != nil {
		s.mu.Lock()
		s.metrics.TxRejected++
		s.mu.Unlock()
		code := CodeTxRejected
		if errors.Is(err, zmempool.ErrPoolFull) || errors.Is(err, zmempool.ErrPerSenderLimit) {
			code = CodeUnavailable
		}
		return nil, &RPCError{Code: code, Message: "transaction rejected", Data: err.Error()}
	}
	s.mu.Lock()
	s.metrics.TxAccepted++
	s.mu.Unlock()
	return map[string]any{"id": id.String(), "status": "accepted"}, nil
}

// ---- contracts -------------------------------------------------------------

func (s *Service) getContractStorage(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Address string `json:"address"`
		Slot    uint64 `json:"slot"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Address)
	if e != nil {
		return nil, e
	}
	acc, err := s.backend.State().GetAccount(a)
	if err != nil {
		return nil, errInternal(err)
	}
	if !acc.IsContract() {
		return nil, errNotFound("contract " + p.Address)
	}
	return map[string]any{"address": a.Bech32(), "slot": p.Slot, "value": s.backend.State().GetSlot(a, p.Slot)}, nil
}

func (s *Service) getContractCode(_ context.Context, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Address string `json:"address"`
	}
	if e := decodeParams(raw, &p); e != nil {
		return nil, e
	}
	a, e := parseAddress(p.Address)
	if e != nil {
		return nil, e
	}
	acc, err := s.backend.State().GetAccount(a)
	if err != nil {
		return nil, errInternal(err)
	}
	if !acc.IsContract() {
		return nil, errNotFound("contract " + p.Address)
	}
	code, ok := s.backend.State().GetCode(acc.CodeHash)
	if !ok {
		return nil, errInternal(errors.New("contract code missing"))
	}
	return map[string]any{"address": a.Bech32(), "code_hex": hexEncode(code), "code_hash": acc.CodeHash.String(), "size": len(code)}, nil
}

// ---- metrics ---------------------------------------------------------------

func (s *Service) metricsMethod(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"rpc_requests":   s.metrics.RPCRequests,
		"rpc_errors":     s.metrics.RPCErrors,
		"tx_accepted":    s.metrics.TxAccepted,
		"tx_rejected":    s.metrics.TxRejected,
		"blocks_applied": s.backend.Height(),
		"peer_count":     s.peerCnt(),
		"height":         s.backend.Height(),
		"mempool_size":   s.pool.Size(),
		"uptime_sec":     int64(time.Since(s.started).Seconds()),
	}
	if s.reg != nil {
		c := s.reg.Snapshot()
		out["registry"] = c
	}
	return out, nil
}

// Serve starts an HTTP server on addr and returns it (call Shutdown to stop).
func (s *Service) Serve(addr string) (*http.Server, net.Addr, error) {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	// Explorer UI (static, same-origin, no external resources).
	mux.Handle("/explorer/", http.StripPrefix("/explorer", s.Explorer()))
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.reg == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(s.reg.WritePrometheus())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","height":`+strconv.FormatUint(s.backend.Height(), 10)+`}`)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() { _ = srv.Serve(ln) }()
	return srv, ln.Addr(), nil
}

func hexDecode(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexVal(s[i*2])
		lo, ok2 := hexVal(s[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex at offset %d", i*2)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
