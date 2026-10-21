package daisy

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ivoras/daisy/internal/assets"
	"github.com/ivoras/daisy/internal/dcrypto"
	"github.com/ivoras/daisy/internal/util"
)

// CurrentBlockVersion is the version of the block metadata
const CurrentBlockVersion = 1

// GenesisBlockPreviousBlockHash is the hard-coded canonical stand-in hash of the non-existent previous block
const GenesisBlockPreviousBlockHash = "1000000000000000000000000000000000000000000000000000000000000001"

// ChainParams describe the genesis block and other blockchain properties
var defaultChainParams = ChainParams{
	ConsensusType:             ChainConsensusPoA,
	GenesisBlockHash:          "9a0ff19183d1525a36de803047de4b73eb72506be8c81296eb463476a5c2d9e2",
	GenesisBlockHashSignature: "30460221008b8b3b3cfee2493ef58f2f6a1f1768b564f4c9e9a341ad42912cbbcf5c3ec82f022100fbcdfd0258fa1a5b073d18f688c2fb3d8f9a7c59204c6777f2bbf1faeb1eb1ed",
	GenesisBlockTimestamp:     "2017-05-06T10:38:50+02:00",
}

var chainParams = defaultChainParams

const chainParamsBaseName = "chainparams.json"

// Blocks (SQLite databases) are stored as flat files in a directory
const blockchainSubdirectoryBaseName = "blocks"
const rawBlockFilenameFormat = "block_%08x.db"
const rawBlockDirnameFormat = "%04x"
const genesisBlockHeight = 0

var blockchainSubdirectory string

/*
 * Block metadata fields:
 *
 * PreviousBlockHash|1000000000000000000000000000000000000000000000000000000000000001
 * Creator|ivoras@gmail.com
 * PreviousBlockHashSignature|3046022100db037ae6cb3c6e37cbc8ec592ba7eed2e6d18e6a3caedc4e2e81581eb97acb67022100d46d8ed27b5d78a8509b1eb8549c9b6b8f1c0a134c0c7af23bb93ab8cc842e2d
 * CreatorPublicKey|1:a3c07ef6cbee246f231a61ff36bbcd8e8563723e3703eb345ecdd933d7709ae2
 * Version|1
 *
 * Of these, only the Creator field is optional. By default, for new blocks, it is taken
 * from the "BlockCreator" field in the pubkey metadata (if it exists).
 */

// Block is the working representation of a blockchain block
type Block struct {
	*DbBlockchainBlock
	db *sql.DB
}

// BlockKeyOp is the representation of a key op record from the blocks' _keys table.
type BlockKeyOp struct {
	op               string
	publicKeyHash    string
	publicKeyBytes   []byte
	signatureKeyHash string
	signature        []byte
	metadata         map[string]string
}

func ensureBlockchainSubdirectoryExists() {
	blockchainSubdirectory = filepath.Join(cfg.DataDir, blockchainSubdirectoryBaseName)
	if _, err := os.Stat(blockchainSubdirectory); err != nil {
		// Probably doesn't exist, create it
		log.Println("Creating directory", blockchainSubdirectory)
		err := os.Mkdir(blockchainSubdirectory, 0700)
		if err != nil {
			log.Fatalln(err)
		}
	}
}

// Initializes the blockchain: creates database entries and the genesis block file
func blockchainInit(createDefault bool) {
	ensureBlockchainSubdirectoryExists()
	if err := recoverReorganization(); err != nil {
		log.Fatalf("recoverReorganization: %v", err)
	}
	if err := reconcileChainState(); err != nil {
		log.Fatalf("reconcileChainState: %v", err)
	}
	// The blocks waiting for their predecessors are only kept in memory, so these files are leftovers
	if err := os.RemoveAll(orphanDirname()); err != nil {
		log.Println(err)
	}
	if dbGetBlockchainHeight() == -1 && createDefault {
		log.Println("Writing down the default Genesis block. Let there be light.")

		// This is basically testing the crypto code, no real purpose.
		keypair, publicKeyHash, err := cryptoGetAPrivateKey()
		if err != nil {
			log.Panicln(err)
		}
		signature, err := dcrypto.SignPublicKeyHash(keypair, publicKeyHash)
		if err != nil {
			log.Panicln(err)
		}
		if err = dcrypto.VerifyPublicKeyHashSignature(&keypair.PublicKey, publicKeyHash, signature); err != nil {
			log.Panicln(err)
		}

		/*
			// Sign the Genesis block's "previous block" hash
			genesisPrevBlockHash, err := hex.DecodeString(GenesisBlockPreviousBlockHash)
			if err != nil {
				log.Panicln(err)
			}
			signature, err = dcrypto.SignBytes(keypair, genesisPrevBlockHash)
			if err != nil {
				log.Panicln(err)
			}
			log.Println(GenesisBlockPreviousBlockHash)
			log.Println(hex.EncodeToString(signature))
		*/

		// Bring the genesis block into existence
		genesisBlock := assets.GenesisDB
		if util.HashBytesToHexString(genesisBlock) != chainParams.GenesisBlockHash {
			log.Panicln("Genesis block hash unexpected:", util.HashBytesToHexString(genesisBlock))
		}

		/*
			// Sign the Genesis block's hash
			genesisBlockHash, err := hex.DecodeString(GenesisBlockHash)
			if err != nil {
				log.Panicln(err)
			}
			signature, err = dcrypto.SignBytes(keypair, genesisBlockHash)
			if err != nil {
				log.Panicln(err)
			}
			log.Println(hex.EncodeToString(signature))
		*/

		if err := blockchainEnsureBlockDir(genesisBlockHeight); err != nil {
			log.Panicln(err)
		}
		genesisBlockFilename := blockchainGetFilename(genesisBlockHeight)
		err = os.WriteFile(genesisBlockFilename, genesisBlock, 0644)
		if err != nil {
			log.Panic(err)
		}
		b, err := OpenBlockFile(genesisBlockFilename)
		if err != nil {
			log.Panicln(err)
		}
		b.Height = 0
		b.TimeAccepted, err = time.Parse(time.RFC3339, chainParams.GenesisBlockTimestamp)
		if err != nil {
			log.Panicln("Error parsing genesis block timestamp", err)
		}
		b.HashSignature, err = hex.DecodeString(chainParams.GenesisBlockHashSignature)
		if err != nil {
			log.Panicln(err)
		}
		blockKeyOps, err := b.dbGetKeyOps()
		if err != nil {
			log.Panicln(err)
		}
		for _, keyOps := range blockKeyOps {
			for _, keyOp := range keyOps {
				if dbPublicKeyExists(keyOp.publicKeyHash) {
					continue
				}
				dbWritePublicKey(keyOp.publicKeyBytes, keyOp.publicKeyHash, 0)
			}
		}
		err = dbInsertBlock(b.DbBlockchainBlock)
		if err != nil {
			log.Panicln(err)
		}
	} else {
		// The chainparams file will only exist for non-default blockchains
		cpFilename := filepath.Join(cfg.DataDir, chainParamsBaseName)
		if util.FileExists(cpFilename) {
			log.Println("Loading custom blockchain params from", cpFilename)
			cpJSON, err := os.ReadFile(cpFilename)
			if err != nil {
				log.Fatal("Error reading chainparams file", cpFilename, err)
			}
			err = json.Unmarshal(cpJSON, &chainParams)
			if err != nil {
				log.Fatal("Error decoding chainparams file", cpFilename, err)
			}
			peers := dbGetSavedPeers()
			for _, peer := range chainParams.BootstrapPeers {
				_, ok := peers[peer]
				if !ok {
					dbSavePeer(peer)
				}
			}
		} else {
			log.Println("Using default blockchain params")
		}
		log.Println("P2P peers:", dbGetSavedPeers())
	}
	err := blockchainVerifyEverything()
	if err != nil {
		log.Fatalf("blockchainVerifyEverything: %v", err)
	}
}

// Verifies the entire best chain to see if there are errors. It replays the chain from the genesis block, so that every
// block is checked against the signatory keys as they were when it was signed.
func blockchainVerifyEverything() error {
	if cfg.faster {
		log.Println("Skipping blockchain consistency checks")
		return nil
	}
	log.Println("Verifying all the blocks (use --faster to skip)...")
	maxHeight := dbGetBlockchainHeight()
	keys := newKeySet()
	previousHash := GenesisBlockPreviousBlockHash
	totalScore := 0
	for height := 0; height <= maxHeight; height++ {
		if height > 0 && height%1000 == 0 {
			log.Println("Verifying block", height)
		}
		if err := blockchainEnsureBlockDir(height); err != nil {
			return err
		}
		blockFilename := blockchainGetFilename(height)
		fileHash, err := util.HashFileToHexString(blockFilename)
		if err != nil {
			return fmt.Errorf("block %d: %v", height, err)
		}
		dbb, err := dbGetBlockByHeight(height)
		if err != nil {
			return fmt.Errorf("block %d: %v", height, err)
		}
		if fileHash != dbb.Hash {
			return fmt.Errorf("block %d: file hash %s doesn't match db hash %s", height, fileHash, dbb.Hash)
		}
		if height == 0 && fileHash != chainParams.GenesisBlockHash {
			return fmt.Errorf("block %d: it's supposed to be the genesis block but its hash doesn't match %s",
				height, chainParams.GenesisBlockHash)
		}
		if dbb.PreviousBlockHash != previousHash {
			return fmt.Errorf("block %d: its previous block hash %s is not the hash of the block before it, %s",
				height, dbb.PreviousBlockHash, previousHash)
		}

		// The metadata within the block must be the same as the one in the index
		b, err := OpenBlockFile(blockFilename)
		if err != nil {
			return fmt.Errorf("block %d: cannot open block db file: %v", height, err)
		}
		if b.PreviousBlockHash != dbb.PreviousBlockHash || b.SignaturePublicKeyHash != dbb.SignaturePublicKeyHash ||
			b.Version != dbb.Version || !bytes.Equal(b.PreviousBlockHashSignature, dbb.PreviousBlockHashSignature) {
			b.Close()
			return fmt.Errorf("block %d: the metadata in the block file doesn't match the one in the database", height)
		}
		b.HashSignature = dbb.HashSignature

		var ops map[string][]BlockKeyOp
		if height == genesisBlockHeight {
			ops, err = verifyGenesisBlock(b, keys)
		} else {
			ops, err = validateBlock(b, height, keys)
		}
		b.Close()
		if err != nil {
			return fmt.Errorf("block %d: %v", height, err)
		}
		totalScore += blockScore(keys, height, dbb.SignaturePublicKeyHash)
		if dbb.TotalScore != totalScore {
			return fmt.Errorf("block %d: total score is %d, but it should be %d", height, dbb.TotalScore, totalScore)
		}
		keys.apply(height, ops)
		previousHash = dbb.Hash
	}

	// The table of keys is derived from the blocks, so it must agree with what the blocks say
	tableKeys, err := dbLoadKeySet()
	if err != nil {
		return err
	}
	if len(tableKeys.keys) != len(keys.keys) {
		return fmt.Errorf("the signatory keys in the database (%d) are not the ones from the blocks (%d)", len(tableKeys.keys), len(keys.keys))
	}
	for hash, ki := range keys.keys {
		tk, ok := tableKeys.keys[hash]
		if !ok || tk.addHeight != ki.addHeight || tk.revokeHeight != ki.revokeHeight {
			return fmt.Errorf("the key %s in the database is not the way the blocks say it is", hash)
		}
	}
	return nil
}

// verifyGenesisBlock checks the genesis block: the keys which it adds are the first signatories, and it is signed by one
// of them. The keys are added to the key set. Returns the key operations of the block.
func verifyGenesisBlock(b *Block, keys *keySet) (map[string][]BlockKeyOp, error) {
	ops, err := b.dbGetKeyOps()
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, errors.New("no key ops in the genesis block")
	}
	keys.apply(genesisBlockHeight, ops)
	signatory, ok := keys.get(b.SignaturePublicKeyHash)
	if !ok {
		return nil, fmt.Errorf("the genesis block is signed by %s, which it doesn't add", b.SignaturePublicKeyHash)
	}
	pub, err := dcrypto.DecodePublicKeyBytes(signatory.publicKey)
	if err != nil {
		return nil, err
	}
	if err = dcrypto.VerifyHexBytes(pub, b.Hash, b.HashSignature); err != nil {
		return nil, fmt.Errorf("block hash signature is invalid (%v)", err)
	}
	if err = dcrypto.VerifyHexBytes(pub, b.PreviousBlockHash, b.PreviousBlockHashSignature); err != nil {
		return nil, fmt.Errorf("previous block hash signature is invalid (%v)", err)
	}
	for key, keyOps := range ops {
		for _, kop := range keyOps {
			if kop.op != "A" {
				return nil, fmt.Errorf("the genesis block can only add keys, not %q (%s)", kop.op, key)
			}
			signer, ok := keys.get(kop.signatureKeyHash)
			if !ok {
				return nil, fmt.Errorf("key op for %s is signed by %s, which is not a genesis key", key, kop.signatureKeyHash)
			}
			signingKey, err := dcrypto.DecodePublicKeyBytes(signer.publicKey)
			if err != nil {
				return nil, err
			}
			if err = dcrypto.VerifyPublicKeyHashSignature(signingKey, key, kop.signature); err != nil {
				return nil, fmt.Errorf("key op signature invalid for signer %s: %v", kop.signatureKeyHash, err)
			}
		}
	}
	return ops, nil
}

// QuorumForHeight calculates the required key op quorum for the given block height
func QuorumForHeight(h int) int {
	if h < 149 {
		return 1
	}
	return int(math.Log(float64(h)) * 2)
}

// Returns the (sharded) directory which holds the block with the given height: 64k blocks per directory
func blockchainDirname(h int) string {
	return filepath.Join(blockchainSubdirectory, fmt.Sprintf(rawBlockDirnameFormat, h/65536))
}

// Formats the block height into a blockchain file (SQLite database) filename
func blockchainGetFilename(h int) string {
	return filepath.Join(blockchainDirname(h), fmt.Sprintf(rawBlockFilenameFormat, h))
}

func blockchainEnsureBlockDir(h int) error {
	dirName := blockchainDirname(h)
	return os.MkdirAll(dirName, 0755)
}

// OpenBlockByHeight opens a block stored in the blockchain at the given height
func OpenBlockByHeight(height int) (*Block, error) {
	b := Block{DbBlockchainBlock: &DbBlockchainBlock{Height: height}}
	if err := blockchainEnsureBlockDir(height); err != nil {
		return nil, err
	}
	blockFilename := blockchainGetFilename(height)
	hash, err := util.HashFileToHexString(blockFilename)
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

// OpenBlockFile reads block metadata from the given database file.
// Note that it will not fill-in all the fields. Notable, height is not stored in the block db's metadata.
func OpenBlockFile(fileName string) (*Block, error) {
	hash, err := util.HashFileToHexString(fileName)
	if err != nil {
		return nil, err
	}
	db, err := dbOpen(fileName, true)
	if err != nil {
		return nil, err
	}
	b := Block{DbBlockchainBlock: &DbBlockchainBlock{Hash: hash}, db: db}
	if b.Version, err = b.dbGetMetaInt("Version"); err != nil {
		return nil, err
	}
	if b.PreviousBlockHash, err = b.dbGetMetaString("PreviousBlockHash"); err != nil {
		return nil, err
	}
	if b.SignaturePublicKeyHash, err = b.dbGetMetaString("CreatorPublicKey"); err != nil {
		return nil, err
	}
	if b.PreviousBlockHashSignature, err = b.dbGetMetaHexBytes("PreviousBlockHashSignature"); err != nil {
		return nil, err
	}
	if b.TimeAccepted, err = b.dbGetMetaTime("Timestamp"); err != nil {
		st, err := os.Stat(fileName)
		if err != nil {
			return nil, err
		}
		b.TimeAccepted = st.ModTime()
	}
	return &b, nil
}

func (b *Block) Close() error {
	return b.db.Close()
}

// Returns an integer value from the _meta table within the block
func (b *Block) dbGetMetaInt(key string) (int, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return -1, err
	}
	return strconv.Atoi(value)
}

// Returns a timestamp value from the _meta table within the block
func (b *Block) dbGetMetaTime(key string) (time.Time, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, value)
	return t, err
}

// Returns a string value from the _meta table within the block
func (b *Block) dbGetMetaString(key string) (string, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

// Returns a byte blob value from the _meta table within the block (stored in the db as a hex string)
func (b *Block) dbGetMetaHexBytes(key string) ([]byte, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return nil, err
	}
	return hex.DecodeString(value)
}

// Returns a map of key operations stored in the block. Map keys are public key hashes, values are lists of ops.
func (b *Block) dbGetKeyOps() (map[string][]BlockKeyOp, error) {
	var count int
	if err := b.db.QueryRow("SELECT COUNT(*) FROM _keys").Scan(&count); err != nil {
		log.Println("Error reading db _keys")
		return nil, err
	}
	keyOps := make(map[string][]BlockKeyOp)
	rows, err := b.db.Query("SELECT op, pubkey_hash, pubkey, sigkey_hash, signature, COALESCE(metadata, '') FROM _keys")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var publicKeyHex string
		var signatureHex string
		var metadataJSON string
		var keyOp BlockKeyOp
		if err = rows.Scan(&keyOp.op, &keyOp.publicKeyHash, &publicKeyHex, &keyOp.signatureKeyHash, &signatureHex, &metadataJSON); err != nil {
			return nil, err
		}
		if keyOp.publicKeyBytes, err = hex.DecodeString(publicKeyHex); err != nil {
			return nil, err
		}
		publicKey, err := dcrypto.DecodePublicKeyBytes(keyOp.publicKeyBytes)
		if err != nil {
			return nil, err
		}
		if keyOp.publicKeyHash != dcrypto.MustPublicKeyHash(publicKey) {
			return nil, fmt.Errorf("Public key hash doesn't match for %s", keyOp.publicKeyHash)
		}
		if keyOp.signature, err = hex.DecodeString(signatureHex); err != nil {
			return nil, err
		}
		if metadataJSON != "" {
			if err = json.Unmarshal([]byte(metadataJSON), &keyOp.metadata); err != nil {
				return nil, err
			}
		}
		if _, ok := keyOps[keyOp.publicKeyHash]; ok {
			keyOps[keyOp.publicKeyHash] = append(keyOps[keyOp.publicKeyHash], keyOp)
		} else {
			keyOps[keyOp.publicKeyHash] = make([]BlockKeyOp, 1)
			keyOps[keyOp.publicKeyHash][0] = keyOp
		}
	}
	for hash, oneKeyOps := range keyOps {
		for _, keyOp := range oneKeyOps {
			if keyOp.op != oneKeyOps[0].op {
				return nil, fmt.Errorf("Mixed key ops for a single public key %s", hash)
			}
		}
	}
	return keyOps, nil
}

// Ensures special metadata tables exist in a SQLite database
func dbEnsureBlockchainTables(db *sql.DB) {
	if !dbTableExists(db, "_meta") {
		_, err := db.Exec(metaTableCreate)
		if err != nil {
			log.Fatal(err)
		}
	}
	if !dbTableExists(db, "_keys") {
		_, err := db.Exec(keysTableCreate)
		if err != nil {
			log.Fatal(err)
		}
	}
}

// Stores a key-value pair into the _meta table in the SQLite database
func dbSetMetaString(db *sql.DB, key string, value string) error {
	_, err := db.Exec("INSERT OR REPLACE INTO _meta(key, value) VALUES (?, ?)", key, value)
	return err
}

// Stores a key-value pair into the _meta table in the SQLite database
func dbSetMetaInt(db *sql.DB, key string, value int) error {
	_, err := db.Exec("INSERT OR REPLACE INTO _meta(key, value) VALUES (?, ?)", key, value)
	return err
}

// Copies a given file to the blockchain directory and names it as a block with the given height
func blockchainCopyFile(fn string, height int) error {
	if err := blockchainEnsureBlockDir(height); err != nil {
		return err
	}
	blockFilename := blockchainGetFilename(height)
	in, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer func() {
		err = in.Close()
		if err != nil {
			log.Printf("blockchainCopyFile in.Close: %v", err)
		}
	}()
	out, err := os.Create(blockFilename)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Close()
}
