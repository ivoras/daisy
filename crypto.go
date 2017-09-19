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
	"unsafe"
)

var bigIntZero = big.NewInt(0)

type ecdsaSignature struct {
	R *big.Int
	S *big.Int
}

func cryptoInit() {
	if dbNumPrivateKeys() == 0 {
		log.Println("Generating the default private key...")
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
	publicKeyHash := getPubKeyHash(publicKey)

	dbWritePublicKey(publicKey, publicKeyHash, height)
	dbWritePrivateKey(privateKey, publicKeyHash)

	return keys
}

// Returns a hex string prefixed with the hash type and ":",
// e.g. "1:b12d4ac..."
func getPubKeyHash(b []byte) string {
	hash := sha256.Sum256(b)
	return "1:" + hex.EncodeToString(hash[:])
}

// getAPrivateKey returns a random keypair read from the database
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
	pubKey, err := x509.ParsePKIXPublicKey(dbPubKey.publicKeyBytes)
	if err != nil {
		log.Panicln(err)
		return nil, "", err
	}
	keys.PublicKey = *pubKey.(*ecdsa.PublicKey)
	if !elliptic.P256().IsOnCurve(keys.PublicKey.X, keys.PublicKey.Y) {
		return nil, "", fmt.Errorf("Elliptic key verification error for %s", publicKeyHash)
	}

	// Check if we can get the right public key hash back again
	testPublicKeyBytes, err := x509.MarshalPKIXPublicKey(&keys.PublicKey)
	if err != nil {
		log.Panicln(err)
	}
	testPublicKeyHash := getPubKeyHash(testPublicKeyBytes)
	if testPublicKeyHash != publicKeyHash {
		return nil, "", fmt.Errorf("Loaded keypair %s, but the calculated public key hash doesn't match: %s", publicKeyHash, testPublicKeyHash)
	}

	return keys, publicKeyHash, nil
}

// Decodes the given bytes into a public key
func cryptoDecodePublicKeyBytes(key []byte) (*ecdsa.PublicKey, error) {
	ikey, err := x509.ParsePKIXPublicKey(key)
	return ikey.(*ecdsa.PublicKey), err
}

// Returns a hash of the given public key
func cryptoMustGetPublicKeyHash(key *ecdsa.PublicKey) string {
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		log.Fatalln(err)
	}
	return getPubKeyHash(publicKeyBytes)
}

// Signs the given public key hash with the given private key and returns the signature as a byte blob.
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

// Signs a hex-encoded byte blob. and returns a hex-encoded signature byte blob
func cryptoSignHex(myPrivateKey *ecdsa.PrivateKey, hash string) (string, error) {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return "", err
	}
	signatureBytes, err := cryptoSignBytes(myPrivateKey, hashBytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(signatureBytes), nil
}

// Verifies the given signature of a hash, both hex-encoded. Returns nil if everything's ok.
func cryptoVerifyHex(publicKey *ecdsa.PublicKey, hash string, signature string) error {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	signatureBytes, err := hex.DecodeString(signature)
	if err != nil {
		return err
	}
	return cryptoVerifyBytes(publicKey, hashBytes, signatureBytes)
}

// Verifies the given signature of a hash. Returns nil if everything's ok.
func cryptoVerifyHexBytes(publicKey *ecdsa.PublicKey, hash string, signatureBytes []byte) error {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	return cryptoVerifyBytes(publicKey, hashBytes, signatureBytes)

}

// Signes a byte blob with the given private key.
func cryptoSignBytes(myPrivateKey *ecdsa.PrivateKey, hash []byte) ([]byte, error) {
	var sig ecdsaSignature
	var err error
	var signature []byte
	for {
		sig.R, sig.S, err = ecdsa.Sign(rand.Reader, myPrivateKey, hash)
		signature, err = asn1.Marshal(sig)
		if err != nil {
			return nil, err
		}
		if sig.R.Cmp(bigIntZero) != 0 {
			break
		}
	}
	return signature, nil
}

// Verifies a signed byte blob
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

// Returns a random positive 63-bit integer
func randInt63() int64 {
	buf := make([]byte, 8)
	n, err := rand.Read(buf)
	if err != nil {
		log.Panic(err)
	}
	if n != len(buf) {
		log.Panic("Cannot read 8 random bytes")
	}
	v := *(*int64)(unsafe.Pointer(&buf[0]))
	if v >= 0 {
		return v
	}
	return -v
}
