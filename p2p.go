package main

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
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

const p2pClientVersionString = "godaisy/0.2"

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

// The temporary ID of this node, using strong RNG
var p2pEphemeralID = randInt63() & 0xffffffffffff

// Everything useful describing one p2p connection
type p2pConnection struct {
	conn              net.Conn
	address           string // host:port
	peer              *bufio.ReadWriter
	peerID            int64
	isConnectable     bool // using the default port
	testedConnectable bool // using the default port
	chainHeight       int
	refreshTime       time.Time
	chanToPeer        chan interface{} // structs go out
	chanFromPeer      chan StrIfMap    // StrIfMaps go in
}

// A set of p2p connections
type p2pPeersSet struct {
	peers map[*p2pConnection]time.Time
	lock  WithMutex // Warning: do not do any IO/network operations while holding this lock
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
			if onlyConnectable && !peer.isConnectable {
				continue
			}
			addresses = append(addresses, peer.address)
		}
	})
	return addresses
}

func (p *p2pPeersSet) tryPeersConnectable() {
	addressesToTry := map[string]string{}

	p.lock.With(func() {
		for peer := range p.peers {
			if peer.testedConnectable || peer.isConnectable {
				continue
			}
			host, port, err := splitAddress(peer.address)
			if err != nil {
				continue
			}
			if port == DefaultP2PPort {
				// we're already connected to it
				continue
			}

			address := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
			peer.testedConnectable = true

			addressesToTry[peer.address] = address
		}
	})

	for paddress, address := range addressesToTry {
		conn, err := net.Dial("tcp", address)
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
	}
}

func (p *p2pPeersSet) saveConnectablePeers() {
	dbPeers := dbGetSavedPeers()
	localAddresses := getLocalAddresses()

	p.lock.With(func() {
		for peer := range p.peers {
			if !peer.isConnectable {
				continue
			}
			host, _, err := splitAddress(peer.address)
			if err != nil {
				continue
			}
			canonicalAddress := fmt.Sprintf("%s:%d", host, DefaultP2PPort)
			addr, err := net.ResolveTCPAddr("tcp", canonicalAddress)
			if err != nil {
				continue
			}
			if _, ok := dbPeers[addr.IP.String()]; ok {
				// Already in db
				continue
			}
			if inStrings(addr.String(), localAddresses) {
				// Local interface
				continue
			}
			log.Println("Detected canonical peer at", canonicalAddress)
			dbSavePeer(canonicalAddress)
		}
	})

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
		if p2pCoordinator.badPeers.Has(conn.RemoteAddr().String()) {
			log.Println("Ignoring bad peer", conn.RemoteAddr().String())
			continue
		}
		p2pc, err := p2pSetupPeer(conn.RemoteAddr().String(), conn)
		if err != nil {
			log.Println("Error setting up peer", conn.RemoteAddr().String(), err)
			continue
		}
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
		err := p2pc.conn.Close()
		if err != nil {
			log.Printf("p2pc.conn.Close: %v", err)
		}
		log.Println("Finished cleaning up connection", p2pc.address)
	}()

	// Only store the IP address as the address.
	// This must be done in the goroutine because resolving can block for a long time.
	addr, err := net.ResolveTCPAddr("tcp", p2pc.address)
	if err == nil {
		p2pc.address = addr.String()
	}

	p2pc.peer = bufio.NewReadWriter(bufio.NewReader(p2pc.conn), bufio.NewWriter(p2pc.conn))

	// XXX: the state machine shouldn't start by the listener sending something
	// (security best practices)
	helloMsg := p2pMsgHelloStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  chainParams.GenesisBlockHash,
			Msg:   p2pMsgHello,
		},
		Version:     p2pClientVersionString,
		ChainHeight: dbGetBlockchainHeight(),
		MyPeers:     p2pPeers.GetAddresses(true),
	}
	err = p2pc.sendMsg(helloMsg)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Handling connection", p2pc.address)
	exit := false

	go func() {
		var line []byte
		for {
			line, err = p2pc.peer.ReadBytes('\n')
			if err != nil {
				log.Println("Error reading data from", p2pc.address, err)
				p2pc.chanFromPeer <- StrIfMap{"_error": "Error reading data"}
				break
			}
			var msg StrIfMap
			err = json.Unmarshal(line, &msg)
			if err != nil {
				log.Println("Cannot parse JSON", strconv.QuoteToASCII(string(line)), "from", p2pc.address)
				p2pc.chanFromPeer <- StrIfMap{"_error": "Cannot parse JSON"}
				break
			}

			var root string
			if root, err = msg.GetString("root"); err != nil {
				log.Printf("Problem with chain root from  %v: %v", p2pc.address, err)
				p2pc.chanFromPeer <- StrIfMap{"_error": "Problem with chain root"}
				break
			}
			if root != chainParams.GenesisBlockHash {
				log.Printf("Received message from %v for a different chain than mine (%s vs %s). Ignoring.", p2pc.conn, root, chainParams.GenesisBlockHash)
				continue
			}
			p2pc.chanFromPeer <- msg
		}
		log.Println("Shutting down receiver for", p2pc.address)
		exit = true // In any case, if this goroutine exits, we want to shut down everything
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for !exit {
		select {
		case msg := <-p2pc.chanFromPeer:
			// log.Printf("... chainFromPeer: %s: %s", p2pc.address, jsonifyWhatever(msg))
			var _error string
			if _error, err = msg.GetString("_error"); err == nil {
				log.Printf("Fatal error from %v: %v", p2pc.address, _error)
				exit = true
				break
			}
			var cmd string
			if cmd, err = msg.GetString("msg"); err != nil {
				log.Printf("Error with msg from %v: %v", p2pc.address, err)
				exit = true
				break
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
				log.Println("Error sending to peer:", err)
				exit = true
			}
		case <-ticker.C:
			// so the exit variable gets tested
			continue
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
	var remotePeers []string
	if remotePeers, err = msg.GetStringList("my_peers"); err == nil {
		p2pCtrlChannel <- p2pCtrlMessage{msgType: p2pCtrlConnectPeers, payload: remotePeers}
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
		p2pCoordinator.badPeers.Add(p2pc.address)
		err = p2pc.conn.Close()
		if err != nil {
			log.Printf("p2pc.conn.Close: %v", err)
		}
		return
	}
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
	log.Printf("*** Sending block hashes from %d to %d to %s", minBlockHeight, maxBlockHeight, p2pc.address)
	respMsg := p2pMsgBlockHashesStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  chainParams.GenesisBlockHash,
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
	log.Println("handleBlockHashes: got", jsonifyWhatever(heights))
	for _, h := range heights {
		if dbBlockHeightExists(h) {
			log.Println("handleBlockHashes: already have block:", h)
			if dbGetBlockHashByHeight(h) != hashes[h] {
				log.Println("ERROR: Blockchain desynced: received block hash at height", h, "to be", hashes[h], "instead of", dbGetBlockHashByHeight(h))
				return
			}
			continue
		}
		if p2pCoordinator.recentlyRequestedBlocks.TestAndSet(hashes[h]) {
			continue
		}
		log.Println("Requesting block", hashes[h])
		msg := p2pMsgGetBlockStruct{
			p2pMsgHeader: p2pMsgHeader{
				P2pID: p2pEphemeralID,
				Root:  chainParams.GenesisBlockHash,
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

	var msgBlockEncoding, msgBlockData string

	if cfg.p2pBlockInline {
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
		msgBlockData = fmt.Sprintf("http://%s:%d/block/%d", getLocalAddresses()[0], cfg.httpPort, dbb.Height)
		log.Println("*** Instructing the peer to get a block from", msgBlockData)
	}

	respMsg := p2pMsgBlockStruct{
		p2pMsgHeader: p2pMsgHeader{
			P2pID: p2pEphemeralID,
			Root:  chainParams.GenesisBlockHash,
			Msg:   p2pMsgBlock,
		},
		Hash:          hash,
		HashSignature: hex.EncodeToString(dbb.HashSignature),
		Encoding:      msgBlockEncoding,
		Data:          msgBlockData,
		Size:          fileSize,
	}
	p2pc.chanToPeer <- respMsg
	log.Println("*** Sent block", hash, "to", p2pc.address)
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
	if err != nil {
		log.Printf("encoding: %v", err)
		return
	}
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
		defer func() {
			err = blockFile.Close()
			if err != nil {
				log.Printf("handleBlock blockFile.Close: %v", err)
			}
			err = os.Remove(blockFile.Name())
			if err != nil {
				log.Printf("remove: %v", err)
			}
		}()
		r, err := zlib.NewReader(bytes.NewReader(zlibData))
		if err != nil {
			log.Println(err)
			return
		}
		defer func() {
			err = r.Close()
			if err != nil {
				log.Printf("handleBlock r.Close: %v", err)
			}
		}()
		written, err := io.Copy(blockFile, r)
		if err != nil {
			log.Println(err)
			return
		}
		if written != fileSize {
			log.Println("Error decoding block: sizes don't match:", written, "vs", fileSize)
			return
		}
	} else if encoding == "http" {
		log.Println("Getting block", hash, "from", dataString)
		resp, err := http.Get(dataString)
		if err != nil {
			log.Println("Error receiving block at", dataString, err)
			return
		}
		defer resp.Body.Close()
		blockFile, err = ioutil.TempFile("", "daisy")
		if err != nil {
			log.Println(err)
			return
		}
		written, err := io.Copy(blockFile, resp.Body)
		if err != nil {
			log.Println("Error saving block:", err)
			blockFile.Close()
			os.Remove(blockFile.Name())
			return
		}
		if written != fileSize {
			log.Println("Error decoding block: sizes don't match:", written, "vs", fileSize)
			blockFile.Close()
			os.Remove(blockFile.Name())
			return
		}
		err = blockFile.Close()
		if err != nil {
			log.Printf("handleBlock blockFile.Close: %v", err)
		}
		defer func() {
			err = os.Remove(blockFile.Name())
			if err != nil {
				log.Printf("remove: %v", err)
			}
		}()
	} else {
		log.Println("Unknown block encoding:", encoding)
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
	blk.DbBlockchainBlock.TimeAccepted = time.Now()
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
	blk.Close()
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

	localAddresses := getLocalAddresses()
	if inStrings(addr.IP.String(), localAddresses) {
		return nil, fmt.Errorf("Refusing to connect to myself at %s", addr.IP)
	}

	conn, err := net.Dial("tcp", address)
	if err != nil {
		log.Println("Error connecting to", address, err)
		return nil, err
	}
	return p2pSetupPeer(address, conn)
}

// Creates the p2pConnection structure for the peer and adds it to the peer list.
// Does not start the handler goroutine.
func p2pSetupPeer(address string, conn net.Conn) (*p2pConnection, error) {
	p2pc := p2pConnection{
		conn:         conn,
		address:      address,
		chanToPeer:   make(chan interface{}, 5),
		chanFromPeer: make(chan StrIfMap, 5),
	}
	p2pPeers.Add(&p2pc)
	return &p2pc, nil
}
