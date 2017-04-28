package main

import (
	"log"
)

func main() {
	log.Println("Starting up...")
	configInit()
	dbInit()
	go p2pServer()
}
