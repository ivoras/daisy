# TODO: cryptography and P2P review findings

Found in a review of `internal/dcrypto`, consensus and key ops, the block tree, the P2P handlers, the coordinator,
the block HTTP server, discovery and `daisy pull`. Things already listed under "Known problems" in `AGENTS.md`
(no encryption or authentication on the wire, no finality) are not repeated here.

There are no deployed uses of Daisy yet, so protocol and consensus changes may be made freely (bump
`p2pProtocolVersion`/`p2pMinProtocolVersion`, change validity rules). `internal/assets/genesis.db` still must not
change: rules that the embedded genesis block can't satisfy must exempt height 0.

Order of work: 1) P2P resource limits (#5–#8, #12, #13), 2) peer management and SQLite (#10, #11, #9),
3) consensus (#1–#4).

## Cryptography and consensus

- [ ] **#1 Key-op signatures don't say what they approve (high, confirmed).**
  `dcrypto.SignPublicKeyHash` signs only `sha256(pubkey)`: not the operation ("A"/"R"), not the chain, not the
  height. The signatures that added a key are also valid signatures for revoking it, so any one signatory can copy
  the quorum's "A" signatures from the block that added key K into an "R" op in their own block and revoke K.
  Confirmed with a test at height 200 (quorum 10): `validateKeyOps` accepted the replayed revoke. Key ops can also
  be replayed across chains that share keys, and across forks.
  Fix: sign a domain-separated message, e.g. `sha256("daisy-keyop" ‖ genesisHash ‖ op ‖ pubkeyHash)`. The genesis
  block keeps the old format.

- [ ] **#2 Any signatory can win fork choice whenever they like (high).**
  Out-of-turn blocks score 1 and nothing limits how fast blocks are made (no timestamp rule, no "signed recently"
  rule as in Clique). One rogue signatory can sign hundreds of blocks in seconds and replace up to `maxReorgDepth`
  (128) blocks of honest history.
  Fix: a required, hashed block timestamp that must increase by a minimum period and not be too far in the future,
  plus a Clique-style limit (a signer may sign at most one of every ⌊N/2⌋+1 consecutive blocks) where it doesn't
  halt small chains.

- [ ] **#3 Key ops can freeze for good (medium).**
  `QuorumForHeight` jumps to 10 at height 149 and keeps growing. A chain with fewer active signatories than that can
  never add or revoke a key again. Cap the quorum by the number of active signatories (e.g. ⌊active/2⌋+1) and update
  the README table and its test.

- [ ] **#4 Smaller crypto points (low).**
  - `DecodePublicKeyBytes` accepts any ECDSA curve; require P-256.
  - `decodePublicKeyHash` doesn't check the `1:` type prefix or that the hash is 32 bytes. ECDSA truncates over-long
    hashes, so require exactly 32 bytes everywhere a hash is signed or verified.
  - Signatures aren't normalized to low-S, so a third party can alter `HashSignature`. Sign with low-S. Requiring it
    on verification would reject the embedded genesis signature (it is high-S), so at most require it above height 0.
  - No domain separation between block-hash signatures and key-op signatures (folded into #1).
  - `daisy pull` trusts whatever the server sends, over plain HTTP: the fetched chainparams supply the genesis
    hash, the creator key and the signature, so a man in the middle can substitute a whole chain and its seed and
    peer-list URLs. Add an `-expect-genesis <hash>` option and print the hash for the user to confirm.

## P2P resource exhaustion

- [x] **#5 No read deadline anywhere (high).**
  Nothing calls `SetReadDeadline`. A connection that never says hello, or goes quiet, stays open forever. Inbound
  slots (`p2pMaxInbound = 128`) aren't limited per IP, so one host can hold all of them with idle sockets.
  Fix: a hello deadline (~10–30 s), an idle deadline of a few minutes reset on each message, and a per-IP inbound
  cap (~4).
  Done: 20 s hello deadline, 5 min idle deadline reset on every message (nodes `ping` every 30 s), at most 4 inbound connections per IP (`p2pMaxInboundPerHost`); protocol 3.

- [x] **#6 `getblockhashes` has no range limit (high).**
  `handleGetBlockHashes` returns whatever `[min, max]` is asked for; repeated requests can fill the outbox (up to
  262,144 messages) and run the node out of memory. Honest sync breaks too: `handleSearchForBlocks` asks for
  `[tip-32, peerHeight]` in one message, and past ~850k blocks the reply exceeds the 64 MiB message limit and the
  receiver bans the honest peer. Fix: clamp the range on both sides (e.g. 2,000) and page through it.
  Done: `p2pMaxBlockHashes` = 2000 enforced by the responder (`clampHashRange`) and the requester, which pages through the peer's chain (`syncState` in `p2psync.go`); tested by syncing 23 blocks in pages of 4.

- [x] **#7 `getblock` can use up file descriptors and memory (high).**
  Protocol 2: every request opens the block file at once (`newBlockSender`), so thousands of requests mean thousands
  of open files (EMFILE breaks SQLite and `accept()` too). Protocol 1 with inline blocks: each request builds up to
  ~43 MiB of base64 in memory before queuing it. Fix: cap outstanding senders per connection (2–4), open the file
  on the first `nextMessage`, drop duplicate requests for a block already queued.
  Done: 2 active senders per connection, extra requests wait as hashes (max 4096), duplicates and unknown blocks are dropped, the file is opened on the first `nextMessage` (a vanished file ends the transfer); the inline/http encodings are removed.

- [x] **#8 Unsigned blocks are downloaded in full, parsed by SQLite, and only then checked (high).**
  The `block` message carries `hash` and `hash_signature`; verifying the signature against the active keys is cheap
  and could happen before anything is downloaded or reserved. Today an attacker can push 1 GiB transfers unasked
  (two connections × two transfers fill the global 4 GiB `p2pIncomingBytes` budget, and a chunk every 2 minutes
  keeps it full, which stops chunked downloads from every honest peer), make `acceptBlock` hash and open a 1 GiB
  file while holding the global `chainLock`, and fill the 16-slot orphan pool with unsigned junk.
  Fix: check the announced signature against the active keys first; only accept transfers for blocks we asked for
  or count the budget per host; check the received file's hash equals the announced one before `acceptBlock`.
  Done: a `block` is only received if we asked for it or its signature verifies against any key in `pubkeys`; per-host (1 GiB) and global (4 GiB) accounting; the received file is hashed against the announced hash before `acceptBlock`.

- [ ] **#9 SQLite opens untrusted files without hardening (medium).**
  `dbOpen(..., true)` is only `mode=ro`. A crafted block can make `_meta` or `_keys` a view running expensive
  recursive CTEs while `chainLock` is held. Fix: `PRAGMA trusted_schema=OFF`, `SQLITE_DBCONFIG_DEFENSIVE`,
  `PRAGMA query_only`; check that `_meta` and `_keys` are real tables; consider `PRAGMA quick_check` and a query
  time limit.

## Eclipse and peer management

- [ ] **#10 A peer can get an honest peer banned by claiming its ID (medium-high).**
  Every message we send carries `p2p_id`, so anyone can learn any node's ID. An attacker connects first claiming
  honest node H's ID; when we dial H, `handleMsgHello` treats H as the duplicate and puts its address in `badPeers`
  for 15 minutes. Fix: use the ID only to detect connections to ourselves, never to ban anyone else; if
  deduplication stays, close the newer connection without banning.

- [ ] **#11 Two IPs can make us shun honest peers (medium).**
  `selfInfo.observe`: with two IPs an attacker can make us believe we are at honest peer X's address, so
  `isSelf(X)` is true and we never dial X; up to 16 addresses. Votes never expire, so the first 16 claimed addresses
  stay forever. Fix: take `observed_host` only from outbound, handshaken peers; one vote per peer host overall;
  expire entries; don't use it in `isSelf` for dialing.

- [x] **#12 Repeated hellos are processed every time (low).**
  Each hello re-submits `my_peers`, adds observation votes and can change the claimed protocol. Process only the
  first hello.
  Done: `handleMsgHello` processes only the first hello of a connection.

- [x] **#13 Fake block hashes cost us requests (low).**
  `handleBlockHashes` requests every unknown hash: a 64 MiB map of fake hashes queues ~900k `getblock` messages, and
  the 5 s expiry of `recentlyRequestedBlocks` repeats it every 5 s. Cap the entries handled per message and only
  accept heights near the range we asked for.
  Done: at most `p2pMaxBlockHashes` entries are handled per `blockhashes`, heights must be in the range last asked for or within [tip-128, tip+2000], non-hash values are ignored; hashes far above the tip start a paged search.

## What looks solid

- ECDSA uses `SignASN1`/`VerifyASN1` with `crypto/rand`; the private DB is mode 0600; loaded keys are checked
  against their hash.
- `blockHTTPURL` always connects to the peer's own address, and redirects are refused.
- Reads are bounded (`readLine`, `copyExactly`, `LimitReader` against zlib bombs, chunk order and size checks).
- The outbox never blocks; network locks are handled carefully.
- The address book keeps peer-supplied addresses in the "new" tier and limits them per source and network group.
- Bans are by host, never by an inbound `ip:port`.
