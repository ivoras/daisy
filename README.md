# Daisy - a perfectly minimal blockchain in Go

## Design notes

* Block payloads are SQLite database files. Opened read-only for use, of course.
* Blockchain metadata is mostly separate from the block payloads, with the notable exception of the previous block hash (merkle).
* Example consensus is on the validity of the SQLite files, the presence of a special table named `_meta` (key-value), and the validity of the previous block hash.
* Flood-based p2p network: every node can request a list of known connections from the other nodes.
* Longest chain wins.

