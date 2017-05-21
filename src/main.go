package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	eventQuit = iota
)

type sysEventMessage struct {
	event int
	idata int
}

var sysEventChannel = make(chan sysEventMessage, 5)
var startTime = time.Now()

func main() {
	log.Println("Starting up...")
	sigChannel := make(chan os.Signal, 1)
	signal.Notify(sigChannel, syscall.SIGINT)

	configInit()
	dbInit()
	cryptoInit()
	blockchainInit()
	if processActions() {
		return
	}
	log.Printf("Ephemeral ID: %x\n", p2pEphemeralID)
	go p2pServer()
	go p2pClient()

	for {
		select {
		case msg := <-sysEventChannel:
			switch msg.event {
			case eventQuit:
				log.Println("Exiting")
				os.Exit(msg.idata)
			}
		case sig := <-sigChannel:
			switch sig {
			case syscall.SIGINT:
				sysEventChannel <- sysEventMessage{event: eventQuit, idata: 0}
				log.Println("^C detected")
			case syscall.SIGTERM:
				sysEventChannel <- sysEventMessage{event: eventQuit, idata: 0}
				log.Println("Quit signal detected")
			}
		}
	}

}
