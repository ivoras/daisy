package main

// ChainParams holds blockchain configuration
type ChainParams struct {
	// GenesisBlockHash is the SHA256 hash of the genesis block payload
	GenesisBlockHash string `json:"genesis_block_hash"`

	// GenesisBlockHashSignature is the signature of the genesis block's hash, with the key in the genesis block
	GenesisBlockHashSignature string `json:"genesis_block_hash_signature"`

	// GenesisBlockTimestamp is the timestamp of the genesis block
	GenesisBlockTimestamp string `json:"genesis_block_timestamp"`

	Creator          string `json:"creator"`
	CreatorPublicKey string `json:"creator_public_key"`

	// List of host:port string specifying default peers for this blockchain. If empty, the defaults are used.
	BootstrapPeers []string `json:"bootstrap_peers"`
}
