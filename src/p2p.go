package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"strconv"
)

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
	for {
		line, err := peer.ReadBytes('\n')
		if err != nil {
			log.Panicln("Error reading data from", p2pc.conn, err)
		}
		var cmd map[string]string
		err = json.Unmarshal(line, &cmd)
		if err != nil {
			log.Println("Cannot parse json", string(line), "from", p2pc.conn)
		}
	}
}
