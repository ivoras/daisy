package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"time"
)

// CurrentBlockVersion is the version of the block metadata
const CurrentBlockVersion = 1

// GenesisBlockHash is the SHA256 hash of the genesis block payload
const GenesisBlockHash = "8cee737a33962b419060a10213b8963e3e52cbac9beabf2004c4b2bc9cc900ca"

// GenesisBlockHashSignature is the signature of the genesis block's hash, with the key in the genesis block
const GenesisBlockHashSignature = "30450220225f84a2cd13f20c24c0d010bcf51bde3395c1e7409e78cff1271fb2b074f08a022100b077fbf3cd296015772182065c8ca94558fbd7afd1b7ea619197ed4c5e0dc26f"

// GenesisBlockTimestamp is the timestamp of the genesis block
const GenesisBlockTimestamp = "Sun, 30 Apr 2017 08:00:00 +0200"

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
		b, err := ReadBlockFromFile(genesisBlockFilename)
		if err != nil {
			log.Panicln(err)
		}
		b.Height = 0
		b.TimeAccepted, err = time.Parse(time.RFC1123Z, GenesisBlockTimestamp)
		if err != nil {
			log.Panicln(err)
		}
		err = dbInsertBlock(b.DbBlockchainBlock)
		if err != nil {
			log.Panicln(err)
		}
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

// ReadBlockFromFile reads block metadata from the given database file.
// Note that it will not fill-in all the fields. Notable, height is not stored in tje block db's metadata.
func ReadBlockFromFile(fileName string) (*Block, error) {
	hash, err := hashFileToHexString(fileName)
	if err != nil {
		return nil, err
	}
	db, err := dbOpen(fileName, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	b := Block{DbBlockchainBlock: &DbBlockchainBlock{Hash: hash}, db: db}
	b.Version, err = b.dbGetMetaInt("Version")
	if err != nil {
		return nil, err
	}
	b.PreviousBlockHash, err = b.dbGetMetaString("PreviousBlockHash")
	if err != nil {
		return nil, err
	}
	b.SignaturePublicKeyHash, err = b.dbGetMetaString("CreatorPublicKey")
	if err != nil {
		return nil, err
	}
	b.PreviousBlockHashSignature, err = b.dbGetMetaHexBytes("PreviousBlockHashSignature")
	if err != nil {
		return nil, err
	}

	return &b, nil
}

func (b *Block) dbGetMetaInt(key string) (int, error) {
	var value string
	err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(value)
}

func (b *Block) dbGetMetaString(key string) (string, error) {
	var value string
	err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (b *Block) dbGetMetaHexBytes(key string) ([]byte, error) {
	var value string
	err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(value)
}
