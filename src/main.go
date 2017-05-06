package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

const (
	eventQuit = iota
)

type sysEventMessage struct {
	event int
	idata int
}

var sysEventChannel = make(chan sysEventMessage, 5)

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
	go p2pServer()

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
