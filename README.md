# Daisy - a minimal semi-public blockchain in Go

## Design notes

* Block payloads are SQLite database files. Opened read-only for use, of course.
* Blockchain metadata is mostly separate from the block payloads, with the notable exception of the previous block hash (merkle).
* Consensus rules:
    * The validity of the SQLite files
    * The presence of a special tables named `_meta` and `keys`,
    * The validity of the previous block hash in the `_meta` table
    * The previous block hash is signed with a key which is present in the previous blocs' `_keys` table.
* Flood-based p2p network: every node can request a list of known connections from the other nodes.
* Longest chain wins.

