package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"strconv"
	"time"
)

const p2pClientIDString = "godaisy/1.0"

const msgHello = "hello"

var bootstrapPeers = []string{
	"cosmos.ivoras.net:2017",
	"fielder.ivoras.net:2017",
}

type p2pConnection struct {
	conn net.Conn
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
		p2pc := p2pConnection{conn: conn}
		p2pPeers.Add(&p2pc)
		go p2pc.handleConnection()
	}
}

func p2pClient() {
	// Read the list of (old) peers from the db
}

func (p2pc *p2pConnection) handleConnection() {
	defer p2pc.conn.Close()
	peer := bufio.NewReadWriter(bufio.NewReader(p2pc.conn), bufio.NewWriter(p2pc.conn))
	hellomsg := map[string]string{
		"msg":          msgHello,
		"client_id":    p2pClientIDString,
		"chain_height": strconv.Itoa(dbGetBlockchainHeight()),
	}
	peer.Write(stringMap2JsonBytes(hellomsg))
	for {
		line, err := peer.ReadBytes('\n')
		if err != nil {
			log.Panicln("Error reading data from", p2pc.conn, err)
		}
		var msg map[string]interface{}
		err = json.Unmarshal(line, &msg)
		if err != nil {
			log.Println("Cannot parse json", string(line), "from", p2pc.conn)
			break
		}
		var ok bool
		var ii interface{}
		if ii, ok = msg["msg"]; !ok {
			log.Println("Unexpected message:", string(line))
			break
		}
		var cmd string
		if cmd, ok = ii.(string); !ok {
			log.Println("Unexpected message:", string(line))
		}
		switch cmd {
		case msgHello:
			p2pc.handleMsgHello(msg)
		}
	}
}

func (p2pc *p2pConnection) handleMsgHello(msg map[string]interface{}) {
	log.Println("Hello from", p2pc.conn)

}
