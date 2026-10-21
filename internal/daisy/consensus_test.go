package daisy

import (
	"crypto/elliptic"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/assets"
)

func TestEffectiveQuorum(t *testing.T) {
	for _, tt := range []struct{ height, active, want int }{
		{1, 1, 1}, {1, 5, 1}, {148, 100, 1}, // Below the height where the quorum starts to grow
		{149, 1, 1}, {149, 2, 2}, {149, 3, 2}, {149, 4, 3}, {149, 5, 3}, // A majority of few signatories
		{149, 18, 10}, {149, 19, 10}, {149, 100, 10}, // The quorum of the height, if there are enough of them
		{162755, 3, 2}, {162755, 47, 24}, {162755, 48, 24}, {162755, 1000, 24},
	} {
		if got := EffectiveQuorum(tt.height, tt.active); got != tt.want {
			t.Errorf("EffectiveQuorum(%d, %d) = %d, want %d", tt.height, tt.active, got, tt.want)
		}
	}
}

func TestValidateTimestamp(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	parent := now.Add(-time.Hour)
	for _, tt := range []struct {
		name string
		ts   time.Time
		ctx  blockContext
		ok   bool
	}{
		{"later than the parent", parent.Add(time.Second), blockContext{parentTime: parent, blockPeriod: time.Second, now: now}, true},
		{"the same as the parent", parent, blockContext{parentTime: parent, blockPeriod: time.Second, now: now}, false},
		{"before the parent", parent.Add(-time.Second), blockContext{parentTime: parent, blockPeriod: time.Second, now: now}, false},
		{"too soon for the period", parent.Add(9 * time.Second), blockContext{parentTime: parent, blockPeriod: 10 * time.Second, now: now}, false},
		{"exactly the period", parent.Add(10 * time.Second), blockContext{parentTime: parent, blockPeriod: 10 * time.Second, now: now}, true},
		{"missing", time.Time{}, blockContext{parentTime: parent, blockPeriod: time.Second, now: now}, false},
		{"missing, without checks", time.Time{}, blockContext{}, false},
		{"slightly in the future", now.Add(maxFutureDrift), blockContext{parentTime: parent, blockPeriod: time.Second, now: now}, true},
		{"too far in the future", now.Add(maxFutureDrift + time.Second), blockContext{parentTime: parent, blockPeriod: time.Second, now: now}, false},
		// When the blockchain is verified again, the clock is not a part of the rules
		{"in the future, but not checked", now.Add(time.Hour), blockContext{parentTime: parent, blockPeriod: time.Second}, true},
		// The previous block is a genesis block without a timestamp
		{"no parent timestamp", parent, blockContext{blockPeriod: time.Second, now: now}, true},
	} {
		err := validateTimestamp(&Block{Timestamp: tt.ts}, tt.ctx)
		if (err == nil) != tt.ok {
			t.Errorf("%s: unexpected result: %v", tt.name, err)
		}
	}
}

func TestBlockTimestampRules(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	genesis := tc.genesis.hash
	now := time.Now()

	b1 := tc.newBlock(genesis, a, "1")
	tc.mustAccept(b1, blockNewTip)

	if err := tc.mustRefuse(tc.newBlockAt(b1.hash, a, "no timestamp", time.Time{})); !strings.Contains(err.Error(), "Timestamp") {
		t.Errorf("unexpected error: %v", err)
	}
	for name, ts := range map[string]time.Time{
		"same time":   b1.ts,
		"earlier":     b1.ts.Add(-time.Hour),
		"before 1970": time.Unix(-1000, 0),
	} {
		if err := tc.mustRefuse(tc.newBlockAt(b1.hash, a, name, ts)); !strings.Contains(err.Error(), "timestamp") {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
	if err := tc.mustRefuse(tc.newBlockAt(b1.hash, a, "future", now.Add(time.Minute))); !strings.Contains(err.Error(), "future") {
		t.Errorf("unexpected error: %v", err)
	}
	// A clock which is a little off is fine
	ahead := tc.newBlockAt(b1.hash, a, "slightly ahead", now.Add(5*time.Second))
	tc.mustAccept(ahead, blockNewTip)
	// ... but it makes the next block wait: the time goes on from there
	tc.mustRefuse(tc.newBlockAt(ahead.hash, a, "just after", ahead.ts))
	tc.mustAccept(tc.newBlockAt(ahead.hash, a, "a second later", ahead.ts.Add(time.Second)), blockNewTip)
	tc.checkConsistent()
}

func TestBlockPeriod(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	genesis := tc.genesis.hash
	chainParams.BlockPeriodSeconds = 5 // Restored when the test ends

	tc.mustRefuse(tc.newBlockAt(genesis, a, "4 seconds", tc.genesisTime().Add(4*time.Second)))
	b1 := tc.newBlockAt(genesis, a, "5 seconds", tc.genesisTime().Add(5*time.Second))
	tc.mustAccept(b1, blockNewTip)
	tc.mustRefuse(tc.newBlockAt(b1.hash, a, "again 4 seconds", b1.ts.Add(4*time.Second)))
	tc.mustAccept(tc.newBlockAt(b1.hash, a, "again 5 seconds", b1.ts.Add(5*time.Second)), blockNewTip)
	tc.checkConsistent()

	// The default is a second
	for _, n := range []int{0, -1} {
		cp := ChainParams{BlockPeriodSeconds: n}
		if cp.blockPeriod() != time.Second {
			t.Errorf("the block period for %d is %s", n, cp.blockPeriod())
		}
	}
	if (&ChainParams{BlockPeriodSeconds: 60}).blockPeriod() != time.Minute {
		t.Error("wrong block period")
	}
}

func TestRecentlySignedRule(t *testing.T) {
	newSigners := func(n int) []*testSigner {
		var s []*testSigner
		for i := 0; i < n; i++ {
			s = append(s, newTestSigner(t))
		}
		return s
	}
	refused := func(err error) bool { return err != nil && strings.Contains(err.Error(), "can't sign this block") }

	t.Run("one signatory is not limited", func(t *testing.T) {
		a := newTestSigner(t)
		tc := setupTestChain(t, a)
		prev := tc.genesis.hash
		for i := 0; i < 5; i++ {
			b := tc.newBlock(prev, a, fmt.Sprint(i))
			tc.mustAccept(b, blockNewTip)
			prev = b.hash
		}
	})

	t.Run("two signatories must take turns", func(t *testing.T) {
		s := newSigners(2)
		tc := setupTestChain(t, s...)
		// The signer of the genesis block is not counted as having signed anything
		b1 := tc.newBlock(tc.genesis.hash, s[0], "1")
		tc.mustAccept(b1, blockNewTip)
		if err := tc.mustRefuse(tc.newBlock(b1.hash, s[0], "2 by the same")); !refused(err) {
			t.Errorf("unexpected error: %v", err)
		}
		b2 := tc.newBlock(b1.hash, s[1], "2")
		tc.mustAccept(b2, blockNewTip)
		tc.mustRefuse(tc.newBlock(b2.hash, s[1], "3 by the same"))
		tc.mustAccept(tc.newBlock(b2.hash, s[0], "3"), blockNewTip)
		tc.checkConsistent()
	})

	t.Run("four signatories: one of every three blocks", func(t *testing.T) {
		s := newSigners(4) // The window is two blocks
		tc := setupTestChain(t, s...)
		b1 := tc.newBlock(tc.genesis.hash, s[0], "1")
		tc.mustAccept(b1, blockNewTip)
		b2 := tc.newBlock(b1.hash, s[1], "2")
		tc.mustAccept(b2, blockNewTip)
		tc.mustRefuse(tc.newBlock(b2.hash, s[0], "3 by the signer of block 1"))
		tc.mustRefuse(tc.newBlock(b2.hash, s[1], "3 by the signer of block 2"))
		b3 := tc.newBlock(b2.hash, s[2], "3")
		tc.mustAccept(b3, blockNewTip)
		tc.mustAccept(tc.newBlock(b3.hash, s[0], "4"), blockNewTip) // Two blocks since it has signed
		tc.checkConsistent()
	})

	t.Run("a minority can't build a branch alone", func(t *testing.T) {
		s := newSigners(5) // The window is two blocks, the limit is one block of three
		tc := setupTestChain(t, s...)
		prev := tc.genesis.hash
		for i := 1; i <= 3; i++ {
			b := tc.newBlock(prev, tc.next(prev), fmt.Sprint(i))
			tc.mustAccept(b, blockNewTip)
			prev = b.hash
		}
		// Two of the five signatories fork off the genesis block: they can't take turns in a way that satisfies the rule
		x1 := tc.newBlock(tc.genesis.hash, s[0], "x1")
		tc.mustAcceptAny(x1)
		x2 := tc.newBlock(x1.hash, s[1], "x2")
		tc.mustAcceptAny(x2)
		tc.mustRefuse(tc.newBlock(x2.hash, s[0], "x3"))
		tc.mustRefuse(tc.newBlock(x2.hash, s[1], "x3"))
	})

	t.Run("it applies to the blocks of the branch", func(t *testing.T) {
		s := newSigners(2)
		tc := setupTestChain(t, s...)
		genesis := tc.genesis.hash
		y1 := tc.newBlock(genesis, s[1], "y1")
		tc.mustAcceptAny(y1)
		// The same signer, but on another branch
		x1 := tc.newBlock(genesis, s[1], "x1")
		tc.mustAcceptAny(x1)
		// s[0] has signed nothing on the branch of x1, s[1] has
		if err := tc.mustRefuse(tc.newBlock(x1.hash, s[1], "x2 by the same")); !refused(err) {
			t.Errorf("unexpected error: %v", err)
		}
		tc.mustAcceptAny(tc.newBlock(x1.hash, s[0], "x2"))
		tc.checkConsistent()
	})

	t.Run("revoking a key changes the limit", func(t *testing.T) {
		s := newSigners(2)
		tc := setupTestChain(t, s...)
		b1 := tc.newBlock(tc.genesis.hash, s[0], "revoke s1", testKeyOp{op: "R", target: s[1], signers: []*testSigner{s[0]}})
		tc.mustAccept(b1, blockNewTip)
		// With a single signatory there's no limit, so s[0] can sign again at once
		tc.mustAccept(tc.newBlock(b1.hash, s[0], "2"), blockNewTip)
		tc.checkConsistent()
	})
}

// highS makes the other valid signature out of the given one: (r, s) -> (r, n-s)
func highS(t *testing.T, sig []byte) []byte {
	t.Helper()
	var s struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(sig, &s); err != nil {
		t.Fatal(err)
	}
	s.S = new(big.Int).Sub(elliptic.P256().Params().N, s.S)
	out, err := asn1.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHighSSignaturesAreRefused(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	b := tc.newBlock(tc.genesis.hash, a, "1")
	good := b.sig
	b.sig = highS(t, good)
	if err := tc.mustRefuse(b); !strings.Contains(err.Error(), "low-S") {
		t.Errorf("unexpected error: %v", err)
	}
	b.sig = good
	tc.mustAccept(b, blockNewTip)
}

func TestKeyOpSignaturesCantBeReplayed(t *testing.T) {
	a, b, k := newTestSigner(t), newTestSigner(t), newTestSigner(t)
	tc := setupTestChain(t, a, b)
	tc.signers[k.hash] = k
	genesis := tc.genesis.hash

	adder := tc.inTurn(genesis)
	add := tc.newBlock(genesis, adder, "add k", testKeyOp{op: "A", target: k, signers: []*testSigner{adder}})
	tc.mustAccept(add, blockNewTip)
	addedOps, err := readKeyOps(add.file)
	if err != nil {
		t.Fatal(err)
	}

	// Another signatory copies the signatures of the quorum which has added k into a revocation of k
	replayer := tc.next(add.hash)
	replay := tc.newBlock(add.hash, replayer, "replayed signatures", testKeyOp{op: "R", target: k, copySignatures: addedOps[k.hash]})
	if err := tc.mustRefuse(replay); !strings.Contains(err.Error(), "Failed verification of key op") {
		t.Errorf("unexpected error: %v", err)
	}
	// ... and the same if the signatories sign the "A" message again
	tc.mustRefuse(tc.newBlock(add.hash, replayer, "signed as adds", testKeyOp{op: "R", signAs: "A", target: k, signers: []*testSigner{replayer}}))
	// The key stays
	if got := tc.activeKeys(); len(got) != 3 {
		t.Fatalf("active keys: %v", got)
	}

	// A key op for another chain is not valid on this one, even if it's the same operation on the same key
	otherChain := strings.Repeat("ab", 32)
	tc.mustRefuse(tc.newBlock(add.hash, replayer, "from another chain", testKeyOp{op: "R", signGenesis: otherChain, target: k, signers: []*testSigner{replayer}}))
	m := newTestSigner(t)
	tc.mustRefuse(tc.newBlock(add.hash, replayer, "add m from another chain", testKeyOp{op: "A", signGenesis: otherChain, target: m, signers: []*testSigner{replayer}}))

	// A signature of the right operation is fine
	revoke := tc.newBlock(add.hash, replayer, "revoke k", testKeyOp{op: "R", target: k, signers: []*testSigner{replayer}})
	tc.mustAccept(revoke, blockNewTip)
	if got := tc.activeKeys(); len(got) != 2 {
		t.Fatalf("active keys: %v", got)
	}
	tc.checkConsistent()
}

// The quorum of the height is more than the signatories there are: a majority of them has to be enough
func TestKeyOpQuorumIsLimitedByTheSignatories(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a long chain")
	}
	signers := []*testSigner{newTestSigner(t), newTestSigner(t), newTestSigner(t)}
	tc := setupTestChain(t, signers...)
	prev := tc.genesis.hash
	for i := 1; i <= 150; i++ {
		b := tc.newBlock(prev, tc.next(prev), fmt.Sprint(i))
		tc.mustAccept(b, blockNewTip)
		prev = b.hash
	}
	if q := QuorumForHeight(151); q != 10 {
		t.Fatalf("the quorum at height 151 is %d", q)
	}
	k := newTestSigner(t)
	tc.signers[k.hash] = k
	signer := tc.next(prev)
	// One signature is not enough: a majority of three is two
	oneSig := tc.newBlock(prev, signer, "one signature", testKeyOp{op: "A", target: k, signers: []*testSigner{signers[0]}})
	if err := tc.mustRefuse(oneSig); !strings.Contains(err.Error(), "Quorum of 2") {
		t.Errorf("unexpected error: %v", err)
	}
	twoSigs := tc.newBlock(prev, signer, "two signatures", testKeyOp{op: "A", target: k, signers: []*testSigner{signers[0], signers[1]}})
	tc.mustAccept(twoSigs, blockNewTip)
	if got := tc.activeKeys(); len(got) != 4 {
		t.Fatalf("active keys: %v", got)
	}
	// With four signatories, three are needed
	l := newTestSigner(t)
	tc.signers[l.hash] = l
	signer = tc.next(twoSigs.hash)
	tc.mustRefuse(tc.newBlock(twoSigs.hash, signer, "two of four", testKeyOp{op: "A", target: l, signers: []*testSigner{signers[0], signers[1]}}))
	tc.mustAccept(tc.newBlock(twoSigs.hash, signer, "three of four", testKeyOp{op: "A", target: l, signers: []*testSigner{signers[0], signers[1], signers[2]}}), blockNewTip)
	tc.checkConsistent()
}

func TestExpectedGenesis(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	if err := checkExpectedGenesis(hash, hash); err != nil {
		t.Error(err)
	}
	if err := checkExpectedGenesis(strings.ToUpper(hash)+"\n", hash); err != nil {
		t.Error(err)
	}
	if err := checkExpectedGenesis(strings.Repeat("cd", 32), hash); err == nil || !strings.Contains(err.Error(), hash) {
		t.Errorf("a different hash was accepted: %v", err)
	}
}

// The embedded genesis block has key ops in the old format and a high-S signature: it must stay valid.
func TestGenesisBlockKeepsTheOldFormat(t *testing.T) {
	fn := filepath.Join(t.TempDir(), "genesis.db")
	if err := os.WriteFile(fn, assets.GenesisDB, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBlockFile(fn)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.HashSignature, err = hex.DecodeString(defaultChainParams.GenesisBlockHashSignature); err != nil {
		t.Fatal(err)
	}
	if !b.Timestamp.IsZero() {
		t.Error("the embedded genesis block is supposed to have no Timestamp")
	}
	if _, err := verifyGenesisBlock(b, newKeySet()); err != nil {
		t.Errorf("the embedded genesis block doesn't verify: %v", err)
	}
}
