package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"log"
)

func cryptoInit() {
	if dbNumPrivateKeys() == 0 {
		log.Println("Generating the default private key...")
		generatePrivateKey()
		log.Println("Generated.")
	}
}

func generatePrivateKey() *ecdsa.PrivateKey {
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
	publicKeyHash := getPubKeyHash(publicKey)

	dbWritePublicKey(publicKey, publicKeyHash)
	dbWritePrivateKey(privateKey, publicKeyHash)

	return keys
}

// returns a hex string prefixed with the hash type and ":",
// e.g. "1:b12d4ac..."
func getPubKeyHash(b []byte) string {
	hash := sha256.Sum256(b)
	return "1:" + hex.EncodeToString(hash[:])
}
