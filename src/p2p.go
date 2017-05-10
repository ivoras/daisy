package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"strconv"
)

const p2pClientid = "godaisy/1.0"

const msgHello = "hello"

type p2pConnection struct {
	conn net.Conn
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
		go p2pc.p2pHandleConnection()
	}
}

func (p2pc *p2pConnection) p2pHandleConnection() {
	defer p2pc.conn.Close()
	peer := bufio.NewReadWriter(bufio.NewReader(p2pc.conn), bufio.NewWriter(p2pc.conn))
	hellomsg := map[string]string{
		"msg":          msgHello,
		"client_id":    p2pClientid,
		"chain_height": strconv.Itoa(dbGetBlockchainHeight()),
	}
	peer.Write(stringMap2JsonBytes(hellomsg))
	for {
		line, err := peer.ReadBytes('\n')
		if err != nil {
			log.Panicln("Error reading data from", p2pc.conn, err)
		}
		var msg map[string]string
		err = json.Unmarshal(line, &msg)
		if err != nil {
			log.Println("Cannot parse json", string(line), "from", p2pc.conn)
			break
		}
		var cmd string
		var ok bool
		if cmd, ok = msg["msg"]; !ok {
			log.Println("Unexpected message:", string(line))
			break
		}
		switch cmd {
		case msgHello:
			log.Println("Hello from", p2pc.conn)
		}
	}
}
