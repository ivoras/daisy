package main

import (
	"encoding/hex"
	"fmt"
	"log"
)

// GenesisBlockHash is the SHA256 hash of the genesis block payload
const GenesisBlockHash = "85404bd215ff98f3ea04b8c0c86180a79f2e1c38740d03b02f0bfff358dd1347"

func blockchainInit() {
	if dbGetBlockchainHeight() == -1 {
		log.Println("Noticing the existence of the Genesis block. Let there be light.")

		keypair, err := getAPrivateKey()
		if err != nil {
			log.Fatalln(err)
		}
		publicKeyHash := cryptoMustGetPublicKeyHash(keypair)
		signature, err := cryptoSignPublicKeyHash(keypair, publicKeyHash)
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Println(hex.EncodeToString(signature))
		if err = cryptoVerifyPublicKeyHashSignature(&keypair.PublicKey, publicKeyHash, signature); err != nil {
			log.Fatalln(err)
		}
	}
}
