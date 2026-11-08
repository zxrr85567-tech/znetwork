# Release Process Specification

**Implementation:** `internal/zbuild/`, `scripts/quality-gates.sh`,
`scripts/fuzz-campaign.sh`, `scripts/testnet.sh`, `scripts/release.sh`
**Verified by:** `internal/zbuild/zbuild_test.go`

The rule this process enforces: **a release artifact must be reproducible from a
commit, and every claim about it must be checkable against a hash.** Nothing is
described as a release that was not built from the repository's tracked files and
whose checksums were not verified after writing.

## Gates that must pass before packaging

`scripts/quality-gates.sh`:

```
gofmt          no unformatted files
go vet         clean
go build       all packages
go test        full suite (unit + integration + process-level e2e)
coverprofile   written for the report
--full adds:   -race suite, fuzz smoke on every target
```

`scripts/quality-gates.sh --full` is what was run for this release candidate; the
recorded output is in [../TEST_REPORT.md](../TEST_REPORT.md). A gate failure is
never waived silently: it is either fixed or recorded as a blocker with the exact
command and output.

## Packaging

`internal/zbuild.Create` produces `releases/znetwork-<version>.zip`:

| Rule | Why |
|---|---|
| Only `git ls-files` entries are packaged | the zip matches a commit, not a working tree |
| Excludes `.git/`, `bin/`, `data/`, `*.zip`, `*.log`, `*.key.json` | no chain data, build output or keys in a release |
| Entries sorted by path | deterministic order |
| Timestamps normalised to 1980-01-01 | two builds of the same tree are byte-identical |
| Per-file SHA-256 in the manifest | every file is individually verifiable |
| Archive SHA-256 in the manifest | the package itself is verifiable |

A test asserts that building the same tree twice produces **byte-identical**
archives (`TestCreateIsDeterministic`), which is the property that makes the
checksum meaningful.

## Manifest

`releases/znetwork-<version>.manifest.json`:

```json
{
  "package": "znetwork-v1.0.0.zip",
  "version": "v1.0.0",
  "created_utc": "...",
  "go_version": "go1.24.4 linux/amd64",
  "module_path": "znetwork",
  "file_count": 123,
  "total_bytes": 456789,
  "archive": {"path": "...", "size": ..., "sha256": "...", "mode": "0644"},
  "files": [{"path": "internal/zstate/state.go", "size": ..., "sha256": "...", "mode": "0644"}],
  "build_command": "go build ./... && go test ./..."
}
```

## Checksums

`releases/SHA256SUMS` lists `<sha256>  <filename>` for every artifact in the
release directory. `zbuild.VerifySHA256SUMS` re-hashes each listed file and
fails on the first mismatch; the release script runs it after writing, so a
corrupted copy is caught before the artifact is announced rather than after.

## Release record

`releases/RELEASE_MANIFEST.md` states, for each release:

- version, commit hash, Go toolchain, date;
- what is in the package and what is deliberately excluded;
- the gate results as actually observed;
- **honest status**: which parts are complete, which are stubs, and the explicit
  statement that this is a development/testnet release and not Mainnet.

## Versioning

`Version = "1.0.0"` is printed by `znd version`, `zwallet version` and
`zcli version`, and included in `z_nodeInfo`. The protocol version is separate
(`ztypes.ProtocolVersion = 1`) and appears in every frame, transaction and
genesis; changing either without changing the other is a bug.

## What this process does not provide

- **No signed releases.** There is no PGP or minisign key in this repository;
  checksums are integrity, not authenticity. Anyone who can rewrite the zip can
  rewrite the sums. A release-signing key is a blocker for any public release.
- **No multi-platform binaries.** The package is source; binaries are built by
  the consumer with the documented Go version.
- **No dependency lockfile.** The module has zero third-party dependencies, so
  `go.sum` is empty by construction; `go mod verify` is trivially satisfied but
  is still run as a gate.
- **No distribution hosting.** `releases/` is a directory in this workspace; the
  GitHub remote is not connected (see BLOCKERS.md B-001), so publication is
  explicitly not claimed.
