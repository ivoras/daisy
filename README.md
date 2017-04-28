# Daisy - a minimal semi-open blockchain in Go

## Design notes

* Block payloads are SQLite database files. Opened read-only for use, of course.
* Blockchain metadata is mostly separate from the block payloads, with the notable exception of the previous block hash (merkle).
* Consensus rules:
    * The validity of the SQLite files
    * The presence of a special tables named `_meta` and `keys`,
    * The validity of the previous block hash in the `_meta` table
    * The previous block hash is signed with a key which is present in the previous blocks' `_keys` table.
    * The `_keys` table contains new key additions and revocations. Both signed by a number of existing keys, where the
      number is given as "1 if height < 40 else floor(log(height)*3)
    * Longest chain wins.
* Flood-based p2p network: every node can request a list of known connections from the other nodes.


