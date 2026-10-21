package daisy

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/ivoras/daisy/internal/assets"
	"github.com/ivoras/daisy/internal/dcrypto"
	"github.com/ivoras/daisy/internal/util"
)

func TestBlockchainGetFilename(t *testing.T) {
	blockchainSubdirectory = "/data/blocks"
	for h, want := range map[int]string{
		0:      "/data/blocks/0000/block_00000000.db",
		65535:  "/data/blocks/0000/block_0000ffff.db",
		65536:  "/data/blocks/0001/block_00010000.db",
		200000: "/data/blocks/0003/block_00030d40.db",
	} {
		if got := blockchainGetFilename(h); got != want {
			t.Errorf("blockchainGetFilename(%d) = %s, want %s", h, got, want)
		}
	}
}

// The embedded genesis block must match the hard-coded chain parameters, and its signature
// (created in 2017) must verify with the current crypto code.
func TestEmbeddedGenesisBlock(t *testing.T) {
	if got := util.HashBytesToHexString(assets.GenesisDB); got != defaultChainParams.GenesisBlockHash {
		t.Fatalf("genesis block hash is %s, want %s", got, defaultChainParams.GenesisBlockHash)
	}
	fn := filepath.Join(t.TempDir(), "genesis.db")
	if err := os.WriteFile(fn, assets.GenesisDB, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBlockFile(fn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.PreviousBlockHash != GenesisBlockPreviousBlockHash {
		t.Errorf("unexpected previous block hash %s", b.PreviousBlockHash)
	}
	keyOps, err := b.dbGetKeyOps()
	if err != nil {
		t.Fatal(err)
	}
	ops, ok := keyOps[b.SignaturePublicKeyHash]
	if !ok || len(ops) == 0 {
		t.Fatalf("genesis block doesn't add its own signing key %s", b.SignaturePublicKeyHash)
	}
	pub, err := dcrypto.DecodePublicKeyBytes(ops[0].publicKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := dcrypto.VerifyHex(pub, defaultChainParams.GenesisBlockHash, defaultChainParams.GenesisBlockHashSignature); err != nil {
		t.Errorf("genesis block hash signature: %v", err)
	}
	if err := dcrypto.VerifyHexBytes(pub, b.PreviousBlockHash, b.PreviousBlockHashSignature); err != nil {
		t.Errorf("genesis previous block hash signature: %v", err)
	}
	if _, err := hex.DecodeString(defaultChainParams.GenesisBlockHashSignature); err != nil {
		t.Error(err)
	}
}
