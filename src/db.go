package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const mainDbFileName = "daisy.db"
const privateDbFilename = "private.db"

// DbBlockchainBlock is the convenience structure holding information from the blockchain table
type DbBlockchainBlock struct {
	Hash                   string
	Height                 int
	PreviousBlockHash      string
	SignaturePublicKeyHash string
	Signature              []byte
	TimeAccepted           time.Time
}

// Note: all db times are Unix timestamps in the UTC zone

const blockchainTableCreate = `
CREATE TABLE blockchain (
	hash			VARCHAR NOT NULL PRIMARY KEY,
	height			INTEGER NOT NULL UNIQUE,
	prev_hash		VARCHAR NOT NULL,
	sigkey_hash		VARCHAR NOT NULL, -- public key hash
	signature		VARCHAR NOT NULL,
	time_accepted	INTEGER NOT NULL
);
CREATE INDEX blockchain_sigkey_hash ON blockchain(sigkey_hash);
`

const myKeysTableCreate = `
CREATE TABLE pubkeys (
	pubkey_hash		VARCHAR NOT NULL PRIMARY KEY,
	pubkey			VARCHAR NOT NULL,
	state			CHAR NOT NULL,
	time_added		INTEGER NOT NULL,
	time_revoked	INTEGER,
	metadata		VARCHAR -- JSON
);
`

// DbPubKey is the convenience structure holding information from the pubkeys table
type DbPubKey struct {
	publicKeyHash  string
	publicKeyBytes []byte
	state          string
	timeAdded      time.Time
	isRevoked      bool
	timeRevoked    time.Time
	metadata       map[string]string
}

const privateTableCreate = `
CREATE TABLE privkeys (
	pubkey_hash		VARCHAR NOT NULL PRIMARY KEY,
	privkey			VARCHAR NOT NULL,
	time_added		INTEGER NOT NULL
);
`

var db *sql.DB
var privateDb *sql.DB

func dbInit() {
	dbFileName := fmt.Sprintf("%s/%s", cfg.DataDir, mainDbFileName)
	_, err := os.Stat(dbFileName)
	mainDbFileExists := err == nil
	db, err = sql.Open("sqlite3", dbFileName)
	if err != nil {
		log.Fatal(err)
	}
	if !mainDbFileExists {
		// Create system tables
		_, err = db.Exec(blockchainTableCreate)
		if err != nil {
			log.Fatal(err)
		}
		_, err = db.Exec(myKeysTableCreate)
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

func assertSysDbOpen() {
	if db == nil || privateDb == nil {
		log.Fatal("Databases are not open")
	}
}

func dbWritePublicKey(pubkey []byte, hash string) {
	_, err := db.Exec("INSERT INTO pubkeys(pubkey_hash, pubkey, state, time_added) VALUES (?, ?, ?, ?)", hash, hex.EncodeToString(pubkey), "A", time.Now().Unix())
	if err != nil {
		log.Fatal(err)
	}
}

func dbWritePrivateKey(privkey []byte, hash string) {
	_, err := privateDb.Exec("INSERT INTO privkeys(pubkey_hash, privkey, time_added) VALUES (?, ?, ?)", hash, hex.EncodeToString(privkey), time.Now().Unix())
	if err != nil {
		log.Fatal(err)
	}
}

func dbGetBlockchainHeight() int {
	assertSysDbOpen()
	var height int
	err := db.QueryRow("SELECT COALESCE(MAX(height), -1) FROM blockchain").Scan(&height)
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
	err := db.QueryRow("SELECT pubkey_hash, pubkey, state, time_added, COALESCE(time_revoked, -1), COALESCE(metadata, '') FROM pubkeys WHERE pubkey_hash=?", publicKeyHash).Scan(
		&dbpk.publicKeyHash, &publicKeyHexString, &dbpk.state, dbpk.timeAdded, dbpk.timeRevoked, metadata)
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
	dbpk.timeAdded, err = time.ParseInLocation(time.UnixDate, strconv.Itoa(timeAdded), time.UTC)
	if err != nil {
		return nil, err
	}
	if timeRevoked != -1 {
		dbpk.timeRevoked, err = time.ParseInLocation(time.UnixDate, strconv.Itoa(timeRevoked), time.UTC)
		if err != nil {
			return nil, err
		}
		dbpk.isRevoked = true
	} else {
		dbpk.isRevoked = false
	}
	if metadata != "" {
		err := json.Unmarshal([]byte(metadata), &dbpk.metadata)
		if err != nil {
			return nil, err
		}
	}
	return &dbpk, err
}

func dbGetBlockByHeight(height int) (*DbBlockchainBlock, error) {
	var dbb DbBlockchainBlock
	var signatureHex string
	var timeAccepted int
	err := db.QueryRow("SELECT hash, height, prev_hash, sigkey_hash, signature, time_accepted FROM blockchain WHERE height=?", height).Scan(
		&dbb.Hash, &dbb.Height, &dbb.PreviousBlockHash, &dbb.SignaturePublicKeyHash, &signatureHex, &timeAccepted)
	if err != nil && err != sql.ErrNoRows {
		log.Panicln(err)
	}
	if err == sql.ErrNoRows {
		return nil, err
	}
	dbb.Signature, err = hex.DecodeString(signatureHex)
	if err != nil {
		return nil, err
	}
	dbb.TimeAccepted, err = time.ParseInLocation(time.UnixDate, strconv.Itoa(timeAccepted), time.UTC)
	if err != nil {
		return nil, err
	}
	return &dbb, nil
}

// Inserts a block record into the main database, without validation
func dbInsertBlock(dbb DbBlockchainBlock) error {
	_, err := db.Exec("INSERT INTO blockchain (hash, height, prev_hash, sigkey_hash, signature, time_accepted) VALUES (?, ?, ?, ?, ?, ?)",
		dbb.Hash, dbb.Height, dbb.PreviousBlockHash, dbb.SignaturePublicKeyHash, hex.EncodeToString(dbb.Signature), dbb.TimeAccepted.Unix())
	return err
}
