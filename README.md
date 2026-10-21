# Daisy - a proof of authority blockchain where blocks are SQLite databases

Current status: 

* early alpha / nearly feature-complete wrt blockchain.
* the p2p code is home-made (see [docs/PROTOCOL.md](docs/PROTOCOL.md)), it works across NATs by connecting out (blocks are transferred over the connection which the node itself has opened), and can ask the router to forward ports (`-upnp`). A node behind a NAT that can't forward ports can fetch and publish blocks, but others can't connect to it. There is no encryption or authentication of the p2p connections yet.

Documentation and articles:

* [Read Daisy's whitepaper here](https://docs.google.com/document/d/1YnFKQaXjTBPZ1e-x8tQ2oxddIoc0P-mfQ4hjUuuNzS4/edit?usp=sharing) to find out what it does and how to get started.
* [Read how to start custom / private Daisy blockchains here](https://steemit.com/blockchain/@ivoras/creating-a-custom-private-sql-blockchain-with-daisy).
* [Demo project: putting Wikipedia on the blockchain](https://steemit.com/blockchain/@ivoras/putting-wikipedia-on-the-blockchain)
* [How to use "daisy pull"](https://steemit.com/blockchain/@ivoras/blockchain-pull)

Private hybrid blockchain & SQL database software usable for distributed data collection, data storage and data distribution. Daisy combines the specific benefits of the blockchain: distributed data collection and data immutability through cryptographic signatures of blocks, with the convenience of SQL databases, which means businesses can more easily adopt it in their existing environments.

# What if...

What if there is a blockchain where only certain nodes, in possession of one of accepted private keys, can add data (i.e. new blocks) to a blockchain, whose blocks are (SQLite) databases, and where those existing nodes can accept new ones into their ranks by signing the candidate's keys in a web-of-trust style?

What if public (government) documents were distributed in this way...? What if Wikipedia was...?

What if all this doesn't involve just one global blockchain instance but instead is implemented by software which enables multiple blockchains to be instantiated as easily as new Git repos, and just as easily distributed?

## Intended uses

Some possible use cases I've thought of for blockchains where everyone can download and verify the data, but only a few parties can publish (which is the model Daisy implements):

* Distributing academic articles: create a new blockchain for academic institutions (and Arxiv and such) and allow them to push blocks with articles into the blockchain.
* Distributing municipal and governmental records: each institution / agency could be allowed to publish blocks with records and documents into the blockchain.
* Distributing scientific data: only certified research institutions publish data into the blockchain.
* Distributing sensor data, by having gateways publish daily aggregate data from sensor networks.
* Distributing sports / betting / lottery results
* Wikileaks, of course
* As a basis for a cryptocurrency, by adding consensus logic for transactions
* Making a gigantic world-wide database of e.g. product information: manufacturers could add information about their products, keyed on e.g. UPC codes
* Logging and auditing: create and ship signed, rich logs which can be easily distributed and audited (e.g. each department publishes blocks of their changes).

The blockchain is basically very well suited to data exchange between otherwise competitive or hostile parties - the "trustless" principle is how Bitcoin manages to work in a really hostile global environment. Daisy could help bridge such environments.

## Roadmap

* ✔ Add a `daisy pull` command similar to "docker pull" which would replicate a blockchain from an existing node
* ✔ Make creating new blockchains from scratch possible without changing the code
* ✔ Blocks are transferred over the p2p connection (so nodes behind a NAT work), the protocol is versioned, there are DNS seeds, lists of peers, mDNS, UPnP / NAT-PMP port forwarding and an address book which is hard to fill with a few malicious peers. (The earlier "URL" and inline encodings of blocks are gone.)
* ✔ Security review of the crypto, consensus and p2p code: key operation signatures bound to the operation and the chain, a majority quorum, block timestamps and a limit on how often a signatory can sign, limits on everything a peer can make a node do, and SQLite hardening of the block files received from peers
* Implement nicer error handling when replying to messages
* Refactor db..., action... and blockchain... funcs into struct methods
* Reimplement P2P as a WebSockets protocol, allowing HTTP REST APIs such as getblock to be served from the same port.
* Add a [DHT](https://github.com/nictuku/dht) implementation for node discovery, possibly with some help from IPFS to store bootstrap nodes
* Implement a Bloom filter for tables in SQL queries, to skip querying blocks which don't have the appropriate tables.
* Implement stochastic guarded block importing: if there apparently is a new block in the network: ask a number of peers if they've seen it before importing it.

# Building

Daisy requires Go 1.27 or newer and a C compiler (the SQLite driver uses cgo).

```
go build ./cmd/daisy
go test ./...
```

The source layout is described in [AGENTS.md](AGENTS.md).

# Usage

When the command line app is started, Daisy will initialise its databases and install the default blockchain. It will then connect to a list of peers it maintains and fetch new blocks, if any.

## Querying the blockchain

All the blocks in the blockchain can be queried at the same time by using a command such as `./daisy query "SELECT COUNT(*) FROM wikinews_titles"` (note the quotes!). This will iterate over all the blocks, and in those blocks where the query is successful, will output the results to stdout as JSON objects separated by newlines. Of course, this is limited to read-only queries.

## Adding data to the blockchain

Since this is a private blockchain, not everyone has the ability to create new blocks. I'm thinking of this as a more of a framework for creating new single-purpose blockchain instances. If you want to contribute to the default blockchain (i.e. store data, i.e. add new sqlite databases to the blockchain), run the `./daisy mykeys` command, send me the public key hash to sign, and an explanation / introductory letter saying why and what do you want to do with it, and I'll sign your key and accept it into the blockchain as one of the signatories.

When you have a private key whose public part is added to the list of signatories, running `./daisy signimportblock mydata.db` will import the mydata.db file into the blockchain. Before it's imported, the database is modified to contain the Daisy metadata tables. A signatory can't sign two blocks too close together, see "Limiting how fast a branch can grow" below.

## Replicating a chain with `daisy pull`

`./daisy -dir <empty dir> pull http://host:2018/` fetches the chain parameters and the genesis block from a node. The node isn't authenticated (it's plain HTTP, and the chain parameters, with the genesis block hash and the creator's key, come from the same server), so whoever is in the middle can give you a different chain. Get the genesis block hash from somebody you trust and pass it: `./daisy -dir <dir> pull -expect-genesis <hash> http://host:2018/` (the flag can be given before `pull`, too). The pull is refused, before anything is written, if the hash is not the one. Without it `daisy` prints the hash and the creator's key with a warning.

## Finding peers, and NATs

* `-upnp`: ask the router to forward the ports with UPnP or NAT-PMP (it can be set as `"upnp": true` in the configuration file too).
* `-mdns`: advertise the node on the local network and find the other nodes there (`"mdns": true`).
* `dns_seeds` and `peer_list_urls` in `chainparams.json` (see `chainparams.example.json`): where else to look for peers.
* `http://<node>:2018/status` shows the state of a running node, `http://<node>:2018/peers` the peers it knows to be alive.

# Current status

Basic crypto, block and db operations are implemented, the network part is mostly done. A simple form of DB queries is done. Automated key management operations (i.e. signing someone else's key) are pending (they're manual now).

*WARNING:* This is mostly alpha quality code.

Talks / presentations I gave about Daisy:

* at a local Gophers meetup, September 2017: https://docs.google.com/presentation/d/1RybQluA3SbrM0PMgxmUSTWxyESTcXL9eFLSUvexDQSI/edit?usp=sharing
* at FSEC, September 2017: https://docs.google.com/presentation/d/10lqfxQvRCOco4pxbVjZUyiClqyyCOdN1L3RhGYZbtdM/edit?usp=sharing

## Design notes

*Note:* All this is fluid and can be changed as development progresses.

* This is a database-style blockchain. Daisy doesn't currently implement a cryptocurrency layer (though it would be simple to add this functionality), nor a smart contract layer (which is possible but not as simply). Since Daisy implements a design which is different from common cryptocurrency blockchains, those kinds of features would necessarily be implemented in a different way, with different semantics than what is common.
* Everyone can download the blockchain, only special "miners" can create ones, in a sort-of web-of-trust way. Those nodes who are able to create new blocks are called "signatories." They are in posssion of an accepted private key.
* Block payloads are SQLite database files. Except for a few special metadata tables, their content is not enforced.
* Block metadata is mostly within the block database, with some obvious exceptions such as the block hash.
* Consensus rules for accepting new blocks:
    * The validity of the SQLite files
    * The presence of a special tables named `_meta` and `_keys`,
    * The validity of the previous block hash in the `_meta` table
    * The block's hash and the previous block hash are signed with a key which is one of the accepted private keys, i.e. signatories, i.e. which has been added by the `_keys` table of an earlier block and not revoked. The signatures must be in the canonical "low-S" form (`daisy` always makes them that way).
    * The block has a `Timestamp` in its `_meta` table, which is later than the timestamp of the previous block by at least the block period, and not more than 15 seconds in the future, see "Limiting how fast a branch can grow" below.
    * The signatory did not sign one of the last `floor(N/2)` blocks of the branch, where `N` is the number of valid signatories, see below.
    * The `_keys` table contains new signatory keys additions and revocations. Both operations must be signed by a number of currently valid signatories, which is a majority of them (the *quorum*, see below).
    * The key operations must be signed by at least that number of *different* signatories who are valid at that point of the chain (a revoked key can't sign anything).
    * If there are competing branches, the branch with the highest total score wins, see "Signing turns and forks" below.
* Flood-based p2p network: every node can request a list of known connections from the other nodes. A peer is only remembered and passed on after it has said a valid hello, peers which stop answering are forgotten, and there are limits on the numbers of connections and of known peers.
* Each message contains the genesis (root) block hash, so technically multiple chains can safely communicate on the same TCP port

### Signing turns and forks

Any signatory may sign a block at any time, so two of them can extend the same block and the blockchain forks. All the nodes pick the same branch by giving every block a score and following the branch with the highest total:

* The signatories take turns: for the block at height `H`, it's the turn of the signatory at position `H mod N` in the list of the `N` currently valid signatory key hashes, sorted.
* A block signed by the signatory whose turn it is scores 2, a block signed by any other signatory scores 1 (the genesis block scores 0).
* The branch whose tip has the highest total score is the best chain. If the scores are equal, the tip with the lower block hash wins.
* A branch that forks off more than 128 blocks below the tip is ignored.

Signing out of turn is allowed, so a signatory who is offline doesn't stop the others (within the limit described in the next section), but a block out of turn loses against a block in turn. With a single signatory every block scores 2 and the best chain is simply the longest one. `daisy signimportblock` tells whether the new block is in turn.

### Limiting how fast a branch can grow

The scores alone would let a single rogue signatory sign hundreds of blocks in a few seconds and replace up to 128 blocks of honest history with a "longer" branch. So there are two more rules, and a block which breaks them is invalid:

* **Timestamps.** Every block except for the genesis block has a `Timestamp` (RFC 3339) in its `_meta` table, which is a part of the hashed file. It must be at least the *block period* later than the timestamp of the previous block (`block_period_seconds` in `chainparams.json`, one second if it isn't there), and not more than 15 seconds in the future by the clock of the node that checks it. A block without a timestamp is invalid (the modification time of the file doesn't count). So a branch can't grow faster than one block per period, and it can't run ahead of the wall clock. `daisy signimportblock` writes the timestamp (if the previous block is less than a block period old, the timestamp is made late enough). Keep the clocks of the signatories roughly right.
* **Recently signed (like Clique).** With `N` valid signatories, a signatory may sign at most one of every `floor(N/2)+1` consecutive blocks of a branch: it can't sign if it has signed one of the previous `floor(N/2)` blocks of the same branch. A minority of the signatories therefore can't build a branch on its own. The genesis block doesn't count as signed by anyone. With one signatory there's no limit.

The price is availability, and it is deliberate: with two signatories both have to be online, and they must alternate (neither can sign two blocks in a row); with three or four nobody can sign twice in a row, and so on. With few signatories the safety against a rogue one needs this; chains which can't live with it need to have more signatories.

When another branch becomes the best chain, the blocks it replaces are kept as side branches (in the `sideblocks` table and the `blocks/side` directory) and can come back if their branch is extended. The signatory keys are rolled back and forward together with the blocks.

### Random thoughts and blue-Moon wishes

* How to deal with blockchain abuse? I.e. one of the signatories turns out to be an adversary? Some ideas:

  * Limit the amount of data (in bytes) a signatory can add by the signatory's age (i.e. the longer the key is approved, the more data it can add). This offers absolutely no protection against "sleepers" which turn out to be adversaries after enough time has passed.
  * Require multiple signatures on blocks, which offers no protection against a clique of adversaries, and inconveniences the common use case where there are indeed individul authoritative sources of data.
  * Create a cryptocurrency overlay on the blockchain (possibly using external cryptocurrencies, tokens) where adding data becomes expensive - which doesn't protect against a well-funded adversary.

## How blocks are created

Blocks are SQLite database files. Every party in posession of an *accepted private key* (i.e. a signatory) can create new blocks, sign them, and publish them into the peer-to-peer network. Blocks are accepted (if other criteria are satisfied) only if they are signed by one of the accepted keys.

New blocks can contain operations which add or remove keys from a (global) list of accepted keys, if they contain a sufficient number of signatures from a list of already accepted keys. See the `_keys` table description in the section on Block metadata.

To be accepted as blocks, the SQLite database files have some soft and hard restrictions:

* The databases MUST NOT be created with the "WAL" journal mode. They SHOULD be created with the "OFF" journal mode.
* The databases SHOULD be created with the smallest possible page size, i.e. 512 bytes.
* A block file MUST NOT be larger than 1 GiB (`signimportblock` refuses it, and other nodes wouldn't accept it).
* `_meta` and `_keys` MUST be real tables (not views or virtual tables), `_keys` MUST NOT have more than 1000 records, and the file MUST pass SQLite's `PRAGMA quick_check`. Everything else in a block is up to its creator: views, triggers, indexes and generated columns of the user's own tables are fine.

Block files come from other nodes, so they are opened read-only and with the hardening of SQLite which fits: `trusted_schema` is off (views and triggers in a block can't call functions that are not safe for that), the connection is `query_only`, the limits of SQLite (the size of a value and of a statement, the depth of expressions, no `ATTACH`) are lowered, the page cache is small, and the metadata of a block is read with a time limit. See `internal/daisy/blockdb.go`. This applies to `daisy query`, too.

## Block metadata

### The `_keys` table

This table contains key operations for block creators. Keys can be either accepted or revoked. It is invalid for a `_keys` table to contain both acceptance and revocation records for a single key. Both acceptance and revocation operations require a quorum, where Q different keys which are already accepted sign the operation on the key in question. The number Q is a majority of the valid signatories, at every height above the genesis block:

```
  Q = floor(A/2) + 1
```

Where `A` is the number of valid (not revoked) signatories before the block containing these records (e.g. 2 of 2, 2 of 3, 3 of 4, 3 of 5). A smaller quorum would let a minority of the signatories add keys which they control, become the majority, and get around the rule that limits how often a signatory can sign blocks; this is how Clique works, too. (Earlier versions had a quorum which depended on the block height; it was a single signature on a young chain, and above the majority it would only make keys impossible to change, so it was removed.) The key operations in the genesis block are the only exception, as the first signatories have to be added by somebody.

What is signed is not just the hash of the key, but a message that says what is being done and on which chain:

```
  signature = ECDSA-P256-SHA256-sign( sha256( "daisy-keyop-v1" || 0x00 || genesis_block_hash || op || key_hash ) )
```

where `genesis_block_hash` and `key_hash` are the raw 32 bytes of the hashes (without the `1:` prefix), and `op` is the single letter `A` (add) or `R` (revoke), the `op` column of the record. A signature that approved adding a key is therefore no good for revoking it, and a signature on one chain is no good on another one. The signatures must be low-S. The key operations in the *genesis block* (height 0) are the exception: they are signed in the old format (the signature of the key hash alone, any S), because the genesis block of the default chain was made that way and it can't change. The genesis block is pinned by its hash in the chain parameters, so that is the one and only genesis rule; `daisy newchain` uses it, too.

For example, if `Q` is 3, to add a key `K` to the list of accepted keys, there must be at least 3 records in the `_keys` table pertaining to `K`. Each of the records must contain a valid signature by a different, already accepted key. The key `K` can be then used to sign new blocks immediately after the block which contain this records has been accepted.

# Basic crypto

ECDSA P-256 is used for public key crypto operations (public keys on other curves are not accepted). Signatures are ASN.1 DER encoded, are made over a 32 byte hash (nothing else is signed or verified), and `daisy` makes them low-S, so a third party can't change a valid signature into another valid one.

Strings refered in Daisy as "public keys" are SHA256 hashes of public keys and begin with the string "1:" (the 1 is to indicate a key type, should it need to change in the future). They look like "1:9569f0894e3d2b435a4c49c6a97501f4191b9729ff53be5acee2c7bd4be0e439".
