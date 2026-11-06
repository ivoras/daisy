package daisy

import "testing"

func TestCheckConsensusType(t *testing.T) {
	for ct, ok := range map[string]bool{"": true, "PoA": true, "PoW": false, "poa": false, "x": false} {
		cp := ChainParams{ConsensusType: ct}
		if err := cp.checkConsensusType(); (err == nil) != ok {
			t.Errorf("consensus_type %q: %v", ct, err)
		}
	}
}
