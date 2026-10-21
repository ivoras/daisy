// Package dcrypto implements Daisy's stateless cryptographic primitives: ECDSA P-256
// signatures over hashes, and the "type:hex" public key hash format.
package dcrypto

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
)

// HashSize is the size, in bytes, of every hash which is signed or verified (SHA-256)
const HashSize = sha256.Size

// KeyOpTag is the domain separation tag of the key operation signatures. Nothing else is signed with it.
const KeyOpTag = "daisy-keyop-v1"

// PubKeyHash returns a hex string prefixed with the hash type and ":",
// e.g. "1:b12d4ac..."
func PubKeyHash(b []byte) string {
	hash := sha256.Sum256(b)
	return "1:" + hex.EncodeToString(hash[:])
}

// DecodePublicKeyBytes decodes the given PKIX bytes into an ECDSA P-256 public key
func DecodePublicKeyBytes(key []byte) (*ecdsa.PublicKey, error) {
	ikey, err := x509.ParsePKIXPublicKey(key)
	if err != nil {
		return nil, err
	}
	pub, ok := ikey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not an ECDSA key")
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("public key is on the curve %s, but only P-256 is supported", pub.Curve.Params().Name)
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

// decodePublicKeyHash decodes the hex part of a "1:hex" public key hash, which must be a SHA-256 hash
func decodePublicKeyHash(fn, publicKeyHash string) ([]byte, error) {
	if !strings.HasPrefix(publicKeyHash, "1:") {
		return nil, fmt.Errorf("%s() expects a public key hash in the \"1:hex\" format, not \"%s\"", fn, publicKeyHash)
	}
	b, err := hex.DecodeString(publicKeyHash[2:])
	if err != nil {
		return nil, fmt.Errorf("%s(): %v", fn, err)
	}
	if len(b) != HashSize {
		return nil, fmt.Errorf("%s(): the public key hash is %d bytes, but it should be %d", fn, len(b), HashSize)
	}
	return b, nil
}

// KeyOpMessage returns the message (a SHA-256 hash) which is signed to approve a key operation. It binds the
// operation ("A" to add the key, "R" to revoke it) and the chain (by the hex hash of its genesis block) to the key
// operated on (by its public key hash), so that a signature can't be used for another operation or on another chain:
//
//	sha256( KeyOpTag || 0x00 || genesisHash || op || publicKeyHash )
//
// where the genesis hash and the public key hash are their raw 32 bytes, and op is the single letter.
func KeyOpMessage(genesisHash, op, publicKeyHash string) ([]byte, error) {
	genesis, err := hex.DecodeString(genesisHash)
	if err != nil || len(genesis) != HashSize {
		return nil, fmt.Errorf("KeyOpMessage(): invalid genesis block hash \"%s\"", genesisHash)
	}
	if op != "A" && op != "R" {
		return nil, fmt.Errorf("KeyOpMessage(): invalid key op \"%s\"", op)
	}
	pkh, err := decodePublicKeyHash("KeyOpMessage", publicKeyHash)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(KeyOpTag))
	h.Write([]byte{0})
	h.Write(genesis)
	h.Write([]byte(op))
	h.Write(pkh)
	return h.Sum(nil), nil
}

// SignKeyOp signs a key operation (see KeyOpMessage). The signature is low-S.
func SignKeyOp(myPrivateKey *ecdsa.PrivateKey, genesisHash, op, publicKeyHash string) ([]byte, error) {
	msg, err := KeyOpMessage(genesisHash, op, publicKeyHash)
	if err != nil {
		return nil, err
	}
	return SignBytes(myPrivateKey, msg)
}

// VerifyKeyOp verifies the signature of a key operation (see KeyOpMessage). The signature must be low-S.
func VerifyKeyOp(publicKey *ecdsa.PublicKey, genesisHash, op, publicKeyHash string, signature []byte) error {
	msg, err := KeyOpMessage(genesisHash, op, publicKeyHash)
	if err != nil {
		return err
	}
	return VerifyBytesLowS(publicKey, msg, signature)
}

// SignPublicKeyHash signs the given public key hash with the given private key and returns the signature as a byte blob.
// This is the format of the key operations in the genesis block only: it doesn't say what is being approved, so
// everywhere else SignKeyOp is used.
func SignPublicKeyHash(myPrivateKey *ecdsa.PrivateKey, publicKeyHash string) ([]byte, error) {
	publicKeyHashBytes, err := decodePublicKeyHash("SignPublicKeyHash", publicKeyHash)
	if err != nil {
		return nil, err
	}
	return SignBytes(myPrivateKey, publicKeyHashBytes)
}

// VerifyPublicKeyHashSignature returns nil (i.e. "no error") if the verification succeeds. It verifies the genesis
// block format of the key operations (see SignPublicKeyHash), and accepts high-S signatures, too.
func VerifyPublicKeyHashSignature(publicKey *ecdsa.PublicKey, publicKeyHash string, signature []byte) error {
	publicKeyHashBytes, err := decodePublicKeyHash("VerifyPublicKeyHashSignature", publicKeyHash)
	if err != nil {
		return err
	}
	return VerifyBytes(publicKey, publicKeyHashBytes, signature)
}

// SignHex signs a hex-encoded hash and returns a hex-encoded signature
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

// SignHexBytes signs a hex-encoded hash and returns a signature byte blob
func SignHexBytes(myPrivateKey *ecdsa.PrivateKey, hash string) ([]byte, error) {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return nil, err
	}
	return SignBytes(myPrivateKey, hashBytes)
}

// VerifyHexBytes verifies the given signature of a hex-encoded hash. Returns nil if everything's ok.
// It accepts high-S signatures (see VerifyBytes).
func VerifyHexBytes(publicKey *ecdsa.PublicKey, hash string, signatureBytes []byte) error {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	return VerifyBytes(publicKey, hashBytes, signatureBytes)
}

// VerifyHexBytesLowS is like VerifyHexBytes, but it also requires the canonical low-S signature.
func VerifyHexBytesLowS(publicKey *ecdsa.PublicKey, hash string, signatureBytes []byte) error {
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	return VerifyBytesLowS(publicKey, hashBytes, signatureBytes)
}

type ecdsaSignature struct {
	R, S *big.Int
}

func halfOrder() *big.Int {
	return new(big.Int).Rsh(elliptic.P256().Params().N, 1)
}

// SignBytes signs a hash, which must be exactly HashSize bytes, with the given private key. The signature is ASN.1
// DER encoded, and normalized to the low-S form so that nobody else can make another valid signature of the same hash
// out of it (for every signature (R, S) there is also a valid (R, N-S)).
func SignBytes(myPrivateKey *ecdsa.PrivateKey, hash []byte) ([]byte, error) {
	if len(hash) != HashSize {
		return nil, fmt.Errorf("sign: the hash is %d bytes, but it should be %d", len(hash), HashSize)
	}
	if myPrivateKey.Curve != elliptic.P256() {
		return nil, errors.New("sign: only P-256 keys are supported")
	}
	signature, err := ecdsa.SignASN1(rand.Reader, myPrivateKey, hash)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	var sig ecdsaSignature
	if rest, err := asn1.Unmarshal(signature, &sig); err != nil || len(rest) != 0 {
		return nil, errors.New("sign: cannot parse the signature")
	}
	if sig.S.Cmp(halfOrder()) > 0 {
		sig.S = new(big.Int).Sub(elliptic.P256().Params().N, sig.S)
		if signature, err = asn1.Marshal(sig); err != nil {
			return nil, fmt.Errorf("sign: %w", err)
		}
	}
	return signature, nil
}

// IsLowS tells if the ASN.1 DER signature is in the canonical encoding and its S value is not larger than half of the
// order of the curve.
func IsLowS(signature []byte) bool {
	var sig ecdsaSignature
	rest, err := asn1.Unmarshal(signature, &sig)
	if err != nil || len(rest) != 0 || sig.R == nil || sig.S == nil {
		return false
	}
	if canonical, err := asn1.Marshal(sig); err != nil || !bytes.Equal(canonical, signature) {
		return false
	}
	return sig.S.Cmp(halfOrder()) <= 0
}

// VerifyBytes verifies a signed hash, which must be exactly HashSize bytes. It accepts both the low-S and the high-S
// form of a signature, because the signatures made before the low-S normalization (like the one of the genesis
// block) are high-S about half of the time. See VerifyBytesLowS.
func VerifyBytes(publicKey *ecdsa.PublicKey, hash []byte, signature []byte) error {
	if len(hash) != HashSize {
		return fmt.Errorf("the hash is %d bytes, but it should be %d", len(hash), HashSize)
	}
	if ecdsa.VerifyASN1(publicKey, hash, signature) {
		return nil
	}
	return errors.New("Signature verification failed")
}

// VerifyBytesLowS is like VerifyBytes, but it also requires the signature to be the canonical low-S one.
func VerifyBytesLowS(publicKey *ecdsa.PublicKey, hash []byte, signature []byte) error {
	if !IsLowS(signature) {
		return errors.New("Signature is not in the canonical low-S form")
	}
	return VerifyBytes(publicKey, hash, signature)
}
