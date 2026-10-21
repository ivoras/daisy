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

## Block tree and fork choice

- `consensus.go`: scoring (in turn 2, out of turn 1), `isBetterTip`, and `validateBlock`/`validateKeyOps`, which are pure
  functions of the block and a `keySet` (`keyset.go`). Validation never writes anything.
- `chainstate.go`: `acceptBlock` is the only way a block enters the chain (P2P and `signimportblock` both use it).
  It serializes on `chainLock`, stores blocks of other branches in `sideblocks`, and `reorganize`s when a branch wins.
  Blocks whose predecessor is unknown wait in `orphanPool` and the missing block is requested from the peer.
- The `pubkeys` table is the key set as of the tip, and each key records the heights of the blocks that added and
  revoked it, so it can be rolled back (`keySet.rewind`, `dbRewindKeys`). Keys with a negative height are local only.
- When changing any of this, run the tests in `forkchoice_test.go` and read the ordering notes: blocks must be built
  parent-first, and results for equal-score branches are decided by hash.
- `schema_version` (config table) and `reconcileChainState` upgrade databases of older versions. Bump
  `currentSchemaVersion` and extend the reconcile when the derived data changes.

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

## P2P concurrency rules

- Message handlers (`handleMsgHello`, `handleBlockHashes`, ...) run inside the connection's own
  `handleConnection` loop. They must reply with `p2pc.send()`, which only appends to an unbounded
  (but capped) outbox and never blocks; the loop writes it out after each handled message. Never send to
  a channel that the same goroutine is the only reader of.
- Never block while holding `p2pPeers.lock`: use `p2pPeers.Snapshot()` and work on the copy.
- Never block on the coordinator: use `p2pCtrlSend()` (drops the hint if the coordinator is busy).
- Every network operation needs a timeout: `p2pDialer`, `p2pHTTPClient`, and the write deadline in `sendMsg`.

## Untrusted input from peers

- Limits live in `internal/daisy/p2pnet.go`: messages 64 MiB (`p2pMaxMessageSize`), blocks 1 GiB
  (`p2pMaxBlockSize`), inline (`zlib-base64`) blocks only up to 32 MiB. Read peer data through `readLine`,
  `copyExactly` and the `download*Block` helpers, never with unbounded `ReadBytes`/`io.Copy`/`io.ReadAll`.
- The `http` block encoding: the URL from the peer is only a hint. `blockHTTPURL` keeps its port and block
  number, but always connects to the peer's own address; redirects are not followed.
- Bans: `badPeers` holds dialed `ip:port` addresses (set for outbound duplicate/self connections),
  `badHosts` holds IPs which violated the protocol (oversized or malformed messages). Inbound connections have
  random source ports, so never key a ban on an inbound connection's `ip:port`.

## Known problems (not yet fixed)

These were found in a review and are tracked here so they aren't mistaken for intended behaviour:

- P2P has no encryption or authentication. Peer discovery still trusts what peers gossip about who is alive
  (it only dials them, and remembers them after a valid hello), so a few malicious nodes can crowd out others.
- There is no protocol version negotiation: `hello.version` is only logged. Compatibility relies on unknown JSON
  fields and unknown messages being ignored. A rule change would need a protocol number and an activation height.
- Fork choice is by score only; there is no finality, and a branch deeper than `maxReorgDepth` is never accepted,
  so a node that was offline for longer than that during a fork must be re-created (`daisy pull`).
- `mining.CountStartZeroBits` counts zero bits across all bytes rather than only leading ones (the PoW
  code is unused).
- `cfg.httpPort` is unexported, so `http_port` in the JSON config file is silently ignored.
