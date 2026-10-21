package daisy

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func withMaxBlockHashes(t *testing.T, n int) {
	old := p2pMaxBlockHashes
	p2pMaxBlockHashes = n
	t.Cleanup(func() { p2pMaxBlockHashes = old })
}

func TestClampHashRange(t *testing.T) {
	for _, tt := range []struct {
		min, max int
		want     hashRange
	}{
		{0, 10, hashRange{0, 10}},
		{-5, 10, hashRange{0, 10}},
		{5, 5 + p2pMaxBlockHashes - 1, hashRange{5, 5 + p2pMaxBlockHashes - 1}},
		{5, 5 + p2pMaxBlockHashes, hashRange{5, 5 + p2pMaxBlockHashes - 1}},
		{0, 1 << 62, hashRange{0, p2pMaxBlockHashes - 1}},
		{1 << 62, 1 << 62, hashRange{1 << 40, 1<<40 + p2pMaxBlockHashes - 1}},
		{-1 << 62, 1 << 62, hashRange{0, p2pMaxBlockHashes - 1}},
	} {
		if got := clampHashRange(tt.min, tt.max); got != tt.want {
			t.Errorf("clampHashRange(%d, %d) = %v, want %v", tt.min, tt.max, got, tt.want)
		}
	}
}

func insertTestBlockRows(t *testing.T, n int) {
	t.Helper()
	for h := range n {
		_, err := mainDb.Exec(`INSERT INTO blockchain(height, sigkey_hash, hash, hash_signature, prev_hash, prev_hash_signature, time_accepted, version)
			VALUES (?, 'k', ?, '00', 'p', '00', 0, 1)`, h, testHash(fmt.Sprint("row", h)))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestGetBlockHashesIsClamped(t *testing.T) {
	setupTestSysDbs(t)
	withMaxBlockHashes(t, 10)
	insertTestBlockRows(t, 50)
	p2pc, _ := newPipeConnection(t, "203.0.113.40:2017")

	reply := func(min, max float64) p2pMsgBlockHashesStruct {
		t.Helper()
		p2pc.outbox = nil
		p2pc.handleGetBlockHashes(util.StrIfMap{"min_block_height": min, "max_block_height": max})
		if len(p2pc.outbox) != 1 {
			t.Fatalf("%d replies", len(p2pc.outbox))
		}
		return p2pc.outbox[0].(p2pMsgBlockHashesStruct)
	}
	if r := reply(0, 1e12); len(r.Hashes) != 10 || r.Hashes[0] == "" || r.Hashes[9] == "" {
		t.Errorf("a huge range returned %d hashes", len(r.Hashes))
	}
	if r := reply(-1e12, 1e12); len(r.Hashes) != 10 {
		t.Errorf("a range with a negative start returned %d hashes", len(r.Hashes))
	}
	if r := reply(20, 25); len(r.Hashes) != 6 {
		t.Errorf("a small range returned %d hashes", len(r.Hashes))
	}
	if r := reply(45, 1e12); len(r.Hashes) != 5 {
		t.Errorf("the end of the chain returned %d hashes", len(r.Hashes))
	}
	if r := reply(30, 20); len(r.Hashes) != 0 {
		t.Errorf("an empty range returned %d hashes", len(r.Hashes))
	}

	// A peer which doesn't read what it's sent doesn't get more
	p2pc.outbox = nil
	for range p2pMaxPendingReplies {
		p2pc.send("x")
	}
	p2pc.handleGetBlockHashes(util.StrIfMap{"min_block_height": 0.0, "max_block_height": 5.0})
	if len(p2pc.outbox) != p2pMaxPendingReplies {
		t.Error("a reply was queued for a peer which is not reading")
	}
}

func TestFloodIsClamped(t *testing.T) {
	setupTestSysDbs(t)
	withMaxBlockHashes(t, 10)
	insertTestBlockRows(t, 50)
	peer := newTestConnection(nil, "203.0.113.41:2017")
	peer.handshaken = true
	p2pPeers.Add(peer)
	defer p2pPeers.Remove(peer)
	p2pCoordinator.floodPeersWithNewBlocks(0, 49)
	msg := peer.outbox[0].(p2pMsgBlockHashesStruct)
	if len(msg.Hashes) != 10 || msg.Hashes[49] == "" || msg.Hashes[40] == "" || msg.Hashes[39] != "" {
		t.Errorf("the flood has %d hashes, the newest ones are expected", len(msg.Hashes))
	}
}

func TestSyncStatePages(t *testing.T) {
	withMaxBlockHashes(t, 100)
	now := time.Now()
	var s syncState
	if _, ok := s.next(0, now); ok {
		t.Fatal("a sync that hasn't begun asks for something")
	}
	// A node with 5 blocks, the peer has 349
	r := s.begin(5, 349, now)
	if r != (hashRange{0, 99}) {
		t.Fatalf("the first page is %v", r)
	}
	if _, ok := s.next(50, now); ok {
		t.Error("the next page is asked for before the blocks of the first one are here")
	}
	var pages []hashRange
	height := 50
	for range 20 {
		height = s.asked.max // All the blocks of the page have been imported
		r, ok := s.next(height, now)
		if !ok {
			break
		}
		pages = append(pages, r)
	}
	if len(pages) != 3 || pages[0] != (hashRange{100, 199}) || pages[1] != (hashRange{200, 299}) || pages[2] != (hashRange{300, 349}) {
		t.Errorf("pages: %v", pages)
	}
	if _, ok := s.next(349, now); ok || s.active {
		t.Error("the sync didn't end when the target was reached")
	}

	// A page which makes no progress is asked for again, a few times
	s = syncState{}
	s.begin(0, 1000, now)
	if _, ok := s.next(10, now.Add(p2pSyncStallTimeout/2)); ok {
		t.Error("asked again too soon")
	}
	later := now
	retries := 0
	for range 20 {
		later = later.Add(p2pSyncStallTimeout)
		r, ok := s.next(10, later)
		if !ok {
			break
		}
		retries++
		if r.min != 0 || r.max != 99 { // The 32 blocks before the tip are included, but there is no negative height
			t.Errorf("the retry asks for %v", r)
		}
	}
	if retries != p2pSyncMaxRetries || s.active {
		t.Errorf("%d retries, active: %v", retries, s.active)
	}

	// Heights that are acceptable in a message with hashes
	s = syncState{}
	s.begin(1000, 5000, now)
	if !s.allowed(968, 1000, now) || !s.allowed(1067, 1000, now) {
		t.Error("the range that was asked for is not allowed")
	}
	if s.allowed(1068+p2pMaxBlockHashes, 1000, now) || s.allowed(5000, 1000, now) {
		t.Error("heights far above the chain are allowed")
	}
	if !s.allowed(1000+p2pMaxBlockHashes, 1000, now) || !s.allowed(1000-maxReorgDepth, 1000, now) || s.allowed(1000-maxReorgDepth-1, 1000, now) {
		t.Error("the heights near the tip are not what is expected")
	}
	if !s.allowed(1060, 100, now) {
		t.Error("the range that was asked for is not allowed when it is far from the tip")
	}
	if s.allowed(1060, 100, now.Add(p2pAskedRangeTTL)) {
		t.Error("an old request still counts")
	}
	if s.allowed(-1, 0, now) {
		t.Error("a negative height is allowed")
	}
}

// Takes what the node has queued for the peer
func takeOutbox(p2pc *p2pConnection) []any {
	var msgs []any
	p2pc.outboxLock.With(func() {
		msgs = p2pc.outbox
		p2pc.outbox = nil
	})
	return msgs
}

func asMap(t *testing.T, v any) util.StrIfMap {
	t.Helper()
	var m util.StrIfMap
	if err := json.Unmarshal(util.JSONifyBytes(v), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A node which is behind gets a whole long chain, a page at a time, from a peer that answers within the limits
func TestSyncFromScratchOverManyPages(t *testing.T) {
	withMaxBlockHashes(t, 4)
	withSmallChunks(t, 1000)
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	const length = 23
	remoteChain := []testBlock{tc.genesis}
	for i := 1; i <= length; i++ {
		remoteChain = append(remoteChain, tc.newBlock(remoteChain[i-1].hash, a, fmt.Sprint("remote ", i)))
	}
	byHash := map[string]testBlock{}
	for _, b := range remoteChain {
		byHash[b.hash] = b
	}

	p2pc, _ := newPipeConnection(t, "203.0.113.42:2017")
	p2pc.startSync(length)
	hashRequests := 0
	for round := 0; round < 200; round++ {
		msgs := takeOutbox(p2pc)
		if len(msgs) == 0 {
			break
		}
		for _, msg := range msgs {
			switch m := msg.(type) {
			case p2pMsgGetBlockHashesStruct:
				hashRequests++
				if m.MaxBlockHeight-m.MinBlockHeight+1 > p2pMaxBlockHashes {
					t.Fatalf("a request for %d..%d is too long", m.MinBlockHeight, m.MaxBlockHeight)
				}
				hashes := map[string]any{}
				for h := m.MinBlockHeight; h <= min(m.MaxBlockHeight, length); h++ {
					hashes[fmt.Sprint(h)] = remoteChain[h].hash
				}
				p2pc.handleBlockHashes(util.StrIfMap{"hashes": hashes})
			case p2pMsgGetBlockStruct:
				b, ok := byHash[m.Hash]
				if !ok {
					t.Fatalf("a request for an unknown block %s", m.Hash)
				}
				transferBlockFile(t, p2pc, b.hash, b.sig, b.file, nil)
			}
		}
	}
	if tc.tip().Height != length || tc.tip().Hash != remoteChain[length].hash {
		t.Fatalf("the chain is at height %d, want %d", tc.tip().Height, length)
	}
	if hashRequests < length/p2pMaxBlockHashes {
		t.Errorf("only %d requests for hashes", hashRequests)
	}
	if p2pc.sync.active {
		t.Error("the sync didn't end")
	}
	tc.checkConsistent()
}

func TestBlockHashesAreLimited(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	_ = tc
	withMaxBlockHashes(t, 50)
	ids := time.Now().UnixNano()
	hashesFor := func(from, to int, tag string) map[string]any {
		m := map[string]any{}
		for h := from; h <= to; h++ {
			m[fmt.Sprint(h)] = testHash(fmt.Sprint(tag, ids, h))
		}
		return m
	}
	requests := func(p2pc *p2pConnection) (blocks, searches int) {
		for _, m := range takeOutbox(p2pc) {
			switch m.(type) {
			case p2pMsgGetBlockStruct:
				blocks++
			case p2pMsgGetBlockHashesStruct:
				searches++
			}
		}
		return
	}

	// The announcement of new blocks: near our chain (height 0). The ones too far above it are not asked for one by one
	p2pc, _ := newPipeConnection(t, "203.0.113.43:2017")
	p2pc.handleBlockHashes(util.StrIfMap{"hashes": hashesFor(-300, 120, "a")})
	blocks, searches := requests(p2pc)
	if blocks != 50 {
		t.Errorf("%d blocks requested from a long announcement, the limit is 50", blocks)
	}
	if searches != 1 {
		t.Errorf("%d searches, expected the node to start looking for the blocks that are further", searches)
	}

	// A range which was asked for is fine wherever it is, but nothing else outside the area around the tip
	p2pc, _ = newPipeConnection(t, "203.0.113.44:2017")
	p2pc.sync.asked, p2pc.sync.askedAt = hashRange{1000, 1010}, time.Now()
	p2pc.sync.active = true
	hashes := hashesFor(1000, 1010, "b")
	for k, v := range hashesFor(2000, 2005, "c") {
		hashes[k] = v
	}
	p2pc.handleBlockHashes(util.StrIfMap{"hashes": hashes})
	blocks, _ = requests(p2pc)
	if blocks != 11 {
		t.Errorf("%d blocks requested, want the 11 in the range that was asked for", blocks)
	}

	// Garbage that is not a hash is not asked for
	p2pc, _ = newPipeConnection(t, "203.0.113.45:2017")
	p2pc.handleBlockHashes(util.StrIfMap{"hashes": map[string]any{"1": "not a hash", "2": "", "3": testHash(fmt.Sprint("d", ids))}})
	if blocks, _ = requests(p2pc); blocks != 1 {
		t.Errorf("%d blocks requested, want 1", blocks)
	}
}

func TestGetBlockLimits(t *testing.T) {
	a := newTestSigner(t)
	tc := setupTestChain(t, a)
	var blocks []testBlock
	prev := tc.genesis.hash
	for i := range 4 {
		b := tc.newBlock(prev, a, fmt.Sprint(i))
		tc.mustAccept(b, blockNewTip)
		blocks = append(blocks, b)
		prev = b.hash
	}

	p2pc, _ := newPipeConnection(t, "203.0.113.46:2017")
	for _, b := range blocks {
		p2pc.handleGetBlock(util.StrIfMap{"hash": b.hash})
	}
	p2pc.handleGetBlock(util.StrIfMap{"hash": blocks[0].hash}) // Again
	p2pc.handleGetBlock(util.StrIfMap{"hash": blocks[3].hash}) // Again, it's waiting
	p2pc.handleGetBlock(util.StrIfMap{"hash": testHash("nobody has this")})
	p2pc.handleGetBlock(util.StrIfMap{"hash": "not a hash"})

	senders := func() (s []*blockSender) {
		p2pc.outboxLock.With(func() {
			for _, m := range p2pc.outbox {
				s = append(s, m.(*blockSender))
			}
		})
		return
	}
	got := senders()
	if len(got) != p2pMaxActiveSenders || got[0].hash != blocks[0].hash || got[1].hash != blocks[1].hash {
		t.Fatalf("%d blocks are being sent", len(got))
	}
	if len(p2pc.blockReqs.pending) != 2 || len(p2pc.blockReqs.wanted) != 4 {
		t.Errorf("%d waiting, %d wanted", len(p2pc.blockReqs.pending), len(p2pc.blockReqs.wanted))
	}
	for _, s := range got {
		if s.file != nil {
			t.Error("the file of a block that is not being sent yet is open")
		}
	}

	// When a block is sent, the next waiting one starts
	sender := got[0]
	for done := false; !done; {
		var err error
		if _, done, err = sender.nextMessage(); err != nil {
			t.Fatal(err)
		}
	}
	got = senders()
	if len(got) != 3 || got[2].hash != blocks[2].hash || len(p2pc.blockReqs.pending) != 1 {
		t.Errorf("the next block has not been started: %d senders, %d waiting", len(got), len(p2pc.blockReqs.pending))
	}
	// ... and the block that was sent can be asked for again
	if p2pc.blockReqs.wanted[blocks[0].hash] {
		t.Error("a block that has been sent is still wanted")
	}
	for _, s := range got {
		s.close()
	}
}

func TestBlockRequestQueueIsBounded(t *testing.T) {
	setupTestSysDbs(t)
	insertTestBlockRows(t, p2pMaxPendingBlockRequests+100)
	p2pc, _ := newPipeConnection(t, "203.0.113.47:2017")
	p2pc.blockReqs.active = p2pMaxActiveSenders // Busy
	for i := range p2pMaxPendingBlockRequests + 100 {
		p2pc.requestBlockToSend(testHash(fmt.Sprint("row", i)))
	}
	p2pc.requestBlockToSend(testHash("a block that nobody has"))
	if len(p2pc.blockReqs.pending) != p2pMaxPendingBlockRequests || len(p2pc.blockReqs.wanted) != p2pMaxPendingBlockRequests {
		t.Errorf("%d waiting, %d wanted", len(p2pc.blockReqs.pending), len(p2pc.blockReqs.wanted))
	}
}

// A block that has gone away (e.g. in a reorganization) between the request and the start of the transfer
func TestBlockSenderWithVanishedFile(t *testing.T) {
	dir := t.TempDir()
	finished := 0
	missing := newBlockSender("h", nil, dir+"/nothing")
	missing.onFinish = func() { finished++ }
	if _, done, err := missing.nextMessage(); err == nil || !done || finished != 1 {
		t.Errorf("a missing file: err=%v done=%v finished=%d", err, done, finished)
	}
	empty := dir + "/empty"
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := newBlockSender("h", nil, empty)
	s.onFinish = func() { finished++ }
	if _, done, err := s.nextMessage(); err == nil || !done || finished != 2 {
		t.Errorf("an empty file: err=%v done=%v finished=%d", err, done, finished)
	}
	// Abandoning a transfer doesn't start anything
	s = newBlockSender("h", nil, empty)
	s.onFinish = func() { finished++ }
	s.close()
	s.finish()
	if finished != 2 {
		t.Error("an abandoned transfer reported that it was over")
	}
}
