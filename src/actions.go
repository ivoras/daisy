package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// The binary can be called with some actions, like signblock, importblock, signkey.
// This function processes those and returns true if it has found something to execure.
func processActions() bool {
	if flag.NArg() == 0 {
		return false
	}
	cmd := flag.Arg(0)
	switch cmd {
	case "help":
		actionHelp()
		return true
	case "mykeys":
		actionMyKeys()
		return true
	case "query":
		actionQuery(flag.Arg(1))
		return true
	case "signimportblock":
		if flag.NArg() < 2 {
			log.Fatal("Not enough arguments: expecting sqlite db filename")
		}
		actionSignImportBlock(flag.Arg(1))
		return true
	}
	return false
}

// Opens the given block file (SQLite database), creates metadata tables in it, signes the
// block with one of the private keys, and accepts the resulting block into the blockchain.
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

// Runs a SQL query over all the blocks.
func actionQuery(q string) {
	log.Println("Running query:", q)
	errCount := 0
	for h := 1; h <= dbGetBlockchainHeight(); h++ {
		fn := blockchainGetFilename(h)
		db, err := dbOpen(fn, true)
		if err != nil {
			log.Panic(err)
		}
		rows, err := db.Query(q)
		if err != nil {
			errCount++
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			log.Panic(err)
		}
		for rows.Next() {
			columns := make([]interface{}, len(cols))
			columnPointers := make([]interface{}, len(cols))
			for i := range columns {
				columnPointers[i] = &columns[i]
			}
			if err := rows.Scan(columnPointers...); err != nil {
				log.Panic(err)
			}
			row := make(map[string]interface{})
			for i, colName := range cols {
				val := columnPointers[i].(*interface{})
				row[colName] = *val
			}
			buf, err := json.Marshal(row)
			if err != nil {
				log.Panic(err)
			}
			fmt.Println(string(buf))
		}
	}
	if errCount != 0 {
		log.Println("There have been", errCount, "errors.")
	}
}

// Shows the help message.
func actionHelp() {
	fmt.Printf("usage: %s [flags] [command]\n", os.Args[0])
	flag.PrintDefaults()
	fmt.Println("Commands:")
	fmt.Println("\thelp\t\tShows this help message")
	fmt.Println("\tmykeys\t\tShows a list of my public keys")
	fmt.Println("\tquery\t\tExecutes a SQL query on the blockchain (expects 1 argument: SQL query)")
	fmt.Println("\tsignimportblock\tSigns a block (creates metadata tables in it first) and imports it into the blockchain (expects 1 argument: a sqlite db filename)")
}

// Shows the public keys which correspond to private keys in the system database.
func actionMyKeys() {
	for _, k := range dbGetMyPublicKeys() {
		fmt.Println(k)
	}
}
