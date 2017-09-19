package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
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

// Converts the given Unix timestamp to time.Time
func unixTimeStampToUTCTime(ts int) time.Time {
	return time.Unix(int64(ts), 0)
}

// Gets the current Unix timestamp in UTC
func getNowUTC() int64 {
	return time.Now().UTC().Unix()
}

// Mashals the given map of strings to JSON
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

// StrIfMap is a convenient data type for dealing with maps of strings to interface{}
type StrIfMap map[string]interface{}

// GetString returns a string from this map.
func (m StrIfMap) GetString(key string) (string, error) {
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

// GetString returns an Int64 from this map.
func (m StrIfMap) GetInt64(key string) (int64, error) {
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

// GetString returns an int from this map.
func (m StrIfMap) GetInt(key string) (int, error) {
	var ok bool
	var ii interface{}
	if ii, ok = m[key]; !ok {
		return 0, fmt.Errorf("No '%s' key in map", key)
	}
	var val float64
	if val, ok = ii.(float64); !ok {
		return 0, fmt.Errorf("The '%s' key in map is not an int64", key)
	}
	return int(val), nil
}

// GetString returns a map of integers to strings from this map.
func (m StrIfMap) GetIntStringMap(key string) (map[int]string, error) {
	var ok bool
	var ii interface{}
	if ii, ok = m[key]; !ok {
		return nil, fmt.Errorf("No '%s' key in map", key)
	}
	var val map[string]interface{}
	if val, ok = ii.(map[string]interface{}); !ok {
		return nil, fmt.Errorf("The '%s' key in map is not a map[string]interface{}", key)
	}
	var val2 = make(map[int]string)
	for k, v := range val {
		i, err := strconv.Atoi(k)
		if err != nil {
			return nil, err
		}
		var s string
		if s, ok = v.(string); !ok {
			return nil, fmt.Errorf("The value in the hashes map is not a string?")
		}
		val2[i] = s
	}
	return val2, nil
}

// GetStringList returns a slice of strings from this map
func (m StrIfMap) GetStringList(key string) ([]string, error) {
	var ok bool
	var ii interface{}
	if ii, ok = m[key]; !ok {
		return nil, fmt.Errorf("No '%s' key in map", key)
	}
	var ilist []interface{}
	if ilist, ok = ii.([]interface{}); !ok {
		return nil, fmt.Errorf("The '%s' key in map is not an array of interface{}", key)
	}
	var result []string
	for n, is := range ilist {
		var s string
		if s, ok = is.(string); !ok {
			return nil, fmt.Errorf("Element of %d the '%s' key is not a string", n, key)
		}
		result = append(result, s)
	}
	return result, nil
}

// StringSetWithExpiry is a set of strings whose entries disappear after a given time.
type StringSetWithExpiry struct {
	data map[string]time.Time
	age  time.Duration
	lock WithMutex
}

// NewStringSetWithExpiry returns a new StringSetWithExpiry, with the given expiry duration.
func NewStringSetWithExpiry(d time.Duration) *StringSetWithExpiry {
	ss := StringSetWithExpiry{data: make(map[string]time.Time), age: d}
	return &ss
}

// Add adds the given string to the set
func (ss *StringSetWithExpiry) Add(s string) {
	ss.lock.With(func() {
		ss.data[s] = time.Now()
	})
	ss.CheckExpire()
}

// CheckExpire walks the set and removes the entries which have expired.
func (ss *StringSetWithExpiry) CheckExpire() int {
	count := 0
	ss.lock.With(func() {
		var toExpire []string
		for s, t := range ss.data {
			d := time.Since(t)
			if d >= ss.age {
				toExpire = append(toExpire, s)
			}
		}
		for _, s := range toExpire {
			delete(ss.data, s)
		}
		count = len(toExpire)
	})
	return count
}

// Has tests if a string is present and not expired in this set.
func (ss *StringSetWithExpiry) Has(s string) bool {
	var ok bool
	ss.lock.With(func() {
		var t time.Time
		t, ok = ss.data[s]
		if ok {
			if time.Since(t) >= ss.age {
				// It's there but it's expired.
				ok = false
			}
		}
	})
	return ok
}

// TestAndSet atomically tests if the string s is present in the set and adds it if it isn't.
// Returns true iff it was in the set.
func (ss *StringSetWithExpiry) TestAndSet(s string) bool {
	var ok bool
	ss.lock.With(func() {
		var t time.Time
		t, ok = ss.data[s]
		if !ok {
			ss.data[s] = time.Now()
		} else {
			if time.Since(t) >= ss.age {
				// It's there but it's expired.
				ok = false
			}
		}
	})
	return ok
}
