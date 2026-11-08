# Cryptography Specification

**Implementation:** `internal/zcrypto/` (`hash.go`, `keys.go`, `bech32.go`,
`keystore.go`, `pbkdf2.go`)
**Verified by:** `internal/zcrypto/*_test.go` (88.2% statement coverage),
RFC test vectors, `FuzzDecode`

## Primitives

| Purpose | Primitive | Where |
|---|---|---|
| Hashing, ids, roots | SHA-256 | everywhere |
| Signatures | Ed25519 (Go standard library, RFC 8032) | transactions, votes, keystores |
| Password key derivation | PBKDF2-HMAC-SHA256, 600,000 iterations | keystore |
| Key encryption | AES-256-GCM (96-bit nonce, 128-bit tag) | keystore |
| Address encoding | Bech32, HRP `z` | addresses |
| Frame integrity | CRC32C (Castagnoli) | P2P framing only |

No third-party cryptography is used, and no primitive is implemented in this
repository except PBKDF2, which is checked against **RFC 7914 test vectors** in
`TestPBKDF2RFC7914Vector`. Ed25519 is checked against **RFC 8032 test vector 1**
in `TestEd25519RFC8032Vector1`. Bech32 is checked against the **BIP-173** valid
and invalid vectors.

## Domain separation

Every hash is prefixed with a length-framed domain string, so a digest computed
for one purpose can never collide with a digest computed for another:

```
Digest(domain, parts...) = SHA-256( u32be(len(domain)) || domain
                                    || u32be(len(part1)) || part1 || … )
```

The length prefixes make the concatenation unambiguous: `Digest("a", "bc")` and
`Digest("ab", "c")` cannot be confused. `TestDigestFramingIsUnambiguous` asserts
exactly that.

Registered domains (`internal/zcrypto/keys.go`, `internal/ztypes/*.go`):

| Domain | Used for |
|---|---|
| `ZNET/v1/address` | address derivation from a public key |
| `ZNET/v1/transaction` | transaction signatures |
| `ZNET/v1/tx-id` | transaction id |
| `ZNET/v1/tx-payload` | payload tags/commitments |
| `ZNET/v1/block-header` | block hash |
| `ZNET/v1/block-id` | consensus block id |
| `ZNET/v1/genesis` | genesis hash |
| `ZNET/v1/state` | state Merkle tree |
| `ZNET/v1/state-leaf`, `ZNET/v1/state-node` | state tree leaves and internal nodes |
| `ZNET/v1/tx-root`, `ZNET/v1/receipt-root` | per-block roots |
| `ZNET/v1/consensus-vote` | vote signatures |
| `ZNET/v1/consensus-proposal` | proposal signatures |
| `ZNET/v1/validator-set` | validator set hash |
| `ZNET/v1/proposer-seed` | proposer selection |
| `ZNET/v1/contract-address` | contract address derivation |
| `ZNET/v1/contract-code` | contract code hash |
| `ZNET/v1/governance-id` | proposal ids |
| `ZNET/v1/system-treasury` | treasury address |
| `ZNET/v1/keystore-check` | keystore password check |
| `ZNET/v1/peer-handshake` | P2P handshake (reserved) |
| `ZNET/v1/devnet-key` | devnet key derivation (test only) |

Adding a domain requires a protocol version bump; the comment in `keys.go` says
so, and the version byte is inside every frame, transaction and genesis.

## Keys and addresses

```
seed       32 bytes
private    Ed25519(seed)             (64 bytes internally, never serialised)
public     32 bytes
address    Digest("ZNET/v1/address", public)[12:32]      (20 bytes)
bech32     "z1" + bech32(address)
```

Taking the **last** 20 bytes of the digest (rather than the first) is a
deliberate, documented choice: every hash in this system is produced by the same
function, and using a fixed slice of the output keeps address derivation
identical to the rule used for contract addresses.

`ParseAddress` accepts only `z1…` Bech32 with exactly 20 data bytes; a mixed-case
string, wrong HRP or wrong length is rejected (`TestAddressRejectsWrongHRPAndLength`).

## Signatures

Signatures are Ed25519 over a domain-separated digest of the message:

```
signature = Ed25519_sign(private, Digest(domain, message))
```

Verification requires:

1. the public key parses (32 bytes, canonical),
2. the signature verifies,
3. **and for address-bearing messages** the public key derives the claimed
   address (`VerifyAddressSignature`).

Requirement 3 is what stops an attacker from signing an event as somebody else:
a valid signature by a different key fails because the address would not match.

`TestSignVerifyDomainSeparation` proves a signature made for one domain does not
verify in another; `TestAddressSignatureBinding` proves the address binding is
enforced.

## Keystore (ZKS1)

On-disk format:

```json
{
  "version": 1,
  "address": "z1…",
  "publicKey": "<64 hex>",
  "createdAt": "RFC3339",
  "kdf":    {"algo":"pbkdf2-hmac-sha256","iterations":600000,"salt":"<32 bytes hex>"},
  "cipher": {"algo":"aes-256-gcm","nonce":"<12 bytes hex>","ciphertext":"<hex>"}
}
```

Properties, each with a test:

| Property | Enforced by |
|---|---|
| Password ≥ 10 characters | `EncryptKey` (`ErrWeakParams`) |
| KDF iterations ≥ 600,000 | `EncryptKey` and **`Decrypt`** — a hand-edited file cannot weaken the KDF |
| `iterations = 0` means "use the default" | `EncryptKey` (a caller cannot accidentally disable work) |
| Fresh random salt and nonce every encryption | `TestKeystoreIsNonDeterministic` |
| Wrong password and corrupt file are indistinguishable | GCM tag ⇒ `ErrDecrypt` in both cases |
| The address is authenticated data | swapping the address field breaks the tag |
| Unknown or missing JSON fields are rejected | `ParseKeystore` with `DisallowUnknownFields` + explicit checks |
| File mode is 0600 | `SaveKeystore` |
| The seed never appears in the file | `TestKeystoreRoundTrip` asserts the hex seed is absent |
| Only version 1 and the two named algorithms are accepted | `Decrypt` |

**LIMITATION:** there is no key-derivation from a mnemonic (BIP-39 style). Devnet
keys are derived deterministically from a labelled seed via
`Digest("ZNET/v1/devnet-key", label)` and are labelled **TEST ONLY** wherever
they are printed.

## Randomness

All random values (`GenerateKey`, keystore salt, GCM nonce) come from
`crypto/rand`. A failure to read randomness is `ErrNoEntropy` and aborts the
operation — there is no fallback to a weaker source.

## What this build does not claim

- **No encryption of network traffic.** P2P frames are checksummed, not sealed.
- **No zero-knowledge proofs, no threshold signatures, no BLS aggregation.**
  Commit certificates carry one Ed25519 signature per validator.
- **No formal verification** of the Merkle construction or the signature scheme.
  The Merkle implementation is tested against known-answer vectors, including
  odd-node promotion and single-leaf trees.
- **No hardware-wallet or HSM integration.**

These are architectural gaps for a production deployment, listed in
[../BLOCKERS.md](../BLOCKERS.md), not implementation shortcuts.
