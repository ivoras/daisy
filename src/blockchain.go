package main

import (
	"encoding/hex"
	"fmt"
	"log"
)

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
