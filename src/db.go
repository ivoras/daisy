package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	"encoding/hex"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const mainDbFileName = "daisy.db"
const privateDbFilename = "private.db"

const blockchainTableCreate = `
CREATE TABLE blockchain (
	hash			VARCHAR NOT NULL PRIMARY KEY,
	height			INTEGER NOT NULL UNIQUE,
	sigkey_hash		VARCHAR NOT NULL,
	signature		VARCHAR NOT NULL
);
CREATE INDEX blockchain_sigkey_hash ON blockchain(sigkey_hash);
`

const myKeysTableCreate = `
CREATE TABLE pubkeys (
	pubkey_hash		VARCHAR NOT NULL PRIMARY KEY,
	pubkey			VARCHAR NOT NULL,
	state			CHAR NOT NULL,
	time_added		INTEGER NOT NULL,
	time_revoked	INTEGER
);
`

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

func dbNumPrivateKeys() int {
	assertDbOpen()
	var count int
	err := privateDb.QueryRow("SELECT COUNT(*) FROM privkeys").Scan(&count)
	if err != nil {
		log.Fatal(err)
	}
	return count
}

func assertDbOpen() {
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
