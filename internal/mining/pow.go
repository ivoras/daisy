// Package mining contains the (currently unused) proof-of-work primitives.
package mining

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

// userVersionOffset is the offset of the "user version" field in the SQLite database header.
// See https://www.sqlite.org/fileformat2.html#database_header
const userVersionOffset = 60

// MineSqlite3Database mines a SQLite3 database file, by adjusting the user_version field
// in the database header as a "nonce", and using SHA256 for the actual hashing. The file
// must exist and must be closed.
func MineSqlite3Database(fileName string, difficultyBits int) (string, error) {
	startNonce := uint32(time.Now().Unix())
	f, err := os.OpenFile(fileName, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 4)
	for nonce := startNonce + 1; nonce != startNonce; nonce++ {
		binary.LittleEndian.PutUint32(b, nonce)
		if _, err := f.WriteAt(b, userVersionOffset); err != nil {
			return "", err
		}
		if err := f.Sync(); err != nil {
			return "", err
		}
		hash, err := util.HashFileToBytes(fileName)
		if err != nil {
			return "", err
		}
		if CountStartZeroBits(hash) == difficultyBits {
			return hex.EncodeToString(hash), nil
		}
	}
	return "", nil
}

// CountStartZeroBits counts zero bits in the given bytes.
//
// FIXME: this does not stop at the first set bit: it keeps counting the leading
// zero bits of every following byte. The PoW code is unused so this is preserved as-is.
func CountStartZeroBits(b []byte) int {
	nBits := 0
	for _, x := range b {
		if x == 0 {
			nBits += 8
			continue
		}
		for z := 7; z >= 0 && x&(1<<z) == 0; z-- {
			nBits++
		}
	}
	return nBits
}
