package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

/*********************************************************************************************************************
 * Structures and SQL schema for the system tables
 */

const mainDbFileName = "daisy.db"
const privateDbFilename = "private.db"

// DbBlockchainBlock is the convenience structure holding information from the blockchain table
type DbBlockchainBlock struct {
	Height                     int
	Hash                       string
	PreviousBlockHash          string
	SignaturePublicKeyHash     string
	PreviousBlockHashSignature []byte
	HashSignature              []byte
	TimeAccepted               time.Time
	Version                    int
}

// Note: all db times are Unix timestamps in the UTC zone

const blockchainTableCreate = `
CREATE TABLE blockchain (
	height				INTEGER NOT NULL UNIQUE,
	sigkey_hash			VARCHAR NOT NULL,
	hash				VARCHAR NOT NULL PRIMARY KEY,
	hash_signature		VARCHAR NOT NULL,
	prev_hash			VARCHAR NOT NULL,
	prev_hash_signature	VARCHAR NOT NULL,
	time_accepted		INTEGER NOT NULL,
	version				INTEGER NOT NULL
);
CREATE INDEX blockchain_sigkey_hash ON blockchain(sigkey_hash);
`

// DbPubKey is the convenience structure holding information from the pubkeys table
type DbPubKey struct {
	publicKeyHash  string
	publicKeyBytes []byte
	state          string
	timeAdded      time.Time
	isRevoked      bool
	timeRevoked    time.Time
	addBlockHeight int
	metadata       map[string]string
}

const pubKeysTableCreate = `
CREATE TABLE pubkeys (
	pubkey_hash		VARCHAR NOT NULL PRIMARY KEY,
	pubkey			VARCHAR NOT NULL,
	state			CHAR NOT NULL,
	time_added		INTEGER NOT NULL,
	time_revoked	INTEGER,
	block_height	INTEGER NOT NULL,
	metadata		VARCHAR -- JSON
);`

const privateTableCreate = `
CREATE TABLE privkeys (
	pubkey_hash		VARCHAR NOT NULL PRIMARY KEY,
	privkey			VARCHAR NOT NULL,
	time_added		INTEGER NOT NULL
);
`

const configTableCreate = `
CREATE TABLE config (
	key				VARCHAR NOT NULL PRIMARY KEY,
	value			VARCHAR NOT NULL
);
`

const peersTableCreate = `
CREATE TABLE peers (
	address			VARCHAR NOT NULL PRIMARY KEY,	-- in the format "address:port", lowercase
	time_added		INTEGER NOT NULL, -- time last seen
	permanent		BOOLEAN NOT NULL DEFAULT 0
);
`

/*********************************************************************************************************************
 * Structures and SQL schema for the individual blockchain block tables.
 */
const metaTableCreate = `
CREATE TABLE _meta (
    key         VARCHAR NOT NULL PRIMARY KEY,
    value       VARCHAR
);
`

const keysTableCreate = `
CREATE TABLE _keys (
    op              CHAR NOT NULL,
    pubkey_hash     VARCHAR NOT NULL,
    pubkey          VARCHAR NOT NULL,
    sigkey_hash     VARCHAR NOT NULL,
    signature       VARCHAR NOT NULL,
    metadata        VARCHAR,
    PRIMARY KEY (pubkey_hash, sigkey_hash)
);
`

var mainDb *sql.DB
var privateDb *sql.DB

// Initialises the system databases
func dbInit() {
	dbFileName := fmt.Sprintf("%s/%s", cfg.DataDir, mainDbFileName)
	_, err := os.Stat(dbFileName)
	mainDbFileExists := err == nil
	mainDb, err = sql.Open("sqlite3", dbFileName)
	if err != nil {
		log.Fatal(err)
	}
	if !mainDbFileExists || !dbTableExists(mainDb, "blockchain") {
		// Create system tables
		_, err = mainDb.Exec(blockchainTableCreate)
		if err != nil {
			log.Panic(err)
		}
	}
	if !dbTableExists(mainDb, "pubkeys") {
		_, err = mainDb.Exec(pubKeysTableCreate)
		if err != nil {
			log.Panic(err)
		}
	}
	if !dbTableExists(mainDb, "config") {
		_, err = mainDb.Exec(configTableCreate)
		if err != nil {
			log.Panic(err)
		}
	}
	if !dbTableExists(mainDb, "peers") {
		_, err = mainDb.Exec(peersTableCreate)
		if err != nil {
			log.Panic(err)
		}
		for peer := range bootstrapPeers {
			_, err = mainDb.Exec("INSERT INTO peers(address, time_added, permanent) VALUES (?, ?, ?)", peer, getNowUTC(), true)
			if err != nil {
				log.Panic(err)
			}
		}
	}

	dbFileName = fmt.Sprintf("%s/%s", cfg.DataDir, privateDbFilename)
	_, err = os.Stat(dbFileName)
	privateDbExists := err == nil
	privateDb, err = sql.Open("sqlite3", dbFileName)
	if err != nil {
		log.Fatal(err)
	}
	if !privateDbExists {
		// Create tables
		_, err = privateDb.Exec(privateTableCreate)
		if err != nil {
			log.Fatal(err)
		}
		os.Chmod(dbFileName, 0600)
	}
}

// Just opens the given file as a SQLite database
func dbOpen(fileName string, readOnly bool) (*sql.DB, error) {
	if !readOnly {
		return sql.Open("sqlite3", fileName)
	}
	return sql.Open("sqlite3", "file:"+fileName+"?mode=ro")
}

// Counts the number of private keys in the system databases
func dbNumPrivateKeys() int {
	assertSysDbOpen()
	var count int
	err := privateDb.QueryRow("SELECT COUNT(*) FROM privkeys").Scan(&count)
	if err != nil {
		log.Fatal(err)
	}
	return count
}

// Checks to see if a table exists in the given database
func dbTableExists(db *sql.DB, name string) bool {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count)
	if err != nil {
		log.Panicln(err)
	}
	return count > 0
}

// Panics if the system databases are not open
func assertSysDbOpen() {
	if mainDb == nil || privateDb == nil {
		log.Panic("Databases are not open")
	}
}

// Checks if a public key is present in the system databases
func dbPublicKeyExists(hash string) bool {
	var count int
	if err := mainDb.QueryRow("SELECT COUNT(*) FROM pubkeys WHERE pubkey_hash=?", hash).Scan(&count); err != nil {
		log.Panicln(err)
	}
	return count > 0
}

// Writes a public key to the system databases
func dbWritePublicKey(pubkey []byte, hash string, blockHeight int) {
	_, err := mainDb.Exec("INSERT INTO pubkeys(pubkey_hash, pubkey, state, time_added, block_height) VALUES (?, ?, ?, ?, ?)",
		hash, hex.EncodeToString(pubkey), "A", time.Now().Unix(), blockHeight)
	if err != nil {
		log.Panic(err)
	}
}

// Marks a public key as revoked.
func dbRevokePublicKey(hash string) {
	_, err := mainDb.Exec("UPDATE pubkeys SET time_revoked=? WHERE pubkey_hash=?", getNowUTC(), hash)
	if err != nil {
		log.Panic(err)
	}
}

// Writes the given private key byte blob to the system databases
func dbWritePrivateKey(privkey []byte, hash string) {
	_, err := privateDb.Exec("INSERT INTO privkeys(pubkey_hash, privkey, time_added) VALUES (?, ?, ?)", hash, hex.EncodeToString(privkey), time.Now().Unix())
	if err != nil {
		log.Panic(err)
	}
}

// Returns a list of public keys corresponding to private keys in the system databases
func dbGetMyPublicKeys() []string {
	var result []string
	rows, err := privateDb.Query("SELECT pubkey_hash FROM privkeys")
	if err != nil {
		log.Panic(err)
	}
	for rows.Next() {
		var pubkeyHash string
		err := rows.Scan(&pubkeyHash)
		if err != nil {
			log.Panic(err)
		}
		result = append(result, pubkeyHash)
	}
	return result
}

// Returns the current blockchain height
func dbGetBlockchainHeight() int {
	assertSysDbOpen()
	var height int
	err := mainDb.QueryRow("SELECT COALESCE(MAX(height), -1) FROM blockchain").Scan(&height)
	if err != nil {
		log.Panic(err)
	}
	return height
}

// Returns a map of heights and hashes for the requested range of block heights
func dbGetHeightHashes(minHeight, maxHeight int) map[int]string {
	rows, err := mainDb.Query("SELECT height, hash FROM blockchain WHERE height BETWEEN ? AND ? ORDER BY height", minHeight, maxHeight)
	if err != nil {
		log.Panic(err)
	}
	defer rows.Close()
	hh := make(map[int]string)
	for rows.Next() {
		var height int
		var hash string
		if err = rows.Scan(&height, &hash); err != nil {
			log.Panic(err)
		}
		hh[height] = hash
	}
	return hh
}

// Returns a random private key from the system databases
func dbGetAPrivateKey() ([]byte, string, error) {
	var publicKeyHash string
	var privateKey string
	err := privateDb.QueryRow("SELECT pubkey_hash, privkey FROM privkeys LIMIT 1").Scan(&publicKeyHash, &privateKey)
	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}
	if err == sql.ErrNoRows {
		return nil, "", err
	}
	privateKeyBytes, err := hex.DecodeString(privateKey)
	if err != nil {
		log.Println(err)
		return nil, "", err
	}
	return privateKeyBytes, publicKeyHash, nil
}

// Returns the public key corresponding to the given public key hash, by reading it from the system databases.
func dbGetPublicKey(publicKeyHash string) (*DbPubKey, error) {
	var dbpk DbPubKey
	var publicKeyHexString string
	var timeAdded int
	var timeRevoked int
	var metadata string
	err := mainDb.QueryRow("SELECT pubkey_hash, pubkey, state, time_added, COALESCE(time_revoked, -1), COALESCE(metadata, ''), block_height FROM pubkeys WHERE pubkey_hash=?", publicKeyHash).Scan(
		&dbpk.publicKeyHash, &publicKeyHexString, &dbpk.state, &timeAdded, &timeRevoked, &metadata, &dbpk.addBlockHeight)
	if err != nil && err != sql.ErrNoRows {
		log.Panicln(err)
	}
	if err == sql.ErrNoRows {
		return nil, err
	}
	dbpk.publicKeyBytes, err = hex.DecodeString(publicKeyHexString)
	if err != nil {
		return nil, err
	}
	dbpk.timeAdded = unixTimeStampToUTCTime(timeAdded)
	if err != nil {
		return nil, err
	}
	if timeRevoked != -1 {
		dbpk.timeRevoked = unixTimeStampToUTCTime(timeRevoked)
		if err != nil {
			log.Println("Public key timeRevoked parsing failed for", publicKeyHash)
			return nil, err
		}
		dbpk.isRevoked = true
	} else {
		dbpk.isRevoked = false
	}
	if metadata != "" {
		err = json.Unmarshal([]byte(metadata), &dbpk.metadata)
		if err != nil {
			log.Println("Public key metadata unmarshall failed for", publicKeyHash)
			return nil, err
		}
	}
	return &dbpk, nil
}

// Returns a block indexed by the given height.
func dbGetBlockByHeight(height int) (*DbBlockchainBlock, error) {
	var dbb DbBlockchainBlock
	var hashSignatureHex string
	var prevHashSignatureHex string
	var timeAccepted int
	err := mainDb.QueryRow("SELECT hash, height, prev_hash, sigkey_hash, hash_signature, prev_hash_signature, time_accepted, version FROM blockchain WHERE height=?", height).Scan(
		&dbb.Hash, &dbb.Height, &dbb.PreviousBlockHash, &dbb.SignaturePublicKeyHash, &hashSignatureHex, &prevHashSignatureHex, &timeAccepted, &dbb.Version)
	if err != nil && err != sql.ErrNoRows {
		log.Panicln(err)
	}
	if err == sql.ErrNoRows {
		return nil, err
	}
	dbb.PreviousBlockHashSignature, err = hex.DecodeString(prevHashSignatureHex)
	if err != nil {
		return nil, err
	}
	dbb.HashSignature, err = hex.DecodeString(hashSignatureHex)
	if err != nil {
		return nil, err
	}
	dbb.TimeAccepted = unixTimeStampToUTCTime(timeAccepted)
	if err != nil {
		return nil, err
	}
	return &dbb, nil
}

// Returns a block of the given hash
func dbGetBlock(hash string) (*DbBlockchainBlock, error) {
	var dbb DbBlockchainBlock
	var hashSignatureHex string
	var prevHashSignatureHex string
	var timeAccepted int
	err := mainDb.QueryRow("SELECT hash, height, prev_hash, sigkey_hash, hash_signature, prev_hash_signature, time_accepted, version FROM blockchain WHERE hash=?", hash).Scan(
		&dbb.Hash, &dbb.Height, &dbb.PreviousBlockHash, &dbb.SignaturePublicKeyHash, &hashSignatureHex, &prevHashSignatureHex, &timeAccepted, &dbb.Version)
	if err != nil && err != sql.ErrNoRows {
		log.Panicln(err)
	}
	if err == sql.ErrNoRows {
		return nil, err
	}
	dbb.PreviousBlockHashSignature, err = hex.DecodeString(prevHashSignatureHex)
	if err != nil {
		return nil, err
	}
	dbb.HashSignature, err = hex.DecodeString(hashSignatureHex)
	if err != nil {
		return nil, err
	}
	dbb.TimeAccepted = unixTimeStampToUTCTime(timeAccepted)
	if err != nil {
		return nil, err
	}
	return &dbb, nil
}

// Tests if a block with the given hash exists in the db
func dbBlockHashExists(hash string) bool {
	var count int
	err := mainDb.QueryRow("SELECT COUNT(*) FROM blockchain WHERE hash=?", hash).Scan(&count)
	if err != nil {
		log.Panic(err)
	}
	return count > 0
}

// Tests if the block with the given height exists in the db
func dbBlockHeightExists(h int) bool {
	var count int
	err := mainDb.QueryRow("SELECT COUNT(*) FROM blockchain WHERE height=?", h).Scan(&count)
	if err != nil {
		log.Panic(err)
	}
	return count > 0
}

// Inserts a block record into the main database, without validation
func dbInsertBlock(dbb *DbBlockchainBlock) error {
	_, err := mainDb.Exec("INSERT INTO blockchain (hash, height, prev_hash, sigkey_hash, hash_signature, prev_hash_signature, time_accepted, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		dbb.Hash, dbb.Height, dbb.PreviousBlockHash, dbb.SignaturePublicKeyHash, hex.EncodeToString(dbb.HashSignature), hex.EncodeToString(dbb.PreviousBlockHashSignature),
		dbb.TimeAccepted.UTC().Unix(), dbb.Version)
	return err
}

// Gets a list of saved p2p peer addresses
func dbGetSavedPeers() peerStringMap {
	result := peerStringMap{}
	rows, err := mainDb.Query("SELECT address, time_added FROM peers")
	if err != nil {
		log.Panic(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tmInt int
		var address string
		if err = rows.Scan(&address, &tmInt); err != nil {
			log.Println(err)
			continue
		}
		result[address] = unixTimeStampToUTCTime(tmInt)
	}
	return result
}

// Saves a p2p peer address to the db
func dbSavePeer(address string) {
	_, err := mainDb.Exec("INSERT OR REPLACE INTO peers(address, time_added) VALUES (?, ?)", address, getNowUTC())
	if err != nil {
		log.Panic(err)
	}
}
