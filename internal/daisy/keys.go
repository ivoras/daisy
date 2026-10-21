package daisy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"log"

	"github.com/ivoras/daisy/internal/dcrypto"
)

func cryptoInit() {
	if dbNumPrivateKeys() == 0 {
		log.Println("Generating the initial wallet keypair...")
		generatePrivateKey(-1)
		log.Println("Generated.")
	}
}

// Generates a keypair and writes it to the private database
func generatePrivateKey(height int) *ecdsa.PrivateKey {
	keys, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(keys)
	if err != nil {
		log.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&keys.PublicKey)
	if err != nil {
		log.Fatal(err)
	}
	publicKeyHash := dcrypto.PubKeyHash(publicKey)

	dbWritePublicKey(publicKey, publicKeyHash, height)
	dbWritePrivateKey(privateKey, publicKeyHash)

	return keys
}

// cryptoGetAPrivateKey returns a random keypair read from the database
// This is mostly useful when the database has only one keypair ;)
func cryptoGetAPrivateKey() (*ecdsa.PrivateKey, string, error) {
	privateKeyBytes, publicKeyHash, err := dbGetAPrivateKey()
	if err != nil {
		return nil, "", err
	}
	dbPubKey, err := dbGetPublicKey(publicKeyHash)
	if err != nil {
		return nil, "", err
	}
	keys, err := x509.ParseECPrivateKey(privateKeyBytes)
	if err != nil {
		return nil, "", err
	}
	pubKey, err := dcrypto.DecodePublicKeyBytes(dbPubKey.publicKeyBytes)
	if err != nil {
		return nil, "", err
	}
	keys.PublicKey = *pubKey

	// Check if we can get the right public key hash back again
	if testPublicKeyHash := dcrypto.MustPublicKeyHash(&keys.PublicKey); testPublicKeyHash != publicKeyHash {
		return nil, "", fmt.Errorf("Loaded keypair %s, but the calculated public key hash doesn't match: %s", publicKeyHash, testPublicKeyHash)
	}

	return keys, publicKeyHash, nil
}

// Returns a random positive 63-bit integer
func randInt63() int64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		log.Panic(err)
	}
	return int64(binary.LittleEndian.Uint64(buf[:]) >> 1)
}
