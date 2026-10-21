// Package util holds small, dependency-free helpers shared by the rest of Daisy.
package util

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// UnixTimeStampToUTCTime converts the given Unix timestamp to time.Time
func UnixTimeStampToUTCTime(ts int) time.Time {
	return time.Unix(int64(ts), 0)
}

// NowUTC gets the current Unix timestamp in UTC
func NowUTC() int64 {
	return time.Now().UTC().Unix()
}

// HashBytesToHexString returns a hex-encoded SHA256 hash of the given byte slice
func HashBytesToHexString(b []byte) string {
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

// HashFileToHexString returns a hex-encoded SHA256 hash of the given file
func HashFileToHexString(fileName string) (string, error) {
	hash, err := HashFileToBytes(fileName)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash), nil
}

// HashFileToBytes returns the SHA256 hash of the given file
func HashFileToBytes(fileName string) ([]byte, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("HashFileToBytes file.Close: %v", err)
		}
	}()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}

// MustDecodeHex decodes a hex string, panicking on error
func MustDecodeHex(hexs string) []byte {
	b, err := hex.DecodeString(hexs)
	if err != nil {
		log.Panic("MustDecodeHex:", err)
	}
	return b
}

// JSONify converts whatever to a JSON string, panicking on error
func JSONify(i any) string {
	return string(JSONifyBytes(i))
}

// JSONifyBytes converts whatever to JSON bytes, panicking on error
func JSONifyBytes(i any) []byte {
	jsonb, err := json.Marshal(i)
	if err != nil {
		log.Panic(err)
	}
	return jsonb
}

// SplitAddress splits an address string in the form of "host:port" into its separate host and port parts
func SplitAddress(address string) (string, int, error) {
	i := strings.LastIndex(address, ":") // Not using strings.Split because of IPv6
	if i < 0 {
		return address, 0, nil
	}
	port, err := strconv.Atoi(address[i+1:])
	if err != nil {
		return "", 0, err
	}
	return address[:i], port, nil
}

// LocalAddresses returns a list of local IP addresses
func LocalAddresses() []string {
	addresses := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Println(err)
		return addresses
	}
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, "lo") {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			log.Println(err)
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if strings.HasPrefix(ip.String(), "127.") {
				continue
			}
			addresses = append(addresses, ip.String())
		}
	}
	return addresses
}

// IsDirEmpty returns true if a directory is empty.
func IsDirEmpty(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err // Either not empty or error, suits both cases
}

// FileExists returns true if the named file or directory exists.
func FileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}
