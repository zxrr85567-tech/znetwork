# ENVIRONMENT DISCOVERY REPORT — Z NETWORK

**Report ID:** EDR-0001
**Executed:** 2026-11-08 (Asia/Tehran local date)
**Method:** every statement below was produced by executing a command in the live sandbox; none is assumed.

---

## 1. Verified host capabilities

| Property | Verified value | Command used |
|---|---|---|
| OS / kernel | Debian GNU/Linux 13 (trixie), kernel 6.1.189+, x86_64 | `cat /etc/os-release`, `uname -a` |
| CPU | 2 vCPU | `nproc` |
| RAM | 1.9 GiB total, ~1.5 GiB available | `free -h` |
| Disk | 25 GiB volume, 21 GiB free | `df -h /home/user` |
| Network egress | Working — `proxy.golang.org` HTTP 200, `go.dev/dl` reachable | `curl -sI` |
| Workspace root | `/home/user`, empty at session start (0 files) | `ls -la`, `find` |

## 2. Installed toolchain (after bootstrap)

| Tool | Version | Status |
|---|---|---|
| Go | **go1.24.4 linux/amd64** | Installed by `scripts/bootstrap-toolchain.sh` into `$HOME/.local/go` (verify: `~/.local/go/bin/go version`) |
| Node.js | v20.20.2 | Present (used for the JavaScript SDK and integration harness) |
| npm | 10.8.2 | Present |
| Python | 3.13.16 | Present (scripting/glue only) |
| Git | 2.47.3 | Present (local repository; no remote configured) |
| gcc | 14.2.0 | Present (`CGO_ENABLED=1` builds and `-race` possible) |
| make, jq, zip, unzip, sha256sum | present | Build/packaging/verification |

## 3. Unsupported / unavailable capabilities

| Capability | Verified state | Consequence |
|---|---|---|
| **Pre-existing Z Network artifacts** | **None.** `find /home/user -type f` returned 0 files. No `Z-NETWORK-FOUNDATION.zip`, no `Z_AGENT_08_FINAL.zip`, no AGENT_08 source, no fixtures, no reports, no registers. | The recovery step in the directive cannot recover anything. AGENT_08 is **not** a recoverable artifact; it is re-implemented here from scratch and registered as `AGENT_08 v1.0.0` of this repository. No prior claim is treated as evidence. |
| Rust / Cargo | not installed | Rust is not used (see ADR-0001). |
| Docker / container runtime | not installed | Container deployment is authored but **cannot be executed here**; recorded as unverified. |
| sqlite3 CLI / C library | not installed | Index store uses a purpose-built pure-Go file store instead of SQLite (no external dependency at all). |
| GitHub connector | No connected apps in this conversation | **No remote repository operations will be attempted or claimed.** All work is local Git with release bundles and patch files. |
| Public network reachability for a real testnet | Egress exists, but no funded/authorized infrastructure and no owner authorization to publish | Only local (loopback) multi-node networks are built and tested. |
| Hardware wallet | No USB/HSM device | Hardware-wallet *architecture* documented as interface + adapter; not executable here (labelled Experimental/Unverified). |

## 4. Resource constraints that shape the plan

* **1.9 GiB RAM / 2 vCPU** — local test networks are limited to 4 nodes; no long benchmark sweeps.
* **Snapshot limits** — workspace snapshots exclude `.local`, `dist`, `build`, `node_modules`, etc. and are capped (~128 MB / 10k files). Therefore the Go toolchain lives in the excluded `$HOME/.local` and is re-created by `scripts/bootstrap-toolchain.sh`. Build outputs go to `bin/` (`dist`-independent) and release ZIPs are produced under `releases/` but are gitignored.
* **No persistent secrets** — no private keys are committed; dev keys are generated deterministically by a seeded devnet genesis tool and are explicitly labelled as test-only.

## 5. Execution strategy adopted

1. **Single language for the core (Go 1.24)** — one toolchain, static binaries, no runtime deps, `net/http`, `crypto/ed25519`, `crypto/sha256` in stdlib.
2. **Zero third-party Go dependencies** — no module supply chain (mitigates threat T-23/T-24 by construction) and reproducible offline builds. Bech32 (BIP-173), PBKDF2 (RFC 8018) and the VM are implemented and tested against published vectors.
3. **Modules are Go packages under one module (`znetwork`)** with frozen public interfaces, not separate repos: with a single execution environment there is no isolation benefit, and one module guarantees a single coherent protocol version.
4. **Sequential execution** (only one agent executes here); the directive's parallel wave model is preserved as a *dependency* model, and every wave is closed with evidence before the next opens.
5. Because the workspace was empty, Wave 0 "recovery" reduces to: create the repository, freeze interfaces, and honestly register that no prior evidence existed.

## 6. Blockers recorded at session start

| ID | Blocker | Owner action required |
|---|---|---|
| B-001 | No prior Z Network artifacts to verify (AGENT_08 claims unverifiable) | Provide the original ZIPs if they exist; publication claim remains unverified |
| B-002 | No GitHub authorization | Connect the GitHub app if remote publication is wanted |
| B-003 | No paid infrastructure authorization (zero budget) | Mainnet/testnet hosting decision |
| B-004 | Economic parameters are proposals, not approvals | Owner sign-off required before any real-value use |
| B-005 | No independent external security audit possible from inside this environment | Commission external audit before Mainnet candidacy |

## 7. Honesty statement

This report supersedes any narrative that assumes a pre-existing codebase. Everything subsequently claimed as *implemented* is backed by source files in this repository, and everything imported into `TEST_RESULTS.md` is backed by a command that was actually executed.
