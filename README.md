# Daisy - a private blockchain where blocks are SQLite databases

# What if...

What if there is a blockchain where only certain nodes, in possession of one of accepted private keys, can add data (i.e. new blocks) to a blockchain, whose blocks are (SQLite) databases, and where those existing nodes can accept new ones into their ranks by signing the candidate's keys in a web-of-trust style?

What if public (government) documents were distributed in this way...? What if Wikipedia was...?

# Usage

When started, Daisy will initialise its databases and install the default (and currently the only one) blockchain. It will then connect to a list of peers it maintains and fetch new blocks, if any.

## Querying the blockchain

All the blocks in the blockchain can be queried by using a command such as `./daisy query "SELECT COUNT(*) FROM wikinews_titles"` (note the quotes!). This will iterate over all the blocks, and in those blocks where the query is successful, will output the results to stdout as JSON objects separated by newlines. Of course, this is limited to read-only queries.

## Adding data to the blockchain

Since this is a private blockchain, not everyone has the ability to create new blocks. I'm thinking of this as a more of a framework for creating new blockchains. If you want to contribute to the default blockchain (i.e. store data, i.e. add new sqlite databases to the blockchain), run the `./daisy mykeys` command, send me the public key hash to sign, and an explanation / introductory letter saying why and what do you want to do with it, and I'll sign your key and accept it into the blockchain as one of the signatories.

When you have a private key whose public part is added to the list of signatories, running `./daisy signimportblock mydata.db` will import the mydata.db file into the blockchain. Before it's imported, the database is modified to contain the Daisy metadata tables.

## Intended uses

Some possible use cases I've thought of for blockchains where everyone can download and verify the data, but only a few parties can publish:

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

# Current status

Basic crypto, block and db operations are implemented, the network part is mostly done. A simple form of DB queries is done. Automated key management operations (i.e. signing someone else's key) are pending (they're manual now).

*WARNING:* This is mostly alpha quality code.

Talks / presentations I gave about Daisy:

* at a local Gophers meetup, September 2017: https://docs.google.com/presentation/d/1RybQluA3SbrM0PMgxmUSTWxyESTcXL9eFLSUvexDQSI/edit?usp=sharing
* at FSEC, September 2017: https://docs.google.com/presentation/d/10lqfxQvRCOco4pxbVjZUyiClqyyCOdN1L3RhGYZbtdM/edit?usp=sharing

## ToDo

* Implement nicer error handling when replying to messages
* Refactor db..., action... and blockchain... funcs into struct methods
* Implement the "URL" encoding for block transfers: so the data in the JSON messages isn't block data, but an URL to the block data.
* Implement a Bloom filter for tables in SQL queries, to skip querying blocks which don't have the appropriate tables.
* Implement stochastic guarded block importing: if there apparently is a new block in the network: ask a number of peers if they've seen it before importing it.

## Design notes

*Note:* All this is fluid and can be changed as development progresses.

* Everyone can download the blockchain, only special "miners" can create ones, in a sort-of web-of-trust way. Those nodes who are able to create new blocks are called "signatories." They are in posssion of an accepted private key.
* Block payloads are SQLite database files. Except for special metadata tables, their content is not enforced.
* Blockchain metadata is mostly separate from the block payloads, with some obvious exceptions such as the block hash. Metadata critical for blockchain integrity (like the previous block's hash (merkle)is within the block database)
* Consensus rules for accepting new blocks:
    * The validity of the SQLite files
    * The presence of a special tables named `_meta` and `keys`,
    * The validity of the previous block hash in the `_meta` table
    * The previous block hash is signed with a key which is one of the accepted private keys, i.e. signatories, i.e. which is present in the previous blocks' `_keys` table.
    * The `_keys` table contains new signatory keys additions and revocations. Both operations must be signed by a number of currently valid signatories, where the
      number is given as "1 if height < 149 else floor(log(height)*2)"
    * Longest chain wins.
* Flood-based p2p network: every node can request a list of known connections from the other nodes.
* Each message contains the genesis (root) block hash, so technically multiple chains can safely communicate on the same TCP port

### Random thoughts and blue-Moon wishes

* How to deal with blockchain abuse? I.e. one of the signatories turns out to be an adversary? Some ideas:

  * Limit the amount of data (in bytes) a signatory can add by the signatorie's age (i.e. the longer the key is approved, the more data it can add). This offers absolutely no protection against "sleepers" which turn out to be adversaries after enough time has passed.
  * Require multiple signatures on blocks, which offers no protection against a clique of adversaries, and inconveniences the common use case where there are indeed individul authoritative sources of data.
  * Create a cryptocurrency overlay on the blockchain (possibly using external cryptocurrencies, tokens) where adding data becomes expensive - which doesn't protect against a well-funded adversary.

## How blocks are created

Blocks are SQLite database files. Every party in posession of an *accepted private key* (i.e. a signatory) can create new blocks and sign them. Blocks are accepted (if other criteria are satisfied) only if they are signed by one of the accepted keys.

New blocks can contain operations which add or remove keys from a (global) list of accepted keys, if they contain a sufficient number of signatures from a list of already accepted keys. See the `_keys` table description in the section on Block metadata.

To be accepted as blocks, the SQLite database files have some soft and hard restrictions:

* The databases MUST NOT be created with the "WAL" journal mode. They SHOULD be created with the "OFF" journal mode.
* The databases SHOULD be created with the smallest possible page size, i.e. 512 bytes.

## Block metadata

### The `_keys` table

This table contains key operations for block creators. Keys can be either accepted or revoked. It is invalid for a `_keys` table to contain both acceptance and revocation records for a single key. Both acceptance and revocation operations require a quorum, where Q different keys which are already accepted sign the hash of the key in question. The number Q is calculated as:

```
  Q = 1 if H  < 149 else floor(log(H)*2)
```

Where `H` is the block height of the block containing these records.

For example, if `Q` is 3, to add a key `K` to the list of accepted keys, there must be exactly 3 records in the `_keys` table pertaining to `K`. Each of the records must contain a valid signature by a different, already accepted key. The key `K` can be then used to sign new blocks immediately after the block which contain this records has been accepted.

A table of quorums required for specific block heights is:

```
  1   1
  149 10
  245 11
  404 12
  666 13
  1097 14
  1809 15
  2981 16
  4915 17
  8104 18
  13360 19
  22027 20
  36316 21
  59875 22
  98716 23
  162755 24
```

E.g. for block 100000, 23 signatures are required to accept a new signature.

# Basic crypto

ECDSA P-256 is used for public key crypto operations.

Strings refered in Daisy as "public keys" are SHA256 hashes of public keys and begin with the string "1:" (the 1 is to indicate a key type, should it need to change in the future). They look like "1:9569f0894e3d2b435a4c49c6a97501f4191b9729ff53be5acee2c7bd4be0e439".
