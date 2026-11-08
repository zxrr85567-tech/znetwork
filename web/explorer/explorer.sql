-- Z NETWORK explorer schema, served by znd at /v1/explorer.sql
-- The explorer UI is a single HTML document; this file documents the RPC calls it
-- makes so the data contract is reviewable without reading JavaScript.
--
-- Every call is a standard JSON-RPC 2.0 POST to the node's RPC endpoint with
-- {"jsonrpc":"2.0","id":N,"method":M,"params":P}. The explorer runs on the same
-- origin as the RPC server, so no CORS or external network access is required.

-- Chain head, chain id, genesis hash, supply.
z_chainInfo            params: none
-- Response: {chain_id, protocol, height, head_hash, genesis_hash, state_root, supply_uz}

-- A block by height, newest first for the explorer list.
z_getBlocks            params: {from: uint64, limit: uint32}
z_getBlock             params: {height: uint64}
z_getBlockByHash       params: {hash: hex64}
-- Response: {height, hash, prev_hash, time_ms, proposer, tx_count, gas_used,
--            gas_limit, state_root, tx_root, receipt_root, validators_hash,
--            tx_ids: [hex64], commit_sigs}

-- A transaction and its receipt.
z_getTransaction       params: {id: hex64}
-- Response when committed: {id, status:"committed", height, type, sender, nonce,
--   gas_limit, gas_price, fee_uz, memo, time_ms, payload, receipt:{...}}
-- Response when pending: {id, status:"pending", height:null}

-- Accounts.
z_getAccount           params: {address: bech32}
z_getBalance           params: {address: bech32}
z_getNonce             params: {address: bech32}

-- Validators and staking.
z_getValidators        params: none
z_getValidator         params: {address: bech32}
z_getDelegations       params: {address: bech32}
z_getSupply            params: none

-- Governance.
z_getProposals         params: none
z_getProposal          params: {id: uint64}
z_getParams            params: none

-- Contracts.
z_getContractCode      params: {address: bech32}
z_getContractStorage   params: {address: bech32}
