package main

import (
	"log"
)

func main() {
	log.Println("Starting up...")
	configInit()
	dbInit()
	cryptoInit()
	blockchainInit()
	if processActions() {
		return
	}
	go p2pServer()
}
