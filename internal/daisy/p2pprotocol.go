package daisy

import (
	"bytes"
	"compress/zlib"
	"crypto/ecdsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/ivoras/daisy/internal/dcrypto"
	"github.com/ivoras/daisy/internal/util"
)

/*
 * The versions of the p2p protocol.
 *
 * Every node says in its hello which version of the protocol it speaks ("protocol") and the oldest version that it
 * is willing to talk to ("min_protocol"). Two nodes talk if each of them is new enough for the other. A hello without
 * these fields is from a node of an old, unsupported version, and the connection is closed.
 *
 * Version 3 is the oldest one there is: versions 1 and 2 were never deployed, and their support has been removed (their
 * numbers are not reused, so that such nodes are told apart). Blocks are sent in chunks over the p2p connection. A
 * "getblockhashes" request covers at most p2pMaxBlockHashes heights, and the answer is clamped to that, so long ranges
 * are fetched page by page. A "block" must be signed by a key which the receiver knows, or be a block which it has asked
 * for. There is a "ping" message to keep idle connections open, and a connection which is silent for too long is closed.
 *
 * Messages and fields which a node doesn't know are ignored. That's how most additions are made. Adding a version is for
 * the cases where the other side needs to know that it can use something new, or must not be talked to.
 */
const (
	p2pProtocolVersion    = 3
	p2pMinProtocolVersion = 3
)

// What to do with a hello from a peer which speaks the given versions. Returns an error if we should not talk to it.
func p2pCheckProtocol(peerProtocol, peerMinProtocol int) error {
	if peerProtocol < p2pMinProtocolVersion {
		return fmt.Errorf("the peer speaks protocol version %d, and the oldest one that we support is %d", peerProtocol, p2pMinProtocolVersion)
	}
	if peerMinProtocol > p2pProtocolVersion {
		return fmt.Errorf("the peer requires protocol version %d, and we speak %d", peerMinProtocol, p2pProtocolVersion)
	}
	return nil
}

// The message telling the peer that we have connected to its p2p port and it works
const p2pMsgReachable = "reachable"

type p2pMsgReachableStruct struct {
	p2pMsgHeader
	Port int `json:"port"`
}

// The message with a piece of a block (see blockSender)
const p2pMsgBlockChunk = "blockchunk"

type p2pMsgBlockChunkStruct struct {
	p2pMsgHeader
	Hash  string `json:"hash"`
	Index int    `json:"index"`
	Data  string `json:"data"` // zlib compressed, base64 encoded
}

const p2pEncodingChunked = "chunked"

// The size of the pieces of the blocks which are sent (before compression). A variable so that the tests can make it small.
var p2pChunkSize = 512 << 10

const (
	// How many blocks can be received on one connection at the same time
	p2pMaxIncomingTransfers = 2

	// How many bytes of blocks can be being received on all the connections together, and from one host (all of its
	// connections). The blocks are kept in temporary files until they are all there, and the peers can announce sizes
	// without ever sending the data. The limit per host is what stops one host from using up everyone's share.
	p2pMaxIncomingBytes        = 4 << 30
	p2pMaxIncomingBytesPerHost = 1 << 30

	// A transfer in which nothing has arrived for this long is dropped
	p2pTransferTimeout = 2 * time.Minute

	// The longest signature (ASN.1 DER ECDSA P-256 signatures are at most 72 bytes) which is looked at
	p2pMaxSignatureSize = 128

	// How many of the blocks which we have asked a peer for are remembered, and for how long
	p2pMaxRequestedBlocks = 4096
	p2pRequestedBlockTTL  = 30 * time.Minute
)

// incomingBudget accounts for the bytes of the blocks which are being received, in total and for each host.
type incomingBudget struct {
	lock     util.WithMutex
	maxTotal int64
	maxHost  int64
	total    int64
	byHost   map[string]int64
}

func newIncomingBudget(maxTotal, maxHost int64) *incomingBudget {
	return &incomingBudget{maxTotal: maxTotal, maxHost: maxHost, byHost: make(map[string]int64)}
}

// reserve takes size bytes of the budget on behalf of the host, if there is room for them
func (b *incomingBudget) reserve(host string, size int64) bool {
	ok := false
	b.lock.With(func() {
		if b.total+size > b.maxTotal || b.byHost[host]+size > b.maxHost {
			return
		}
		b.total += size
		b.byHost[host] += size
		ok = true
	})
	return ok
}

// release gives back what reserve has taken
func (b *incomingBudget) release(host string, size int64) {
	b.lock.With(func() {
		b.total -= size
		if left := b.byHost[host] - size; left > 0 {
			b.byHost[host] = left
		} else {
			delete(b.byHost, host)
		}
	})
}

func (b *incomingBudget) totalBytes() int64 {
	var n int64
	b.lock.With(func() { n = b.total })
	return n
}

func (b *incomingBudget) hostBytes(host string) int64 {
	var n int64
	b.lock.With(func() { n = b.byHost[host] })
	return n
}

var p2pIncoming = newIncomingBudget(p2pMaxIncomingBytes, p2pMaxIncomingBytesPerHost)

// lazyMessage is something in the outbox which produces the messages when they are being sent, rather than having them all
// made in advance. It's for the data which is too big to be kept in memory in the form of messages.
type lazyMessage interface {
	// nextMessage returns the next message to send, and tells if it was the last one.
	nextMessage() (msg any, done bool, err error)
	// close releases the resources if the rest of the messages is never going to be needed
	close()
}

/*
 * Sending a block in chunks
 */

// blockSender sends a block file as the "block" message with the chunked encoding, followed by the "blockchunk" messages.
// The file is opened when the first message is asked for, not before, so that a peer which asks for many blocks doesn't
// make us keep many files open (see blockRequests).
type blockSender struct {
	hash          string
	hashSignature []byte
	fileName      string
	file          *os.File
	size          int64
	sent          int64
	index         int
	started       bool
	finished      bool
	buf           []byte
	onFinish      func() // called once when the transfer is over, successful or not (not when it's abandoned with close)
}

func newBlockSender(hash string, hashSignature []byte, fileName string) *blockSender {
	return &blockSender{hash: hash, hashSignature: hashSignature, fileName: fileName}
}

// open opens the file of the block, which may have gone away since it was asked for (e.g. in a reorganization)
func (s *blockSender) open() error {
	f, err := os.Open(s.fileName)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if st.Size() <= 0 || st.Size() > p2pMaxBlockSize {
		f.Close()
		return fmt.Errorf("the block file has an unacceptable size %d", st.Size())
	}
	s.file, s.size = f, st.Size()
	return nil
}

func (s *blockSender) nextMessage() (any, bool, error) {
	if !s.started {
		s.started = true
		if err := s.open(); err != nil {
			s.finish()
			return nil, true, err
		}
		return p2pMsgBlockStruct{
			Root:          chainParams.GenesisBlockHash,
			Msg:           p2pMsgBlock,
			Hash:          s.hash,
			HashSignature: hex.EncodeToString(s.hashSignature),
			Size:          s.size,
			Encoding:      p2pEncodingChunked,
		}, false, nil
	}
	n := min(int64(p2pChunkSize), s.size-s.sent)
	if n <= 0 {
		s.finish()
		return nil, true, errors.New("nothing left to send")
	}
	if int64(cap(s.buf)) < n {
		s.buf = make([]byte, n)
	}
	if _, err := io.ReadFull(s.file, s.buf[:n]); err != nil {
		s.finish()
		return nil, true, err
	}
	var zbuf bytes.Buffer
	w, err := zlib.NewWriterLevel(&zbuf, zlib.BestSpeed)
	if err != nil {
		s.finish()
		return nil, true, err
	}
	if _, err = w.Write(s.buf[:n]); err == nil {
		err = w.Close()
	}
	if err != nil {
		s.finish()
		return nil, true, err
	}
	msg := p2pMsgBlockChunkStruct{
		Root:  chainParams.GenesisBlockHash,
		Msg:   p2pMsgBlockChunk,
		Hash:  s.hash,
		Index: s.index,
		Data:  base64.StdEncoding.EncodeToString(zbuf.Bytes()),
	}
	s.index++
	s.sent += n
	done := s.sent >= s.size
	if done {
		s.finish()
	}
	return msg, done, nil
}

// close releases the file of a transfer which is abandoned
func (s *blockSender) close() {
	s.finished = true
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
}

// finish releases the file of a transfer which is over, and tells whoever is interested
func (s *blockSender) finish() {
	already := s.finished
	s.close()
	if !already && s.onFinish != nil {
		s.onFinish()
	}
}

/*
 * Which blocks a peer is being sent
 *
 * A peer can ask for any number of blocks. They are not all started at once: only a few are being sent at a time (each
 * keeps a file open), the others wait as bare hashes, up to a limit. What's over the limit is dropped, the peer finds out
 * that it has not got the block and asks again. A request for a block which is already waiting or being sent is ignored.
 */

const (
	p2pMaxActiveSenders        = 2
	p2pMaxPendingBlockRequests = 4096
)

type blockRequests struct {
	wanted  map[string]bool // the blocks which are being sent or are waiting
	active  int             // how many of them are being sent
	pending []string        // the ones waiting, in the order of the requests
}

// requestBlockToSend handles the peer's request for a block
func (p2pc *p2pConnection) requestBlockToSend(hash string) {
	br := &p2pc.blockReqs
	if br.wanted[hash] || !dbBlockHashKnown(hash) {
		return // It's coming, or we don't have it
	}
	if br.wanted == nil {
		br.wanted = make(map[string]bool)
	}
	switch {
	case br.active < p2pMaxActiveSenders:
		br.wanted[hash] = true
		p2pc.startBlockSender(hash)
	case len(br.pending) < p2pMaxPendingBlockRequests:
		br.wanted[hash] = true
		br.pending = append(br.pending, hash)
	default:
		log.Println("Too many block requests from", p2pc.address+", ignoring", hash)
	}
}

// startBlockSender queues the sending of a block. The block must have been put in blockRequests.wanted.
func (p2pc *p2pConnection) startBlockSender(hash string) {
	br := &p2pc.blockReqs
	dbb, onBestChain, err := dbGetAnyBlock(hash)
	if err != nil {
		log.Println("Cannot send block", hash, "to", p2pc.address+":", err)
		delete(br.wanted, hash)
		return
	}
	fileName := sideBlockFilename(dbb.Hash)
	if onBestChain {
		fileName = blockchainGetFilename(dbb.Height)
	}
	sender := newBlockSender(hash, dbb.HashSignature, fileName)
	sender.onFinish = func() { p2pc.blockSenderDone(hash) }
	br.active++
	p2pc.send(sender)
	log.Println("*** Sending block", hash, "to", p2pc.address)
}

// blockSenderDone starts the next waiting block when a transfer is over. It's called by the connection's goroutine.
func (p2pc *p2pConnection) blockSenderDone(hash string) {
	br := &p2pc.blockReqs
	delete(br.wanted, hash)
	br.active--
	for br.active < p2pMaxActiveSenders && len(br.pending) > 0 {
		next := br.pending[0]
		br.pending = br.pending[1:]
		p2pc.startBlockSender(next)
	}
}

/*
 * Receiving a block in chunks
 */

type incomingBlock struct {
	file          *os.File
	hash          string
	hashSignature string
	host          string // whose share of the budget it is counted in
	size          int64
	received      int64
	nextIndex     int
	lastActivity  time.Time
}

// budgetHost is the key under which the bytes received from the peer are counted
func (p2pc *p2pConnection) budgetHost() string {
	if p2pc.host != "" {
		return p2pc.host
	}
	if host, _, err := net.SplitHostPort(p2pc.address); err == nil {
		return host
	}
	return p2pc.address
}

// startIncomingBlock prepares to receive a block whose "block" message with the chunked encoding has arrived. The caller
// has checked that the block is welcome (see handleBlock), this checks that there is room for it.
func (p2pc *p2pConnection) startIncomingBlock(hash, hashSignature string, size int64) {
	if _, ok := p2pc.incoming[hash]; ok {
		return
	}
	if len(p2pc.incoming) >= p2pMaxIncomingTransfers {
		log.Println("Too many blocks being received from", p2pc.address+", ignoring", hash)
		return
	}
	host := p2pc.budgetHost()
	if !p2pIncoming.reserve(host, size) {
		log.Println("Too much data is being received (from", host+"), ignoring block", hash, "from", p2pc.address)
		return
	}
	f, err := os.CreateTemp("", "daisy")
	if err != nil {
		p2pIncoming.release(host, size)
		log.Println("Error creating temp file", err)
		return
	}
	if p2pc.incoming == nil {
		p2pc.incoming = make(map[string]*incomingBlock)
	}
	p2pc.incoming[hash] = &incomingBlock{file: f, hash: hash, hashSignature: hashSignature, host: host, size: size, lastActivity: time.Now()}
}

// abortIncomingBlock gives up on a block which is being received
func (p2pc *p2pConnection) abortIncomingBlock(in *incomingBlock, reason string) {
	if reason != "" {
		log.Printf("Dropping block %s from %s: %s", in.hash, p2pc.address, reason)
	}
	in.file.Close()
	if err := os.Remove(in.file.Name()); err != nil {
		log.Println(err)
	}
	p2pIncoming.release(in.host, in.size)
	delete(p2pc.incoming, in.hash)
}

func (p2pc *p2pConnection) abortAllIncomingBlocks() {
	for _, in := range p2pc.incoming {
		p2pc.abortIncomingBlock(in, "")
	}
}

// expireIncomingBlocks drops the transfers which have stalled
func (p2pc *p2pConnection) expireIncomingBlocks() {
	for _, in := range p2pc.incoming {
		if time.Since(in.lastActivity) > p2pTransferTimeout {
			p2pc.abortIncomingBlock(in, "the transfer has stalled")
		}
	}
}

// handleBlockChunk adds a piece to the block which is being received. The pieces must arrive in order, and
// there must be no more of them than what the sender has announced.
func (p2pc *p2pConnection) handleBlockChunk(msg util.StrIfMap) {
	hash, err := msg.GetString("hash")
	if err != nil {
		return
	}
	in, ok := p2pc.incoming[hash]
	if !ok {
		return // We have given up on it, or didn't want it in the first place
	}
	index, err := msg.GetInt("index")
	if err != nil || index != in.nextIndex {
		p2pc.abortIncomingBlock(in, "chunks are out of order")
		return
	}
	data, err := msg.GetString("data")
	if err != nil {
		p2pc.abortIncomingBlock(in, "chunk without data")
		return
	}
	raw, err := decodeChunk(data)
	if err != nil {
		p2pc.abortIncomingBlock(in, err.Error())
		return
	}
	if int64(len(raw)) > int64(p2pChunkSize) || in.received+int64(len(raw)) > in.size {
		p2pc.abortIncomingBlock(in, "more data than announced")
		return
	}
	if _, err = in.file.Write(raw); err != nil {
		p2pc.abortIncomingBlock(in, err.Error())
		return
	}
	in.received += int64(len(raw))
	in.nextIndex++
	in.lastActivity = time.Now()
	if in.received < in.size {
		return
	}

	// It's all here
	fileName := in.file.Name()
	if err = in.file.Close(); err != nil {
		p2pc.abortIncomingBlock(in, err.Error())
		return
	}
	p2pIncoming.release(in.host, in.size)
	delete(p2pc.incoming, hash)
	defer func() {
		if err := os.Remove(fileName); err != nil {
			log.Println(err)
		}
	}()
	// The signature was checked against the announced hash, this makes sure that the data really is that block before it
	// gets anywhere near the block tree (which would hash it and open it while holding the chain lock)
	if got, err := util.HashFileToHexString(fileName); err != nil || got != in.hash {
		log.Printf("Dropping block %s from %s: the data is not the block that was announced (%s, %v)", in.hash, p2pc.address, got, err)
		return
	}
	p2pc.importBlock(fileName, in.hash, in.hashSignature)
}

// decodeChunk decompresses a piece of a block, producing at most p2pChunkSize bytes
func decodeChunk(data string) ([]byte, error) {
	zdata, err := base64Decode(data)
	if err != nil {
		return nil, err
	}
	r, err := zlib.NewReader(bytes.NewReader(zdata))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, int64(p2pChunkSize)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty chunk")
	}
	return raw, nil
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

/*
 * What the node knows about itself from what the peers tell.
 */

// What the peers tell us is only informational (the status endpoint), they can lie. What decides whom we dial is what we have
// found out ourselves: the router's address when we have a port mapped (external), and the addresses at which a connection
// that we made came back to us (selfHosts).
type selfInfoType struct {
	lock      util.WithMutex
	observed  map[string]selfVote  // Peer host -> the address it says it sees us at. One vote per peer host
	reachable map[string]time.Time // Peer host -> when it said that it has connected to our p2p port
	external  string               // The address of the router, if we have got a port mapped on it
	selfHosts map[string]time.Time // The addresses at which we have found ourselves, by connecting to them -> when
}

// An opinion of a peer about our address
type selfVote struct {
	seenAs string
	at     time.Time
}

func newSelfInfo() selfInfoType {
	return selfInfoType{observed: make(map[string]selfVote), reachable: make(map[string]time.Time), selfHosts: make(map[string]time.Time)}
}

var selfInfo = newSelfInfo()

// selfInfoClock is the clock of selfInfo, a variable so that the tests can move time
var selfInfoClock = time.Now

const (
	// How many peers need to agree before we believe something about ourselves
	selfInfoQuorum = 2

	// The bounds of the data kept about the peers, whatever they say
	selfInfoMaxHosts = 16
	selfInfoMaxVotes = 64

	// How long a vote of a peer counts. A peer which still has the same opinion repeats it with every new connection.
	selfInfoVoteTTL = time.Hour

	// How long an address stays known to be ours
	selfInfoSelfHostTTL = time.Hour
)

// observe records that the peer says it sees us at the given address. A peer host has one vote: a new one replaces
// the old. Only to be called for the hello of a peer that we have connected to.
func (s *selfInfoType) observe(seenAs, peerHost string) {
	if net.ParseIP(seenAs) == nil || peerHost == "" {
		return
	}
	s.lock.With(func() {
		now := selfInfoClock()
		if _, ok := s.observed[peerHost]; !ok && len(s.observed) >= selfInfoMaxVotes {
			for host, v := range s.observed {
				if now.Sub(v.at) > selfInfoVoteTTL {
					delete(s.observed, host)
				}
			}
			if len(s.observed) >= selfInfoMaxVotes {
				return
			}
		}
		s.observed[peerHost] = selfVote{seenAs: seenAs, at: now}
	})
}

// seenAs returns the address which most of the peers see us at, or "" if they don't agree. Informational.
func (s *selfInfoType) seenAs() string {
	best, bestVotes := "", 0
	s.lock.With(func() {
		now := selfInfoClock()
		counts := make(map[string]int)
		for _, v := range s.observed {
			if now.Sub(v.at) <= selfInfoVoteTTL {
				counts[v.seenAs]++
			}
		}
		for host, n := range counts {
			if n >= selfInfoQuorum && (n > bestVotes || (n == bestVotes && host < best)) {
				best, bestVotes = host, n
			}
		}
	})
	return best
}

// isSelf tells if the address is known to be ours, so that we shouldn't dial it. It only relies on what we have proven
// ourselves, never on what the peers say.
func (s *selfInfoType) isSelf(host string) bool {
	if s == nil || host == "" {
		return false
	}
	found := false
	s.lock.With(func() {
		at, ok := s.selfHosts[host]
		found = s.external == host || (ok && selfInfoClock().Sub(at) <= selfInfoSelfHostTTL)
	})
	return found
}

// confirmReachable records that the peer says it has been able to connect to our p2p port. Each peer host counts once.
func (s *selfInfoType) confirmReachable(peerHost string) {
	if peerHost == "" {
		return
	}
	s.lock.With(func() {
		now := selfInfoClock()
		if _, ok := s.reachable[peerHost]; !ok && len(s.reachable) >= selfInfoMaxVotes {
			for host, at := range s.reachable {
				if now.Sub(at) > selfInfoVoteTTL {
					delete(s.reachable, host)
				}
			}
			if len(s.reachable) >= selfInfoMaxVotes {
				return
			}
		}
		s.reachable[peerHost] = now
	})
}

// isReachable tells if enough peers have been able to connect to us. Informational.
func (s *selfInfoType) isReachable() bool {
	n := 0
	s.lock.With(func() {
		now := selfInfoClock()
		for _, at := range s.reachable {
			if now.Sub(at) <= selfInfoVoteTTL {
				n++
			}
		}
	})
	return n >= selfInfoQuorum
}

// addSelfHost records an address which we know to be ours, because a connection to it has come back to us
func (s *selfInfoType) addSelfHost(host string) {
	if host == "" {
		return
	}
	s.lock.With(func() {
		now := selfInfoClock()
		if _, ok := s.selfHosts[host]; !ok && len(s.selfHosts) >= selfInfoMaxHosts {
			oldest, oldestAt := "", now
			for h, at := range s.selfHosts {
				if oldest == "" || at.Before(oldestAt) {
					oldest, oldestAt = h, at
				}
			}
			delete(s.selfHosts, oldest)
		}
		s.selfHosts[host] = now
	})
}

func (s *selfInfoType) getExternal() string {
	var host string
	s.lock.With(func() { host = s.external })
	return host
}

func (s *selfInfoType) setExternal(host string) {
	s.lock.With(func() { s.external = host })
}

// importBlock passes a block which has been received, as a file, to the block tree
func (p2pc *p2pConnection) importBlock(fileName, hash, hashSignature string) {
	signature, err := hex.DecodeString(hashSignature)
	if err != nil {
		log.Println("Error decoding hash signature", p2pc.address, err)
		return
	}
	info, err := acceptBlock(fileName, signature)
	if err != nil {
		log.Println("Cannot import block:", err)
		return
	}
	if info.hash != hash {
		log.Printf("The block from %s is %s, not the %s which it announced", p2pc.address, info.hash, hash)
	}
	if info.result == blockOrphan {
		// It doesn't connect to anything we know: the peer should have the blocks in between
		log.Println("Block", info.hash, "is waiting for", info.missing)
		p2pc.requestBlock(info.missing)
	}
	if info.accepted() {
		p2pc.syncProgress()
	}
}

/*
 * Who may have signed a block that we are sent
 *
 * The "block" message announces the hash of the block and its signature. Checking it costs a few signature verifications
 * and tells whether the block can possibly be valid, so it's done before anything is reserved or downloaded. The blocks of
 * the side branches can be signed by keys which are not active at our tip (they were revoked, or they are added by the
 * branch itself), so every key we have ever heard of counts, whatever its state. A branch which adds a new key can only be
 * received by asking for its blocks (see p2pConnection.requested).
 */

type signatoryKeys struct {
	lock  util.WithMutex
	db    *sql.DB // the database the keys are from
	count int     // the number of rows of the pubkeys table when they were loaded
	keys  []*ecdsa.PublicKey
}

var signatories signatoryKeys

// current returns all the keys in the pubkeys table. The table only grows and the keys in its rows don't change, so
// they're loaded again only when the number of rows is different.
func (s *signatoryKeys) current() []*ecdsa.PublicKey {
	var keys []*ecdsa.PublicKey
	s.lock.With(func() {
		db := mainDb
		if db == nil {
			return
		}
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM pubkeys").Scan(&count); err != nil {
			log.Println("Cannot count the public keys:", err)
			return
		}
		if db != s.db || count != s.count {
			s.load(db, count)
		}
		keys = s.keys
	})
	return keys
}

func (s *signatoryKeys) load(db *sql.DB, count int) {
	rows, err := db.Query("SELECT pubkey FROM pubkeys")
	if err != nil {
		log.Println("Cannot read the public keys:", err)
		return
	}
	defer rows.Close()
	var keys []*ecdsa.PublicKey
	for rows.Next() {
		var pubKeyHex string
		if err = rows.Scan(&pubKeyHex); err != nil {
			continue
		}
		raw, err := hex.DecodeString(pubKeyHex)
		if err != nil {
			continue
		}
		if key, err := dcrypto.DecodePublicKeyBytes(raw); err == nil {
			keys = append(keys, key)
		}
	}
	if rows.Err() != nil {
		return
	}
	s.db, s.count, s.keys = db, count, keys
}

// signedByKnownSignatory tells if the signature of the hash is made by one of the keys which we know
func signedByKnownSignatory(hash string, signature []byte) bool {
	for _, key := range signatories.current() {
		if dcrypto.VerifyHexBytes(key, hash, signature) == nil {
			return true
		}
	}
	return false
}

// isBlockHash tells if the string looks like the hash of a block: 32 bytes in hex
func isBlockHash(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
