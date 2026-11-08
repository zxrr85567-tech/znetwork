# Z NETWORK — Dependency Graph

Generated from `go list -f '{{join .Imports "\n"}}'` for every package. The graph
is acyclic by construction; `go vet` and the build would fail otherwise.

## Layered view

```
layer 0  (no internal dependencies)
         zcrypto      zencoding      zmetrics      zbuild

layer 1
         zmerkle  -> zcrypto, zencoding
         zvm      -> ztypes
         ztypes   -> zcrypto, zencoding

layer 2
         zstate   -> zcrypto, zmerkle, ztypes
         zmempool -> zstate, ztypes
         zstore   -> zcrypto, zencoding, zstate, ztypes
         zindexer -> zcrypto, ztypes
         zdevnet  -> zcrypto, ztypes

layer 3
         zconsensus -> zcrypto, ztypes
         zexec      -> zcrypto, zstate, ztypes, zvm
         znet       -> zconsensus, zcrypto, zencoding, ztypes

layer 4
         znode -> zconsensus, zcrypto, zexec, zmempool, zmerkle, zmetrics,
                  znet, zstate, zstore, ztypes

layer 5
         zrpc      -> zcrypto, zexec, zmempool, zmetrics, zstate, ztypes
         zsdk      -> zcrypto, ztypes, zvm

layer 6
         znodedcmd  -> zcrypto, zindexer, zmetrics, znet, znode, zrpc, zstore, ztypes
         zwalletcmd -> zcrypto, zsdk, ztypes
         zclicmd    -> zcrypto, zdevnet, zsdk, ztypes

layer 7
         cmd/znoded  -> znodedcmd
         cmd/zwallet -> zwalletcmd
         cmd/zcli    -> zclicmd
         cmd/zrelease -> zbuild
```

## Why the layering matters

- **`ztypes` does not import `zstate` or `zexec`.** Types are data; state and
  execution depend on them, never the reverse. That keeps encodings stable
  independent of execution changes.
- **`zconsensus` does not import `znode`.** The engine is a pure state machine
  over votes and proposals; the node drives it. This is what makes consensus
  testable without a network (18 unit tests) and reusable in the sync path.
- **`zstate` does not import `zexec`.** State offers primitives; execution
  composes them. A state test never has to construct an execution engine.
- **`znode` is the only package that touches all of chain, mempool, net, store
  and consensus.** Everything above it (RPC, SDK, CLIs) talks to interfaces it
  exposes (`Backend`, `ReceiptBackend`), which is why `zrpc` can be tested against
  a lightweight fake chain.
- **`znet` imports `zconsensus`** only for the vote/proposal types it encodes, not
  for engine behaviour.

## Fan-in (how central a package is)

| Package | Imported by | Risk if changed |
|---|---|---|
| `ztypes` | 14 packages | any encoding change is a **protocol break** |
| `zcrypto` | 13 | domain strings are consensus-critical |
| `zstate` | 5 | state encoding is consensus-critical |
| `zconsensus` | 2 | vote digest is consensus-critical |
| `zexec` | 2 | gas/state transitions are consensus-critical |
| `znode` | 2 | orchestration only |
| everything else | ≤ 3 | local |

Anything in the top five rows requires a protocol version bump and a re-run of
the full multi-node suite, because two nodes with different rules cannot agree.

## External dependencies

```
go 1.24
build:    none
test:     none
runtime:  crypto/* (stdlib), encoding/*, hash/*, net/http, log/slog, flag
```

`go.mod` has no `require` block and there is no `go.sum` content, which is the
strongest form of dependency control available.
