package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"time"
)

const p2pClientVersionString = "godaisy/1.0"

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
	Version     string `json:"version"`
	ChainHeight int    `json:"chain_height"`
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
	Hash     string `json:"hash"`
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
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
	conn        net.Conn
	address     string // host:port
	peer        *bufio.ReadWriter
	peerID      int64
	chainHeight int
	refreshTime time.Time
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
		p2pc := p2pConnection{conn: conn, address: conn.RemoteAddr().String()}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
	}
}

func p2pClient() {
	peers := dbGetSavedPeers()
	for peer := range peers {
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
	}
	err := p2pc.sendMsg(helloMsg)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Handling connection", p2pc.conn)
	for {
		line, err := p2pc.peer.ReadBytes('\n')
		if err != nil {
			log.Println("Error reading data from", p2pc.conn, err)
			break
		}
		var msg map[string]interface{}
		err = json.Unmarshal(line, &msg)
		if err != nil {
			log.Println("Cannot parse json", string(line), "from", p2pc.conn)
			break
		}

		var root string
		if root, err = siMapGetString(msg, "root"); err != nil {
			log.Printf("Problem with chain root from  %v: %v", p2pc.conn, err)
			break
		}
		if root != GenesisBlockHash {
			log.Printf("Received message from %v for a different chain than mine (%s vs %s). Ignoring.", p2pc.conn, root, GenesisBlockHash)
			continue
		}

		var cmd string
		if cmd, err = siMapGetString(msg, "msg"); err != nil {
			log.Printf("Error with msg from %v: %v", p2pc.conn, err)
		}
		switch cmd {
		case p2pMsgHello:
			p2pc.handleMsgHello(msg)
		case p2pMsgGetBlockHashes:
			p2pc.handleGetBlockHashes(msg)
		case p2pMsgGetBlock:
			p2pc.handleGetBlock(msg)
		}
	}
	// The connection has been dismissed
}

func (p2pc *p2pConnection) handleMsgHello(rawMsg map[string]interface{}) {
	var ver string
	var err error
	if ver, err = siMapGetString(rawMsg, "version"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	if p2pc.chainHeight, err = siMapGetInt(rawMsg, "chain_height"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	if p2pc.peerID == 0 {
		if p2pc.peerID, err = siMapGetInt64(rawMsg, "p2p_id"); err != nil {
			log.Println(p2pc.conn, err)
			return
		}
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
		log.Printf("%v is apperently myself (%x). Dropping it.", p2pc.conn, p2pc.peerID)
		dup = true
	}
	if dup {
		p2pc.conn.Close()
		return
	}
	dbSavePeer(p2pc.address)
	p2pc.refreshTime = time.Now()
	if p2pc.chainHeight > dbGetBlockchainHeight() {
		p2pCtrlChannel <- p2pCtrlMessage{msgType: p2pCtrlSearchForBlocks, payload: p2pc}
	}
}

func (p2pc *p2pConnection) handleGetBlockHashes(msg map[string]interface{}) {
	var minBlockHeight int
	var maxBlockHeight int
	var err error
	if minBlockHeight, err = siMapGetInt(msg, "min_block_height"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	if maxBlockHeight, err = siMapGetInt(msg, "max_block_height"); err != nil {
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
	p2pc.sendMsg(respMsg)
}

func (p2pc *p2pConnection) handleGetBlock(msg map[string]interface{}) {
	hash, err := siMapGetString(msg, "hash")
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
	f, err := os.Open(fileName)
	if err != nil {
		log.Println(err)
		return
	}
	var zbuf bytes.Buffer
	w := zlib.NewWriter(&zbuf)
	_, err = io.Copy(w, f)
	if err != nil {
		log.Println(err)
		return
	}
	w.Close()
	b64block := base64.StdEncoding.EncodeToString(zbuf.Bytes())
	respMsg := p2pMsgBlockStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  GenesisBlockHash,
			Msg:   p2pMsgBlock,
		},
		Hash: hash,
		Data: b64block,
	}
	p2pc.sendMsg(respMsg)
}

// Data related to the (single instance of) the global p2p coordinator. This is also a
// single-threaded object, its fields and methods are only expected to be accessed from
// the Run() goroutine.
type p2pCoordinatorType struct {
	timeTicks                chan int
	lastTickBlockchainHeight int
}

var p2pCoordinator = p2pCoordinatorType{}

func (co *p2pCoordinatorType) Run() {
	co.lastTickBlockchainHeight = dbGetBlockchainHeight()
	for {
		select {
		case msg := <-p2pCtrlChannel:
			switch msg.msgType {
			case p2pCtrlSearchForBlocks:
				co.handleSearchForBlocks(msg.payload.(*p2pConnection))
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
	p2pcStart.sendMsg(msg)
}

// Executed periodically to perform time-dependant actions. Do not rely on the
// time period to be predictable or precise.
func (co *p2pCoordinatorType) handleTimeTick() {
	newHeight := dbGetBlockchainHeight()
	if newHeight > co.lastTickBlockchainHeight {
		co.floodPeersWithNewBlocks(co.lastTickBlockchainHeight, newHeight)
		co.lastTickBlockchainHeight = newHeight
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
			p2pc.sendMsg(msg)
		}
	})
}
