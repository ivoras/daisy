package main

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"time"
)

// mineSqlite3Database mines a SQLite3 database file, by adjusting the user_version field
// in the database header as a "nonce", and using SHA256 for the actual hashing. The file
// must exist and must be closed.
func mineSqlite3Database(fileName string, difficultyBits int) (string, error) {
	startNonce := uint32(time.Now().Unix())
	f, err := os.OpenFile(fileName, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 4)
	for nonce := startNonce + 1; nonce != startNonce; nonce++ {
		binary.LittleEndian.PutUint32(b, nonce)
		_, err := f.WriteAt(b, 60) // https://www.sqlite.org/fileformat2.html#database_header
		if err != nil {
			return "", err
		}
		f.Sync()
		hash, err := hashFileToBytes(fileName)
		if err != nil {
			return "", err
		}
		nZeroes := countStartZeroBits(hash)
		if nZeroes == difficultyBits {
			return hex.EncodeToString(hash), nil
		}
	}
	return "", nil
}
