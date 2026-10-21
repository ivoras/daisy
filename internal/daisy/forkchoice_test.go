package daisy

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestKeySet(t *testing.T) {
	op := func(o string, key string) map[string][]BlockKeyOp {
		return map[string][]BlockKeyOp{key: {{op: o, publicKeyHash: key, publicKeyBytes: []byte(key)}}}
	}
	ks := newKeySet()
	ks.apply(0, op("A", "b"))
	ks.apply(0, op("A", "a"))
	ks.apply(5, op("A", "c"))
	ks.apply(7, op("R", "a"))
	if got := ks.active(); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("active = %v", got)
	}
	if !ks.known("a") {
		t.Error("a revoked key is still known")
	}
	if _, ok := ks.get("a"); ok {
		t.Error("a revoked key can't be used")
	}

	clone := ks.clone()
	ks.rewind(6) // Before the revocation
	if got := ks.active(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("after rewinding to 6, active = %v", got)
	}
	ks.rewind(4) // Before c was added
	if got := ks.active(); !slices.Equal(got, []string{"a", "b"}) || ks.known("c") {
		t.Errorf("after rewinding to 4, active = %v", got)
	}
	if got := clone.active(); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("rewinding the key set changed its clone: %v", got)
	}
}

func TestScoring(t *testing.T) {
	ks := newKeySet()
	for _, k := range []string{"c", "a", "b"} {
		ks.keys[k] = &keyInfo{hash: k, revokeHeight: noHeight}
	}
	// The sorted keys are a, b, c
	for height, want := range map[int]string{1: "b", 2: "c", 3: "a", 4: "b"} {
		if got := inTurnSigner(ks, height); got != want {
			t.Errorf("height %d: in turn is %s, want %s", height, got, want)
		}
	}
	if blockScore(ks, 1, "b") != scoreInTurn || blockScore(ks, 1, "a") != scoreOutOfTurn || blockScore(ks, 0, "a") != 0 {
		t.Error("wrong block scores")
	}
	low := &DbBlockchainBlock{Hash: "9", TotalScore: 5}
	high := &DbBlockchainBlock{Hash: "9", TotalScore: 6}
	if !isBetterTip(high, low) || isBetterTip(low, high) {
		t.Error("the higher score must win")
	}
	// The same score: the lower hash wins, the same way on every node
	a, b := &DbBlockchainBlock{Hash: "1", TotalScore: 5}, &DbBlockchainBlock{Hash: "2", TotalScore: 5}
	if !isBetterTip(a, b) || isBetterTip(b, a) {
		t.Error("ties must be broken by the lower hash")
	}
}

func TestLinearChain(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	prev := tc.genesis.hash
	for i := 1; i <= 5; i++ {
		b := tc.newBlock(prev, a, fmt.Sprint(i))
		info := tc.mustAccept(b, blockNewTip)
		if info.height != i {
			t.Fatalf("height is %d, want %d", info.height, i)
		}
		prev = b.hash
	}
	if tip := tc.tip(); tip.Height != 5 || tip.Hash != prev || tip.TotalScore != 2*5 {
		t.Errorf("unexpected tip: height %d, score %d", tip.Height, tip.TotalScore)
	}
	// Again is not a problem
	tc.mustAccept(tc.newBlock(tc.genesis.hash, a, "1"), blockOnSideBranch) // Another block 1: it's a branch, but not a better one
	b := tc.newBlock(prev, a, "6")
	tc.mustAccept(b, blockNewTip)
	if _, err := tc.accept(b); err != nil {
		t.Error(err)
	} else if info, _ := tc.accept(b); info.result != blockAlreadyKnown {
		t.Errorf("a known block is %q", info.result)
	}
	tc.checkConsistent()
}

func TestSignatoriesTakeTurns(t *testing.T) {
	signers := []*testSigner{newTestSigner(t), newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	prev := tc.genesis.hash
	for i := 1; i <= 6; i++ {
		s := tc.inTurn(prev)
		b := tc.newBlock(prev, s, fmt.Sprint(i))
		tc.mustAccept(b, blockNewTip)
		prev = b.hash
	}
	if tc.tip().TotalScore != 12 {
		t.Errorf("all blocks were in turn, the score is %d", tc.tip().TotalScore)
	}
	// Signing out of turn is allowed, it's only worth less
	b := tc.newBlock(prev, tc.outOfTurn(prev), "out of turn")
	tc.mustAccept(b, blockNewTip)
	if tc.tip().TotalScore != 13 {
		t.Errorf("the score is %d, want 13", tc.tip().TotalScore)
	}
	tc.checkConsistent()
}

// Two signatories sign the block at the same height: the one whose turn it is wins, whichever was first.
func TestForkInTurnBlockWins(t *testing.T) {
	signers := []*testSigner{newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	genesis := tc.genesis.hash

	late := tc.newBlock(genesis, tc.outOfTurn(genesis), "out of turn")
	inTurn := tc.newBlock(genesis, tc.inTurn(genesis), "in turn")

	tc.mustAccept(late, blockNewTip)
	if tc.tip().Hash != late.hash {
		t.Fatal("the first block must be the tip")
	}
	tc.mustAccept(inTurn, blockReorganized)
	if tc.tip().Hash != inTurn.hash || tc.tip().Height != 1 || tc.tip().TotalScore != 2 {
		t.Fatalf("the block in turn must be the tip, got %s", tc.tip().Hash)
	}
	if !tc.isSideBlock(late.hash) {
		t.Error("the replaced block must be kept as a side block")
	}
	if tc.fileHashAtHeight(1) != inTurn.hash {
		t.Error("the file of block 1 must be the new one")
	}
	tc.checkConsistent()

	// Another out of turn block loses at once
	another := tc.newBlock(genesis, tc.outOfTurn(genesis), "another out of turn")
	tc.mustAccept(another, blockOnSideBranch)
	if tc.tip().Hash != inTurn.hash {
		t.Fatal("the tip must not change")
	}
	tc.checkConsistent()
}

// A branch of blocks which are out of turn can win, if it's long enough. And then lose again.
// (There are three signatories, because with two of them nobody can sign two blocks in a row, which a branch of
// out of turn blocks followed by one in turn needs.)
func TestForkLongerBranchWins(t *testing.T) {
	signers := []*testSigner{newTestSigner(t), newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	genesis := tc.genesis.hash

	y1 := tc.newBlock(genesis, tc.inTurn(genesis), "y1") // 2
	tc.mustAccept(y1, blockNewTip)

	x1 := tc.newBlock(genesis, tc.outOfTurn(genesis), "x1") // 1
	tc.mustAccept(x1, blockOnSideBranch)
	x2 := tc.newBlock(x1.hash, tc.outOfTurn(x1.hash), "x2") // The branch now has 2, the same as the best chain: the hashes decide
	info2, err := tc.accept(x2)
	if err != nil {
		t.Fatal(err)
	}
	if info2.result != blockOnSideBranch && info2.result != blockReorganized {
		t.Fatalf("unexpected result %q", info2.result)
	}
	// Make the branch unambiguously better: one more block, by the signatory in turn
	x3 := tc.newBlock(x2.hash, tc.inTurn(x2.hash), "x3")
	if info2.result == blockReorganized {
		tc.mustAccept(x3, blockNewTip)
	} else {
		tc.mustAccept(x3, blockReorganized)
	}
	if tc.tip().Hash != x3.hash || tc.tip().Height != 3 {
		t.Fatalf("unexpected tip at height %d", tc.tip().Height)
	}
	if !tc.isSideBlock(y1.hash) {
		t.Error("y1 must be a side block now")
	}
	for h, want := range map[int]string{1: x1.hash, 2: x2.hash, 3: x3.hash} {
		if got := tc.fileHashAtHeight(h); got != want {
			t.Errorf("height %d: file is %s, want %s", h, got, want)
		}
	}
	tc.checkConsistent()

	// And the first branch comes back with more blocks
	// The scores of the blocks of the first branch are 2, 4, 6 and 8 against 4 or 5: it takes over at the latest with the last block
	var y4 testBlock
	prev := y1.hash
	for i := 2; i <= 4; i++ {
		y4 = tc.newBlock(prev, tc.inTurn(prev), fmt.Sprint("y", i))
		if _, err := tc.accept(y4); err != nil {
			t.Fatal(err)
		}
		prev = y4.hash
	}
	if tc.tip().Hash != y4.hash || tc.tip().Height != 4 {
		t.Fatalf("unexpected tip at height %d", tc.tip().Height)
	}
	if !tc.isSideBlock(x3.hash) || tc.isSideBlock(y1.hash) {
		t.Error("the branches must have been swapped")
	}
	tc.checkConsistent()
}

// The keys added or revoked by the blocks which are replaced must be undone, and those of the new blocks applied.
func TestReorganizationUndoesKeyOps(t *testing.T) {
	a, b, k := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	tc := setupTestChain(t, a, b)
	tc.signers[k.hash] = k
	genesis := tc.genesis.hash

	// Branch P adds the key K, and K signs blocks
	p1signer := tc.inTurn(genesis)
	p1 := tc.newBlock(genesis, p1signer, "p1", testKeyOp{op: "A", target: k, signers: []*testSigner{p1signer}})
	tc.mustAccept(p1, blockNewTip)
	if got := tc.activeKeys(); len(got) != 3 || !slices.Contains(got, k.hash) {
		t.Fatalf("the key was not added: %v", got)
	}
	p2 := tc.newBlock(p1.hash, k, "p2 signed by the new key")
	tc.mustAccept(p2, blockNewTip)

	// Branch Q has no K, and it's better: two blocks in turn
	q1 := tc.newBlock(genesis, tc.inTurn(genesis), "q1")
	tc.mustAccept(q1, blockOnSideBranch) // 2 against 3 or 4
	q2 := tc.newBlock(q1.hash, tc.inTurn(q1.hash), "q2")
	if _, err := tc.accept(q2); err != nil { // 4 against 3 or 4: the hashes may decide
		t.Fatal(err)
	}
	q3 := tc.newBlock(q2.hash, tc.inTurn(q2.hash), "q3")
	if _, err := tc.accept(q3); err != nil { // 6 against 3 or 4
		t.Fatal(err)
	}
	if tc.tip().Hash != q3.hash {
		t.Fatalf("branch Q must be the best chain, the tip is at height %d", tc.tip().Height)
	}
	if got := tc.activeKeys(); slices.Contains(got, k.hash) || len(got) != 2 {
		t.Fatalf("the key must be gone with its branch: %v", got)
	}
	// A block signed by K can't be added to Q
	tc.mustRefuse(tc.newBlock(q3.hash, k, "signed by K on the wrong branch"))
	tc.checkConsistent()

	// P gets better again
	prev := p2.hash
	var last testBlock
	for i := 3; i <= 7; i++ {
		last = tc.newBlock(prev, tc.next(prev), fmt.Sprint("p", i))
		tc.mustAcceptAny(last)
		prev = last.hash
	}
	if tc.tip().Hash != last.hash {
		t.Fatalf("branch P must be the best chain, tip is at height %d", tc.tip().Height)
	}
	if got := tc.activeKeys(); len(got) != 3 || !slices.Contains(got, k.hash) {
		t.Fatalf("the key must be back: %v", got)
	}
	tc.checkConsistent()
}

func TestRevokedKeyCantSign(t *testing.T) {
	a, b := newTestSigner(t), newTestSigner(t)
	tc := setupTestChain(t, a, b)
	genesis := tc.genesis.hash

	s := tc.inTurn(genesis)
	revoke := tc.newBlock(genesis, s, "revoke b", testKeyOp{op: "R", target: b, signers: []*testSigner{s}})
	tc.mustAccept(revoke, blockNewTip)
	if got := tc.activeKeys(); len(got) != 1 || got[0] != a.hash {
		t.Fatalf("active keys: %v", got)
	}
	err := tc.mustRefuse(tc.newBlock(revoke.hash, b, "by the revoked key"))
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("unexpected error: %v", err)
	}
	// ... but it signed the blocks before the revocation, and they are still fine
	tc.mustAccept(tc.newBlock(revoke.hash, a, "by a"), blockNewTip)
	tc.checkConsistent()

	// A side branch which forks before the revocation can still use the key
	late := tc.newBlock(genesis, b, "b signs at height 1")
	if _, err := tc.accept(late); err != nil {
		t.Errorf("a block before the revocation must be fine: %v", err)
	}
}

func TestInvalidBlocksAreRefused(t *testing.T) {
	a, b, stranger := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	tc := setupTestChain(t, a, b)
	genesis := tc.genesis.hash

	if err := tc.mustRefuse(tc.newBlock(genesis, stranger, "not a signatory")); !strings.Contains(err.Error(), "Cannot find an accepted public key") {
		t.Errorf("unexpected error: %v", err)
	}

	bad := tc.newBlock(genesis, a, "wrong signature")
	other := tc.newBlock(genesis, a, "other block")
	bad.sig = other.sig
	tc.mustRefuse(bad)

	// A new key added by somebody who isn't a signatory
	tc.mustRefuse(tc.newBlock(genesis, a, "bad key op", testKeyOp{op: "A", target: stranger, signers: []*testSigner{stranger}}))
	// An existing key can't be added again
	tc.mustRefuse(tc.newBlock(genesis, a, "add b again", testKeyOp{op: "A", target: b, signers: []*testSigner{a}}))
	// ... and an unknown key can't be revoked
	tc.mustRefuse(tc.newBlock(genesis, a, "revoke a stranger", testKeyOp{op: "R", target: stranger, signers: []*testSigner{a}}))

	if tc.tip().Height != 0 {
		t.Errorf("none of the blocks should have been accepted, the tip is at %d", tc.tip().Height)
	}
	// None of that changed the keys
	if got := tc.activeKeys(); len(got) != 2 {
		t.Errorf("active keys: %v", got)
	}
	tc.checkConsistent()
}

func TestOrphanBlocksWaitForTheirPredecessors(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "1")
	b2 := tc.newBlock(b1.hash, a, "2")
	b3 := tc.newBlock(b2.hash, a, "3")
	b4 := tc.newBlock(b3.hash, a, "4")

	info := tc.mustAccept(b3, blockOrphan)
	if info.missing != b2.hash {
		t.Errorf("missing %s, want %s", info.missing, b2.hash)
	}
	tc.mustAccept(b4, blockOrphan)
	tc.mustAccept(b2, blockOrphan)
	if tc.tip().Height != 0 {
		t.Fatal("nothing can be accepted yet")
	}
	tc.mustAccept(b1, blockNewTip)
	if tip := tc.tip(); tip.Height != 4 || tip.Hash != b4.hash {
		t.Fatalf("the waiting blocks should have been added, the tip is at %d", tip.Height)
	}
	if len(orphanPool.blocks) != 0 {
		t.Errorf("%d orphans left", len(orphanPool.blocks))
	}
	tc.checkConsistent()
}

func TestOrphanPoolIsLimited(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	for i := 0; i < maxOrphans+5; i++ {
		missing := fmt.Sprintf("%064x", i+1)
		tc.mustAccept(tc.newBlock(missing, a, fmt.Sprint(i)), blockOrphan)
	}
	if len(orphanPool.blocks) != maxOrphans {
		t.Errorf("%d orphans are kept, the limit is %d", len(orphanPool.blocks), maxOrphans)
	}
	files, _ := filepathGlobOrphans()
	if len(files) != maxOrphans {
		t.Errorf("%d orphan files, want %d", len(files), maxOrphans)
	}
}

func TestDeepReorganizationIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a long chain")
	}
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	prev := tc.genesis.hash
	var hashes []string
	for i := 1; i <= maxReorgDepth+10; i++ {
		b := tc.newBlock(prev, a, fmt.Sprint(i))
		tc.mustAccept(b, blockNewTip)
		prev = b.hash
		hashes = append(hashes, b.hash)
	}
	tipHeight := tc.tip().Height

	// A branch off the genesis block is too deep to consider, whatever it contains
	err := tc.mustRefuse(tc.newBlock(tc.genesis.hash, a, "too deep"))
	if !strings.Contains(err.Error(), "fork") {
		t.Errorf("unexpected error: %v", err)
	}
	// ... but one just within the limit is fine
	forkPoint := hashes[tipHeight-maxReorgDepth-1] // The block at height tipHeight-maxReorgDepth
	tc.mustAccept(tc.newBlock(forkPoint, a, "just deep enough"), blockOnSideBranch)
	if tc.tip().Height != tipHeight {
		t.Error("the tip must not change")
	}
}

func TestVerificationFindsProblems(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	prev := tc.genesis.hash
	for i := 1; i <= 3; i++ {
		b := tc.newBlock(prev, a, fmt.Sprint(i))
		tc.mustAccept(b, blockNewTip)
		prev = b.hash
	}
	tc.checkConsistent()

	tamper := func(query string, args ...any) {
		t.Helper()
		if _, err := mainDb.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct{ name, query, want string }{
		{"previous block hash", "UPDATE blockchain SET prev_hash=? WHERE height=2", "previous block hash"},
		{"score", "UPDATE blockchain SET total_score=999 WHERE height=3", "total score"},
		{"key table", "UPDATE pubkeys SET block_height=2", "key"},
	} {
		var orig string
		switch tt.name {
		case "previous block hash":
			mainDb.QueryRow("SELECT prev_hash FROM blockchain WHERE height=2").Scan(&orig)
			tamper(tt.query, strings.Repeat("0", 64))
			err := blockchainVerifyEverything()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s: unexpected result: %v", tt.name, err)
			}
			tamper("UPDATE blockchain SET prev_hash=? WHERE height=2", orig)
		case "score":
			tamper(tt.query)
			err := blockchainVerifyEverything()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s: unexpected result: %v", tt.name, err)
			}
			tamper("UPDATE blockchain SET total_score=6 WHERE height=3")
		case "key table":
			tamper(tt.query)
			err := blockchainVerifyEverything()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s: unexpected result: %v", tt.name, err)
			}
			tamper("UPDATE pubkeys SET block_height=0")
		}
		tc.checkConsistent() // Repaired
	}
}

// A database of the previous version doesn't have the scores, the heights of the revocations, the side blocks...
func TestMigrationOfOldDatabase(t *testing.T) {
	a, b := newTestSigner(t), newTestSigner(t)
	tc := setupTestChain(t, a, b)
	prev := tc.genesis.hash
	for i := 1; i <= 4; i++ {
		s := tc.inTurn(prev)
		var ops []testKeyOp
		if i == 3 {
			ops = append(ops, testKeyOp{op: "R", target: b, signers: []*testSigner{s}})
		}
		blk := tc.newBlock(prev, s, fmt.Sprint(i), ops...)
		tc.mustAccept(blk, blockNewTip)
		prev = blk.hash
	}
	want := tc.tip().TotalScore
	tc.checkConsistent()

	// Make it look the way it did before: the old tables, and the key of the creator of the chain with a height of -1
	for _, q := range []string{
		"DROP TABLE sideblocks",
		"ALTER TABLE blockchain DROP COLUMN total_score",
		"ALTER TABLE pubkeys DROP COLUMN revoke_block_height",
		"ALTER TABLE peers DROP COLUMN failures",
		"DELETE FROM config WHERE key='schema_version'",
		"UPDATE pubkeys SET block_height=-1, time_revoked=NULL",
	} {
		if _, err := mainDb.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dbMigrate(false)
	if err := reconcileChainState(); err != nil {
		t.Fatal(err)
	}
	if got := tc.tip().TotalScore; got != want {
		t.Errorf("total score is %d, want %d", got, want)
	}
	if got := tc.activeKeys(); len(got) != 1 {
		t.Errorf("after the revocation there should be one key: %v", got)
	}
	tc.checkConsistent()
	if dbGetConfig("schema_version") != fmt.Sprint(currentSchemaVersion) {
		t.Error("the schema version isn't recorded")
	}
}

func TestRecoverInterruptedReorganization(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b1 := tc.newBlock(tc.genesis.hash, a, "1")
	tc.mustAccept(b1, blockNewTip)

	// The reorganization has committed the database, but died before renaming the file...
	staged := stagedBlockFilename(1)
	if err := copyFileTo(b1.file, staged); err != nil {
		t.Fatal(err)
	}
	// ... and one of the files is stale, it's not the block which the database says
	stale := stagedBlockFilename(2)
	if err := copyFileTo(tc.genesis.file, stale); err != nil {
		t.Fatal(err)
	}
	if err := recoverReorganization(); err != nil {
		t.Fatal(err)
	}
	tc.checkConsistent() // No *.new files left
}

func TestBlocksAreServedByHash(t *testing.T) {
	signers := []*testSigner{newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	genesis := tc.genesis.hash
	late := tc.newBlock(genesis, tc.outOfTurn(genesis), "out of turn")
	inTurn := tc.newBlock(genesis, tc.inTurn(genesis), "in turn")
	tc.mustAccept(late, blockNewTip)
	tc.mustAccept(inTurn, blockReorganized)

	_, err := dbGetSideBlock(late.hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = dbGetSideBlock(inTurn.hash); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a block of the best chain isn't a side block: %v", err)
	}
	for _, hash := range []string{late.hash, inTurn.hash} {
		if !dbBlockHashKnown(hash) {
			t.Errorf("%s is not known", hash)
		}
	}
}
