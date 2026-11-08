// Package zsdk is the official Go client for Z Network (AGENT_19).
//
// It speaks the documented JSON-RPC API and builds, signs and submits real
// transactions. The SDK never holds a private key on the server side: signing
// happens in the caller's process, and only the signed bytes are transmitted.
//
// Example:
//
//	c, err := zsdk.Dial("http://127.0.0.1:26657")
//	key, _ := zsdk.LoadKeystore("me.key.json", "password")
//	tx, err := zsdk.Transfer(c, key, recipient, 5*ztypes.CoinScale, zsdk.Options{})
//	id, err := c.SendTx(tx)
package zsdk

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
	"znetwork/internal/zvm"
)

// Client is a JSON-RPC client for one node.
type Client struct {
	URL  string
	HTTP *http.Client
	// ChainID is fetched lazily from the node and cached.
	ChainID string
	id      int
}

// Dial creates a client. The chain id is fetched immediately so a client cannot
// accidentally sign for the wrong chain.
func Dial(url string) (*Client, error) {
	c := &Client{URL: url, HTTP: &http.Client{Timeout: 15 * time.Second}}
	info, err := c.ChainInfo()
	if err != nil {
		return nil, err
	}
	c.ChainID = info.ChainID
	return c, nil
}

// rpc performs one call.
func (c *Client) rpc(method string, params any, out any) error {
	c.id++
	body := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("zsdk: %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("zsdk: %s: decoding response: %w", method, err)
	}
	if envelope.Error != nil {
		return &RPCError{Method: method, Code: envelope.Error.Code, Message: envelope.Error.Message, Data: envelope.Error.Data}
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("zsdk: %s: decoding result: %w", method, err)
		}
	}
	return nil
}

// RPCError is a server-side error.
type RPCError struct {
	Method  string
	Code    int
	Message string
	Data    string
}

func (e *RPCError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("%s failed: %s: %s", e.Method, e.Message, e.Data)
	}
	return fmt.Sprintf("%s failed: %s (code %d)", e.Method, e.Message, e.Code)
}

// ChainInfo is the z_chainInfo result.
type ChainInfo struct {
	ChainID     string `json:"chain_id"`
	Protocol    uint8  `json:"protocol"`
	Height      uint64 `json:"height"`
	HeadHash    string `json:"head_hash"`
	GenesisHash string `json:"genesis_hash"`
	StateRoot   string `json:"state_root"`
	SupplyUZ    uint64 `json:"supply_uz"`
}

// ChainInfo fetches chain identity and head state.
func (c *Client) ChainInfo() (*ChainInfo, error) {
	var out ChainInfo
	if err := c.rpc("z_chainInfo", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Height returns the node's current height.
func (c *Client) Height() (uint64, error) {
	var out struct {
		Height uint64 `json:"height"`
	}
	if err := c.rpc("z_getHeight", nil, &out); err != nil {
		return 0, err
	}
	return out.Height, nil
}

// Account is the z_getAccount result.
type Account struct {
	Address       string `json:"address"`
	BalanceUZ     uint64 `json:"balance_uz"`
	BalanceZ      string `json:"balance_z"`
	Nonce         uint64 `json:"nonce"`
	IsContract    bool   `json:"is_contract"`
	CodeHash      string `json:"code_hash"`
	CreatedHeight uint64 `json:"created_height"`
}

// Account fetches an account.
func (c *Client) Account(addr string) (*Account, error) {
	var out Account
	if err := c.rpc("z_getAccount", map[string]any{"address": addr}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PendingNonce returns the next usable nonce for an address.
func (c *Client) PendingNonce(addr string) (uint64, error) {
	var out struct {
		Nonce uint64 `json:"nonce"`
	}
	if err := c.rpc("z_pendingNonce", map[string]any{"address": addr}, &out); err != nil {
		return 0, err
	}
	return out.Nonce, nil
}

// Block fetches a block by height.
func (c *Client) Block(height uint64) (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getBlock", map[string]any{"height": height}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BlockByHash fetches a block by hash.
func (c *Client) BlockByHash(hash string) (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getBlockByHash", map[string]any{"hash": hash}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Blocks fetches a range of blocks.
func (c *Client) Blocks(from, limit uint64) ([]map[string]any, error) {
	var out []map[string]any
	if err := c.rpc("z_getBlocks", map[string]any{"from": from, "limit": limit}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Transaction fetches a transaction by id.
func (c *Client) Transaction(id string) (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getTransaction", map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Validators lists validators.
// Delegations returns the delegations held by a delegator address.
func (c *Client) Delegations(delegator string) (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getDelegations", map[string]any{"delegator": delegator}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Receipt returns the execution receipt of a committed transaction.
func (c *Client) Receipt(id string) (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getReceipt", map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Validators() ([]map[string]any, error) {
	var out struct {
		Validators []map[string]any `json:"validators"`
	}
	if err := c.rpc("z_getValidators", nil, &out); err != nil {
		return nil, err
	}
	return out.Validators, nil
}

// Proposals lists governance proposals.
func (c *Client) Proposals() ([]map[string]any, error) {
	var out struct {
		Proposals []map[string]any `json:"proposals"`
	}
	if err := c.rpc("z_getProposals", nil, &out); err != nil {
		return nil, err
	}
	return out.Proposals, nil
}

// Params returns the live parameter set.
func (c *Client) Params() (map[string]uint64, error) {
	var out struct {
		Params map[string]uint64 `json:"params"`
	}
	if err := c.rpc("z_getParams", nil, &out); err != nil {
		return nil, err
	}
	return out.Params, nil
}

// Supply returns the accounting breakdown.
func (c *Client) Supply() (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getSupply", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Mempool lists pooled transactions.
func (c *Client) Mempool() (map[string]any, error) {
	var out map[string]any
	if err := c.rpc("z_getMempool", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendTx submits a signed transaction and returns its id.
func (c *Client) SendTx(tx *ztypes.Tx) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.rpc("z_sendRawTransaction", map[string]any{"tx": hex.EncodeToString(tx.Encode())}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ContractStorage reads a contract storage slot.
func (c *Client) ContractStorage(addr string, slot uint64) (uint64, error) {
	var out struct {
		Value uint64 `json:"value"`
	}
	if err := c.rpc("z_getContractStorage", map[string]any{"address": addr, "slot": slot}, &out); err != nil {
		return 0, err
	}
	return out.Value, nil
}

// ContractCode returns contract bytecode.
func (c *Client) ContractCode(addr string) ([]byte, error) {
	var out struct {
		CodeHex string `json:"code_hex"`
	}
	if err := c.rpc("z_getContractCode", map[string]any{"address": addr}, &out); err != nil {
		return nil, err
	}
	return hex.DecodeString(out.CodeHex)
}

// WaitForTx polls until a transaction is committed or the timeout expires. It
// returns the committed height, or an error.
func (c *Client) WaitForTx(id string, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := c.Transaction(id)
		if err == nil {
			if status, _ := res["status"].(string); status == "committed" {
				switch h := res["height"].(type) {
				case float64:
					return uint64(h), nil
				}
			}
		}
		var rpcErr *RPCError
		if err != nil && !asRPCError(err, &rpcErr) {
			return 0, err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return 0, fmt.Errorf("zsdk: transaction %s not committed within %s", id, timeout)
}

func asRPCError(err error, target **RPCError) bool {
	if e, ok := err.(*RPCError); ok {
		*target = e
		return true
	}
	return false
}

// Options parameterise local transaction building.
type Options struct {
	// GasLimit defaults to 500000 when zero.
	GasLimit uint64
	// GasPrice defaults to 1 when zero (the development-network minimum).
	GasPrice uint64
	// Memo is optional and is committed in the transaction id.
	Memo string
	// Nonce overrides the nonce fetched from the node.
	Nonce  *uint64
	TimeMs int64
}

func (o Options) withDefaults() Options {
	if o.GasLimit == 0 {
		o.GasLimit = 500_000
	}
	if o.GasPrice == 0 {
		o.GasPrice = 1
	}
	if o.TimeMs == 0 {
		o.TimeMs = time.Now().UnixMilli()
	}
	return o
}

// BuildTx builds and signs a transaction of the given type.
func BuildTx(c *Client, key *zcrypto.PrivateKey, typ ztypes.TxType, payload []byte, opt Options) (*ztypes.Tx, error) {
	opt = opt.withDefaults()
	if c.ChainID == "" {
		info, err := c.ChainInfo()
		if err != nil {
			return nil, err
		}
		c.ChainID = info.ChainID
	}
	nonce := uint64(0)
	if opt.Nonce != nil {
		nonce = *opt.Nonce
	} else {
		n, err := c.PendingNonce(key.Address().Bech32())
		if err != nil {
			return nil, err
		}
		nonce = n
	}
	tx := &ztypes.Tx{
		ChainID:  c.ChainID,
		Type:     typ,
		Sender:   key.Address(),
		Nonce:    nonce,
		GasLimit: opt.GasLimit,
		GasPrice: opt.GasPrice,
		TimeMs:   opt.TimeMs,
		Memo:     opt.Memo,
		Payload:  payload,
	}
	if err := tx.Sign(key); err != nil {
		return nil, err
	}
	return tx, nil
}

// Transfer builds and signs a transfer.
func Transfer(c *Client, key *zcrypto.PrivateKey, to ztypes.Address, amountUZ uint64, opt Options) (*ztypes.Tx, error) {
	return BuildTx(c, key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: to, Amount: amountUZ}).Encode(), opt)
}

// Delegate builds and signs a delegation.
func Delegate(c *Client, key *zcrypto.PrivateKey, validator ztypes.Address, amountUZ uint64, opt Options) (*ztypes.Tx, error) {
	return BuildTx(c, key, ztypes.TxDelegate, ztypes.NewDelegatePayload(validator, amountUZ).Encode(), opt)
}

// Undelegate builds and signs an undelegation.
func Undelegate(c *Client, key *zcrypto.PrivateKey, validator ztypes.Address, amountUZ uint64, opt Options) (*ztypes.Tx, error) {
	return BuildTx(c, key, ztypes.TxUndelegate, ztypes.NewUndelegatePayload(validator, amountUZ).Encode(), opt)
}

// RegisterValidator builds and signs a validator registration.
func RegisterValidator(c *Client, key *zcrypto.PrivateKey, moniker string, commissionBps uint16, opt Options) (*ztypes.Tx, error) {
	if len(moniker) == 0 || len(moniker) > ztypes.MaxMonikerLen {
		return nil, fmt.Errorf("zsdk: moniker must be 1..%d bytes", ztypes.MaxMonikerLen)
	}
	p := &ztypes.ValidatorRegisterPayload{PubKey: key.PublicKey(), Moniker: moniker, CommissionBps: commissionBps}
	return BuildTx(c, key, ztypes.TxValidatorRegister, p.Encode(), opt)
}

// SubmitProposal builds and signs a governance proposal.
func SubmitProposal(c *Client, key *zcrypto.PrivateKey, title, description string, kind ztypes.GovProposalKind, paramKey string, paramValue, depositUZ uint64, opt Options) (*ztypes.Tx, error) {
	p := &ztypes.GovProposalPayload{
		Title: title, Description: description, Kind: kind,
		ParamKey: paramKey, ParamValue: paramValue, Deposit: depositUZ,
	}
	return BuildTx(c, key, ztypes.TxGovProposal, p.Encode(), opt)
}

// Vote builds and signs a governance vote.
func Vote(c *Client, key *zcrypto.PrivateKey, proposalID uint64, option ztypes.VoteOption, opt Options) (*ztypes.Tx, error) {
	return BuildTx(c, key, ztypes.TxGovVote, (&ztypes.GovVotePayload{ProposalID: proposalID, Option: option}).Encode(), opt)
}

// DeployContract assembles, builds and signs a contract deployment.
func DeployContract(c *Client, key *zcrypto.PrivateKey, assembly string, opt Options) (*ztypes.Tx, error) {
	asm, err := zvm.Assemble(assembly)
	if err != nil {
		return nil, fmt.Errorf("zsdk: assembling contract: %w", err)
	}
	opt = opt.withDefaults()
	if opt.GasLimit < uint64(len(asm.Code))*300 {
		opt.GasLimit = uint64(len(asm.Code))*300 + 100_000
	}
	return BuildTx(c, key, ztypes.TxContractDeploy, (&ztypes.ContractDeployPayload{Code: asm.Code}).Encode(), opt)
}

// CallContract builds and signs a contract call.
func CallContract(c *Client, key *zcrypto.PrivateKey, contract ztypes.Address, input []byte, opt Options) (*ztypes.Tx, error) {
	return BuildTx(c, key, ztypes.TxContractCall, (&ztypes.ContractCallPayload{Contract: contract, Input: input}).Encode(), opt)
}

// DecodeTx decodes a hex-encoded transaction (for inspection tools).
func DecodeTx(hexStr string) (*ztypes.Tx, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(hexStr, "0x"))
	if err != nil {
		return nil, err
	}
	return ztypes.DecodeTx(raw)
}
