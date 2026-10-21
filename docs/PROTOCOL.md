# The Daisy p2p protocol

Nodes talk over TCP (port 2017 by default). Every message is one line of JSON (at most 64 MiB). Fields and messages that a node doesn't
know are ignored, which is how most additions are made compatible. Every message has these fields:

| field    | meaning                                                                                    |
|----------|--------------------------------------------------------------------------------------------|
| `root`   | the hash of the genesis block of the chain; messages for another chain are ignored         |
| `msg`    | the type of the message                                                                    |
| `p2p_id` | the random ID of the sending node (made when the process starts)                           |

## Versions and the handshake

Both sides start by sending `hello`. Nothing but `hello` is accepted before the peer's hello has arrived.

| field | meaning |
|---|---|
| `version` | the name of the software, only informational |
| `protocol` | the version of this protocol which the node speaks. Absent: 1 |
| `min_protocol` | the oldest version which the node is willing to talk to. Absent: 1 |
| `chain_height`, `tip_hash`, `chain_score` | the best chain of the node. Absent in the hello of the oldest nodes |
| `my_peers` | addresses of peers that the node is connected to and that accept connections |
| `observed_host` | the address (no port) at which the sender sees the receiver. Since version 2 |

Two nodes talk only if each is new enough for the other: `peer.protocol >= our min_protocol` and `peer.min_protocol <= our protocol`.
Otherwise the connection is closed (and not retried for a while). A node uses a feature only if the peer's version has it.

| version | what it added |
|---|---|
| 1 | the original protocol |
| 2 | blocks sent in chunks over the p2p connection; `observed_host`; the `reachable` message |

**Changing the protocol.** Adding an optional field or a message that old nodes can ignore needs no new version. A new version is for
when the peer has to know that it may use something (so it doesn't send what the other side can't handle), or when nodes must be
kept apart (raise `min_protocol`). Rules that decide which blocks are valid are a different matter: a change in them forks
the blockchain, and needs a way for all nodes to switch at the same block, which doesn't exist yet.

## Messages

| message | fields | notes |
|---|---|---|
| `hello` | see above | |
| `getblockhashes` | `min_block_height`, `max_block_height` | asks for the hashes of the blocks of the best chain |
| `blockhashes` | `hashes`: `{height: hash}` | the answer; also sent unasked when the best chain changes |
| `getblock` | `hash` | asks for any block of the block tree, wherever it is |
| `block` | `hash`, `hash_signature`, `size`, `encoding`, `data` | see below |
| `blockchunk` | `hash`, `index`, `data` | a piece of a block sent with the `chunked` encoding |
| `reachable` | `port` | "I have connected to your p2p port and it works" |

### Sending blocks

The `encoding` of the `block` message says how the data arrives:

* `chunked` (version 2): `data` is empty and `size` is the size of the block file. It is followed by `blockchunk` messages
  with consecutive `index` values (from 0), each carrying a zlib compressed, base64 encoded piece of up to 512 KiB. The block is
  complete when `size` bytes have arrived. Chunks are sent one at a time, between them the connection handles the messages
  that have arrived, so a big block doesn't make the connection unresponsive. Nothing but the connection that the receiver
  itself opened is needed, so it works for nodes behind a NAT.
* `http` (version 1): `data` is the URL `http://host:port/block/<hash or height>` on the sender's HTTP server. The receiver
  keeps only the port and the block from the URL, and connects to the address of the sender, it never follows redirects.
* `zlib-base64` (version 1): `data` is the whole block, compressed and encoded, for blocks up to 32 MiB.

A node sends version 2 peers the `chunked` encoding, and the others `http` (or `zlib-base64` if asked to with `-p2pblockinline`).

Limits: a block is at most 1 GiB, at most 2 blocks are received on a connection at the same time, and at most 4 GiB of blocks
over all the connections. A transfer that stalls for 2 minutes is dropped.

## HTTP

The HTTP port (2018) serves: `/block/<height or hash>` (the block file), `/chainparams.json`, `/peers` (the addresses of the
peers that this node has talked to, as `{"peers": [...]}`) and `/status` (the state of the node).

## Finding peers

* The **address book** (the `peers` table) has the *new* addresses (heard of from a peer, a seed or the network) and the *tried* addresses
  (of peers which have said hello). Each network group (/16 or /32) and each source has a limited number of the new addresses, so a few
  nodes can't fill it. The node connects to 8 peers chosen at random from different networks, trying the tried and the new addresses alike,
  and waiting longer and longer after failures. Dead and old addresses are forgotten, the bootstrap peers never.
* **Bootstrap peers**, **DNS seeds** (`dns_seeds`) and **lists of peers** (`peer_list_urls`, JSON or lines of text) from `chainparams.json`.
  `daisy pull` also takes the peers from `/peers` of the node it pulls from.
* **Gossip**: the `my_peers` of `hello`. Only the address (with the default port) is taken; private addresses are ignored unless the peer is
  on a private network itself.
* **Local network** (`-mdns`): the nodes advertise `_daisy._tcp` with the genesis hash in the TXT record.
* **NAT** (`-upnp`): UPnP or NAT-PMP is used to ask the router to forward the p2p and HTTP ports. Without it, a node behind a NAT
  works by connecting to others, but nobody can connect to it. Peers tell the node which address they see it at
  (`observed_host`) and whether they could connect to it (`reachable`); when two peers agree, `/status` shows it.
