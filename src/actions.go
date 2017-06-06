package main

import (
	"encoding/hex"
	"flag"
	"log"
	"strconv"
	"time"
)

// The binary can be called with some actions, like signblock, importblock, signkey
func processActions() bool {
	if flag.NArg() == 0 {
		return false
	}
	cmd := flag.Arg(0)
	switch cmd {
	case "signimportblock":
		if flag.NArg() < 2 {
			log.Fatal("Not enough arguments: expecting sqlite db filename")
		}
		actionSignImportBlock(flag.Arg(1))
		return true
	}
	return false
}

func actionSignImportBlock(fn string) {
	db, err := dbOpen(fn, false)
	if err != nil {
		log.Fatal(err)
	}
	dbEnsureBlockchainTables(db)
	keypair, publicKeyHash, err := cryptoGetAPrivateKey()
	if err != nil {
		log.Fatal(err)
	}
	lastBlockHeight := dbGetBlockchainHeight()
	dbb, err := dbGetBlockByHeight(lastBlockHeight)
	if err != nil {
		log.Fatal(err)
	}
	if err = dbSetMeta(db, "Version", strconv.Itoa(CurrentBlockVersion)); err != nil {
		log.Panic(err)
	}
	dbSetMeta(db, "PreviousBlockHash", dbb.Hash)
	signature, err := cryptoSignHex(keypair, dbb.Hash)
	if err != nil {
		log.Fatal(err)
	}
	dbSetMeta(db, "PreviousBlockHashSignature", signature)
	pkdb, err := dbGetPublicKey(publicKeyHash)
	if err != nil {
		log.Panic(err)
	}
	previousBlockHashSignature, _ := hex.DecodeString(signature)
	if creatorString, ok := pkdb.metadata["BlockCreator"]; ok {
		dbSetMeta(db, "Creator", creatorString)
	}
	dbSetMeta(db, "CreatorPublicKey", pkdb.publicKeyHash)
	if err = db.Close(); err != nil {
		log.Panic(err)
	}
	blockHashHex, err := hashFileToHexString(fn)
	if err != nil {
		log.Panic(err)
	}
	signature, err = cryptoSignHex(keypair, blockHashHex)
	if err != nil {
		log.Panic(err)
	}
	blockHashSignature, _ := hex.DecodeString(signature)

	newBlockHeight := lastBlockHeight + 1
	newBlock := DbBlockchainBlock{Hash: blockHashHex, HashSignature: blockHashSignature, PreviousBlockHash: dbb.Hash, PreviousBlockHashSignature: previousBlockHashSignature,
		Version: CurrentBlockVersion, SignaturePublicKeyHash: pkdb.publicKeyHash, Height: newBlockHeight, TimeAccepted: time.Now()}

	err = blockchainCopyFile(fn, newBlockHeight)
	if err != nil {
		log.Panic(err)
	}

	err = dbInsertBlock(&newBlock)
	if err != nil {
		log.Panic(err)
	}

}
