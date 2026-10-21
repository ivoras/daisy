package daisy

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

/*
 * The versions of the p2p protocol.
 *
 * Every node says in its hello which version of the protocol it speaks ("protocol") and the oldest version that it
 * is willing to talk to ("min_protocol"). Two nodes talk if each of them is new enough for the other. A node which
 * doesn't say anything speaks version 1.
 *
 *  1  The original protocol. Blocks are sent either as a message with the zlib-compressed, base64 encoded block
 *     ("zlib-base64"), or as the URL of the block on the sender's HTTP server ("http"). The latter does not work if the
 *     sender is behind a NAT.
 *  2  Blocks are sent in chunks over the p2p connection ("chunked" encoding), so only the connection which the node
 *     itself has opened is needed. The hello says which address the peer sees us at ("observed_host"), and there is the
 *     "reachable" message.
 *
 * Messages and fields which a node doesn't know are ignored. That's how most additions are made. Adding a version is for
 * the cases where the other side needs to know that it can use something new, or must not be talked to.
 */
const (
	p2pProtocolVersion    = 2
	p2pMinProtocolVersion = 1
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

	// How many bytes of blocks can be being received on all the connections together. The blocks are kept in temporary files
	// until they are all there, and the peers can announce sizes without ever sending the data.
	p2pMaxIncomingBytes = 4 << 30

	// A transfer in which nothing has arrived for this long is dropped
	p2pTransferTimeout = 2 * time.Minute
)

var p2pIncomingBytes atomic.Int64

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
type blockSender struct {
	hash          string
	hashSignature []byte
	file          *os.File
	size          int64
	sent          int64
	index         int
	started       bool
	buf           []byte
}

func newBlockSender(hash string, hashSignature []byte, fileName string) (*blockSender, error) {
	f, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &blockSender{hash: hash, hashSignature: hashSignature, file: f, size: st.Size()}, nil
}

func (s *blockSender) nextMessage() (any, bool, error) {
	if !s.started {
		s.started = true
		return p2pMsgBlockStruct{
			P2pID:         p2pEphemeralID,
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
		s.close()
		return nil, true, errors.New("nothing left to send")
	}
	if int64(cap(s.buf)) < n {
		s.buf = make([]byte, n)
	}
	if _, err := io.ReadFull(s.file, s.buf[:n]); err != nil {
		s.close()
		return nil, true, err
	}
	var zbuf bytes.Buffer
	w, err := zlib.NewWriterLevel(&zbuf, zlib.BestSpeed)
	if err != nil {
		s.close()
		return nil, true, err
	}
	if _, err = w.Write(s.buf[:n]); err == nil {
		err = w.Close()
	}
	if err != nil {
		s.close()
		return nil, true, err
	}
	msg := p2pMsgBlockChunkStruct{
		P2pID: p2pEphemeralID,
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
		s.close()
	}
	return msg, done, nil
}

func (s *blockSender) close() {
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
}

/*
 * Receiving a block in chunks
 */

type incomingBlock struct {
	file          *os.File
	hash          string
	hashSignature string
	size          int64
	received      int64
	nextIndex     int
	lastActivity  time.Time
}

// startIncomingBlock prepares to receive a block whose "block" message with the chunked encoding has arrived
func (p2pc *p2pConnection) startIncomingBlock(hash, hashSignature string, size int64) {
	if _, ok := p2pc.incoming[hash]; ok {
		return
	}
	if len(p2pc.incoming) >= p2pMaxIncomingTransfers {
		log.Println("Too many blocks being received from", p2pc.address+", ignoring", hash)
		return
	}
	if p2pIncomingBytes.Add(size) > p2pMaxIncomingBytes {
		p2pIncomingBytes.Add(-size)
		log.Println("Too much data is being received, ignoring block", hash, "from", p2pc.address)
		return
	}
	f, err := os.CreateTemp("", "daisy")
	if err != nil {
		p2pIncomingBytes.Add(-size)
		log.Println("Error creating temp file", err)
		return
	}
	if p2pc.incoming == nil {
		p2pc.incoming = make(map[string]*incomingBlock)
	}
	p2pc.incoming[hash] = &incomingBlock{file: f, hash: hash, hashSignature: hashSignature, size: size, lastActivity: time.Now()}
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
	p2pIncomingBytes.Add(-in.size)
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
	p2pIncomingBytes.Add(-in.size)
	delete(p2pc.incoming, hash)
	defer func() {
		if err := os.Remove(fileName); err != nil {
			log.Println(err)
		}
	}()
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

type selfInfoType struct {
	lock      util.WithMutex
	observed  map[string]map[string]bool // The address that peers see us at -> the peers (their hosts) that have said so
	reachable map[string]bool            // The peers (their hosts) that have connected to our p2p port
	external  string                     // The address of the router, if we have got a port mapped on it
}

var selfInfo = selfInfoType{observed: make(map[string]map[string]bool), reachable: make(map[string]bool)}

const (
	// How many peers need to agree before we believe something about ourselves
	selfInfoQuorum = 2

	// The bounds of the data kept about the peers, whatever they say
	selfInfoMaxHosts = 16
	selfInfoMaxVotes = 16
)

// observe records that the peer says it sees us at the given address
func (s *selfInfoType) observe(seenAs, peerHost string) {
	if net.ParseIP(seenAs) == nil || peerHost == "" {
		return
	}
	s.lock.With(func() {
		voters, ok := s.observed[seenAs]
		if !ok {
			if len(s.observed) >= selfInfoMaxHosts {
				return
			}
			voters = make(map[string]bool)
			s.observed[seenAs] = voters
		}
		if len(voters) < selfInfoMaxVotes {
			voters[peerHost] = true
		}
	})
}

// seenAs returns the address which most of the peers see us at, or "" if they don't agree
func (s *selfInfoType) seenAs() string {
	best, bestVotes := "", 0
	s.lock.With(func() {
		for host, voters := range s.observed {
			if len(voters) >= selfInfoQuorum && len(voters) > bestVotes {
				best, bestVotes = host, len(voters)
			}
		}
	})
	return best
}

// isSelf tells if the address is one of those we are known by from the outside
func (s *selfInfoType) isSelf(host string) bool {
	if s == nil || host == "" {
		return false
	}
	found := false
	s.lock.With(func() {
		found = s.external == host || len(s.observed[host]) >= selfInfoQuorum
	})
	return found
}

func (s *selfInfoType) confirmReachable(peerHost string) {
	if peerHost == "" {
		return
	}
	s.lock.With(func() {
		if len(s.reachable) < selfInfoMaxVotes {
			s.reachable[peerHost] = true
		}
	})
}

// isReachable tells if enough peers have been able to connect to us
func (s *selfInfoType) isReachable() bool {
	reachable := false
	s.lock.With(func() { reachable = len(s.reachable) >= selfInfoQuorum })
	return reachable
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
}
