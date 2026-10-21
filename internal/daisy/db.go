package daisy

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ivoras/daisy/internal/util"
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

	// TotalScore is the sum of the scores of this block and all the blocks before it, see blockScore().
	// The branch with the highest total score is the best chain.
	TotalScore int
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
	version				INTEGER NOT NULL,
	total_score			INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX blockchain_sigkey_hash ON blockchain(sigkey_hash);
`

// Valid blocks which are not (or not any more) on the best chain, i.e. the other branches of the block tree.
// Same columns as the blockchain table, except that there can be several blocks at one height.
const sideBlocksTableCreate = `
CREATE TABLE sideblocks (
	height				INTEGER NOT NULL,
	sigkey_hash			VARCHAR NOT NULL,
	hash				VARCHAR NOT NULL PRIMARY KEY,
	hash_signature		VARCHAR NOT NULL,
	prev_hash			VARCHAR NOT NULL,
	prev_hash_signature	VARCHAR NOT NULL,
	time_accepted		INTEGER NOT NULL,
	version				INTEGER NOT NULL,
	total_score			INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sideblocks_prev_hash ON sideblocks(prev_hash);
`

// The version of the layout and contents of the system tables (see dbMigrate and reconcileChainState)
const currentSchemaVersion = 2

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
	block_height	INTEGER NOT NULL, -- the block which added the key. Negative: the key is only known locally, it isn't on the blockchain
	metadata		VARCHAR, -- JSON
	revoke_block_height	INTEGER -- the block which revoked the key
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
	permanent		BOOLEAN NOT NULL DEFAULT 0, -- the bootstrap peers
	failures		INTEGER NOT NULL DEFAULT 0, -- failed connection attempts since the last success
	tried			BOOLEAN NOT NULL DEFAULT 0, -- the peer has said hello (see addrbook.go)
	net_group		VARCHAR NOT NULL DEFAULT '', -- the network group of the address, see networkGroup()
	source_group	VARCHAR NOT NULL DEFAULT '', -- who told us about the address
	last_try		INTEGER NOT NULL DEFAULT 0,
	last_success	INTEGER NOT NULL DEFAULT 0
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
	dbFileName := filepath.Join(cfg.DataDir, mainDbFileName)
	_, err := os.Stat(dbFileName)
	mainDbFileExists := err == nil
	mainDb, err = sql.Open("sqlite3", dbFileName)
	if err != nil {
		log.Fatal(err)
	}
	freshSchema, newPeersTable := false, false
	if !mainDbFileExists || !dbTableExists(mainDb, "blockchain") {
		// Create system tables
		_, err = mainDb.Exec(blockchainTableCreate)
		if err != nil {
			log.Panic(err)
		}
		freshSchema = true
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
		newPeersTable = true
	}

	dbMigrate(freshSchema)
	if newPeersTable {
		for peer := range bootstrapPeers {
			abAddBootstrap(peer)
		}
	}

	dbFileName = filepath.Join(cfg.DataDir, privateDbFilename)
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
		err = os.Chmod(dbFileName, 0600)
		if err != nil {
			log.Fatalf("chmod: %v", err)
		}
	}
}

// Just opens the given file as a SQLite database. Read-only is how block files are opened, and they may come
// from anywhere, so that is the hardened way of opening them (see blockdb.go).
func dbOpen(fileName string, readOnly bool) (*sql.DB, error) {
	if !readOnly {
		return sql.Open("sqlite3", fileName)
	}
	return sql.Open(blockDriverName, blockDSN(fileName))
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

// Writes the given private key byte blob to the system databases
func dbWritePrivateKey(privkey []byte, hash string) {
	_, err := privateDb.Exec("INSERT INTO privkeys(pubkey_hash, privkey, time_added) VALUES (?, ?, ?)", hash, hex.EncodeToString(privkey), time.Now().Unix())
	if err != nil {
		log.Panic(err)
	}
}

// Returns a list of public keys hashes corresponding to private keys in the system databases
func dbGetMyPublicKeyHashes() []string {
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
	defer func() {
		err = rows.Close()
		if err != nil {
			log.Fatalf("dbGetHeightHashes rows.Close: %v", err)
		}
	}()
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
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatal(err)
	}
	if errors.Is(err, sql.ErrNoRows) {
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
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Panicln(err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	dbpk.publicKeyBytes, err = hex.DecodeString(publicKeyHexString)
	if err != nil {
		return nil, err
	}
	dbpk.timeAdded = util.UnixTimeStampToUTCTime(timeAdded)
	if err != nil {
		return nil, err
	}
	if timeRevoked != -1 {
		dbpk.timeRevoked = util.UnixTimeStampToUTCTime(timeRevoked)
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

// Returns a block hash by its height
func dbGetBlockHashByHeight(height int) string {
	var hash string
	err := mainDb.QueryRow("SELECT hash FROM blockchain WHERE height=?", height).Scan(&hash)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Panicln(err)
		}
	}
	return hash
}

// The columns of the blockchain and sideblocks tables, in the order scanBlock() expects them
const blockColumns = "hash, height, prev_hash, sigkey_hash, hash_signature, prev_hash_signature, time_accepted, version, total_score"

// Reads a block record from the result of selecting blockColumns
func scanBlock(row *sql.Row) (*DbBlockchainBlock, error) {
	var dbb DbBlockchainBlock
	var hashSignatureHex, prevHashSignatureHex string
	var timeAccepted int
	err := row.Scan(&dbb.Hash, &dbb.Height, &dbb.PreviousBlockHash, &dbb.SignaturePublicKeyHash, &hashSignatureHex,
		&prevHashSignatureHex, &timeAccepted, &dbb.Version, &dbb.TotalScore)
	if err != nil {
		return nil, err // Including sql.ErrNoRows
	}
	if dbb.PreviousBlockHashSignature, err = hex.DecodeString(prevHashSignatureHex); err != nil {
		return nil, err
	}
	if dbb.HashSignature, err = hex.DecodeString(hashSignatureHex); err != nil {
		return nil, err
	}
	dbb.TimeAccepted = util.UnixTimeStampToUTCTime(timeAccepted)
	return &dbb, nil
}

// Returns a block of the best chain, indexed by the given height.
func dbGetBlockByHeight(height int) (*DbBlockchainBlock, error) {
	return scanBlock(mainDb.QueryRow("SELECT "+blockColumns+" FROM blockchain WHERE height=?", height))
}

// Returns a block of the best chain, of the given hash
func dbGetBlock(hash string) (*DbBlockchainBlock, error) {
	return scanBlock(mainDb.QueryRow("SELECT "+blockColumns+" FROM blockchain WHERE hash=?", hash))
}

// Returns a block which is not on the best chain, of the given hash
func dbGetSideBlock(hash string) (*DbBlockchainBlock, error) {
	return scanBlock(mainDb.QueryRow("SELECT "+blockColumns+" FROM sideblocks WHERE hash=?", hash))
}

// Returns the block of the given hash, wherever it is in the block tree.
// onBestChain tells if it's part of the best chain.
func dbGetAnyBlock(hash string) (dbb *DbBlockchainBlock, onBestChain bool, err error) {
	if dbb, err = dbGetBlock(hash); err == nil {
		return dbb, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	dbb, err = dbGetSideBlock(hash)
	return dbb, false, err
}

// Returns the last block of the best chain
func dbGetTipBlock() (*DbBlockchainBlock, error) {
	return dbGetBlockByHeight(dbGetBlockchainHeight())
}

// Tests if a block with the given hash is known, either on the best chain or on another branch
func dbBlockHashKnown(hash string) bool {
	_, _, err := dbGetAnyBlock(hash)
	return err == nil
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

// Something we can run SQL statements on: the database or a transaction
type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Inserts a block record into the main database, without validation
func dbInsertBlock(dbb *DbBlockchainBlock) error {
	return dbInsertBlockInto(mainDb, "blockchain", dbb)
}

// Inserts a block record into the given table (blockchain or sideblocks), without validation
func dbInsertBlockInto(ex dbExecer, table string, dbb *DbBlockchainBlock) error {
	_, err := ex.Exec("INSERT INTO "+table+" ("+blockColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		dbb.Hash, dbb.Height, dbb.PreviousBlockHash, dbb.SignaturePublicKeyHash, hex.EncodeToString(dbb.HashSignature),
		hex.EncodeToString(dbb.PreviousBlockHashSignature), dbb.TimeAccepted.UTC().Unix(), dbb.Version, dbb.TotalScore)
	return err
}

// Reads a value from the config table. Returns "" if there isn't one.
func dbGetConfig(key string) string {
	var value string
	err := mainDb.QueryRow("SELECT value FROM config WHERE key=?", key).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Panic(err)
	}
	return value
}

// Writes a value to the config table
func dbSetConfig(key, value string) {
	if _, err := mainDb.Exec("INSERT OR REPLACE INTO config(key, value) VALUES (?, ?)", key, value); err != nil {
		log.Panic(err)
	}
}

func dbColumnExists(db *sql.DB, table, column string) bool {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", table, column).Scan(&count)
	if err != nil {
		log.Panicln(err)
	}
	return count > 0
}

// Brings the system tables of an older version up to the current layout. This only adds the empty columns and
// tables: filling them with the data derived from the blocks is done by reconcileChainState() when the blocks
// are available.
func dbMigrate(freshSchema bool) {
	add := func(table, column, definition string) {
		if dbColumnExists(mainDb, table, column) {
			return
		}
		log.Println("Upgrading the database: adding", table+"."+column)
		if _, err := mainDb.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition); err != nil {
			log.Panic(err)
		}
	}
	add("blockchain", "total_score", "INTEGER NOT NULL DEFAULT 0")
	add("pubkeys", "revoke_block_height", "INTEGER")
	add("peers", "failures", "INTEGER NOT NULL DEFAULT 0")
	if !dbColumnExists(mainDb, "peers", "tried") {
		add("peers", "tried", "BOOLEAN NOT NULL DEFAULT 0")
		add("peers", "net_group", "VARCHAR NOT NULL DEFAULT ''")
		add("peers", "source_group", "VARCHAR NOT NULL DEFAULT ''")
		add("peers", "last_try", "INTEGER NOT NULL DEFAULT 0")
		add("peers", "last_success", "INTEGER NOT NULL DEFAULT 0")
		dbMigrateAddressBook()
	}
	if !dbTableExists(mainDb, "sideblocks") {
		if _, err := mainDb.Exec(sideBlocksTableCreate); err != nil {
			log.Panic(err)
		}
	}
	if freshSchema {
		dbSetConfig("schema_version", strconv.Itoa(currentSchemaVersion))
	}
}

// Loads the signatory keys known to the blockchain (as of its tip) into memory
func dbLoadKeySet() (*keySet, error) {
	rows, err := mainDb.Query("SELECT pubkey_hash, pubkey, block_height, COALESCE(revoke_block_height, -1) FROM pubkeys WHERE block_height >= 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ks := newKeySet()
	for rows.Next() {
		var ki keyInfo
		var pubKeyHex string
		if err = rows.Scan(&ki.hash, &pubKeyHex, &ki.addHeight, &ki.revokeHeight); err != nil {
			return nil, err
		}
		if ki.publicKey, err = hex.DecodeString(pubKeyHex); err != nil {
			return nil, err
		}
		ks.keys[ki.hash] = &ki
	}
	return ks, rows.Err()
}

// Records the key operations of a block, which has become part of the best chain, in the pubkeys table
func dbApplyKeyOps(ex dbExecer, height int, ops map[string][]BlockKeyOp) error {
	for keyHash, keyOps := range ops {
		switch keyOps[0].op {
		case "A":
			// The key may already be in the table, with a negative height, if it's one of this node's own keys
			_, err := ex.Exec(`INSERT INTO pubkeys(pubkey_hash, pubkey, state, time_added, block_height) VALUES (?, ?, 'A', ?, ?)
				ON CONFLICT(pubkey_hash) DO UPDATE SET block_height=excluded.block_height, time_revoked=NULL, revoke_block_height=NULL`,
				keyHash, hex.EncodeToString(keyOps[0].publicKeyBytes), time.Now().Unix(), height)
			if err != nil {
				return err
			}
		case "R":
			if _, err := ex.Exec("UPDATE pubkeys SET time_revoked=?, revoke_block_height=? WHERE pubkey_hash=?", time.Now().Unix(), height, keyHash); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid key op %q", keyOps[0].op)
		}
	}
	return nil
}

// Forgets the key operations of all the blocks above the given height
func dbRewindKeys(ex dbExecer, height int) error {
	if _, err := ex.Exec("UPDATE pubkeys SET time_revoked=NULL, revoke_block_height=NULL WHERE revoke_block_height > ?", height); err != nil {
		return err
	}
	// Keys which were added by the discarded blocks stay in the table, but are not on the blockchain any more
	_, err := ex.Exec("UPDATE pubkeys SET block_height=-1, time_revoked=NULL, revoke_block_height=NULL WHERE block_height > ?", height)
	return err
}

func dbClearSavedPeers() error {
	_, err := mainDb.Exec("DELETE FROM peers")
	return err
}

// Gets a list of saved p2p peer addresses
func dbGetSavedPeers() peerStringMap {
	result := peerStringMap{}
	rows, err := mainDb.Query("SELECT address, time_added FROM peers")
	if err != nil {
		log.Panic(err)
	}
	defer func() {
		err = rows.Close()
		if err != nil {
			log.Fatalf("dbGetSavedPeers rows.Close: %v", err)
		}
	}()
	for rows.Next() {
		var tmInt int
		var address string
		if err = rows.Scan(&address, &tmInt); err != nil {
			log.Println(err)
			continue
		}
		result[address] = util.UnixTimeStampToUTCTime(tmInt)
	}
	return result
}

// Fills in the address book columns for the peers saved by the versions before them: the bootstrap peers are tried
// ones, the rest has not been proven to be a peer of this chain.
func dbMigrateAddressBook() {
	rows, err := mainDb.Query("SELECT address FROM peers")
	if err != nil {
		log.Panic(err)
	}
	var addresses []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			addresses = append(addresses, a)
		}
	}
	rows.Close()
	for _, a := range addresses {
		group := ""
		if host, ok := splitPeerAddress(a); ok {
			group = networkGroup(host)
		}
		if _, err := mainDb.Exec("UPDATE peers SET net_group=?, source_group='migration', tried=permanent WHERE address=?", group, a); err != nil {
			log.Panic(err)
		}
	}
}
