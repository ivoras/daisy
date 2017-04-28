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
	go p2pServer()
}
