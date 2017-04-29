package main

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"os"
)

// GenesisBlockHash is the SHA256 hash of the genesis block payload
const GenesisBlockHash = "4c498d853114d9163fbdb88ec78aead6db3fa7c5f7aae153232bfa68e7dca374"

const blockchainSubdirectoryName = "blocks"
const blockFilenameFormat = "%s/block_%08d.db"

var blockchainSubdirectory string

// Block is the working representation of a blockchain block
type Block struct {
	*DbBlockchainBlock
	db *sql.DB
}

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
		genesisBlockFilename := fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, 0)
		ioutil.WriteFile(genesisBlockFilename, genesisBlock, 0644)
	}
}

// OpenBlockByHeight opens a block stored in the blockchain at the given height
func OpenBlockByHeight(height int) (*Block, error) {
	b := Block{DbBlockchainBlock: &DbBlockchainBlock{Height: height}}
	blockFilename := fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, height)
	hash, err := hashFileToHexString(blockFilename)
	if err != nil {
		return nil, err
	}
	dbb, err := dbGetBlockByHeight(height)
	if err != nil {
		return nil, err
	}
	if hash != dbb.Hash {
		return nil, fmt.Errorf("Recorded block hash doesn't match actual: %s vs %s", dbb.Hash, hash)
	}
	b.DbBlockchainBlock = dbb
	b.db, err = dbOpen(blockFilename, true)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ReadBlockFromFile reads block metadata from the given database file
func ReadBlockFromFile(fileName string, height int) (*Block, error) {
	hash, err := hashFileToHexString(fileName)
	if err != nil {
		return nil, err
	}
	db, err := dbOpen(fileName, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	b := Block{DbBlockchainBlock: &DbBlockchainBlock{Height: height, Hash: hash}}

	return &b, nil
}
