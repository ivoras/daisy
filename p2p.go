package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const p2pClientVersionString = "godaisy/0.1"

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

// The temporary ID of this node, strong RNG
var p2pEphemeralID = randInt63() & 0xffffffffffff

// Everything useful describing one p2p connection
type p2pConnection struct {
	conn         net.Conn
	address      string // host:port
	peer         *bufio.ReadWriter
	peerID       int64
	chainHeight  int
	refreshTime  time.Time
	chanToPeer   chan interface{} // structs go out
	chanFromPeer chan StrIfMap    // StrIfMaps go in
}

// A set of p2p connections
type p2pPeersSet struct {
	peers map[*p2pConnection]time.Time
	lock  WithMutex
}

// The global set of p2p connections
var p2pPeers = p2pPeersSet{peers: make(map[*p2pConnection]time.Time)}

// Messages to the p2p controller goroutine
const (
	p2pCtrlSearchForBlocks = iota
	p2pCtrlHaveNewBlock
	p2pCtrlDiscoverPeers
)

type p2pCtrlMessage struct {
	msgType int
	payload interface{}
}

var p2pCtrlChannel = make(chan p2pCtrlMessage, 8)

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

func (p *p2pPeersSet) GetAddresses(onlyCanonical bool) []string {
	var addresses []string
	p.lock.With(func() {
		for peer := range p.peers {
			if onlyCanonical {
				if !strings.HasSuffix(peer.address, fmt.Sprintf(":%d", DefaultP2PPort)) {
					continue
				}
			}
			addresses = append(addresses, peer.address)
		}
	})
	return addresses
}

func p2pServer() {
	serverAddress := ":" + strconv.Itoa(cfg.P2pPort)
	l, err := net.Listen("tcp", serverAddress)
	if err != nil {
		log.Println("Cannot listen on", serverAddress)
		log.Fatal(err)
	}
	defer l.Close()
	log.Println("Listening on", serverAddress)
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Println("Error accepting socket:", err)
			sysEventChannel <- sysEventMessage{event: eventQuit}
			return
		}
		if p2pCoordinator.badPeers.Has(conn.RemoteAddr().String()) {
			log.Println("Ignoring bad peer", conn.RemoteAddr().String())
			continue
		}
		p2pc := p2pConnection{conn: conn, address: conn.RemoteAddr().String(), chanToPeer: make(chan interface{}, 5), chanFromPeer: make(chan StrIfMap, 5)}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
	}
}

func p2pClient() {
	p2pCoordinator.connectDbPeers()
}

func (p2pc *p2pConnection) sendMsg(msg interface{}) error {
	bmsg, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	n, err := p2pc.peer.Write(bmsg)
	if err != nil {
		return err
	}
	if n != len(bmsg) {
		return fmt.Errorf("Didn't write entire message: %v vs %v", n, len(bmsg))
	}
	n, err = p2pc.peer.Write([]byte("\n"))
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("Didn't write newline")
	}
	err = p2pc.peer.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (p2pc *p2pConnection) handleConnection() {
	defer p2pc.conn.Close()
	defer p2pPeers.Remove(p2pc)

	p2pc.peer = bufio.NewReadWriter(bufio.NewReader(p2pc.conn), bufio.NewWriter(p2pc.conn))
	helloMsg := p2pMsgHelloStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgHello,
		},
		Version:     p2pClientVersionString,
		ChainHeight: dbGetBlockchainHeight(),
		MyPeers:     p2pPeers.GetAddresses(false),
	}
	err := p2pc.sendMsg(helloMsg)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Handling connection", p2pc.conn)

	go func() {
		for {
			line, err := p2pc.peer.ReadBytes('\n')
			if err != nil {
				log.Println("Error reading data from", p2pc.conn, err)
				p2pc.chanFromPeer <- StrIfMap{"_error": "Error reading data"}
				break
			}
			var msg StrIfMap
			err = json.Unmarshal(line, &msg)
			if err != nil {
				log.Println("Cannot parse JSON", string(line), "from", p2pc.conn)
				p2pc.chanFromPeer <- StrIfMap{"_error": "Cannot parse JSON"}
				break
			}

			var root string
			if root, err = msg.GetString("root"); err != nil {
				log.Printf("Problem with chain root from  %v: %v", p2pc.conn, err)
				p2pc.chanFromPeer <- StrIfMap{"_error": "Problem with chain root"}
				break
			}
			if root != GenesisBlockHash {
				log.Printf("Received message from %v for a different chain than mine (%s vs %s). Ignoring.", p2pc.conn, root, GenesisBlockHash)
				continue
			}
			p2pc.chanFromPeer <- msg
		}
	}()

	for {
		select {
		case msg := <-p2pc.chanFromPeer:
			var _error string
			if _error, err = msg.GetString("_error"); err == nil {
				log.Printf("Fatal error from %v: %v", p2pc.conn, _error)
				break
			}
			var cmd string
			if cmd, err = msg.GetString("msg"); err != nil {
				log.Printf("Error with msg from %v: %v", p2pc.conn, err)
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
			}
		case msg := <-p2pc.chanToPeer:
			err := p2pc.sendMsg(msg)
			if err != nil {
				log.Println(err)
				break
			}
		}

	}
	// The connection has been dismissed
}

func (p2pc *p2pConnection) handleMsgHello(msg StrIfMap) {
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
	if remotePeers, err := msg.GetStringList("my_peers"); err == nil {
		p2pCtrlChannel <- p2pCtrlMessage{msgType: p2pCtrlDiscoverPeers, payload: remotePeers}
	}
	log.Printf("Hello from %v %s (%x) %d blocks", p2pc.conn, ver, p2pc.peerID, p2pc.chainHeight)
	// Check for duplicates
	dup := false
	p2pPeers.lock.With(func() {
		for p := range p2pPeers.peers {
			if p.peerID == p2pc.peerID && p != p2pc {
				log.Printf("%v looks like a duplicate of %v (%x), dropping it.", p2pc.conn, p.conn, p2pc.peerID)
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
		p2pCoordinator.badPeers.Add(p2pc.address)
		p2pc.conn.Close()
		return
	}
	p2pc.checkSavePeer()
	p2pc.refreshTime = time.Now()
	if p2pc.chainHeight > dbGetBlockchainHeight() {
		p2pCtrlChannel <- p2pCtrlMessage{msgType: p2pCtrlSearchForBlocks, payload: p2pc}
	}
}

// Handle getblockhashes
func (p2pc *p2pConnection) handleGetBlockHashes(msg StrIfMap) {
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
	respMsg := p2pMsgBlockHashesStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgBlockHashes,
		},
		Hashes: dbGetHeightHashes(minBlockHeight, maxBlockHeight),
	}
	p2pc.chanToPeer <- respMsg
}

// Handle receiving blockhashes
func (p2pc *p2pConnection) handleBlockHashes(msg StrIfMap) {
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
	sort.Ints(heights)
	for h := range heights {
		if dbBlockHeightExists(h) {
			//log.Println("Already have block", h)
			continue
		}
		if p2pCoordinator.recentlyRequestedBlocks.TestAndSet(hashes[h]) {
			continue
		}
		log.Println("Requesting block", hashes[h])
		msg := p2pMsgGetBlockStruct{
			p2pMsgHeader: p2pMsgHeader{
				P2pID: p2pEphemeralID,
				Root:  GenesisBlockHash,
				Msg:   p2pMsgGetBlock,
			},
			Hash: hashes[h],
		}
		p2pc.chanToPeer <- msg
	}
}

// getblock: a request to transfer a block
func (p2pc *p2pConnection) handleGetBlock(msg StrIfMap) {
	hash, err := msg.GetString("hash")
	if err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	dbb, err := dbGetBlock(hash)
	if err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	fileName := blockchainGetFilename(dbb.Height)
	st, err := os.Stat(fileName)
	if err != nil {
		log.Println(err)
		return
	}
	fileSize := st.Size()
	f, err := os.Open(fileName)
	if err != nil {
		log.Println(err)
		return
	}
	defer f.Close()
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
	b64block := base64.StdEncoding.EncodeToString(zbuf.Bytes())
	respMsg := p2pMsgBlockStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgBlock,
		},
		Hash:          hash,
		HashSignature: hex.EncodeToString(dbb.HashSignature),
		Encoding:      "zlib-base64",
		Data:          b64block,
		Size:          fileSize,
	}
	p2pc.chanToPeer <- respMsg
}

// block: A block is received
func (p2pc *p2pConnection) handleBlock(msg StrIfMap) {
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
	if dbBlockHashExists(hash) {
		log.Println("Replacing blocks not yet implemented")
		return
	}
	fileSize, err := msg.GetInt64("size")
	if err != nil {
		log.Println(err)
	}
	var blockFile *os.File
	encoding, err := msg.GetString("encoding")
	if encoding == "zlib-base64" {
		zlibData, err := base64.StdEncoding.DecodeString(dataString)
		if err != nil {
			log.Println(err)
			return
		}
		blockFile, err = ioutil.TempFile("", "daisy")
		if err != nil {
			log.Println(err)
			return
		}
		defer os.Remove(blockFile.Name())
		r, err := zlib.NewReader(bytes.NewReader(zlibData))
		if err != nil {
			log.Println(err)
			return
		}
		written, err := io.Copy(blockFile, r)
		r.Close()
		blockFile.Close()
		if written != fileSize {
			log.Println("Error decoding block: sizes don't match:", written, "vs", fileSize)
			return
		}
	} else {
		log.Println("Unsupported encoding:", encoding)
		return
	}
	blk, err := OpenBlockFile(blockFile.Name())
	if err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	blk.HashSignature, err = hex.DecodeString(hashSignature)
	if err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	height, err := checkAcceptBlock(blk)
	if err != nil {
		log.Println("Cannot import block:", err)
		return
	}
	blk.Height = height
	err = blockchainCopyFile(blockFile.Name(), height)
	if err != nil {
		log.Println("Cannot copy block file:", err)
		return
	}
	err = dbInsertBlock(blk.DbBlockchainBlock)
	if err != nil {
		log.Println("Cannot insert block:", err)
		return
	}
	log.Println("Accepted block", blk.Hash, "at height", blk.Height)
}

func (p2pc *p2pConnection) checkSavePeer() {
	i := strings.LastIndex(p2pc.address, ":")
	var host string
	if i > -1 {
		host = p2pc.address[0:i]
	} else {
		host = p2pc.address
	}
	canonicalAddress := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
	addr, err := net.ResolveTCPAddr("tcp", canonicalAddress)
	if err != nil {
		return
	}
	// Detect if there's a canonical peer on the other side, somewhat brute-forceish
	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return
	}
	log.Println("Detected canonical peer at", canonicalAddress)
	conn.Close()
	dbSavePeer(canonicalAddress)
}

// Data related to the (single instance of) the global p2p coordinator. This is also a
// single-threaded object, its fields and methods are only expected to be accessed from
// the Run() goroutine.
type p2pCoordinatorType struct {
	timeTicks                chan int
	lastTickBlockchainHeight int
	recentlyRequestedBlocks  *StringSetWithExpiry
	lastReconnectTime        time.Time
	badPeers                 *StringSetWithExpiry
}

var p2pCoordinator = p2pCoordinatorType{
	recentlyRequestedBlocks: NewStringSetWithExpiry(5 * time.Second),
	lastReconnectTime:       time.Now(),
	timeTicks:               make(chan int),
	badPeers:                NewStringSetWithExpiry(15 * time.Minute),
}

func (co *p2pCoordinatorType) Run() {
	co.lastTickBlockchainHeight = dbGetBlockchainHeight()
	go co.timeTickSource()
	for {
		select {
		case msg := <-p2pCtrlChannel:
			switch msg.msgType {
			case p2pCtrlSearchForBlocks:
				co.handleSearchForBlocks(msg.payload.(*p2pConnection))
			case p2pCtrlDiscoverPeers:
				co.handleDiscoverPeers(msg.payload.([]string))
			}
		case <-co.timeTicks:
			co.handleTimeTick()
		}
	}
}

func (co *p2pCoordinatorType) timeTickSource() {
	for {
		time.Sleep(1 * time.Second)
		co.timeTicks <- 1
	}
}

// Retrieves block hashes from a node which apparently has more blocks than we do.
// ToDo: This is a simplistic version. Make it better by introducing quorums.
func (co *p2pCoordinatorType) handleSearchForBlocks(p2pcStart *p2pConnection) {
	msg := p2pMsgGetBlockHashesStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgGetBlockHashes,
		},
		MinBlockHeight: dbGetBlockchainHeight(),
		MaxBlockHeight: p2pcStart.chainHeight,
	}
	p2pcStart.chanToPeer <- msg
}

func (co *p2pCoordinatorType) handleDiscoverPeers(addresses []string) {
	for _, address := range addresses {
		i := strings.LastIndex(address, ":")
		var host string
		if i > -1 {
			host = address[0:i]
		} else {
			host = address
		}
		canonicalAddress := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
		if p2pPeers.HasAddress(canonicalAddress) || co.badPeers.Has(canonicalAddress) {
			continue
		}
		addr, err := net.ResolveTCPAddr("tcp", canonicalAddress)
		if err != nil {
			return
		}
		// Detect if there's a canonical peer on the other side, somewhat brute-forceish
		conn, err := net.DialTCP("tcp", nil, addr)
		if err != nil {
			return
		}
		p2pc := p2pConnection{conn: conn, address: canonicalAddress}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
		log.Println("Detected canonical peer at", canonicalAddress)
		dbSavePeer(canonicalAddress)
	}
}

// Executed periodically to perform time-dependant actions. Do not rely on the
// time period to be predictable or precise.
func (co *p2pCoordinatorType) handleTimeTick() {
	newHeight := dbGetBlockchainHeight()
	if newHeight > co.lastTickBlockchainHeight {
		co.floodPeersWithNewBlocks(co.lastTickBlockchainHeight, newHeight)
		co.lastTickBlockchainHeight = newHeight
	}
	if time.Since(co.lastReconnectTime) >= 10*time.Minute {
		co.lastReconnectTime = time.Now()
		co.connectDbPeers()
	}
}

func (co *p2pCoordinatorType) floodPeersWithNewBlocks(minHeight, maxHeight int) {
	blockHashes := dbGetHeightHashes(minHeight, maxHeight)
	msg := p2pMsgBlockHashesStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgBlockHashes,
		},
		Hashes: blockHashes,
	}
	p2pPeers.lock.With(func() {
		for p2pc := range p2pPeers.peers {
			p2pc.chanToPeer <- msg
		}
	})
}

func (co *p2pCoordinatorType) connectDbPeers() {
	peers := dbGetSavedPeers()
	for peer := range peers {
		if p2pPeers.HasAddress(peer) {
			continue
		}
		if co.badPeers.Has(peer) {
			continue
		}
		conn, err := net.Dial("tcp", peer)
		if err != nil {
			log.Println("Error connecting to", peer, err)
			continue
		}
		p2pc := p2pConnection{conn: conn, address: peer}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
	}
}
