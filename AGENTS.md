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
./daisy -dir $d/n2 pull -expect-genesis <genesis hash> http://localhost:12018   # replicate the genesis block
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

## Protocol, address book, NAT

- `docs/PROTOCOL.md` is the reference. Change it with the code. `p2pProtocolVersion` / `p2pMinProtocolVersion` (in
  `p2pprotocol.go`) are bumped only when a peer must know that it may use something new, or must be kept away;
  adding an optional field or message needs no bump (unknown ones are ignored). Gate new behaviour on `p2pc.protocol`.
- Blocks go through `blockSender` (a `lazyMessage` in the outbox: one chunk per flush pass so the connection keeps
  reading; the file is opened on the first `nextMessage`). A peer's `getblock` requests go through `requestBlockToSend`:
  2 senders at a time, the rest wait as hashes (bounded), duplicates are dropped. Received chunks are checked for order and
  total size, bounded per connection, per host and globally (`p2pIncoming`), and the finished file is hashed before `acceptBlock`.
- Protocol 3 is the only one spoken (`p2pMinProtocolVersion`); the `http` and `zlib-base64` block encodings are gone.
  `getblockhashes`/`blockhashes` are limited to `p2pMaxBlockHashes` and walked page by page (`p2psync.go`, `syncState`).
- `addrbook.go` is the only place that writes the `peers` table (`abAdd`, `abGood`, `abFailed`, `abSelect`...). Peers
  are made known by gossip, seeds, lists and mDNS through `p2pCtrlConnectPeers` -> `handleConnectPeers`, which only puts them
  in the book; `maintainConnections` (every tick, and after new addresses) decides whom to connect to. An address
  becomes "tried" only after a valid hello.
- `nat.go` (UPnP/NAT-PMP, `-upnp`) and `mdns.go` (`-mdns`) are opt-in and start from `run.go`; `nat.go` talks to the
  router through the `portMapper` interface so it's tested with a fake. `DAISY_TEST_MDNS=1` enables a test that needs
  a network with multicast.
- `selfInfo` is what peers tell us about ourselves (the address they see, whether they could connect). It needs a
  quorum of different peers before it's believed, and it's only informational or used to avoid connecting to ourselves.

## Block tree and fork choice

- `consensus.go`: scoring (in turn 2, out of turn 1), `isBetterTip`, and `validateBlock`/`validateKeyOps`, which are pure
  functions of the block, a `keySet` (`keyset.go`) and a `blockContext` (genesis hash, the parent's timestamp, "now", the
  block period, the signers of the previous blocks of the branch). Validation never writes anything. The comment at the top
  of the file describes all the rules and why they are there; read it before changing any of them.
- Rules above height 0 (the genesis block is pinned by its hash, so the rules it can't satisfy exempt it): a `Timestamp` in
  `_meta` at least `block_period_seconds` after the parent's and at most `maxFutureDrift` ahead of the local clock (replaying
  the chain in `blockchainVerifyEverything` passes no "now", so that check is skipped); a signer may not have signed any of
  the previous `floor(N/2)` blocks of the branch (`recentSignersOf`, `recentSignerWindow`); low-S signatures; key ops signed
  over `dcrypto.KeyOpMessage` (op, genesis hash, key hash); key op quorum is `EffectiveQuorum` (`QuorumForHeight` capped to a
  majority of the active signatories). The genesis block's key ops use the old `SignPublicKeyHash` format, also in `newchain`.
- Tests: with `testChain`, the timestamps of the blocks advance a second per block from a genesis a day in the past
  (`newBlockAt` sets one explicitly); `tc.next(prev)` picks a signer who is allowed to sign (in turn if possible), because
  `tc.inTurn` may return a signer that signed too recently. A scenario with two signatories can't have one of them sign
  twice in a row: use three.
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
  (`TestEmbeddedGenesisBlock`, `TestGenesisBlockKeepsTheOldFormat`): that block has key ops in the old format, a high-S
  signature and no Timestamp, so every new rule must exempt height 0. `QuorumForHeight` and `EffectiveQuorum` are
  consensus-critical; the first is pinned by a test against the README table. The key op message
  (`dcrypto.KeyOpMessage`) is consensus-critical, too. Changing any of these forks the chain.
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
- Every network operation needs a timeout: `p2pDialer` and the write deadline in `sendMsg`. A connection must say hello
  within `p2pHelloTimeout`, and then be heard from within `p2pIdleTimeout` (every message resets it; nodes `ping` each other
  every `p2pPingInterval`). At most `p2pMaxInboundPerHost` inbound connections per IP.

## Untrusted input from peers

- Limits live in `internal/daisy/p2pnet.go`: messages 64 MiB (`p2pMaxMessageSize`), blocks 1 GiB
  (`p2pMaxBlockSize`). Read peer data through `readLine` and the chunk handlers, never with unbounded
  `ReadBytes`/`io.Copy`/`io.ReadAll`.
- A `block` message is only acted on (budget reserved, temp file created) if we asked that peer for the block (`noteRequested`)
  or its `hash_signature` verifies against a key in the `pubkeys` table, whatever the key's state (`signedByKnownSignatory`).
  `blockhashes` heights must be in the range we asked for or near our tip, and at most `p2pMaxBlockHashes` are handled.
  A second `hello` on a connection is ignored.
- Bans: `badPeers` holds dialed `ip:port` addresses (set for outbound duplicate/self connections),
  `badHosts` holds IPs which violated the protocol (oversized or malformed messages). Inbound connections have
  random source ports, so never key a ban on an inbound connection's `ip:port`.

## Known problems (not yet fixed)

These were found in a review and are tracked here so they aren't mistaken for intended behaviour:

- P2P has no encryption or authentication. Peers can still lie about who is alive; the address book limits how much of
  it one network or one source can fill, and addresses are remembered as tried only after a valid hello.
- The p2p protocol is versioned, but the *consensus rules* are not: a change in which blocks are valid would fork the chain
  and needs an activation height in `chainparams.json` that doesn't exist yet. (The rules about timestamps, signing
  frequency and key op signatures were added before any chain was deployed, so they apply from height 1.)
- `chainparams.json` (including `block_period_seconds`) is not covered by the genesis block hash, and `daisy pull` takes it
  from an unauthenticated server; `-expect-genesis` only pins the genesis hash.
- With few signatories the recently-signed rule needs most of them online: with two, both (see `consensus.go`).
- Fork choice is by score only; there is no finality, and a branch deeper than `maxReorgDepth` is never accepted,
  so a node that was offline for longer than that during a fork must be re-created (`daisy pull`).
- `mining.CountStartZeroBits` counts zero bits across all bytes rather than only leading ones (the PoW
  code is unused).
- `cfg.httpPort` is unexported, so `http_port` in the JSON config file is silently ignored.
