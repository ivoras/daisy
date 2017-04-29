package main

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
)

// GenesisBlockHash is the SHA256 hash of the genesis block payload
const GenesisBlockHash = "85404bd215ff98f3ea04b8c0c86180a79f2e1c38740d03b02f0bfff358dd1347"

const blockchainSubdirectoryName = "blocks"
const blockFilenameFormat = "%s/block_%08d.db"

var blockchainSubdirectory string

func blockchainInit() {
	blockchainSubdirectory = fmt.Sprintf("%s/%s", cfg.DataDir, blockchainSubdirectoryName)
	if _, err := os.Stat(blockchainSubdirectory); err != nil {
		// Probably doesn't exist, create it
		log.Println("Creating directory", blockchainSubdirectory)
		err := os.Mkdir(blockchainSubdirectory, 0755)
		if err != nil {
			log.Fatalln(err)
		}
	}

	if dbGetBlockchainHeight() == -1 {
		log.Println("Noticing the existence of the Genesis block. Let there be light.")

		// This is basically testing the crypto code, no real purpose.
		keypair, err := getAPrivateKey()
		if err != nil {
			log.Panicln(err)
		}
		publicKeyHash := cryptoMustGetPublicKeyHash(keypair)
		signature, err := cryptoSignPublicKeyHash(keypair, publicKeyHash)
		if err != nil {
			log.Panicln(err)
		}
		fmt.Println(hex.EncodeToString(signature))
		if err = cryptoVerifyPublicKeyHashSignature(&keypair.PublicKey, publicKeyHash, signature); err != nil {
			log.Panicln(err)
		}

		// Bring the genesis block into existence
		ioutil.WriteFile(fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, 0), MustAsset("bindata/genesis.db"), 0644)
	}
}
