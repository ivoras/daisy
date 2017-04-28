package main

import (
	"log"
)

func main() {
	log.Println("Starting up...")
	configInit()
	dbInit()
	cryptoInit()
	go p2pServer()
}
