package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// WithMutex extends the Mutex type with the convenient .With(func) function
type WithMutex struct {
	sync.Mutex
}

// With executes the given function with the mutex locked
func (m *WithMutex) With(f func()) {
	m.Mutex.Lock()
	f()
	m.Mutex.Unlock()
}

func unixTimeStampToUTCTime(ts int) time.Time {
	return time.Unix(int64(ts), 0)
}

func getNowUTC() int64 {
	return time.Now().UTC().Unix()
}

func stringMap2JsonBytes(m map[string]string) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		log.Panicln("Cannot json-ise the map:", err)
	}
	return b
}

func siMapGetString(m map[string]interface{}, key string) (string, error) {
	var ok bool
	var ii interface{}
	if ii, ok = m[key]; !ok {
		return "", fmt.Errorf("No '%s' key in map", key)
	}
	var val string
	if val, ok = ii.(string); !ok {
		return "", fmt.Errorf("The '%s' key in map is not a string", key)
	}
	return val, nil
}

func siMapGetInt64(m map[string]interface{}, key string) (int64, error) {
	var ok bool
	var ii interface{}
	if ii, ok = m[key]; !ok {
		return 0, fmt.Errorf("No '%s' key in map", key)
	}
	var val float64
	if val, ok = ii.(float64); !ok {
		return 0, fmt.Errorf("The '%s' key in map is not an int64", key)
	}
	return int64(val), nil
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
