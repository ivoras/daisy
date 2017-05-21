package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strconv"
	"time"
)

const p2pClientVersionString = "godaisy/1.0"

type p2pMsgHeader struct {
	Root  string `json:"root"`
	Msg   string `json:"msg"`
	P2pID int64  `json:"p2p_id"`
}

const p2pMsgHello = "hello"

type p2pMsgHelloStruct struct {
	p2pMsgHeader
	Version     string `json:"version"`
	ChainHeight int    `json:"chain_height"`
}

type peerStringMap map[string]time.Time

var bootstrapPeers = peerStringMap{
	"cosmos.ivoras.net:2017":  time.Now(),
	"fielder.ivoras.net:2017": time.Now(),
}

var p2pEphemeralID = rand.Int63() & 0xffffffffffff

type p2pConnection struct {
	conn    net.Conn
	address string // host:port
	peer    *bufio.ReadWriter
	peerID  int64
}

type p2pPeersSet struct {
	peers map[*p2pConnection]time.Time
	lock  WithMutex
}

var p2pPeers = p2pPeersSet{peers: make(map[*p2pConnection]time.Time)}

func (p *p2pPeersSet) Add(c *p2pConnection) {
	p.lock.With(func() {
		p.peers[c] = time.Now()
	})
}

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
		}
	}
}

func (p2pc *p2pConnection) handleMsgHello(rawMsg map[string]interface{}) {
	var ver string
	var err error
	if ver, err = siMapGetString(rawMsg, "version"); err != nil {
		log.Println(p2pc.conn, err)
		return
	}
	log.Println("Hello from", p2pc.conn, ver)
	if p2pc.peerID == 0 {
		if p2pc.peerID, err = siMapGetInt64(rawMsg, "p2p_id"); err != nil {
			log.Println(p2pc.conn, err)
			return
		}
	}
	// Check for duplicates
	dup := false
	p2pPeers.lock.With(func() {
		for p := range p2pPeers.peers {
			if p.peerID == p2pc.peerID {
				log.Printf("%v looks like a duplicate of %v (%x), dropping it.", p2pc.conn, p.conn, p2pc.peerID)
				dup = true
				return
			}
		}
	})
	if p2pc.peerID == p2pEphemeralID {
		log.Printf("%v is apperently myself. Dropping it.", p2pc.conn)
		dup = true
	}
	if dup {
		p2pc.conn.Close()
		return
	}
	dbSavePeer(p2pc.address)
}
