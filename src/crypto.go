package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
)

type ecdsaSignature struct {
	R *big.Int
	S *big.Int
}

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

// Returns a hex string prefixed with the hash type and ":",
// e.g. "1:b12d4ac..."
func getPubKeyHash(b []byte) string {
	hash := sha256.Sum256(b)
	return "1:" + hex.EncodeToString(hash[:])
}

// Returns a hex-encoded hash of a random byte slice
func hashBytesToHexString(b []byte) string {
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

// getAPrivateKey returns a random keypair read from the database
func getAPrivateKey() (*ecdsa.PrivateKey, error) {
	privateKeyBytes, publicKeyHash, err := dbGetAPrivateKey()
	if err != nil {
		log.Panicln(err)
		return nil, err
	}
	dbPubKey, err := dbGetPublicKey(publicKeyHash)
	keys, err := x509.ParseECPrivateKey(privateKeyBytes)
	if err != nil {
		log.Panicln(err)
		return nil, err
	}
	pubKey, err := x509.ParsePKIXPublicKey(dbPubKey.publicKeyBytes)
	if err != nil {
		log.Panicln(err)
		return nil, err
	}
	keys.PublicKey = *pubKey.(*ecdsa.PublicKey)
	if !elliptic.P256().IsOnCurve(keys.PublicKey.X, keys.PublicKey.Y) {
		return nil, fmt.Errorf("Elliptic key verification error for %s", publicKeyHash)
	}

	// Check if we can get the right public key hash back again
	testPublicKeyBytes, err := x509.MarshalPKIXPublicKey(&keys.PublicKey)
	if err != nil {
		log.Panicln(err)
	}
	testPublicKeyHash := getPubKeyHash(testPublicKeyBytes)
	if testPublicKeyHash != publicKeyHash {
		return nil, fmt.Errorf("Loaded keypair %s, but the calculated public key hash doesn't match: %s", publicKeyHash, testPublicKeyHash)
	}

	return keys, nil
}

func cryptoMustGetPublicKeyHash(keypair *ecdsa.PrivateKey) string {
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(&keypair.PublicKey)
	if err != nil {
		log.Fatalln(err)
	}
	return getPubKeyHash(publicKeyBytes)
}

func cryptoSignPublicKeyHash(myPrivateKey *ecdsa.PrivateKey, publicKeyHash string) ([]byte, error) {
	if publicKeyHash[1] != ':' {
		return nil, fmt.Errorf("cryptoSignPublicKeyHash() expects a public key in the \"type:hex\" format, not \"%s\"", publicKeyHash)
	}
	publicKeyHashBytes, err := hex.DecodeString(publicKeyHash[2:])
	if err != nil {
		return nil, err
	}
	return cryptoSignBytes(myPrivateKey, publicKeyHashBytes)
}

// Returns nil (i.e. "no error") if the verification succeeds
func cryptoVerifyPublicKeyHashSignature(publicKey *ecdsa.PublicKey, publicKeyHash string, signature []byte) error {
	if publicKeyHash[1] != ':' {
		return fmt.Errorf("cryptoVerifyPublicKeyHash() expects a public key in the \"type:hex\" format, not \"%s\"", publicKeyHash)
	}
	publicKeyHashBytes, err := hex.DecodeString(publicKeyHash[2:])
	if err != nil {
		return err
	}
	return cryptoVerifyBytes(publicKey, publicKeyHashBytes, signature)
}

func cryptoSignBytes(myPrivateKey *ecdsa.PrivateKey, data []byte) ([]byte, error) {
	var sig ecdsaSignature
	var err error
	sig.R, sig.S, err = ecdsa.Sign(rand.Reader, myPrivateKey, data)
	signature, err := asn1.Marshal(sig)
	if err != nil {
		return nil, err
	}
	return signature, nil
}

func cryptoVerifyBytes(publicKey *ecdsa.PublicKey, hash []byte, signature []byte) error {
	var sig ecdsaSignature
	_, err := asn1.Unmarshal(signature, &sig)
	if err != nil {
		return err
	}
	if ecdsa.Verify(publicKey, hash, sig.R, sig.S) {
		// Verification succeded
		return nil
	}
	return fmt.Errorf("Signature verification failed")
}
