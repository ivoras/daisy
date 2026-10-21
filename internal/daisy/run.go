package daisy

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

// Passes messages such as eventQuit
var sysEventChannel = make(chan sysEventMessage, 5)

// Main is the entry point of the Daisy node and command line tool.
func Main() {
	log.Println("Starting up", p2pClientVersionString, "...")
	sigChannel := make(chan os.Signal, 1)
	signal.Notify(sigChannel, syscall.SIGINT, syscall.SIGTERM)

	configInit()
	if processPreBlockchainActions() {
		return
	}
	dbInit()
	cryptoInit()
	blockchainInit(true)
	if processActions() {
		return
	}
	log.Printf("Ephemeral ID: %x\n", p2pEphemeralID)
	go p2pCoordinator.Run()
	go p2pServer()
	go p2pClient()
	go blockWebServer()
	if cfg.UPnP {
		natMapper = newNATManager(map[int]string{cfg.P2pPort: "Daisy P2P", cfg.httpPort: "Daisy blocks"})
		natMapper.start()
	}
	if cfg.MDNS {
		mdns = startMDNS()
	}

	for {
		select {
		case msg := <-sysEventChannel:
			switch msg.event {
			case eventQuit:
				log.Println("Exiting")
				natMapper.stop()
				mdns.stop()
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
