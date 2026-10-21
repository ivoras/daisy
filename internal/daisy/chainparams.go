package daisy

import "time"

const (
	ChainConsensusPoA = 0
	ChainConsensusPoW = 1
)

// The minimum difference between the timestamps of consecutive blocks, if the chain doesn't say otherwise
const defaultBlockPeriodSeconds = 1

// blockPeriod returns the minimum difference between the timestamps of two consecutive blocks
func (cp *ChainParams) blockPeriod() time.Duration {
	if cp.BlockPeriodSeconds <= 0 {
		return defaultBlockPeriodSeconds * time.Second
	}
	return time.Duration(cp.BlockPeriodSeconds) * time.Second
}

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

	// DNS names ("host" or "host:port") whose addresses are peers of this blockchain. The default port is used if there is none.
	DNSSeeds []string `json:"dns_seeds"`

	// URLs of lists of peers (JSON {"peers": ["host:port"]}, a JSON array or one address per line)
	PeerListURLs []string `json:"peer_list_urls"`

	// The minimum number of seconds between the timestamps of two consecutive blocks. One if not set.
	BlockPeriodSeconds int `json:"block_period_seconds"`

	// Consensus algorithm used: "PoA", "PoW"
	ConsensusTypeString string `json:"consensus_type"`
	ConsensusType       int    `json:"-"`

	// Description of the blockchain (e.g. its purpose)
	Description string `json:"description"`
}
