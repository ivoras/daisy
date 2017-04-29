package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
)

// GenesisBlockHash is the SHA256 hash of the genesis block payload
const GenesisBlockHash = "d8c44e8513ea5d76ddce49538dbf7e2d6223947418d4c2bb8d4668a378c5dfbf"

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
		if err = cryptoVerifyPublicKeyHashSignature(&keypair.PublicKey, publicKeyHash, signature); err != nil {
			log.Panicln(err)
		}
		signature, err = cryptoSignBytes(keypair, make([]byte, 0)) // empty array signature
		if err != nil {
			log.Panicln(err)
		}

		// Bring the genesis block into existence
		genesisBlock := MustAsset("bindata/genesis.db")
		if hashBytesToHexString(genesisBlock) != GenesisBlockHash {
			log.Panicln("Genesis block hash unexpected")
		}

		ioutil.WriteFile(fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, 0), genesisBlock, 0644)
	}
}
