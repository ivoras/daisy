package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"time"
)

func unixTimeStampToUTCTime(ts int) time.Time {
	return time.Unix(int64(ts), 0)
}

func stringMap2JsonBytes(m map[string]string) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		log.Panicln("Cannot json-ise the map:", err)
	}
	return b
}

// Returns a hex-encoded hash of the given byte slice
func hashBytesToHexString(b []byte) string {
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

// Returns a hex-encoded hash of the given file
func hashFileToHexString(fileName string) (string, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
