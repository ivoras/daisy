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

const myKeysTableCreate = `
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

/*********************************************************************************************************************
 * Structures and SQL schema for the blockchain block tables.
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

func dbInit() {
	dbFileName := fmt.Sprintf("%s/%s", cfg.DataDir, mainDbFileName)
	_, err := os.Stat(dbFileName)
	mainDbFileExists := err == nil
	mainDb, err = sql.Open("sqlite3", dbFileName)
	if err != nil {
		log.Fatal(err)
	}
	if !mainDbFileExists {
		// Create system tables
		_, err = mainDb.Exec(blockchainTableCreate)
		if err != nil {
			log.Fatal(err)
		}
		_, err = mainDb.Exec(myKeysTableCreate)
		if err != nil {
			log.Fatal(err)
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

func dbOpen(fileName string, readOnly bool) (*sql.DB, error) {
	if !readOnly {
		return sql.Open("sqlite3", fileName)
	}
	return sql.Open("sqlite3", "file:"+fileName+"?mode=ro")
}

func dbNumPrivateKeys() int {
	assertSysDbOpen()
	var count int
	err := privateDb.QueryRow("SELECT COUNT(*) FROM privkeys").Scan(&count)
	if err != nil {
		log.Fatal(err)
	}
	return count
}

func dbTableExists(db *sql.DB, name string) bool {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count)
	if err != nil {
		log.Panicln(err)
	}
	return count > 0
}

func assertSysDbOpen() {
	if mainDb == nil || privateDb == nil {
		log.Fatal("Databases are not open")
	}
}

func dbPublicKeyExists(hash string) bool {
	var count int
	if err := mainDb.QueryRow("SELECT COUNT(*) FROM pubkeys WHERE pubkey_hash=?", hash).Scan(&count); err != nil {
		log.Panicln(err)
	}
	return count > 0
}

func dbWritePublicKey(pubkey []byte, hash string, blockHeight int) {
	_, err := mainDb.Exec("INSERT INTO pubkeys(pubkey_hash, pubkey, state, time_added, block_height) VALUES (?, ?, ?, ?, ?)",
		hash, hex.EncodeToString(pubkey), "A", time.Now().Unix(), blockHeight)
	if err != nil {
		log.Panicln(err)
	}
}

func dbWritePrivateKey(privkey []byte, hash string) {
	_, err := privateDb.Exec("INSERT INTO privkeys(pubkey_hash, privkey, time_added) VALUES (?, ?, ?)", hash, hex.EncodeToString(privkey), time.Now().Unix())
	if err != nil {
		log.Panicln(err)
	}
}

func dbGetBlockchainHeight() int {
	assertSysDbOpen()
	var height int
	err := mainDb.QueryRow("SELECT COALESCE(MAX(height), -1) FROM blockchain").Scan(&height)
	if err != nil {
		log.Fatal(err)
	}
	return height
}

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

// Inserts a block record into the main database, without validation
func dbInsertBlock(dbb *DbBlockchainBlock) error {
	_, err := mainDb.Exec("INSERT INTO blockchain (hash, height, prev_hash, sigkey_hash, hash_signature, prev_hash_signature, time_accepted, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		dbb.Hash, dbb.Height, dbb.PreviousBlockHash, dbb.SignaturePublicKeyHash, hex.EncodeToString(dbb.HashSignature), hex.EncodeToString(dbb.PreviousBlockHashSignature),
		dbb.TimeAccepted.UTC().Unix(), dbb.Version)
	return err
}
