> **Repository status:** development / testnet build — **not** a Mainnet, not audited,
> no funds involved. See `BLOCKERS.md` before relying on anything here.
>
> History note: this repository starts from a single import commit. The upstream
> development history is recorded in `HISTORY.provenance.txt`, and a git bundle with
> per-commit history is archived with the v1.0.0-rc3 release.
>
> CI: `.github/workflows/ci.yml` runs gofmt, vet, build, the full test suite, the race
> detector, and five fuzz targets on every push.

# Z NETWORK

A self-contained blockchain ecosystem in Go: BFT consensus, deterministic
execution with a small smart-contract VM, Merkle-backed state with proofs,
staking and governance economics, a p2p network, JSON-RPC, a Go SDK, three CLI
tools, a node-served block explorer, Prometheus metrics and reproducible
packaging.

> **This is a development / testnet release candidate (`v1.0.0-rc1`).**
> It has not been audited. It is not Mainnet. Do not use it to hold value.
> The complete list of what is missing is in [BLOCKERS.md](BLOCKERS.md).

---

## Quick start

```bash
source scripts/env.sh          # Go 1.24.4 from ~/.local/go (see scripts/bootstrap-toolchain.sh)

# 1. build
go build ./...

# 2. run the whole test suite
go test ./...

# 3. every gate, including -race and fuzz smoke
scripts/quality-gates.sh --full

# 4. a real 4-validator network on loopback
scripts/testnet.sh up --validators 4 --block-time 600
scripts/testnet.sh status
#    open the explorer at http://127.0.0.1:26657/explorer/
#    metrics at           http://127.0.0.1:26657/metrics
scripts/testnet.sh down
```

## What it can do (all verified)

| Capability | Where it is proven |
|---|---|
| 4-validator BFT consensus over TCP | `internal/znode/multinode_test.go`, live testnet |
| Full transaction lifecycle: sign → mempool → block → state → index → explorer | `internal/e2e` with real binaries and processes |
| Restart recovery without a genesis file | `TestSingleNodeFullLifecycle` |
| Smart contracts (deploy + call, events in receipts) | live deploy with `contract_deployed` receipt |
| Staking: delegate, undelegate, time-based unbonding | live run, `z_getDelegations` |
| Governance: propose, vote, quorum tally, parameter change | live run, `z_getProposal` |
| JSON-RPC, 24 methods | 18 tests + live checks |
| Explorer UI and Prometheus metrics | headless live UI check, live `/metrics` |
| Reproducible, checksum-verified source package | two byte-identical builds |

## Layout

```
cmd/                 znoded, zwallet, zcli, zrelease
internal/            22 packages (see MODULE_REGISTER.md)
specifications/      11 normative documents describing what the code does
security/            threat model (42 threats with mitigations and residuals)
scripts/             env, quality gates, fuzz campaign, testnet, release
web/explorer/        single-file explorer + headless live UI check
documentation/       release manifest template
```

## Tools

```bash
znd init --home NODE                     # create an encrypted validator key
znd run --home NODE --genesis genesis.json [--seeds IP:PORT] [--moniker NAME]
znd version

zcli status|block|blocks|tx|account|delegations|validators|proposals|params|supply|mempool --rpc URL
zcli devnet --out DIR --validators 4      # generate a devnet (set ZWALLET_PASSWORD)
zcli genesis hash|check --file genesis.json
zcli decode --tx-hex HEX

zwallet generate|address|balance|send|delegate|undelegate|register-validator
zwallet propose|vote|deploy|call|sign        (passwords via --password-file or ZWALLET_PASSWORD)
```

## Read this before trusting anything

| Document | Why |
|---|---|
| [BLOCKERS.md](BLOCKERS.md) | 17 numbered gaps: no audit, no transport encryption, no slashing, no disk-backed state, unsigned artifacts |
| [TEST_REPORT.md](TEST_REPORT.md) | every measured result, plus an explicit list of what is **not** tested (no soak, no load numbers, no partition test) |
| [specifications/README.md](specifications/README.md) | what the code actually does, with the constants it actually uses |
| [security/THREAT_MODEL.md](security/THREAT_MODEL.md) | threats, mitigations, and the risks accepted with eyes open |
| [PROJECT_STATE.md](PROJECT_STATE.md) | current state, capability matrix, measured numbers |

## Design decisions that shaped the code

- **Zero third-party dependencies.** No supply chain surface, offline
  reproducibility, and an audit scope that fits inside this repository.
  Consequence: hand-written Prometheus exposition, PBKDF2 and Bech32.
- **No floats anywhere in the protocol.** Amounts are integer micro-units; every
  addition and multiplication is overflow-checked.
- **Domain-separated hashing.** Every hash carries a length-framed domain string,
  so a digest for one purpose cannot collide with another.
- **The chain is the authority on height.** Consensus engines are positioned from
  the chain, never from a private counter — this was a real liveness bug found
  under CPU contention and fixed.
- **Failures are receipts, not exceptions.** A failed transaction is included,
  charged, and consumes a nonce — exactly like a revert on a production chain.
- **Mocks only in tests, and labelled as such.** Every number in the reports came
  from a process that ran.

## Honest status in one sentence

The system is real, integrated, tested and reproducible — and it is a testnet,
because it has no audit, no transport encryption, no slashing and no production
storage layer, all of which are documented rather than glossed over.

## License

No license file is included in this build. Add one before distributing, or treat
the code as all-rights-reserved until then.
