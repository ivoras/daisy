package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"strconv"
	"time"
)

// CurrentBlockVersion is the version of the block metadata
const CurrentBlockVersion = 1

// GenesisBlockHash is the SHA256 hash of the genesis block payload
const GenesisBlockHash = "8cee737a33962b419060a10213b8963e3e52cbac9beabf2004c4b2bc9cc900ca"

// GenesisBlockHashSignature is the signature of the genesis block's hash, with the key in the genesis block
const GenesisBlockHashSignature = "30450220225f84a2cd13f20c24c0d010bcf51bde3395c1e7409e78cff1271fb2b074f08a022100b077fbf3cd296015772182065c8ca94558fbd7afd1b7ea619197ed4c5e0dc26f"

// GenesisBlockTimestamp is the timestamp of the genesis block
const GenesisBlockTimestamp = "Sun, 30 Apr 2017 10:14:28 +0200"

const blockchainSubdirectoryName = "blocks"
const blockFilenameFormat = "%s/block_%08d.db"

var blockchainSubdirectory string

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

func blockchainInit() {
	blockchainSubdirectory = fmt.Sprintf("%s/%s", cfg.DataDir, blockchainSubdirectoryName)
	if _, err := os.Stat(blockchainSubdirectory); err != nil {
		// Probably doesn't exist, create it
		log.Println("Creating directory", blockchainSubdirectory)
		err := os.Mkdir(blockchainSubdirectory, 0755)
		if err != nil {
			log.Fatalln(err)
		}
	}

	if dbGetBlockchainHeight() == -1 {
		log.Println("Noticing the existence of the Genesis block. Let there be light.")

		// This is basically testing the crypto code, no real purpose.
		keypair, err := getAPrivateKey()
		if err != nil {
			log.Panicln(err)
		}
		publicKeyHash := cryptoMustGetPublicKeyHash(&keypair.PublicKey)
		signature, err := cryptoSignPublicKeyHash(keypair, publicKeyHash)
		if err != nil {
			log.Panicln(err)
		}
		if err = cryptoVerifyPublicKeyHashSignature(&keypair.PublicKey, publicKeyHash, signature); err != nil {
			log.Panicln(err)
		}
		signature, err = cryptoSignBytes(keypair, make([]byte, 0)) // empty array signature
		if err != nil {
			log.Panicln(err)
		}

		// Bring the genesis block into existence
		genesisBlock := MustAsset("bindata/genesis.db")
		if hashBytesToHexString(genesisBlock) != GenesisBlockHash {
			log.Panicln("Genesis block hash unexpected")
		}
		genesisBlockFilename := fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, 0)
		ioutil.WriteFile(genesisBlockFilename, genesisBlock, 0644)
		b, err := ReadBlockFromFile(genesisBlockFilename)
		if err != nil {
			log.Panicln(err)
		}
		b.Height = 0
		b.TimeAccepted, err = time.Parse(time.RFC1123Z, GenesisBlockTimestamp)
		if err != nil {
			log.Panicln(err)
		}
		b.HashSignature, err = hex.DecodeString(GenesisBlockHashSignature)
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
	}
	err := blockchainVerifyEverything()
	if err != nil {
		log.Fatalln(err)
	}
}

func blockchainVerifyEverything() error {
	maxHeight := dbGetBlockchainHeight()
	var err error
	for height := 0; height <= maxHeight; height++ {
		if height%1000 == 0 {
			log.Println("Verifying block", height)
		}
		blockFilename := fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, height)
		fileHash, err := hashFileToHexString(blockFilename)
		if err != nil {
			return fmt.Errorf("Error verifying block %d: %s", height, err)
		}
		dbb, err := dbGetBlockByHeight(height)
		if err != nil {
			return fmt.Errorf("Db error verifying block %d: %s", height, err)
		}
		if fileHash != dbb.Hash {
			msg := fmt.Sprintf("Error verifying block %d: file hash %s doesn't match db hash %s", height, fileHash, dbb.Hash)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		dbpk, err := dbGetPublicKey(dbb.SignaturePublicKeyHash)
		if err != nil {
			msg := fmt.Sprintf("Db error verifying block %d: error getting public key %s", height, dbb.SignaturePublicKeyHash)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		creatorPublicKey, err := cryptoDecodePublicKeyBytes(dbpk.publicKeyBytes)
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: cannot decode public key %s", height, dbb.SignaturePublicKeyHash)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		hashBytes, err := hex.DecodeString(dbb.Hash)
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: cannot decode hash %s", height, dbb.Hash)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		err = cryptoVerifyBytes(creatorPublicKey, hashBytes, dbb.HashSignature)
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: block hash signature is invalid (%s)", height, err)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		previousHashBytes, err := hex.DecodeString(dbb.PreviousBlockHash)
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: cannot decode previous block hash %s", height, dbb.PreviousBlockHash)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		err = cryptoVerifyBytes(creatorPublicKey, previousHashBytes, dbb.PreviousBlockHashSignature)
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: previous block hash signature is invalid (%s)", height, err)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		b, err := OpenBlockByHeight(height)
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: cannot open block db file: %s", height, err)
			log.Println(msg)
			err = fmt.Errorf(msg)
			continue
		}
		blockKeyOps, err := b.dbGetKeyOps()
		if err != nil {
			msg := fmt.Sprintf("Error verifying block %d: cannot get key ops: %s", height, err)
			log.Println(msg)
			err = fmt.Errorf(msg)
		}
		Q := QuorumForHeight(height)
		for keyOpKeyHash, keyOps := range blockKeyOps {
			if len(keyOps) != Q {
				msg := fmt.Sprintf("Error verifying block %d: key ops for %s don't have quorum: %d vs Q=%d", height, keyOpKeyHash, len(keyOps), Q)
				log.Println(msg)
				err = fmt.Errorf(msg)
			}
			op := keyOps[0].op
			for _, kop := range keyOps {
				if kop.op != op {
					msg := fmt.Sprintf("Error verifying block %d: key ops for %s don't match: %s vs %s", height, keyOpKeyHash, kop.op, op)
					log.Println(msg)
					err = fmt.Errorf(msg)
				}
				dbSigningKey, err := dbGetPublicKey(kop.signatureKeyHash)
				if err != nil {
					msg := fmt.Sprintf("Error verifying block %d: cannot get public key %s from main db", height, kop.signatureKeyHash)
					log.Println(msg)
					err = fmt.Errorf(msg)
				}
				signingKey, err := cryptoDecodePublicKeyBytes(dbSigningKey.publicKeyBytes)
				if err != nil {
					msg := fmt.Sprintf("Error verifying block %d: cannot decode public key %s", height, dbSigningKey.publicKeyHash)
					log.Println(msg)
					err = fmt.Errorf(msg)
				}
				if err = cryptoVerifyPublicKeyHashSignature(signingKey, kop.publicKeyHash, kop.signature); err != nil {
					msg := fmt.Sprintf("Error verifying block %d: key op signature invalid for signer %s: %s", height, kop.signatureKeyHash, err)
					log.Println(msg)
					err = fmt.Errorf(msg)
				}
			}
		}
	}
	return err
}

// QuorumForHeight calculates the required key op quorum for the given block height
func QuorumForHeight(h int) int {
	if h < 149 {
		return 1
	}
	return int(math.Log(float64(h)) * 2)
}

// OpenBlockByHeight opens a block stored in the blockchain at the given height
func OpenBlockByHeight(height int) (*Block, error) {
	b := Block{DbBlockchainBlock: &DbBlockchainBlock{Height: height}}
	blockFilename := fmt.Sprintf(blockFilenameFormat, blockchainSubdirectory, height)
	hash, err := hashFileToHexString(blockFilename)
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

// ReadBlockFromFile reads block metadata from the given database file.
// Note that it will not fill-in all the fields. Notable, height is not stored in tje block db's metadata.
func ReadBlockFromFile(fileName string) (*Block, error) {
	hash, err := hashFileToHexString(fileName)
	if err != nil {
		return nil, err
	}
	db, err := dbOpen(fileName, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
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
	return &b, nil
}

func (b *Block) dbGetMetaInt(key string) (int, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return -1, err
	}
	return strconv.Atoi(value)
}

func (b *Block) dbGetMetaString(key string) (string, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

func (b *Block) dbGetMetaHexBytes(key string) ([]byte, error) {
	var value string
	if err := b.db.QueryRow("SELECT value FROM _meta WHERE key=?", key).Scan(&value); err != nil {
		return nil, err
	}
	return hex.DecodeString(value)
}

func (b *Block) dbGetKeyOps() (map[string][]BlockKeyOp, error) {
	var count int
	if err := b.db.QueryRow("SELECT COUNT(*) FROM _keys").Scan(&count); err != nil {
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
		if err := rows.Scan(&keyOp.op, &keyOp.publicKeyHash, &publicKeyHex, &keyOp.signatureKeyHash, &signatureHex, &metadataJSON); err != nil {
			return nil, err
		}
		if keyOp.publicKeyBytes, err = hex.DecodeString(publicKeyHex); err != nil {
			return nil, err
		}
		publicKey, err := cryptoDecodePublicKeyBytes(keyOp.publicKeyBytes)
		if err != nil {
			return nil, err
		}
		if keyOp.publicKeyHash != cryptoMustGetPublicKeyHash(publicKey) {
			return nil, fmt.Errorf("Public key hash doesn't match for %s", keyOp.publicKeyHash)
		}
		if keyOp.signature, err = hex.DecodeString(signatureHex); err != nil {
			return nil, err
		}
		if metadataJSON != "" {
			if err = json.Unmarshal([]byte(metadataJSON), keyOp.metadata); err != nil {
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
	return keyOps, nil
}
