package daisy

import (
	"log"
	"slices"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

/*
 * Finding out which blocks a peer has, page by page
 *
 * The hashes of the blocks of the best chain are asked for with "getblockhashes" and come in "blockhashes". Neither
 * can cover more than p2pMaxBlockHashes heights: the answering side cuts the range, and the asking side doesn't ask for more.
 * A node which is far behind therefore goes through the peer's chain one page at a time: when the blocks of a page have
 * been imported (the page is "done"), the next one is asked for. If nothing happens for a while, the page is asked for again,
 * a few times.
 */

// The most heights covered by one getblockhashes, and the most hashes handled from one blockhashes. A variable so that the
// tests can make it small.
var p2pMaxBlockHashes = 2000

const (
	// A page whose blocks haven't made progress for this long is asked for again, at most p2pSyncMaxRetries times
	p2pSyncStallTimeout = 60 * time.Second
	p2pSyncMaxRetries   = 5

	// The answer to a request is expected within this time
	p2pAskedRangeTTL = 2 * time.Minute

	// A peer which doesn't read is not sent more than this many messages with hashes
	p2pMaxPendingReplies = 32
)

type hashRange struct {
	min, max int
}

// clampHashRange cuts the range so that it's valid and has at most p2pMaxBlockHashes heights
func clampHashRange(minHeight, maxHeight int) hashRange {
	minHeight = max(minHeight, 0)
	minHeight = min(minHeight, 1<<40) // so that the sums below can't overflow
	maxHeight = min(maxHeight, minHeight+p2pMaxBlockHashes-1)
	return hashRange{min: minHeight, max: maxHeight}
}

// syncState is what a connection knows about how far it has got with the blocks of the peer. It's not safe for
// concurrent use, p2pConnection.syncLock guards it.
type syncState struct {
	active       bool
	target       int       // the height of the chain of the peer (that's what it said, it isn't known to be true)
	asked        hashRange // the last range we have asked the peer for
	askedAt      time.Time
	lastProgress time.Time
	retries      int
}

// begin starts (or restarts) the search for the blocks of the peer, and returns the first request. The range starts a bit
// before our tip, so that the blocks of a competing branch, which forks off below it, are found too.
func (s *syncState) begin(myHeight, peerHeight int, now time.Time) hashRange {
	if !s.active {
		s.retries = 0
		s.lastProgress = now
	}
	s.active = true
	s.target = max(s.target, peerHeight)
	return s.ask(clampHashRange(myHeight-reorgSearchWindow, s.target), now)
}

func (s *syncState) ask(r hashRange, now time.Time) hashRange {
	s.asked, s.askedAt = r, now
	return r
}

// next tells if another page of hashes is to be asked for, and which
func (s *syncState) next(myHeight int, now time.Time) (hashRange, bool) {
	if !s.active {
		return hashRange{}, false
	}
	if myHeight >= s.target {
		s.active = false
		return hashRange{}, false
	}
	if s.asked.max < s.target && myHeight >= s.asked.max {
		// The page is done and there is more
		s.lastProgress = now
		s.retries = 0
		return s.ask(clampHashRange(s.asked.max+1, s.target), now), true
	}
	if now.Sub(s.lastProgress) >= p2pSyncStallTimeout {
		s.lastProgress = now
		s.retries++
		if s.retries > p2pSyncMaxRetries {
			s.active = false
			return hashRange{}, false
		}
		return s.ask(clampHashRange(myHeight-reorgSearchWindow, s.target), now), true
	}
	return hashRange{}, false
}

// progress records that a block has been imported
func (s *syncState) progress(now time.Time) {
	s.lastProgress = now
	s.retries = 0
}

// allowed tells if a hash of the given height in a "blockhashes" message is acceptable: either it is in the range which was
// asked for, or it's near our tip, which is what a peer who announces its new blocks sends.
func (s *syncState) allowed(height, myHeight int, now time.Time) bool {
	if height < 0 {
		return false
	}
	if !s.askedAt.IsZero() && now.Sub(s.askedAt) < p2pAskedRangeTTL && height >= s.asked.min && height <= s.asked.max {
		return true
	}
	return height >= max(0, myHeight-maxReorgDepth) && height <= myHeight+p2pMaxBlockHashes
}

// sendGetBlockHashes asks the peer for the hashes of the blocks in the range
func (p2pc *p2pConnection) sendGetBlockHashes(r hashRange) {
	log.Printf("Searching for blocks from %d to %d at %s", r.min, r.max, p2pc.address)
	p2pc.send(p2pMsgGetBlockHashesStruct{
		Root:           chainParams.GenesisBlockHash,
		Msg:            p2pMsgGetBlockHashes,
		MinBlockHeight: r.min,
		MaxBlockHeight: r.max,
	})
}

// startSync looks for the blocks of a peer which apparently has a better chain than we do (it can be called from any goroutine)
func (p2pc *p2pConnection) startSync(peerHeight int) {
	var r hashRange
	p2pc.syncLock.With(func() { r = p2pc.sync.begin(dbGetBlockchainHeight(), peerHeight, time.Now()) })
	p2pc.sendGetBlockHashes(r)
}

// continueSync asks for the next page of hashes of the peer if that's due
func (p2pc *p2pConnection) continueSync() {
	var r hashRange
	var ok bool
	p2pc.syncLock.With(func() {
		if p2pc.sync.active {
			r, ok = p2pc.sync.next(dbGetBlockchainHeight(), time.Now())
		}
	})
	if ok {
		p2pc.sendGetBlockHashes(r)
	}
}

// syncProgress is called when a block from the peer has been imported
func (p2pc *p2pConnection) syncProgress() {
	p2pc.syncLock.With(func() { p2pc.sync.progress(time.Now()) })
	p2pc.continueSync()
}

// outboxLength returns the number of the messages waiting to be sent to the peer
func (p2pc *p2pConnection) outboxLength() int {
	n := 0
	p2pc.outboxLock.With(func() { n = len(p2pc.outbox) })
	return n
}

// Handle getblockhashes
func (p2pc *p2pConnection) handleGetBlockHashes(msg util.StrIfMap) {
	minBlockHeight, err := msg.GetInt("min_block_height")
	if err != nil {
		log.Println(p2pc.address, err)
		return
	}
	maxBlockHeight, err := msg.GetInt("max_block_height")
	if err != nil {
		log.Println(p2pc.address, err)
		return
	}
	if p2pc.outboxLength() >= p2pMaxPendingReplies {
		log.Println("Ignoring the request for block hashes from", p2pc.address+", which is not reading what it has been sent")
		return
	}
	r := clampHashRange(minBlockHeight, maxBlockHeight)
	log.Printf("*** Sending block hashes from %d to %d to %s", r.min, r.max, p2pc.address)
	p2pc.send(p2pMsgBlockHashesStruct{
		Root:   chainParams.GenesisBlockHash,
		Msg:    p2pMsgBlockHashes,
		Hashes: dbGetHeightHashes(r.min, r.max),
	})
}

// Handle receiving blockhashes
func (p2pc *p2pConnection) handleBlockHashes(msg util.StrIfMap) {
	hashes, err := msg.GetIntStringMap("hashes")
	if err != nil {
		log.Println(p2pc.address, err)
		return
	}
	heights := make([]int, 0, len(hashes))
	for h := range hashes {
		heights = append(heights, h)
	}
	slices.Sort(heights)
	log.Println("handleBlockHashes: got", len(heights), "hashes")
	if len(heights) > p2pMaxBlockHashes {
		log.Println(p2pc.address, "has sent more hashes than the limit of", p2pMaxBlockHashes)
	}
	myHeight := dbGetBlockchainHeight()
	now := time.Now()
	handled := 0
	beyond := 0 // the highest height announced which is too far above our chain to be a block following our tip
	p2pc.syncLock.With(func() {
		for _, h := range heights {
			if p2pc.sync.allowed(h, myHeight, now) {
				continue
			}
			if h > myHeight+p2pMaxBlockHashes {
				beyond = max(beyond, h)
			}
			delete(hashes, h)
		}
	})
	for _, h := range heights {
		if handled >= p2pMaxBlockHashes {
			break
		}
		hash, ok := hashes[h]
		if !ok || !isBlockHash(hash) {
			continue
		}
		handled++
		if h < myHeight-maxReorgDepth {
			// Such a branch would not be accepted anyway
			continue
		}
		if dbBlockHashKnown(hash) {
			// On the best chain, or on another branch
			continue
		}
		p2pc.requestBlock(hash)
	}
	if beyond > 0 {
		// The peer is much further than the hashes that it has announced: they are for us to ask for, page by page
		p2pc.startSyncIfIdle(beyond)
	}
	p2pc.continueSync()
}

// startSyncIfIdle begins looking for the blocks of the peer unless that's already being done
func (p2pc *p2pConnection) startSyncIfIdle(peerHeight int) {
	active := false
	p2pc.syncLock.With(func() { active = p2pc.sync.active })
	if active {
		return
	}
	p2pc.startSync(peerHeight)
}
