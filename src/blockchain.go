package main

import (
	"log"
)

func blockchainInit() {
	if dbGetBlockchainHeight() == -1 {
		log.Println("Noticing the existence of the Genesis block. Let there be light.")
	}
}
