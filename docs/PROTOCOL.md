# The Daisy p2p protocol

Nodes talk over TCP (port 2017 by default). Every message is one line of JSON (at most 64 MiB). Fields and messages that a node doesn't
know are ignored, which is how most additions are made compatible. Every message has these fields:

| field    | meaning                                                                                    |
|----------|--------------------------------------------------------------------------------------------|
| `root`   | the hash of the genesis block of the chain; messages for another chain are ignored         |
| `msg`    | the type of the message                                                                    |
| `p2p_id` | the random ID of the sending node (made when the process starts). Only in `hello`, the other messages leave it out |

## Versions and the handshake

Both sides start by sending `hello`. Nothing but `hello` is accepted before the peer's hello has arrived, and only the first
`hello` of a connection is processed (it adds peers, counts as a vote about our address and sets the protocol version);
the later ones are ignored.

A connection which hasn't sent a valid hello within 20 seconds is closed. After that, the peer must send *some* message at
least every 5 minutes (the read deadline is moved with every message; it applies to a whole message, so sending a few bytes
now and then doesn't help). Nodes send each other a `ping` every 30 seconds, so a healthy peer with nothing to say is not
dropped; a peer which sends nothing for 5 minutes is.

The `p2p_id` of the `hello` is not proof of anything: everybody who has connected to a node knows its ID and can claim it. A node
uses it only to find out that it has connected to itself (the connection is closed; if it was the node that dialed, that address
is avoided and forgotten), and to drop the newer one of two connections which come from the same IP address and claim the same
ID. Nobody is banned for a claimed ID, and a connection from another IP address is never dropped because of one.

A host can have at most 4 inbound connections to a node at the same time, and a node accepts at most 128 inbound connections.

| field | meaning |
|---|---|
| `version` | the name of the software, only informational |
| `protocol` | the version of this protocol which the node speaks. Required |
| `min_protocol` | the oldest version which the node is willing to talk to. Required |
| `chain_height`, `tip_hash`, `chain_score` | the best chain of the node. Required; the peer's chain is better if its score is higher (or equal, with a lower tip hash) |
| `my_peers` | addresses of peers that the node is connected to and that accept connections |
| `observed_host` | the address (no port) at which the sender sees the receiver |

Two nodes talk only if each is new enough for the other: `peer.protocol >= our min_protocol` and `peer.min_protocol <= our protocol`.
A hello without the required fields is from a node of an old version, and is treated the same way. Otherwise the connection is
closed (and not retried for a while). A node uses a feature only if the peer's version has it.

The current version is 3, and it is also the oldest one: versions 1 and 2 were never deployed and are not supported. Their
numbers are not reused, so that such nodes are kept apart.

**Changing the protocol.** Adding an optional field or a message that old nodes can ignore needs no new version. A new version is for
when the peer has to know that it may use something (so it doesn't send what the other side can't handle), or when nodes must be
kept apart (raise `min_protocol`). Rules that decide which blocks are valid are a different matter: a change in them forks
the blockchain, and needs a way for all nodes to switch at the same block, which doesn't exist yet.

## Messages

| message | fields | notes |
|---|---|---|
| `hello` | see above | |
| `getblockhashes` | `min_block_height`, `max_block_height` | asks for the hashes of the blocks of the best chain. At most 2000 heights |
| `blockhashes` | `hashes`: `{height: hash}` | the answer; also sent unasked when the best chain changes. At most 2000 hashes |
| `getblock` | `hash` | asks for any block of the block tree, wherever it is |
| `block` | `hash`, `hash_signature`, `size`, `encoding` | see below |
| `blockchunk` | `hash`, `index`, `data` | a piece of a block |
| `reachable` | `port` | "I have connected to your p2p port and it works" |
| `ping` | | shows that the sender is alive; nothing else |

### Block hashes

A range in `getblockhashes` covers at most 2000 heights (`p2pMaxBlockHashes`): the answering node cuts a longer range at
`min_block_height + 1999` (and a negative start at 0), and the asking node doesn't ask for more. A node which is behind goes through
the chain of its peer one page at a time: it asks for the range from a little below its own tip, requests the blocks it doesn't
have, and when they have been imported it asks for the next 2000 heights, and so on up to the height the peer has said it has. If
the blocks don't arrive (nothing is imported for a minute) it asks again, up to five times. The peer may be asked again by a node
which sees `blockhashes` for blocks that are much higher than its own chain; the unasked `blockhashes` that a node sends when its
chain grows carry at most the 2000 newest hashes.

From a `blockhashes` message a node handles at most 2000 hashes, in the order of the heights, and only those with a height in the
range which it has asked that peer for lately, or from 128 blocks below its tip (`maxReorgDepth`) to 2000 above it, which is what the
announcement of a new block looks like. Anything that isn't the hash of a block is ignored. A node answers `getblockhashes` only if
what it has already sent to the peer has been written out (at most 32 messages are waiting).

### Sending blocks

The `block` message announces a block: `hash`, the `hash_signature` (the signature of the hash by the signatory who made the block,
hex), the `size` of the block file and the `encoding`, which is `chunked`. `data` is not used. It is followed by `blockchunk`
messages with consecutive `index` values (from 0), each carrying a zlib compressed, base64 encoded piece of up to 512 KiB. The
block is complete when `size` bytes have arrived. Chunks are sent one at a time, between them the connection handles the messages
that have arrived, so a big block doesn't make the connection unresponsive. Nothing but the connection that the receiver itself
opened is needed, so it works for nodes behind a NAT.

**What the sender does with `getblock`.** At most 2 blocks are being sent on a connection at a time. The file of a block is opened
when its transfer starts, not when it is requested. Requests that come while the 2 are busy wait as bare hashes (at most 4096 of
them), and a request over that limit, or for a block that is already being sent or waiting, or one that the node doesn't have, is
ignored. The peer will see that the block doesn't come and ask again.

**What the receiver does with `block`.** Before anything is reserved or stored the receiver checks that the block is welcome. It is,
if the receiver has asked this peer for it lately (the hashes asked for are remembered for 30 minutes, at most 4096 of them), or if
the `hash_signature` of the announced hash is made by one of the signatory keys that the receiver knows: all the keys of its `pubkeys`
table, whether they are active, revoked or only from a branch that has been discarded. (A branch which adds a new signatory can only
be received by asking for its blocks.) Then the transfer needs room in the limits: a block is at most 1 GiB, at most 2 blocks are
received on a connection at the same time, at most 1 GiB is being received from a host (all its connections together) and at most
4 GiB from all the hosts. A transfer that stalls for 2 minutes is dropped. When all the chunks are there the receiver hashes the file
and drops it if the hash is not the announced one, only then does the block go to the block tree for validation.

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
  (`observed_host`) and whether they could connect to it (`reachable`); when two peers agree, `/status` shows it. This is
  informational only: `observed_host` is taken only from the hello of a connection which the node itself made, each peer host
  has one vote (a new one replaces the old), `reachable` is counted once per host, and votes expire after an hour. What the
  peers say never stops the node from connecting to an address. That is decided only by the router's address (when a port is
  mapped) and by the addresses at which a connection of the node has come back to itself.
