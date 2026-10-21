// Package dcrypto implements Daisy's stateless cryptographic primitives: ECDSA P-256
// signatures over hashes, and the "type:hex" public key hash format.
package dcrypto

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
)

// PubKeyHash returns a hex string prefixed with the hash type and ":",
// e.g. "1:b12d4ac..."
func PubKeyHash(b []byte) string {
	hash := sha256.Sum256(b)
	return "1:" + hex.EncodeToString(hash[:])
}

// DecodePublicKeyBytes decodes the given PKIX bytes into an ECDSA public key
func DecodePublicKeyBytes(key []byte) (*ecdsa.PublicKey, error) {
	ikey, err := x509.ParsePKIXPublicKey(key)
	if err != nil {
		return nil, err
	}
	pub, ok := ikey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not an ECDSA key")
	}
	return pub, nil
}

// MustPublicKeyHash returns a hash of the given public key
func MustPublicKeyHash(key *ecdsa.PublicKey) string {
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		log.Fatalln(err)
	}
	return PubKeyHash(publicKeyBytes)
}

// decodePublicKeyHash decodes the hex part of a "type:hex" public key hash
func decodePublicKeyHash(fn, publicKeyHash string) ([]byte, error) {
	if len(publicKeyHash) < 2 || publicKeyHash[1] != ':' {
		return nil, fmt.Errorf("%s() expects a public key in the \"type:hex\" format, not \"%s\"", fn, publicKeyHash)
	}
	return hex.DecodeString(publicKeyHash[2:])
}

// SignPublicKeyHash signs the given public key hash with the given private key and returns the signature as a byte blob.
func SignPublicKeyHash(myPrivateKey *ecdsa.PrivateKey, publicKeyHash string) ([]byte, error) {
	publicKeyHashBytes, err := decodePublicKeyHash("SignPublicKeyHash", publicKeyHash)
	if err != nil {
		return nil, err
	}
	return SignBytes(myPrivateKey, publicKeyHashBytes)
}

// VerifyPublicKeyHashSignature returns nil (i.e. "no error") if the verification succeeds
func VerifyPublicKeyHashSignature(publicKey *ecdsa.PublicKey, publicKeyHash string, signature []byte) error {
	publicKeyHashBytes, err := decodePublicKeyHash("VerifyPublicKeyHashSignature", publicKeyHash)
	if err != nil {
		return err
	}
	return VerifyBytes(publicKey, publicKeyHashBytes, signature)
}

// SignHex signs a hex-encoded byte blob and returns a hex-encoded signature
func SignHex(myPrivateKey *ecdsa.PrivateKey, hash string) (string, error) {
	signatureBytes, err := SignHexBytes(myPrivateKey, hash)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(signatureBytes), nil
}

// VerifyHex verifies the given signature of a hash, both hex-encoded. Returns nil if everything's ok.
func VerifyHex(publicKey *ecdsa.PublicKey, hash string, signature string) error {
	signatureBytes, err := hex.DecodeString(signature)
	if err != nil {
		return err
	}
	return VerifyHexBytes(publicKey, hash, signatureBytes)
}

// SignHexBytes signs a hex-encoded byte blob and returns a signature byte blob
func SignHexBytes(myPrivateKey *ecdsa.PrivateKey, hash string) ([]byte, error) {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return nil, err
	}
	return SignBytes(myPrivateKey, hashBytes)
}

// VerifyHexBytes verifies the given signature of a hex-encoded hash. Returns nil if everything's ok.
func VerifyHexBytes(publicKey *ecdsa.PublicKey, hash string, signatureBytes []byte) error {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	return VerifyBytes(publicKey, hashBytes, signatureBytes)
}

// SignBytes signs a byte blob with the given private key. The signature is ASN.1 DER encoded.
func SignBytes(myPrivateKey *ecdsa.PrivateKey, hash []byte) ([]byte, error) {
	signature, err := ecdsa.SignASN1(rand.Reader, myPrivateKey, hash)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return signature, nil
}

// VerifyBytes verifies a signed byte blob
func VerifyBytes(publicKey *ecdsa.PublicKey, hash []byte, signature []byte) error {
	if ecdsa.VerifyASN1(publicKey, hash, signature) {
		return nil
	}
	return errors.New("Signature verification failed")
}
