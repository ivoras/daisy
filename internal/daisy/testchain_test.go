package daisy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/dcrypto"
	"github.com/ivoras/daisy/internal/util"
)

// This file has what's needed to build blockchains out of real, signed blocks, to test the rules about them.

type testSigner struct {
	key  *ecdsa.PrivateKey
	hash string
	pub  []byte
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{key: key, hash: dcrypto.PubKeyHash(pub), pub: pub}
}

// A key operation to put in a block: op is "A" or "R", signed by each of the signers
type testKeyOp struct {
	op      string
	target  *testSigner
	signers []*testSigner

	// To make bad key ops: sign the operation signAs instead of op, or for the chain with the genesis block signGenesis
	signAs      string
	signGenesis string

	// Records to copy into the block as they are, to replay the signatures of another block
	copySignatures []BlockKeyOp
}

type testBlock struct {
	file    string
	hash    string
	prev    string
	ts      time.Time
	sig     []byte // The signature of the block hash
	prevSig []byte // The signature of the previous block hash
}

type testChain struct {
	t       *testing.T
	dir     string
	signers map[string]*testSigner // By key hash
	genesis testBlock
	counter int
	times   map[string]time.Time // The timestamps of the blocks, by hash
}

// setupTestChain creates a blockchain in a new data directory, with a genesis block which adds all the given signers.
// The first of them signs the genesis block. It changes the global state, and restores it when the test ends.
func setupTestChain(t *testing.T, genesisSigners ...*testSigner) *testChain {
	t.Helper()
	oldCfg, oldParams, oldSub := cfg, chainParams, blockchainSubdirectory
	tc := &testChain{t: t, dir: t.TempDir(), signers: make(map[string]*testSigner), times: make(map[string]time.Time)}
	cfg.DataDir = filepath.Join(tc.dir, "data")
	cfg.faster = false
	if err := os.Mkdir(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tc.dir, "incoming"), 0700); err != nil {
		t.Fatal(err)
	}
	dbInit()
	ensureBlockchainSubdirectoryExists()
	orphanPool.blocks = make(map[string]*orphanBlock)
	t.Cleanup(func() {
		mainDb.Close()
		privateDb.Close()
		mainDb, privateDb = nil, nil
		cfg, chainParams, blockchainSubdirectory = oldCfg, oldParams, oldSub
	})

	var ops []testKeyOp
	for _, s := range genesisSigners {
		tc.signers[s.hash] = s
		ops = append(ops, testKeyOp{op: "A", target: s, signers: []*testSigner{genesisSigners[0]}})
	}
	// The genesis block is a day old, so there's room for a lot of blocks which are a second apart
	tc.genesis = tc.newBlockAt(GenesisBlockPreviousBlockHash, genesisSigners[0], "genesis", time.Now().Add(-24*time.Hour).Truncate(time.Second), ops...)
	chainParams.GenesisBlockHash = tc.genesis.hash

	if err := blockchainEnsureBlockDir(0); err != nil {
		t.Fatal(err)
	}
	if err := blockchainCopyFile(tc.genesis.file, 0); err != nil {
		t.Fatal(err)
	}
	err := dbInsertBlock(&DbBlockchainBlock{
		Hash: tc.genesis.hash, HashSignature: tc.genesis.sig, PreviousBlockHash: tc.genesis.prev,
		PreviousBlockHashSignature: tc.genesis.prevSig, SignaturePublicKeyHash: genesisSigners[0].hash,
		Height: 0, Version: CurrentBlockVersion, TimeAccepted: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	genesisOps, err := readKeyOps(tc.genesis.file)
	if err != nil {
		t.Fatal(err)
	}
	if err = dbApplyKeyOps(mainDb, 0, genesisOps); err != nil {
		t.Fatal(err)
	}
	dbSetConfig("schema_version", fmt.Sprint(currentSchemaVersion))
	return tc
}

// newBlock creates a signed block file with the given previous block. The extra text goes into the block, so
// that blocks which are otherwise the same have different hashes. The timestamp of the block is a second later than
// the one of the previous block.
func (tc *testChain) newBlock(prev string, signer *testSigner, extra string, ops ...testKeyOp) testBlock {
	tc.t.Helper()
	return tc.newBlockAt(prev, signer, extra, tc.times[prev].Add(time.Second), ops...)
}

// newBlockAt is newBlock() with the given timestamp. If it is zero, the block doesn't have one.
func (tc *testChain) newBlockAt(prev string, signer *testSigner, extra string, ts time.Time, ops ...testKeyOp) testBlock {
	tc.t.Helper()
	if tc.times[prev].IsZero() && prev != GenesisBlockPreviousBlockHash {
		ts = tc.genesisTime().Add(time.Second) // A made up parent, like in the tests of orphans
	}
	tc.signers[signer.hash] = signer
	tc.counter++
	file := filepath.Join(tc.dir, "incoming", fmt.Sprintf("block%d.db", tc.counter))
	db, err := dbOpen(file, false)
	if err != nil {
		tc.t.Fatal(err)
	}
	must := func(err error) {
		tc.t.Helper()
		if err != nil {
			tc.t.Fatal(err)
		}
	}
	_, err = db.Exec("PRAGMA journal_mode=DELETE")
	must(err)
	dbEnsureBlockchainTables(db)
	_, err = db.Exec("CREATE TABLE payload (v TEXT)")
	must(err)
	_, err = db.Exec("INSERT INTO payload VALUES (?)", extra)
	must(err)
	prevSig, err := dcrypto.SignHex(signer.key, prev)
	must(err)
	must(dbSetMetaInt(db, "Version", CurrentBlockVersion))
	must(dbSetMetaString(db, "PreviousBlockHash", prev))
	must(dbSetMetaString(db, "PreviousBlockHashSignature", prevSig))
	if !ts.IsZero() {
		must(dbSetMetaString(db, "Timestamp", ts.Format(time.RFC3339)))
	}
	must(dbSetMetaString(db, "CreatorPublicKey", signer.hash))
	for _, op := range ops {
		for _, c := range op.copySignatures {
			_, err = db.Exec("INSERT INTO _keys (op, pubkey_hash, pubkey, sigkey_hash, signature) VALUES (?, ?, ?, ?, ?)",
				op.op, op.target.hash, hex.EncodeToString(op.target.pub), c.signatureKeyHash, hex.EncodeToString(c.signature))
			must(err)
		}
		for _, s := range op.signers {
			var sig []byte
			if prev == GenesisBlockPreviousBlockHash {
				sig, err = dcrypto.SignPublicKeyHash(s.key, op.target.hash) // The genesis block has the old format
			} else {
				signAs, signGenesis := op.op, tc.genesis.hash
				if op.signAs != "" {
					signAs = op.signAs
				}
				if op.signGenesis != "" {
					signGenesis = op.signGenesis
				}
				sig, err = dcrypto.SignKeyOp(s.key, signGenesis, signAs, op.target.hash)
			}
			must(err)
			_, err = db.Exec("INSERT INTO _keys (op, pubkey_hash, pubkey, sigkey_hash, signature) VALUES (?, ?, ?, ?, ?)",
				op.op, op.target.hash, hex.EncodeToString(op.target.pub), s.hash, hex.EncodeToString(sig))
			must(err)
		}
	}
	must(db.Close())

	hash, err := util.HashFileToHexString(file)
	must(err)
	sig, err := dcrypto.SignHexBytes(signer.key, hash)
	must(err)
	prevSigBytes, err := hex.DecodeString(prevSig)
	must(err)
	tc.times[hash] = ts
	return testBlock{file: file, hash: hash, prev: prev, ts: ts, sig: sig, prevSig: prevSigBytes}
}

func (tc *testChain) genesisTime() time.Time {
	return tc.times[tc.genesis.hash]
}

func (tc *testChain) accept(b testBlock) (acceptInfo, error) {
	return acceptBlock(b.file, b.sig)
}

func (tc *testChain) mustAccept(b testBlock, want acceptResult) acceptInfo {
	tc.t.Helper()
	info, err := tc.accept(b)
	if err != nil {
		tc.t.Fatalf("block %s was refused: %v", b.hash, err)
	}
	if info.result != want {
		tc.t.Fatalf("block %s: result is %q, want %q", b.hash, info.result, want)
	}
	return info
}

// mustAcceptAny accepts the block, whatever happens to the best chain
func (tc *testChain) mustAcceptAny(b testBlock) acceptInfo {
	tc.t.Helper()
	info, err := tc.accept(b)
	if err != nil {
		tc.t.Fatalf("block %s was refused: %v", b.hash, err)
	}
	return info
}

func (tc *testChain) mustRefuse(b testBlock) error {
	tc.t.Helper()
	info, err := tc.accept(b)
	if err == nil {
		tc.t.Fatalf("block %s was accepted (%s), but it's not valid", b.hash, info.result)
	}
	return err
}

// branchState returns the signatory keys after the given block, and the signers who can't sign the block after it,
// because they have signed too recently
func (tc *testChain) branchState(prev string) (keys *keySet, recent []string, height int) {
	tc.t.Helper()
	dbb, onBestChain, err := dbGetAnyBlock(prev)
	if err != nil {
		tc.t.Fatal(err)
	}
	keys, _, _, err = keysAfterBlock(dbb, onBestChain)
	if err != nil {
		tc.t.Fatal(err)
	}
	if recent, err = recentSignersOf(dbb, recentSignerWindow(keys)); err != nil {
		tc.t.Fatal(err)
	}
	return keys, recent, dbb.Height + 1
}

// inTurn returns the signatory whose turn it is to sign the block after the given one. (It may not be allowed to,
// if it has signed one of the previous blocks.)
func (tc *testChain) inTurn(prev string) *testSigner {
	tc.t.Helper()
	keys, _, height := tc.branchState(prev)
	s := tc.signers[inTurnSigner(keys, height)]
	if s == nil {
		tc.t.Fatal("don't know the signer in turn")
	}
	return s
}

// outOfTurn returns a signatory who is allowed to sign the block after the given one, but whose turn it is not
func (tc *testChain) outOfTurn(prev string) *testSigner {
	tc.t.Helper()
	keys, recent, height := tc.branchState(prev)
	inTurn := inTurnSigner(keys, height)
	for _, h := range keys.active() {
		if h != inTurn && !slices.Contains(recent, h) {
			return tc.signers[h]
		}
	}
	tc.t.Fatal("there is no signatory out of turn who can sign")
	return nil
}

// next returns the signatory in turn to sign the block after the given one, or if it can't, another one who can
func (tc *testChain) next(prev string) *testSigner {
	tc.t.Helper()
	keys, recent, height := tc.branchState(prev)
	inTurn := inTurnSigner(keys, height)
	if !slices.Contains(recent, inTurn) {
		return tc.signers[inTurn]
	}
	for _, h := range keys.active() {
		if !slices.Contains(recent, h) {
			return tc.signers[h]
		}
	}
	tc.t.Fatal("nobody can sign")
	return nil
}

func (tc *testChain) tip() *DbBlockchainBlock {
	tc.t.Helper()
	tip, err := dbGetTipBlock()
	if err != nil {
		tc.t.Fatal(err)
	}
	return tip
}

func (tc *testChain) activeKeys() []string {
	tc.t.Helper()
	keys, err := dbLoadKeySet()
	if err != nil {
		tc.t.Fatal(err)
	}
	return keys.active()
}

// checkConsistent verifies the entire blockchain, which also checks that the files, the database and the
// key table all agree, and that there are no leftovers of a reorganization.
func (tc *testChain) checkConsistent() {
	tc.t.Helper()
	if err := blockchainVerifyEverything(); err != nil {
		tc.t.Fatalf("the blockchain doesn't verify: %v", err)
	}
	staged, _ := filepath.Glob(filepath.Join(blockchainSubdirectory, "*", "*.new"))
	if len(staged) != 0 {
		tc.t.Fatalf("leftover files: %v", staged)
	}
}

func (tc *testChain) fileHashAtHeight(h int) string {
	tc.t.Helper()
	hash, err := util.HashFileToHexString(blockchainGetFilename(h))
	if err != nil {
		tc.t.Fatal(err)
	}
	return hash
}

func (tc *testChain) isSideBlock(hash string) bool {
	_, onBestChain, err := dbGetAnyBlock(hash)
	return err == nil && !onBestChain
}

func filepathGlobOrphans() ([]string, error) {
	return filepath.Glob(filepath.Join(orphanDirname(), "*.db"))
}
