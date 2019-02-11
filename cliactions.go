package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
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
			log.Fatalln("Not enough arguments: expecting <sqlite db filename>")
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
			log.Fatalln("Not enough arguments: expecing chainparams.json")
		}
		actionNewChain(flag.Arg(1))
		return true
	case "pull":
		if flag.NArg() < 2 {
			log.Fatalln("Not enough arguments: expecting chain URL")
		}
		actionPull(flag.Arg(1))
		return true
	}
	return false
}

// Opens the given block file (SQLite database), creates metadata tables in it, signes the
// block with one of the private keys, and accepts the resulting block into the blockchain.
func actionSignImportBlock(fn string) {
	db, err := dbOpen(fn, false)
	if err != nil {
		log.Fatalln(err)
	}
	dbEnsureBlockchainTables(db)
	keypair, publicKeyHash, err := cryptoGetAPrivateKey()
	if err != nil {
		log.Fatalln(err)
	}
	lastBlockHeight := dbGetBlockchainHeight()
	dbb, err := dbGetBlockByHeight(lastBlockHeight)
	if err != nil {
		log.Fatalln(err)
	}
	if err = dbSetMetaInt(db, "Version", CurrentBlockVersion); err != nil {
		log.Panic(err)
	}
	err = dbSetMetaString(db, "PreviousBlockHash", dbb.Hash)
	if err != nil {
		log.Fatalln(err)
	}
	signature, err := cryptoSignHex(keypair, dbb.Hash)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "PreviousBlockHashSignature", signature)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "Timestamp", time.Now().Format(time.RFC3339))
	if err != nil {
		log.Fatalln(err)
	}

	pkdb, err := dbGetPublicKey(publicKeyHash)
	if err != nil {
		log.Panic(err)
	}
	previousBlockHashSignature, err := hex.DecodeString(signature)
	if err != nil {
		log.Fatalln(err)
	}
	if creatorString, ok := pkdb.metadata["BlockCreator"]; ok {
		err = dbSetMetaString(db, "Creator", creatorString)
		if err != nil {
			log.Fatalln(err)
		}
	}
	err = dbSetMetaString(db, "CreatorPublicKey", pkdb.publicKeyHash)
	if err != nil {
		log.Fatalln(err)
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
	for h := dbGetBlockchainHeight(); h > 0; h-- {
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
				if reflect.TypeOf(*val).String() == "[]uint8" {
					row[colName] = string((*val).([]byte))
				} else {
					row[colName] = *val
				}
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
	fmt.Println("\tpull\t\tPulls a blockchain from a HTTP URL (expects 1 argument: URL, e.g. http://example.com:2018/)")
}

// Shows the public keys which correspond to private keys in the system database.
func actionMyKeys() {
	for _, k := range dbGetMyPublicKeyHashes() {
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
		log.Fatalln(err)
	}

	ncp := NewChainParams{}
	err = json.Unmarshal(jsonData, &ncp)
	if err != nil {
		log.Fatalln(err)
	}
	if ncp.GenesisBlockTimestamp == "" {
		ncp.GenesisBlockTimestamp = time.Now().Format(time.RFC3339)
	}
	if ncp.CreatorPublicKey != "" || ncp.GenesisBlockHash != "" || ncp.GenesisBlockHashSignature != "" {
		log.Fatalln("chainparams.json must not contain cryptographic properties")
	}
	log.Println("Creating a new blockchain from", jsonFilename)

	empty, err := isDirEmpty(cfg.DataDir)
	if err != nil {
		log.Fatalln(err)
	}
	if !empty {
		log.Fatalln("Data directory must not be empty:", cfg.DataDir)
	}

	ensureBlockchainSubdirectoryExists()
	freshDb := true
	if ncp.GenesisDb != "" && fileExists(ncp.GenesisDb) {
		err = blockchainCopyFile(ncp.GenesisDb, 0)
		if err != nil {
			log.Fatalln(err)
		}
		freshDb = false
	}

	// Modify the new genesis db to include the metadata
	blockFilename := blockchainGetFilename(0)
	log.Println("Creating the genesis block at", blockFilename)
	db, err := dbOpen(blockFilename, false)
	if err != nil {
		log.Fatalln("dbOpen", err)
	}
	if freshDb {
		_, err = db.Exec("PRAGMA page_size=512")
		if err != nil {
			log.Fatalln(err)
		}
	}
	_, err = db.Exec("PRAGMA journal_mode=DELETE")
	if err != nil {
		log.Fatalln(err)
	}
	dbEnsureBlockchainTables(db)
	err = dbSetMetaInt(db, "Version", CurrentBlockVersion)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "PreviousBlockHash", GenesisBlockPreviousBlockHash)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "Creator", ncp.Creator)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "Timestamp", ncp.GenesisBlockTimestamp)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "Description", ncp.Description)
	if err != nil {
		log.Fatalln(err)
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

	pubKeys := dbGetMyPublicKeyHashes()
	if len(pubKeys) != 1 {
		log.Fatalln("There should have been only one genesis keypair, found", len(pubKeys))
	}
	err = dbSetMetaString(db, "CreatorPublicKey", pubKeys[0])

	pKey, pubKeyHash, err := cryptoGetAPrivateKey()
	if err != nil {
		log.Fatalln(err)
	}
	if pubKeyHash != pubKeys[0] {
		log.Fatalln("The impossible has happened: two attempts to get the single public key have different results:", pubKeys[0], pubKeyHash)
	}
	log.Println("Genesis public key:", pubKeyHash)
	prevSig, err := cryptoSignHex(pKey, GenesisBlockPreviousBlockHash)
	if err != nil {
		log.Fatalln("cryptoSignHex", err)
	}
	err = dbSetMetaString(db, "PreviousBlockHashSignature", prevSig)
	if err != nil {
		log.Fatalln(err)
	}
	err = dbSetMetaString(db, "CreatorPubKey", pubKeyHash)
	if err != nil {
		log.Fatalln(err)
	}

	// Write the public key into the genesis block
	pubKey, err := dbGetPublicKey(pubKeyHash)
	if err != nil {
		log.Fatalln("Error getting public key from db", err)
	}
	selfSig, err := cryptoSignPublicKeyHash(pKey, pubKeyHash)
	if err != nil {
		log.Fatalln("Error signing publicKey", err)
	}
	_, err = db.Exec("INSERT INTO _keys (op, pubkey_hash, pubkey, sigkey_hash, signature) VALUES (?, ?, ?, ?, ?)",
		"A", pubKeyHash, hex.EncodeToString(pubKey.publicKeyBytes), pubKeyHash, hex.EncodeToString(selfSig))
	if err != nil {
		log.Fatalln("Error recording the genesis block public key")
	}

	err = db.Close()
	if err != nil {
		log.Fatalln(err)
	}

	// Hash it, sign it, generate chainparams
	hash, err := hashFileToHexString(blockFilename)
	if err != nil {
		log.Fatalln(err)
	}
	ncp.GenesisBlockHash = hash
	ncp.CreatorPublicKey = pubKeyHash
	ncp.GenesisBlockHashSignature, err = cryptoSignHex(pKey, hash)
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("Genesis block hash:", ncp.GenesisBlockHash)

	// Save the chainparams to the data dir
	cpJSON, err := json.Marshal(ncp.ChainParams)
	if err != nil {
		log.Fatalln(err)
	}
	err = ioutil.WriteFile(fmt.Sprintf("%s/%s", cfg.DataDir, chainParamsBaseName), cpJSON, 0644)
	if err != nil {
		log.Fatalln(err)
	}

	// Record the genesis block into the system database
	newBlock := DbBlockchainBlock{
		Hash:                       ncp.GenesisBlockHash,
		HashSignature:              mustDecodeHex(ncp.GenesisBlockHashSignature),
		PreviousBlockHash:          GenesisBlockPreviousBlockHash,
		PreviousBlockHashSignature: mustDecodeHex(prevSig),
		Version:                    CurrentBlockVersion,
		SignaturePublicKeyHash:     pubKeyHash,
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

func actionPull(baseURL string) {
	if !strings.HasSuffix(baseURL, "/") {
		baseURL = baseURL + "/"
	}
	// Step 1: fetch chainparams
	cpURL := fmt.Sprintf("%schainparams.json", baseURL)
	resp, err := http.Get(cpURL)
	if err != nil {
		log.Fatalln("Error getting chainparams", cpURL, err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalln("Error reading chainparams", cpURL, err)
	}
	err = json.Unmarshal(body, &chainParams)
	if err != nil {
		log.Println(string(body))
		log.Fatalln("Error decoding chainparams", cpURL, err)
	}
	if chainParams.GenesisBlockHash == "" || chainParams.GenesisBlockHashSignature == "" {
		log.Fatalln("Incomplete chainparams data", cpURL)
	}

	// Step 2: Fetch the genesis block
	gbURL := fmt.Sprintf("%s/block/0", baseURL)
	resp, err = http.Get(gbURL)
	if err != nil {
		log.Fatalln("Error getting genesis block", gbURL, err)
	}
	defer resp.Body.Close()
	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalln("Error reading genesis block", gbURL, err)
	}

	// Step 3: initialise data directories
	if fileExists(cfg.DataDir) {
		if empty, err := isDirEmpty(cfg.DataDir); err != nil || !empty {
			log.Fatalln("Blockchain directory must be empty", cfg.DataDir)
		}
	}
	if _, err = os.Stat(cfg.DataDir); err != nil {
		log.Println("Data directory", cfg.DataDir, "doesn't exist, creating.")
		err = os.Mkdir(cfg.DataDir, 0700)
		if err != nil {
			log.Panicln(err)
		}
	}
	ensureBlockchainSubdirectoryExists()

	blockFilename := blockchainGetFilename(0)
	err = ioutil.WriteFile(blockFilename, body, 0664)
	if err != nil {
		log.Fatalln("Cannot write genesis block", blockFilename, err)
	}

	hash, err := hashFileToHexString(blockFilename)
	if err != nil {
		log.Fatalln(err)
	}
	if hash != chainParams.GenesisBlockHash {
		log.Fatalln("Mismatching genesis block hash")
	}

	// Step 4: Initialise databases
	dbInit()
	dbClearSavedPeers()
	cryptoInit()

	blk, err := OpenBlockFile(blockFilename)
	if err != nil {
		log.Fatalln("Error opening genesis block", blockFilename, err, "--", cfg.DataDir, "is in inconsistent state")
	}
	kops, err := blk.dbGetKeyOps()
	if err != nil {
		log.Fatalln("Error reading genesis block keys", err)
	}
	if len(kops) == 0 {
		log.Fatalln("No key ops in genesis block?!")
	}

	verified := false
	for kHash, ops := range kops {
		for _, op := range ops {
			if op.op == "A" {
				pubKey, err := cryptoDecodePublicKeyBytes(op.publicKeyBytes)
				if err != nil {
					log.Fatalln("Error decoding genesis block public key", kHash, err)
				}
				if chainParams.CreatorPublicKey != getPubKeyHash(op.publicKeyBytes) {
					continue
				}
				if err = cryptoVerifyHex(pubKey, chainParams.GenesisBlockHash, chainParams.GenesisBlockHashSignature); err == nil {
					verified = true
					dbWritePublicKey(op.publicKeyBytes, chainParams.CreatorPublicKey, 0)
				} else {
					log.Fatalln("Error verifying genesis block signature", err)
				}
			}
		}
	}
	if !verified {
		log.Fatalln("Cannot verify genesis block signature")
	}
	blk.Close()

	hashSignature, err := hex.DecodeString(chainParams.GenesisBlockHashSignature)
	if err != nil {
		log.Fatalln("Error hex-decoding hash signature", err)
	}
	blk.HashSignature = hashSignature
	err = dbInsertBlock(blk.DbBlockchainBlock)
	if err != nil {
		log.Panic(err)
	}

	// Save the chainparams to the data dir
	cpJSON, err := json.Marshal(chainParams)
	if err != nil {
		log.Fatalln(err)
	}
	err = ioutil.WriteFile(fmt.Sprintf("%s/%s", cfg.DataDir, chainParamsBaseName), cpJSON, 0644)
	if err != nil {
		log.Fatalln(err)
	}

	// Reopen the database to verify
	log.Println("Reloading to verify...")
	blockchainInit(false)

	// If we make it to here, everything's ok.
	log.Println("All done.")
}
