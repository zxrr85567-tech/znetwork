# RPC Specification

**Implementation:** `internal/zrpc/` (`server.go`, `explorer.go`)
**Verified by:** `internal/zrpc/server_test.go` (18 tests), live checks against a
running `znd`

## Endpoint

`znd --rpc ADDR` serves JSON-RPC 2.0 over HTTP POST at `/`, plus:

| Path | Method | Purpose |
|---|---|---|
| `/` | POST | JSON-RPC |
| `/healthz` | GET | `{"status":"ok","height":N}` — no auth, for probes |
| `/metrics` | GET | Prometheus text exposition (0.0.4) |
| `/explorer/` | GET | bundled block explorer UI |
| `/explorer/explorer.sql` | GET | the explorer's data contract |

`Version = "1.0.0"`. The server sets `ReadHeaderTimeout 5s`, `ReadTimeout 15s`,
`WriteTimeout 30s`, `IdleTimeout 60s`, `MaxHeaderBytes 16 KiB`.

## Request/response shape

```json
{"jsonrpc":"2.0","id":1,"method":"z_getBlock","params":{"height":19}}
→ {"jsonrpc":"2.0","id":1,"result":{...}}
→ {"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"not found","data":"block 19"}}
```

- `params` may be omitted or `null` for methods that take none. A method that
  takes none **rejects** parameters (`-32602`) rather than ignoring them, so a
  typo cannot look successful.
- Unknown fields in `params` are rejected.
- Batches are supported: at most **20** requests per batch, at most **1,000**
  results. Oversized batches are `-32600`.
- Request bodies are limited to **1 MiB**.

## Methods

### Chain

| Method | Params | Returns |
|---|---|---|
| `z_chainInfo` | — | `chain_id, protocol, height, head_hash, genesis_hash, state_root, supply_uz, validator_count, peer_count, block_gas_limit, base_tx_gas` |
| `z_nodeInfo` | — | node version, protocol, uptime, peers, mempool size |
| `z_getHeight` | — | `{"height":N}` |
| `z_getBlock` | `{height}` | block DTO |
| `z_getBlockByHash` | `{hash}` | block DTO |
| `z_getBlocks` | `{from, limit}` | block DTO array (limit ≤ 1000) |
| `z_getSupply` | — | supply breakdown and `balanced` invariant flag |
| `z_metrics` | — | counters, height, mempool, peers, uptime; `registry` when metrics are attached |

### Accounts and transactions

| Method | Params | Returns |
|---|---|---|
| `z_getAccount` | `{address}` | address, balance (uz and Z), nonce, contract flag, code hash, created height |
| `z_getBalance` | `{address}` | balance |
| `z_getNonce` | `{address}` | committed nonce |
| `z_pendingNonce` | `{address}` | nonce including pooled transactions |
| `z_getTransaction` | `{id}` | transaction DTO, `status: committed`/`pending`, with `receipt` when committed and retained |
| `z_getReceipt` | `{id}` | execution receipt: gas, fee, events, contract address, output, error |
| `z_getMempool` | — | pooled transactions (≤ 200) with size and bytes |
| `z_sendRawTransaction` | `{tx: hex}` | `{"tx_id":"..."}` — **write method** |

### Staking and governance

| Method | Params | Returns |
|---|---|---|
| `z_getValidators` | — | validator list with power, status, moniker, commission |
| `z_getValidator` | `{address}` | one validator |
| `z_getDelegations` | `{delegator}` or `{delegator, validator}` | delegation records incl. unbonding entries |
| `z_getProposals` | — | proposals with tallies and status |
| `z_getProposal` | `{id}` | one proposal |
| `z_getParams` | — | all ten consensus/economics parameters |

### Contracts

| Method | Params | Returns |
|---|---|---|
| `z_getContractCode` | `{address}` | bytecode (hex) and code hash |
| `z_getContractStorage` | `{address, slot}` | 64-bit storage word |

Field names are stable and documented here; the explorer's
[`/explorer.sql`](../web/explorer/explorer.sql) repeats the contract in a form
that can be reviewed without reading JavaScript.

## Error codes

| Code | Constant | Meaning |
|---|---|---|
| `-32700` | `CodeParseError` | body is not JSON |
| `-32600` | `CodeInvalidRequest` | not JSON-RPC 2.0, batch too large |
| `-32601` | `CodeMethodNotFound` | unknown method |
| `-32602` | `CodeInvalidParams` | bad params, unknown field, bad address/hash |
| `-32603` | `CodeInternalError` | internal failure (never used for user error) |
| `-32000` | `CodeTxRejected` | transaction rejected by the mempool |
| `-32001` | `CodeNotFound` | block/tx/receipt/account not found |
| `-32002` | `CodeUnavailable` | backend unavailable |
| `-32003` | `CodeUnauthorized` | missing or wrong bearer token on a write method |

Only `z_sendRawTransaction` is a write method and only it requires the bearer
token when `ZND_RPC_TOKEN` is set. Read methods stay open so an explorer can be
served from the same node.

## Metrics

`/metrics` is Prometheus text format 0.0.4, hand-written to avoid a client
dependency. Every sample carries `chain_id` and `node` labels.

```
znet_tx_accepted_total          counter
znet_tx_rejected_total          counter
znet_blocks_applied_total       counter
znet_votes_cast_total           counter
znet_votes_received_total       counter
znet_rounds_advanced_total      counter
znet_heights_completed_total    counter
znet_rpc_errors_total           counter
znet_peers_dropped_total        counter
znet_height                     gauge
znet_mempool_size               gauge
znet_peer_count                 gauge
znet_supply_uz                  gauge
znet_validator_count            gauge
```

Verified on a live validator: after ~25 blocks, `/metrics` reported
`znet_height 25`, `znet_blocks_applied_total 25`, `znet_votes_cast_total 25`,
`znet_heights_completed_total 25`, `znet_validator_count 1` — and `/healthz`
agreed on the height. Counters that are declared but never incremented are a
lie, so this was checked against real traffic rather than assumed.

## Concurrency and resource limits

- The service is an `http.Handler`; all methods are safe for concurrent use.
- Backend reads take the chain lock briefly; no RPC call holds a lock while
  writing to the network or the client.
- `z_getBlocks` clamps its limit; `z_getMempool` caps at 200 entries.
- Response sizes are bounded by those clamps, not by the caller.

## Client support

- `internal/zsdk` — Go client with typed helpers (`Transfer`, `Delegate`,
  `Undelegate`, `RegisterValidator`, `DeployContract`, `CallContract`,
  `Delegations`, `Receipt`, `WaitForTx`).
- `zcli` — read-only inspection commands plus devnet/keygen tooling.
- `zwallet` — signing client (send, delegate, undelegate, register-validator,
  propose, vote, deploy, call, sign).
- `web/explorer` — browser client, same-origin only.

All three clients were exercised against a live 4-validator network.

## Known gaps

1. **No WebSocket or subscription API.** Polling only.
2. **No pagination cursors** for `z_getBlocks` beyond `from`/`limit`.
3. **No rate limiting.** The RPC is bound to loopback by default; exposing it
   requires a reverse proxy, which is documented in `deployment/`.
