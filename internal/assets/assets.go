// Package assets holds the files embedded into the Daisy binary.
package assets

import _ "embed"

// GenesisDB is the genesis block (a SQLite database) of the default blockchain.
//
//go:embed genesis.db
var GenesisDB []byte
