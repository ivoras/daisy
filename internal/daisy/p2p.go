package daisy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

const p2pClientVersionString = "godaisy/0.3"

// Header for JSON messages we're sending
type p2pMsgHeader struct {
	Root  string `json:"root"`
	Msg   string `json:"msg"`
	P2pID int64  `json:"p2p_id"`
}

// The hello message
const p2pMsgHello = "hello"

type p2pMsgHelloStruct struct {
	p2pMsgHeader
	Version     string   `json:"version"`
	ChainHeight int      `json:"chain_height"`
	MyPeers     []string `json:"my_peers"`

	// The last block of the sender's best chain, and the total score of the chain (see blockScore). Older versions
	// don't send these, in which case only the heights can be compared.
	TipHash    string `json:"tip_hash"`
	ChainScore int    `json:"chain_score"`

	// The versions of the protocol that the sender speaks, and the oldest that it will talk to (see p2pProtocolVersion)
	Protocol    int `json:"protocol"`
	MinProtocol int `json:"min_protocol"`

	// The address (without the port) that the sender sees the receiver at
	ObservedHost string `json:"observed_host"`
}

// The message asking for block hashes
const p2pMsgGetBlockHashes = "getblockhashes"

type p2pMsgGetBlockHashesStruct struct {
	p2pMsgHeader
	MinBlockHeight int `json:"min_block_height"`
	MaxBlockHeight int `json:"max_block_height"`
}

// The message reporting block hashes a node has
const p2pMsgBlockHashes = "blockhashes"

type p2pMsgBlockHashesStruct struct {
	p2pMsgHeader
	Hashes map[int]string `json:"hashes"`
}

// The message asking for block data
const p2pMsgGetBlock = "getblock"

type p2pMsgGetBlockStruct struct {
	p2pMsgHeader
	Hash string `json:"hash"`
}

// The message containing one block's data
const p2pMsgBlock = "block"

type p2pMsgBlockStruct struct {
	p2pMsgHeader
	Hash          string `json:"hash"`
	HashSignature string `json:"hash_signature"`
	Size          int64  `json:"size"`
	Encoding      string `json:"encoding"`
	Data          string `json:"data"`
}

// Map of peer addresses, for easy set-like behaviour
type peerStringMap map[string]time.Time

var bootstrapPeers = peerStringMap{
	"cosmos.ivoras.net:2017":  time.Now(),
	"fielder.ivoras.net:2017": time.Now(),
}

// The temporary ID of this node, using strong RNG
var p2pEphemeralID = randInt63() & 0xffffffffffff

// Everything useful describing one p2p connection
type p2pConnection struct {
	conn              net.Conn
	address           string                    // host:port
	outbound          bool                      // we have connected to the peer (as opposed to the peer connecting to us)
	savedAddress      string                    // outbound: the address under which the peer is saved in the database
	handshaken        bool                      // the peer has sent a valid hello. Changed with the p2pPeers lock held, only by the connection's goroutine.
	protocol          atomic.Int32              // the version of the protocol that the peer speaks, from its hello
	incoming          map[string]*incomingBlock // the blocks being received in chunks. Only used by the connection's goroutine.
	peer              *bufio.ReadWriter
	peerID            int64
	isConnectable     bool // using the default port
	testedConnectable bool // using the default port
	chainHeight       int
	refreshTime       time.Time
	chanFromPeer      chan util.StrIfMap // StrIfMaps go in

	// Messages waiting to be written to the peer. Anyone (including the connection's own handlers)
	// may add to it with send() without ever blocking. The handleConnection() loop writes them out.
	outboxLock  util.WithMutex
	outbox      []any
	outboxReady chan struct{} // capacity 1: signals that the outbox is not empty
}

// Network timeouts. Without them a single stalled peer can stall a whole goroutine forever.
const (
	p2pDialTimeout  = 10 * time.Second
	p2pWriteTimeout = 30 * time.Second

	// A connection whose outbox grows beyond this many messages is not keeping up and is dropped.
	p2pMaxOutboxLength = 1 << 18

	// The most addresses accepted from a single peer's "my_peers" list
	p2pMaxPeerAddresses = 16

	// How many connections there can be: those initiated by the peers, and those initiated by us
	p2pMaxInbound  = 128
	p2pMaxOutbound = 32

	// How far back from the tip the hashes of the blocks are compared with the peers' to detect a competing branch
	reorgSearchWindow = 32
)

var p2pDialer = &net.Dialer{Timeout: p2pDialTimeout}

// HTTP client for fetching blocks from peers (the "http" block encoding).
var p2pHTTPClient = &http.Client{
	Timeout: 30 * time.Minute, // The whole exchange including the body, blocks can be large
	// Never follow redirects: they would lead away from the peer we have agreed to talk to
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext:           p2pDialer.DialContext,
		TLSHandshakeTimeout:   p2pDialTimeout,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// send queues a message for sending to the peer. It never blocks, so it is safe to call from
// anywhere, including from the connection's own message handlers and while holding locks.
// If the peer is so slow that its outbox overflows, the connection is closed.
func (p2pc *p2pConnection) send(msg any) {
	overflow := false
	p2pc.outboxLock.With(func() {
		if len(p2pc.outbox) >= p2pMaxOutboxLength {
			overflow = true
			return
		}
		p2pc.outbox = append(p2pc.outbox, msg)
	})
	if overflow {
		log.Println("Outbox of", p2pc.address, "overflowed, dropping the connection")
		if p2pc.conn != nil {
			p2pc.conn.Close()
		}
		return
	}
	select {
	case p2pc.outboxReady <- struct{}{}:
	default: // Already signalled
	}
}

// flushOutbox writes the queued messages to the peer. The lazy messages (the blocks being sent in chunks) are not
// written all at once but a piece at a time, between the pieces the connection reads what the peer has sent
// (otherwise two nodes sending big blocks to each other would block each other).
func (p2pc *p2pConnection) flushOutbox() error {
	for {
		var msgs []any
		p2pc.outboxLock.With(func() {
			msgs = p2pc.outbox
			p2pc.outbox = nil
		})
		if len(msgs) == 0 {
			return nil
		}
		var lazy []lazyMessage
		for _, msg := range msgs {
			if l, ok := msg.(lazyMessage); ok {
				lazy = append(lazy, l)
				continue
			}
			if err := p2pc.sendMsg(msg); err != nil {
				for _, l := range lazy {
					l.close()
				}
				return err
			}
		}
		if len(lazy) == 0 {
			continue
		}

		// One piece of the first of the transfers; the others wait for their turn
		msg, done, err := lazy[0].nextMessage()
		var sendErr error
		if err != nil {
			log.Println("Cannot send a block to", p2pc.address+":", err)
			done = true
		} else {
			sendErr = p2pc.sendMsg(msg)
		}
		if sendErr != nil {
			for _, l := range lazy {
				l.close()
			}
			return sendErr
		}
		rest := lazy
		if done {
			rest = lazy[1:]
		}
		if len(rest) == 0 {
			continue
		}
		p2pc.outboxLock.With(func() {
			requeued := make([]any, 0, len(rest)+len(p2pc.outbox))
			for _, l := range rest {
				requeued = append(requeued, l)
			}
			p2pc.outbox = append(requeued, p2pc.outbox...)
		})
		select {
		case p2pc.outboxReady <- struct{}{}:
		default:
		}
		return nil
	}
}

// discardOutbox gets rid of what was never sent, when the connection is over
func (p2pc *p2pConnection) discardOutbox() {
	p2pc.outboxLock.With(func() {
		for _, msg := range p2pc.outbox {
			if l, ok := msg.(lazyMessage); ok {
				l.close()
			}
		}
		p2pc.outbox = nil
	})
}

// p2pCtrlSend passes a message to the p2p coordinator without ever blocking the caller.
// The messages are only hints (try to connect to these peers, look for new blocks), so
// if the coordinator is too busy to take it, it is dropped.
func p2pCtrlSend(msg p2pCtrlMessage) {
	select {
	case p2pCtrlChannel <- msg:
	default:
		log.Println("Coordinator is busy, dropping control message", msg.msgType)
	}
}

// A set of p2p connections
type p2pPeersSet struct {
	peers map[*p2pConnection]time.Time
	lock  util.WithMutex // Warning: do not do any IO/network operations while holding this lock
}

// The global set of p2p connections. XXX: Singletons in Go?
var p2pPeers = p2pPeersSet{peers: make(map[*p2pConnection]time.Time)}

// Adds a p2p connections to the set of p2p connections
func (p *p2pPeersSet) Add(c *p2pConnection) {
	p.lock.With(func() {
		p.peers[c] = time.Now()
	})
}

// Removes a p2p connection from the set of p2p connections
func (p *p2pPeersSet) Remove(c *p2pConnection) {
	p.lock.With(func() {
		delete(p.peers, c)
	})
}

// Snapshot returns the current connections, for callers who need to do something slow with each of them.
func (p *p2pPeersSet) Snapshot(onlyHandshaken bool) []*p2pConnection {
	var peers []*p2pConnection
	p.lock.With(func() {
		peers = make([]*p2pConnection, 0, len(p.peers))
		for peer := range p.peers {
			if onlyHandshaken && !peer.handshaken {
				continue
			}
			peers = append(peers, peer)
		}
	})
	return peers
}

// Count returns the number of the connections of the given kind
func (p *p2pPeersSet) Count(outbound bool) int {
	n := 0
	p.lock.With(func() {
		for peer := range p.peers {
			if peer.outbound == outbound {
				n++
			}
		}
	})
	return n
}

// markHandshaken records that the peer has said hello. If we have connected to the peer using the default port,
// that also tells that others can connect to it.
func (p *p2pPeersSet) markHandshaken(c *p2pConnection) {
	p.lock.With(func() {
		c.handshaken = true
		if c.outbound {
			if _, port, err := util.SplitAddress(c.address); err == nil && port == DefaultP2PPort {
				c.isConnectable = true
			}
		}
	})
}

func (p *p2pPeersSet) HasAddress(address string) bool {
	found := false
	p.lock.With(func() {
		for peer := range p.peers {
			if peer.address == address {
				found = true
				break
			}
		}
	})
	return found
}

func (p *p2pPeersSet) GetAddresses(onlyConnectable bool) []string {
	var addresses []string
	p.lock.With(func() {
		for peer := range p.peers {
			if onlyConnectable && !(peer.isConnectable && peer.handshaken) {
				continue
			}
			addresses = append(addresses, peer.address)
		}
	})
	return addresses
}

func (p *p2pPeersSet) tryPeersConnectable() {
	type candidate struct {
		peer    *p2pConnection
		address string
	}
	addressesToTry := map[string]candidate{}

	p.lock.With(func() {
		for peer := range p.peers {
			if peer.testedConnectable || peer.isConnectable {
				continue
			}
			host, port, err := util.SplitAddress(peer.address)
			if err != nil {
				continue
			}
			if port == DefaultP2PPort {
				// we're already connected to it
				continue
			}

			address := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
			peer.testedConnectable = true

			addressesToTry[peer.address] = candidate{peer: peer, address: address}
		}
	})

	for paddress, c := range addressesToTry {
		conn, err := p2pDialer.Dial("tcp", c.address)
		if err != nil {
			continue
		}
		p.lock.With(func() {
			for peer := range p.peers {
				if peer.address == paddress {
					peer.isConnectable = true
				}
			}
		})

		err = conn.Close()
		if err != nil {
			log.Println(err)
		}
		if c.peer.protocol.Load() >= 2 {
			// Let the peer know that we have been able to connect to it, which is not something that it can find out
			c.peer.send(p2pMsgReachableStruct{
				P2pID: p2pEphemeralID,
				Root:  chainParams.GenesisBlockHash,
				Msg:   p2pMsgReachable,
				Port:  DefaultP2PPort,
			})
		}
	}
}

func (p *p2pPeersSet) saveConnectablePeers() {
	dbPeers := dbGetSavedPeers()
	localAddresses := util.LocalAddresses()

	// Only collect the addresses under the lock: resolving and the database are too slow to do with it held.
	var connectable []string
	p.lock.With(func() {
		for peer := range p.peers {
			if peer.isConnectable && peer.handshaken {
				connectable = append(connectable, peer.address)
			}
		}
	})

	for _, address := range connectable {
		host, _, err := util.SplitAddress(address)
		if err != nil {
			continue
		}
		canonicalAddress := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
		addr, err := net.ResolveTCPAddr("tcp", canonicalAddress)
		if err != nil {
			continue
		}
		if _, ok := dbPeers[addr.String()]; ok {
			// Already in db
			continue
		}
		if slices.Contains(localAddresses, addr.String()) {
			// Local interface
			continue
		}
		log.Println("Detected canonical peer at", canonicalAddress)
		dbSavePeer(canonicalAddress)
	}
}

func p2pServer() {
	serverAddress := ":" + strconv.Itoa(cfg.P2pPort)
	l, err := net.Listen("tcp", serverAddress)
	if err != nil {
		log.Println("Cannot listen on", serverAddress)
		log.Fatal(err)
	}
	defer func() {
		err = l.Close()
		if err != nil {
			log.Fatalf("p2pServer l.Close: %v", err)
		}
	}()
	log.Println("P2P listening on", serverAddress)
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Println("Error accepting socket:", err)
			sysEventChannel <- sysEventMessage{event: eventQuit}
			return
		}
		if host, err := remoteHost(conn.RemoteAddr()); err == nil && p2pCoordinator.badHosts.Has(host) {
			log.Println("Ignoring bad peer", conn.RemoteAddr().String())
			conn.Close()
			continue
		}
		if p2pPeers.Count(false) >= p2pMaxInbound {
			log.Println("Too many connections, refusing", conn.RemoteAddr().String())
			conn.Close()
			continue
		}
		p2pc, err := p2pSetupPeer(conn.RemoteAddr().String(), conn, false)
		if err != nil {
			log.Println("Error setting up peer", conn.RemoteAddr().String(), err)
			conn.Close()
			continue
		}
		go p2pc.handleConnection()
	}
}

func p2pClient() {
	p2pCoordinator.connectDbPeers()
}

func (p2pc *p2pConnection) sendMsg(msg any) error {
	bmsg, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	// A peer which doesn't read must not be able to block us forever
	if err = p2pc.conn.SetWriteDeadline(time.Now().Add(p2pWriteTimeout)); err != nil {
		return err
	}
	n, err := p2pc.peer.Write(bmsg)
	if err != nil {
		return err
	}
	if n != len(bmsg) {
		return fmt.Errorf("didn't write entire message: %v vs %v", n, len(bmsg))
	}
	n, err = p2pc.peer.Write([]byte("\n"))
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("didn't write newline")
	}
	//log.Println("... successfully wrote", string(bmsg))
	return p2pc.peer.Flush()
}

func (p2pc *p2pConnection) handleConnection() {
	defer func() {
		log.Println("Cleaning up connection", p2pc.address)
		p2pPeers.Remove(p2pc)
		p2pc.abortAllIncomingBlocks()
		p2pc.discardOutbox()
		err := p2pc.conn.Close()
		if err != nil {
			log.Printf("p2pc.conn.Close: %v", err)
		}
		log.Println("Finished cleaning up connection", p2pc.address)
	}()

	// Only store the IP address as the address.
	// This must be done in the goroutine because resolving can block for a long time.
	addr, err := net.ResolveTCPAddr("tcp", p2pc.address)
	if err == nil && addr.String() != p2pc.address {
		p2pc.address = addr.String()
	}

	p2pc.peer = bufio.NewReadWriter(bufio.NewReader(p2pc.conn), bufio.NewWriter(p2pc.conn))

	// XXX: the state machine shouldn't start by the listener sending something
	// (security best practices)
	helloMsg := p2pMsgHelloStruct{
		P2pID:       p2pEphemeralID,
		Root:        chainParams.GenesisBlockHash,
		Msg:         p2pMsgHello,
		Version:     p2pClientVersionString,
		ChainHeight: dbGetBlockchainHeight(),
		MyPeers:     p2pPeers.GetAddresses(true),

		Protocol:    p2pProtocolVersion,
		MinProtocol: p2pMinProtocolVersion,
	}
	if host, err := remoteHost(p2pc.conn.RemoteAddr()); err == nil {
		helloMsg.ObservedHost = host
	}
	if tip, err := dbGetTipBlock(); err == nil {
		helloMsg.ChainHeight = tip.Height
		helloMsg.TipHash = tip.Hash
		helloMsg.ChainScore = tip.TotalScore
	}
	err = p2pc.sendMsg(helloMsg)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Handling connection", p2pc.address)

	// Closed when this function returns, so the reader goroutine can never get stuck handing over a message
	quit := make(chan struct{})
	defer close(quit)

	// toLoop hands a message from the reader goroutine to the loop below. Returns false if the loop is gone.
	toLoop := func(msg util.StrIfMap) bool {
		select {
		case p2pc.chanFromPeer <- msg:
			return true
		case <-quit:
			return false
		}
	}

	go func() {
		for {
			line, err := readLine(p2pc.peer.Reader, p2pMaxMessageSize)
			if err != nil {
				log.Println("Error reading data from", p2pc.address, err)
				if errors.Is(err, errLineTooLong) {
					p2pCoordinator.banPeer(p2pc)
				}
				toLoop(util.StrIfMap{"_error": "Error reading data"})
				break
			}
			var msg util.StrIfMap
			if err = json.Unmarshal(line, &msg); err != nil {
				log.Println("Cannot parse JSON", strconv.QuoteToASCII(string(line[:min(len(line), 200)])), "from", p2pc.address)
				p2pCoordinator.banPeer(p2pc)
				toLoop(util.StrIfMap{"_error": "Cannot parse JSON"})
				break
			}

			root, err := msg.GetString("root")
			if err != nil {
				log.Printf("Problem with chain root from  %v: %v", p2pc.address, err)
				toLoop(util.StrIfMap{"_error": "Problem with chain root"})
				break
			}
			if root != chainParams.GenesisBlockHash {
				log.Printf("Received message from %v for a different chain than mine (%s vs %s). Ignoring.", p2pc.conn, root, chainParams.GenesisBlockHash)
				continue
			}
			if !toLoop(msg) {
				break
			}
		}
		log.Println("Shutting down receiver for", p2pc.address)
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// The message handlers below run in this loop and only ever queue replies with send(), which
	// cannot block. The replies are written out after each handled message.
	for {
		select {
		case msg := <-p2pc.chanFromPeer:
			if _error, err := msg.GetString("_error"); err == nil {
				log.Printf("Fatal error from %v: %v", p2pc.address, _error)
				return
			}
			cmd, err := msg.GetString("msg")
			if err != nil {
				log.Printf("Error with msg from %v: %v", p2pc.address, err)
				return
			}
			if cmd != p2pMsgHello && !p2pc.handshaken {
				log.Printf("Ignoring %q from %v, which hasn't said hello", cmd, p2pc.address)
				continue
			}
			switch cmd {
			case p2pMsgHello:
				p2pc.handleMsgHello(msg)
			case p2pMsgGetBlockHashes:
				p2pc.handleGetBlockHashes(msg)
			case p2pMsgBlockHashes:
				p2pc.handleBlockHashes(msg)
			case p2pMsgGetBlock:
				p2pc.handleGetBlock(msg)
			case p2pMsgBlock:
				p2pc.handleBlock(msg)
			case p2pMsgBlockChunk:
				p2pc.handleBlockChunk(msg)
			case p2pMsgReachable:
				p2pc.handleReachable(msg)
			}
		case <-ticker.C:
			p2pc.expireIncomingBlocks()
		case <-p2pc.outboxReady:
		}
		if err := p2pc.flushOutbox(); err != nil {
			log.Println("Error sending to peer:", err)
			return
		}
	}
}

func (p2pc *p2pConnection) handleMsgHello(msg util.StrIfMap) {
	var ver string
	var err error
	if ver, err = msg.GetString("version"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	if p2pc.chainHeight, err = msg.GetInt("chain_height"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	if p2pc.peerID == 0 {
		if p2pc.peerID, err = msg.GetInt64("p2p_id"); err != nil {
			log.Println(p2pc.conn, err)
			return
		}
	}
	protocol, minProtocol := 1, 1 // A node which doesn't say speaks the original protocol
	if v, err := msg.GetInt("protocol"); err == nil {
		protocol = v
	}
	if v, err := msg.GetInt("min_protocol"); err == nil {
		minProtocol = v
	}
	if err = p2pCheckProtocol(protocol, minProtocol); err != nil {
		log.Printf("Not talking to %v: %v", p2pc.address, err)
		if p2pc.outbound {
			p2pCoordinator.badPeers.Add(p2pc.address)
		}
		p2pc.conn.Close()
		return
	}
	p2pc.protocol.Store(int32(protocol))
	if seenAs, err := msg.GetString("observed_host"); err == nil {
		if peerHost, err := remoteHost(p2pc.conn.RemoteAddr()); err == nil {
			selfInfo.observe(seenAs, peerHost)
		}
	}
	var remotePeers []string
	if remotePeers, err = msg.GetStringList("my_peers"); err == nil {
		p2pCtrlSend(p2pCtrlMessage{msgType: p2pCtrlConnectPeers, payload: remotePeers})
	}
	log.Printf("Hello from %v %s (%x) %d blocks", p2pc.address, ver, p2pc.peerID, p2pc.chainHeight)
	// Check for duplicates
	dup := false
	p2pPeers.lock.With(func() {
		for p := range p2pPeers.peers {
			if p.peerID == p2pc.peerID && p != p2pc {
				log.Printf("%v looks like a duplicate of %v (%x), dropping it.", p2pc.address, p.address, p2pc.peerID)
				dup = true
				return
			}
		}
	})
	if p2pc.peerID == p2pEphemeralID {
		log.Printf("%v is apparently myself (%x). Dropping it.", p2pc.conn, p2pc.peerID)
		dup = true
	}
	if dup {
		if p2pc.outbound {
			// Remember the address we dialed so that we don't keep dialing it. For inbound connections
			// the address has a random source port which says nothing about the peer.
			p2pCoordinator.badPeers.Add(p2pc.address)
		}
		err = p2pc.conn.Close()
		if err != nil {
			log.Printf("p2pc.conn.Close: %v", err)
		}
		return
	}
	p2pc.refreshTime = time.Now()
	p2pPeers.markHandshaken(p2pc)
	if p2pc.outbound && p2pc.savedAddress != "" {
		// A real peer of our chain: only now it's worth remembering
		dbSavePeer(p2pc.savedAddress)
	}

	// Does the peer have something better than we do?
	tip, err := dbGetTipBlock()
	if err != nil {
		log.Println(err)
		return
	}
	better := p2pc.chainHeight > tip.Height
	if tipHash, err := msg.GetString("tip_hash"); err == nil && tipHash != "" && tipHash != tip.Hash {
		score, _ := msg.GetInt("chain_score")
		better = better || score > tip.TotalScore || (score == tip.TotalScore && tipHash < tip.Hash)
	}
	if better {
		p2pCtrlSend(p2pCtrlMessage{msgType: p2pCtrlSearchForBlocks, payload: p2pSearchRequest{p2pc, p2pc.chainHeight}})
	}
}

// reachable: the peer has connected to our p2p port, it works
func (p2pc *p2pConnection) handleReachable(msg util.StrIfMap) {
	port, err := msg.GetInt("port")
	if err != nil || port != DefaultP2PPort {
		return
	}
	if host, err := remoteHost(p2pc.conn.RemoteAddr()); err == nil {
		selfInfo.confirmReachable(host)
	}
}

// requestBlock asks the peer for the block with the given hash, unless we have just done that
func (p2pc *p2pConnection) requestBlock(hash string) {
	if p2pCoordinator.recentlyRequestedBlocks.TestAndSet(hash) {
		return
	}
	log.Println("Requesting block", hash)
	p2pc.send(p2pMsgGetBlockStruct{
		P2pID: p2pEphemeralID,
		Root:  chainParams.GenesisBlockHash,
		Msg:   p2pMsgGetBlock,
		Hash:  hash,
	})
}

// Handle getblockhashes
func (p2pc *p2pConnection) handleGetBlockHashes(msg util.StrIfMap) {
	var minBlockHeight int
	var maxBlockHeight int
	var err error
	if minBlockHeight, err = msg.GetInt("min_block_height"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	if maxBlockHeight, err = msg.GetInt("max_block_height"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	log.Printf("*** Sending block hashes from %d to %d to %s", minBlockHeight, maxBlockHeight, p2pc.address)
	respMsg := p2pMsgBlockHashesStruct{
		P2pID:  p2pEphemeralID,
		Root:   chainParams.GenesisBlockHash,
		Msg:    p2pMsgBlockHashes,
		Hashes: dbGetHeightHashes(minBlockHeight, maxBlockHeight),
	}
	p2pc.send(respMsg)
}

// Handle receiving blockhashes
func (p2pc *p2pConnection) handleBlockHashes(msg util.StrIfMap) {
	var hashes map[int]string
	var err error
	if hashes, err = msg.GetIntStringMap("hashes"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	heights := make([]int, len(hashes))
	n := 0
	for h := range hashes {
		heights[n] = h
		n++
	}
	slices.Sort(heights)
	log.Println("handleBlockHashes: got", len(heights), "hashes")
	myHeight := dbGetBlockchainHeight()
	for _, h := range heights {
		if h < myHeight-maxReorgDepth {
			// Such a branch would not be accepted anyway
			continue
		}
		if dbBlockHashKnown(hashes[h]) {
			// On the best chain, or on another branch
			continue
		}
		p2pc.requestBlock(hashes[h])
	}
}

// getblock: a request to transfer a block
func (p2pc *p2pConnection) handleGetBlock(msg util.StrIfMap) {
	hash, err := msg.GetString("hash")
	if err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	dbb, onBestChain, err := dbGetAnyBlock(hash)
	if err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	fileName := sideBlockFilename(dbb.Hash)
	if onBestChain {
		fileName = blockchainGetFilename(dbb.Height)
	}
	st, err := os.Stat(fileName)
	if err != nil {
		log.Println(err)
		return
	}
	fileSize := st.Size()

	if p2pc.protocol.Load() >= 2 {
		// The block goes over the connection which the peer has opened, no other connection is needed
		sender, err := newBlockSender(hash, dbb.HashSignature, fileName)
		if err != nil {
			log.Println(err)
			return
		}
		p2pc.send(sender)
		log.Println("*** Sending block", hash, "to", p2pc.address)
		return
	}

	var msgBlockEncoding, msgBlockData string

	if cfg.p2pBlockInline && fileSize <= p2pMaxInlineBlockSize {
		f, err := os.Open(fileName)
		if err != nil {
			log.Println(err)
			return
		}
		defer func() {
			err = f.Close()
			if err != nil {
				log.Printf("handleGetBlock f.Close: %v", err)
			}
		}()
		var zbuf bytes.Buffer
		w := zlib.NewWriter(&zbuf)
		written, err := io.Copy(w, f)
		if err != nil {
			log.Println(err)
			return
		}
		if written != fileSize {
			log.Println("Something broke when working with zlib:", written, "vs", fileSize)
			return
		}
		err = w.Close()
		if err != nil {
			log.Panic(err)
		}
		msgBlockEncoding = "zlib-base64"
		msgBlockData = base64.StdEncoding.EncodeToString(zbuf.Bytes())
	} else {
		msgBlockEncoding = "http"
		// The receiver only uses the port and the block number from this URL, and connects to our address as it knows it.
		host := "localhost"
		if addresses := util.LocalAddresses(); len(addresses) > 0 {
			host = addresses[0]
		}
		msgBlockData = fmt.Sprintf("http://%s/block/%s", net.JoinHostPort(host, strconv.Itoa(cfg.httpPort)), dbb.Hash)
		log.Println("*** Instructing the peer to get a block from", msgBlockData)
	}

	respMsg := p2pMsgBlockStruct{
		P2pID:         p2pEphemeralID,
		Root:          chainParams.GenesisBlockHash,
		Msg:           p2pMsgBlock,
		Hash:          hash,
		HashSignature: hex.EncodeToString(dbb.HashSignature),
		Encoding:      msgBlockEncoding,
		Data:          msgBlockData,
		Size:          fileSize,
	}
	p2pc.send(respMsg)
	log.Println("*** Sent block", hash, "to", p2pc.address)
}

// block: A block is received
func (p2pc *p2pConnection) handleBlock(msg util.StrIfMap) {
	hash, err := msg.GetString("hash")
	if err != nil {
		log.Println(err)
		return
	}
	hashSignature, err := msg.GetString("hash_signature")
	if err != nil {
		log.Println(err)
		return
	}
	dataString, err := msg.GetString("data")
	if err != nil {
		log.Println(err)
		return
	}
	if dbBlockHashKnown(hash) {
		return
	}
	fileSize, err := msg.GetInt64("size")
	if err != nil {
		log.Println(err)
		return
	}
	if err = checkBlockSize(fileSize); err != nil {
		log.Println("Rejecting block", hash, "from", p2pc.address+":", err)
		return
	}
	encoding, err := msg.GetString("encoding")
	if err != nil {
		log.Printf("encoding: %v", err)
		return
	}
	if encoding == p2pEncodingChunked {
		// The pieces follow
		p2pc.startIncomingBlock(hash, hashSignature, fileSize)
		return
	}
	if encoding != "zlib-base64" && encoding != "http" {
		log.Println("Unknown block encoding:", encoding)
		return
	}
	var blockURL string
	if encoding == "http" {
		if blockURL, err = blockHTTPURL(dataString, p2pc.conn.RemoteAddr()); err != nil {
			log.Println("Rejecting block", hash, "from", p2pc.address+":", err)
			return
		}
	}

	blockFile, err := os.CreateTemp("", "daisy")
	if err != nil {
		log.Println("Error creating temp file", err)
		return
	}
	defer func() {
		if err := blockFile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			log.Printf("handleBlock blockFile.Close: %v", err)
		}
		if err := os.Remove(blockFile.Name()); err != nil {
			log.Printf("remove: %v", err)
		}
	}()
	if encoding == "http" {
		log.Println("Getting block", hash, "from", blockURL)
		err = downloadHTTPBlock(blockFile, blockURL, fileSize)
	} else {
		err = downloadInlineBlock(blockFile, dataString, fileSize)
	}
	if err != nil {
		log.Println("Error receiving block", hash, "from", p2pc.address+":", err)
		return
	}
	if err = blockFile.Close(); err != nil {
		log.Printf("handleBlock blockFile.Close: %v", err)
		return
	}

	p2pc.importBlock(blockFile.Name(), hash, hashSignature)
}

// Connect to a peer. Does everything except starting the handler goroutine.
// Checks if there already is a connection of this type.
func p2pConnectPeer(address string) (*p2pConnection, error) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}

	if p2pPeers.HasAddress(addr.String()) {
		return nil, fmt.Errorf("Connection to %s already exists", addr.String())
	}
	if p2pCoordinator.isBanned(addr) {
		return nil, fmt.Errorf("%s is a bad peer", addr.String())
	}
	if p2pPeers.Count(true) >= p2pMaxOutbound {
		return nil, errors.New("Too many outbound connections")
	}

	localAddresses := util.LocalAddresses()
	if slices.Contains(localAddresses, addr.IP.String()) {
		return nil, fmt.Errorf("Refusing to connect to myself at %s", addr.IP)
	}

	conn, err := p2pDialer.Dial("tcp", address)
	if err != nil {
		log.Println("Error connecting to", address, err)
		dbPeerFailed(address)
		return nil, err
	}
	p2pc, err := p2pSetupPeer(addr.String(), conn, true)
	if p2pc != nil {
		p2pc.savedAddress = address
	}
	return p2pc, err
}

// Creates the p2pConnection structure for the peer and adds it to the peer list.
// Does not start the handler goroutine.
func p2pSetupPeer(address string, conn net.Conn, outbound bool) (*p2pConnection, error) {
	p2pc := p2pConnection{
		conn:         conn,
		address:      address,
		outbound:     outbound,
		chanFromPeer: make(chan util.StrIfMap, 5),
		outboxReady:  make(chan struct{}, 1),
	}
	p2pPeers.Add(&p2pc)
	return &p2pc, nil
}
