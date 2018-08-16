package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"time"
)

// The binary can be called with some actions, like signblock, importblock, signkey.
// This function processes those and returns true if it has found something to execute.
// The processActions() function is called after the blockchain database is initialised
// and active.
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

// processPreBlockchainActions is called to process actions which need to executed
// before the blockchain database is running.
func processPreBlockchainActions() bool {
	if flag.NArg() == 0 {
		return false
	}
	cmd := flag.Arg(0)
	switch cmd {
	case "newchain":
		if flag.NArg() < 2 {
			log.Fatal("Not enough arguments: expecing chainparams.json")
		}
		actionNewChain(flag.Arg(1))
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
	if err = dbSetMetaInt(db, "Version", CurrentBlockVersion); err != nil {
		log.Panic(err)
	}
	err = dbSetMetaString(db, "PreviousBlockHash", dbb.Hash)
	if err != nil {
		log.Fatal(err)
	}
	signature, err := cryptoSignHex(keypair, dbb.Hash)
	if err != nil {
		log.Fatal(err)
	}
	err = dbSetMetaString(db, "PreviousBlockHashSignature", signature)
	if err != nil {
		log.Fatal(err)
	}
	pkdb, err := dbGetPublicKey(publicKeyHash)
	if err != nil {
		log.Panic(err)
	}
	previousBlockHashSignature, err := hex.DecodeString(signature)
	if err != nil {
		log.Fatal(err)
	}
	if creatorString, ok := pkdb.metadata["BlockCreator"]; ok {
		err = dbSetMetaString(db, "Creator", creatorString)
		if err != nil {
			log.Fatal(err)
		}
	}
	err = dbSetMetaString(db, "CreatorPublicKey", pkdb.publicKeyHash)
	if err != nil {
		log.Fatal(err)
	}
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
	fmt.Println("\tnewchain\tStarts a new chain with the given parameters (expects 1 argument: chainparams.json)")
}

// Shows the public keys which correspond to private keys in the system database.
func actionMyKeys() {
	for _, k := range dbGetMyPublicKeys() {
		fmt.Println(k)
	}
}

// NewChainParams is extended from ChainParams for new chain creation
type NewChainParams struct {
	ChainParams
	GenesisDb string `json:"genesis_db"`
}

func actionNewChain(jsonFilename string) {
	jsonData, err := ioutil.ReadFile(jsonFilename)
	if err != nil {
		log.Fatal(err)
	}

	ncp := NewChainParams{}
	err = json.Unmarshal(jsonData, &ncp)
	if err != nil {
		log.Fatal(err)
	}
	if ncp.GenesisBlockTimestamp == "" {
		ncp.GenesisBlockTimestamp = time.Now().Format(time.RFC3339)
	}
	if ncp.CreatorPublicKey != "" || ncp.GenesisBlockHash != "" || ncp.GenesisBlockHashSignature != "" {
		log.Fatal("chainparams.json must not contain cryptographic properties")
	}
	log.Println("Creating a new blockchain from", jsonFilename)

	empty, err := isDirEmpty(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	if !empty {
		log.Fatal("Data directory must not be empty:", cfg.DataDir)
	}

	ensureBlockchainSubdirectoryExists()
	freshDb := true
	if ncp.GenesisDb != "" && fileExists(ncp.GenesisDb) {
		err = blockchainCopyFile(ncp.GenesisDb, 0)
		if err != nil {
			log.Fatal(err)
		}
		freshDb = false
	}

	// Modify the new genesis db to include the metadata
	blockFilename := fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, 0)
	log.Println("Creating the genesis block at", blockFilename)
	db, err := dbOpen(blockFilename, false)
	if err != nil {
		log.Fatal("dbOpen", err)
	}
	if freshDb {
		_, err = db.Exec("PRAGMA page_size=512")
		if err != nil {
			log.Fatal(err)
		}
	}
	_, err = db.Exec("PRAGMA journal_mode=DELETE")
	if err != nil {
		log.Fatal(err)
	}
	dbEnsureBlockchainTables(db)
	err = dbSetMetaInt(db, "Version", CurrentBlockVersion)
	if err != nil {
		log.Fatal(err)
	}
	err = dbSetMetaString(db, "PreviousBlockHash", GenesisBlockPreviousBlockHash)
	if err != nil {
		log.Fatal(err)
	}
	err = dbSetMetaString(db, "Creator", ncp.Creator)
	if err != nil {
		log.Fatal(err)
	}
	err = dbSetMetaString(db, "Timestamp", ncp.GenesisBlockTimestamp)
	if err != nil {
		log.Fatal(err)
	}

	if len(ncp.BootstrapPeers) > 0 {
		// bootstrapPeers is required to be filled in before dbInit()
		bootstrapPeers = peerStringMap{}
		for _, peer := range ncp.BootstrapPeers {
			bootstrapPeers[peer] = time.Now()
		}
	}

	dbInit()     // Create system databases
	cryptoInit() // Create the genesis keypair

	pubKeys := dbGetMyPublicKeys()
	if len(pubKeys) != 1 {
		log.Fatal("There should have been only one genesis keypair, found", len(pubKeys))
	}
	err = dbSetMetaString(db, "CreatorPublicKey", pubKeys[0])

	pKey, onePubKey, err := cryptoGetAPrivateKey()
	if err != nil {
		log.Fatal(err)
	}
	if onePubKey != pubKeys[0] {
		log.Fatal("The impossible has happened: two attempts to get the single public key have different results:", pubKeys[0], onePubKey)
	}
	log.Println("Genesis public key:", onePubKey)
	prevSig, err := cryptoSignHex(pKey, GenesisBlockPreviousBlockHash)
	if err != nil {
		log.Fatal("cryptoSignHex", err)
	}
	err = dbSetMetaString(db, "PreviousBlockHashSignature", prevSig)
	if err != nil {
		log.Fatal(err)
	}
	err = dbSetMetaString(db, "CreatorPubKey", onePubKey)
	if err != nil {
		log.Fatal(err)
	}
	err = db.Close()
	if err != nil {
		log.Fatal(err)
	}

	// Hash it, sign it, generate chainparams
	hash, err := hashFileToHexString(blockFilename)
	if err != nil {
		log.Fatal(err)
	}
	ncp.GenesisBlockHash = hash
	ncp.CreatorPublicKey = onePubKey
	ncp.GenesisBlockHashSignature, err = cryptoSignHex(pKey, hash)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Genesis block hash:", ncp.GenesisBlockHash)

	// Save the chainparams to the data dir
	cpJSON, err := json.Marshal(ncp.ChainParams)
	if err != nil {
		log.Fatal(err)
	}
	err = ioutil.WriteFile(fmt.Sprintf("%s/%s", cfg.DataDir, chainParamsBaseName), cpJSON, 0644)
	if err != nil {
		log.Fatal(err)
	}

	// Record the genesis block into the system database
	newBlock := DbBlockchainBlock{
		Hash:                       ncp.GenesisBlockHash,
		HashSignature:              mustDecodeHex(ncp.GenesisBlockHashSignature),
		PreviousBlockHash:          GenesisBlockPreviousBlockHash,
		PreviousBlockHashSignature: mustDecodeHex(prevSig),
		Version:                    CurrentBlockVersion,
		SignaturePublicKeyHash:     onePubKey,
		Height:                     0,
		TimeAccepted:               time.Now(),
	}
	err = dbInsertBlock(&newBlock)
	if err != nil {
		log.Panic(err)
	}

	// Reopen the database to verify
	log.Println("Reloading to verify...")
	blockchainInit(false)

	// If we make it to here, everything's ok.
	log.Println("All done.")
}
