package dcrypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"strings"
	"testing"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSignVerifyHex(t *testing.T) {
	key := newKey(t)
	hash := strings.Repeat("ab", 32)
	sig, err := SignHex(key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyHex(&key.PublicKey, hash, sig); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}
	if err := VerifyHex(&key.PublicKey, strings.Repeat("cd", 32), sig); err == nil {
		t.Error("signature verified against the wrong hash")
	}
	if err := VerifyHex(&newKey(t).PublicKey, hash, sig); err == nil {
		t.Error("signature verified against the wrong key")
	}
	if err := VerifyHex(&key.PublicKey, hash, "zz"); err == nil {
		t.Error("garbage signature accepted")
	}
}

func TestSignVerifyPublicKeyHash(t *testing.T) {
	signer := newKey(t)
	subject := newKey(t)
	der, err := x509.MarshalPKIXPublicKey(&subject.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hash := PubKeyHash(der)
	if !strings.HasPrefix(hash, "1:") || len(hash) != 2+64 {
		t.Fatalf("unexpected public key hash format: %s", hash)
	}
	if got := MustPublicKeyHash(&subject.PublicKey); got != hash {
		t.Errorf("MustPublicKeyHash = %s, want %s", got, hash)
	}
	sig, err := SignPublicKeyHash(signer, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublicKeyHashSignature(&signer.PublicKey, hash, sig); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}
	if _, err := SignPublicKeyHash(signer, "nocolon"); err == nil {
		t.Error("malformed public key hash accepted")
	}
	if _, err := SignPublicKeyHash(signer, ""); err == nil {
		t.Error("empty public key hash accepted")
	}
}

func TestDecodePublicKeyBytes(t *testing.T) {
	key := newKey(t)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := DecodePublicKeyBytes(der)
	if err != nil || !pub.Equal(&key.PublicKey) {
		t.Errorf("round trip failed: %v", err)
	}
	if _, err := DecodePublicKeyBytes([]byte("junk")); err == nil {
		t.Error("junk decoded as a public key")
	}
}
