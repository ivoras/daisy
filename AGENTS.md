# AGENTS.md

Guidance for AI coding agents (and humans) working on Daisy: a proof-of-authority blockchain
whose blocks are SQLite database files. See `README.md` for the design and user documentation.

## Toolchain

- Go **1.27** (see `go.mod`; `GOTOOLCHAIN=auto` downloads it if needed). Module path: `github.com/ivoras/daisy`.
- The only third-party dependency is `github.com/mattn/go-sqlite3`, which needs **cgo** and a C compiler.
  Its compiler warnings (`sqlite3-binding.c`) during `go vet`/`go build` are expected noise.

## Commands

```sh
go build ./cmd/daisy      # builds ./daisy
go vet ./...
go test -race ./...
gofmt -l .                # must print nothing
```

Run all of these before sending a PR.

Manual smoke test (uses a throwaway data dir; never point it at your real `~/.daisy`):

```sh
d=$(mktemp -d)
./daisy -dir $d/n1 newchain chainparams.example.json   # create a private chain
./daisy -dir $d/n1 signimportblock some.db             # add a block
./daisy -dir $d/n1 query "SELECT * FROM sometable"
./daisy -dir $d/n1 -port 12017 -http-port 12018 &       # serve it
./daisy -dir $d/n2 pull http://localhost:12018          # replicate the genesis block
```

## Layout

```
cmd/daisy/            main package: just calls daisy.Main()
internal/daisy/       the node: config, CLI actions, blockchain rules, system DBs, P2P, block HTTP server
internal/dcrypto/     stateless crypto: ECDSA P-256 sign/verify, "1:<sha256>" public key hash format
internal/util/        generic helpers (hashing, StrIfMap JSON accessors, StringSetWithExpiry, mutex helper)
internal/mining/      unused proof-of-work primitives
internal/assets/      go:embed'ed genesis block of the default chain (genesis.db)
docs/                 extra documentation links
```

Rule of thumb: anything that touches the global state (`cfg`, `mainDb`, `privateDb`, `chainParams`,
`p2pPeers`) lives in `internal/daisy`; pure functions go in the leaf packages, which must not import
`internal/daisy`.

## Conventions

- Match the surrounding code: plain `log` package logging, error strings as in the neighbouring code,
  short doc comments on functions.
- Prefer the standard library (`slices`, `maps`, `errors.Is`, `os.ReadFile`, `net/http` pattern mux, `any`).
  Don't add dependencies without a strong reason.
- Don't run networking code in tests; the P2P layer uses package-level singletons and dials real hosts.
  Unit-test the pure packages and the deterministic functions in `internal/daisy`.

## Things that are easy to break

- **Consensus compatibility.** Block hashes are SHA-256 of the raw SQLite file, and signatures are
  ASN.1-DER ECDSA P-256 over the raw 32-byte hash. The hard-coded genesis block hash/signature in
  `internal/daisy/blockchain.go` must keep verifying against `internal/assets/genesis.db`
  (`TestEmbeddedGenesisBlock`). `QuorumForHeight` is consensus-critical; it is pinned by a test against
  the README table. Changing any of these forks the chain.
- **Never modify `internal/assets/genesis.db`** (or anything that changes its bytes, including opening it
  read-write with SQLite): its hash is the chain's identity.
- **Block files on disk** are `blocks/<height/65536 as %04x>/block_<height as %08x>.db`. Any code that
  creates block 0 must create the shard directory first (`blockchainEnsureBlockDir`).
- **Data directory** (`-dir`, default `~/.daisy`) holds `daisy.db` (chain index, pubkeys, peers),
  `private.db` (private keys, mode 0600), `chainparams.json` (custom chains) and `blocks/`.

## Known problems (not yet fixed)

These were found in a review and are tracked here so they aren't mistaken for intended behaviour:

- `checkAcceptBlock` writes/revokes public keys while validating, before the block is accepted
  (not transactional). Key-op quorum is checked as `>=` on accept but `==` on startup verification,
  and distinct signers are only enforced by the block's own `_keys` primary key.
- `blockchainVerifyEverything` doesn't check `prev_hash` linkage between consecutive blocks and
  resolves signing keys against the final key set rather than the set at that height.
- No fork handling: a block at an existing height is rejected ("longest chain wins" is not implemented).
- P2P: peer-supplied `http` block URLs are fetched without restriction (SSRF), block/line sizes are
  unbounded, there are no dial timeouts, and `badPeers` matching for inbound connections includes the
  ephemeral source port. Several channel/lock interactions in `p2p.go`/`p2pcoordinator.go` can deadlock,
  and `handleConnection` shares `exit`/`err` between goroutines without synchronisation.
- `mining.CountStartZeroBits` counts zero bits across all bytes rather than only leading ones (the PoW
  code is unused).
- `cfg.httpPort` is unexported, so `http_port` in the JSON config file is silently ignored.
