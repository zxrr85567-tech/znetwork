# Networking Specification

**Implementation:** `internal/znet/` (`znet.go`, `codec.go`)
**Verified by:** `internal/znet/znet_test.go`, `internal/znode/multinode_test.go`,
`FuzzReadMessage`

## Transport

Plain TCP. There is **no encryption and no message authentication** beyond:

- per-frame CRC32C (integrity against corruption, not tampering),
- Ed25519 signatures on votes, proposals and transactions (authenticity of
  consensus content),
- the handshake exchange of chain id and public key (peer identity for
  bookkeeping, *not* a trust anchor).

Everything is documented as unencrypted in [../security/THREAT_MODEL.md](../security/THREAT_MODEL.md)
(threat T-11) and in [../BLOCKERS.md](../BLOCKERS.md). A production deployment
needs a TLS or Noise transport; the message layer is written so that adding one
does not change any codec.

## Framing

```
frame = u32 length (big-endian) || body
body  = u8 type || payload(length-1) || u32 crc32c(type || payload)
```

| Constant | Value |
|---|---|
| `Magic` | `0x5A4E4554` (`"ZNET"`), used in the handshake commentary and tooling |
| `ProtoVersion` | `1` |
| `MaxFrame` | 4 MiB |
| `HandshakeLimit` | 1,024-byte handshake payload |
| `DefaultTimeout` | 10 s for handshake and request/response |
| `IdleTimeout` | 120 s — a silent connection is closed |

Reader rules, all enforced and fuzzed:

1. length 0 or > `MaxFrame` is refused **before** allocating;
2. a short read is an error, never a partial message;
3. a bad CRC is `ErrChecksum` and the connection is dropped;
4. the decoded message must re-encode to exactly the bytes consumed — a length
   prefix that over- or under-declares by even one byte is a framing ambiguity
   and is rejected. `FuzzReadMessage` asserts this property (594,073 executions
   in the recorded campaign).

## Handshake

Both sides send `HELLO` immediately:

```
version    u8  = 1
chainID    string
moniker    string
listenAddr string
pubKey     32 bytes
height     u64
```

Each side then validates:

- `version == ProtoVersion` — otherwise the connection is refused;
- `chainID` equals the local chain id — a peer on another network is refused
  with `ErrChainID` (this is what prevents a devnet node joining a testnet);
- the payload decodes canonically (no trailing bytes).

Both sides send at the same time (symmetric handshake), so neither has to know
whether it dialled or accepted.

## Message types

| Code | Name | Direction | Payload |
|---|---|---|---|
| `0x01` | `HELLO` | both, at connect | handshake above |
| `0x02` | `GET_BLOCKS` | requester → peer | `from u64, limit u32` (limit clamped to 128) |
| `0x03` | `BLOCKS` | peer → requester | `count u32 ‖ blocks` (count ≤ 128) |
| `0x04` | `PROPOSAL` | proposer → all | consensus proposal |
| `0x05` | `VOTE` | validator → all | consensus vote |
| `0x06` | `TX` | submitter → all | encoded transaction |
| `0x07` | `STATUS` | periodic | `chainID, height, headHash, genesisHash` |
| `0x08` | `PING` | both | empty |
| `0x09` | `PONG` | both | empty |
| `0x0A` | `GET_BLOCK_BY` | requester → peer | `hash 32 bytes` |

Block and transaction payloads use the same canonical encoders as storage and
RPC, so there is exactly one byte representation of a block across the whole
system.

## Connection management

- **Duplicate suppression by identity.** If both nodes dial each other at the
  same time, two connections exist. Both ends keep the connection whose
  *initiator* public key is lexicographically smaller and close the other. The
  rule is symmetric, so both ends reach the same decision without negotiation.
- **Reconnect loop.** A seed peer that drops is re-dialled every 500 ms for the
  lifetime of the node, with the connection count bounded by the seed list.
- **Per-frame read deadline.** Every read sets an idle deadline; a peer that
  stops sending is disconnected after `IdleTimeout`.
- **Gossip deduplication.** Relayed messages are cached by SHA-256 of
  `(type, payload)` with a bounded cache; a message is forwarded at most once,
  and never back to the peer it came from. This bounds gossip traffic to
  O(peers × messages) and prevents trivial echo storms.
- **Test-only message filter.** `Server.SetFilter` installs a hook that
  silently drops messages in either direction while the connection stays up —
  a black-hole partition, the capability an on-path attacker has (see
  `internal/znode/partition_test.go`). Production servers never install one
  (the default is nil and every message flows). It is not a protocol feature:
  no wire format, message type, constant or handshake field depends on it, and
  handshakes are never filtered.

## Synchronisation

A node that is behind does not wait for consensus messages it cannot use:

```
1. peer sends STATUS (on connect and after committing a block)
2. if peer.height > local.height: send GET_BLOCKS{from: local.height+1, limit: 128}
3. on BLOCKS: apply each block in order through the same validation path used
   by consensus (Chain.ApplyBlock), rejecting any block that does not extend
   the chain, fails its transaction validation, or fails an invariant
4. after each applied block, position the consensus engine at chainHeight+1
5. repeat until caught up
```

Every block received from a peer is **re-executed** against local state; a peer
cannot hand a node a state root it cannot reproduce. This is why a full node
catches up correctly with no trust in the peer beyond ordering.

## What a malicious peer can and cannot do

| Attempt | Result |
|---|---|
| Send a frame > 4 MiB | connection dropped before allocation |
| Corrupt a frame | CRC mismatch, connection dropped |
| Send a valid frame for another chain | rejected at handshake; rejected per message by chain id |
| Replay a vote | vote set dedup (`ErrDuplicateVote`), power counted once |
| Send two conflicting votes | none adopted after the first; evidence retained |
| Send a block with a bad state root | rejected by `ApplyBlock` (re-executed locally) |
| Send a block from the future | rejected (`does not extend the chain`) |
| Flood blocks | bounded by `MaxBlocksPerMessage = 128` and by re-execution cost |
| Lie in STATUS about height | server only finds nothing to send; no state change |
| Slow-loris a connection | idle deadline closes it |

## Measured behaviour

`scripts/testnet.sh up --validators 4` on one host:

- consensus at 600 ms blocks sustained (143 blocks produced in the run that
  preceded a restart test);
- node 0 (the seed) reported 3 peers; the others 1 peer each — a star topology
  from one seed, which is the intended testnet shape;
- `-race` multi-node tests: 0 data races, 0 failures in 12 consecutive runs
  after the height-ownership fix, and 24/24 package runs green under 6-way
  parallel load.

## Limitations

1. **No transport encryption.** See above.
2. **No peer scoring or ban list.** A misbehaving peer is disconnected, and can
   reconnect. For a testnet this is acceptable; it is a blocker for Mainnet.
3. **No block-range compression.** Blocks are sent as individual encoded
   blocks inside one frame.
4. **Single seed topology in the launcher.** `scripts/testnet.sh` wires every
   node to node 0. The protocol supports a full mesh; the launcher keeps the
   test shape simple and observable.
5. **128-block sync window per request.** Catching up from genesis on a very long
   chain is many round trips (each ~1 RTT). Adequate for a testnet, not for a
   production sync.
