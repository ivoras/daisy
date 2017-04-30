package main

import (
	"flag"
	"log"
)

// The binary can be called with some actions, like signblock, importblock, signkey
func processActions() {
	if flag.NArg() == 0 {
		return
	}
	cmd := flag.Arg(0)
	switch cmd {
	case "signimportblock":
		log.Println("Hllo")
	}
}
