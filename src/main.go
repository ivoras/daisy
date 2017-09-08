package main

import (
	"log"
	"math/rand"
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

// Passes messages such as eventQuit
var sysEventChannel = make(chan sysEventMessage, 5)

// Process start time
var startTime = time.Now()

func main() {
	rand.Seed(p2pEphemeralID + getNowUTC()) // Initialise weak RNG with strong RNG
	log.Println("Starting up", p2pClientVersionString, "...")
	sigChannel := make(chan os.Signal, 1)
	signal.Notify(sigChannel, syscall.SIGINT, syscall.SIGTERM)

	configInit()
	dbInit()
	cryptoInit()
	blockchainInit()
	if processActions() {
		return
	}
	log.Printf("Ephemeral ID: %x\n", p2pEphemeralID)
	go p2pCoordinator.Run()
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
