package dcrypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"
)

func TestDecodePublicKeyRequiresP256(t *testing.T) {
	for _, curve := range []elliptic.Curve{elliptic.P384(), elliptic.P521()} {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodePublicKeyBytes(der); err == nil {
			t.Errorf("a key on %s was accepted", curve.Params().Name)
		}
	}
}

func TestPublicKeyHashFormatIsChecked(t *testing.T) {
	signer := newKey(t)
	good := "1:" + strings.Repeat("ab", 32)
	if _, err := SignPublicKeyHash(signer, good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"2:" + strings.Repeat("ab", 32), // Another type
		"x:" + strings.Repeat("ab", 32),
		"1:" + strings.Repeat("ab", 31), // Too short
		"1:" + strings.Repeat("ab", 33), // Too long: ECDSA would silently truncate it
		"1:" + strings.Repeat("zz", 32),
		strings.Repeat("ab", 32),
	} {
		if _, err := SignPublicKeyHash(signer, bad); err == nil {
			t.Errorf("%q was signed", bad)
		}
		if err := VerifyPublicKeyHashSignature(&signer.PublicKey, bad, []byte{0x30, 0}); err == nil {
			t.Errorf("%q was verified", bad)
		}
	}
}

func TestHashesMustBe32Bytes(t *testing.T) {
	key := newKey(t)
	for _, n := range []int{0, 1, 31, 33, 64} {
		hash := strings.Repeat("ab", n)
		if _, err := SignHex(key, hash); err == nil {
			t.Errorf("a hash of %d bytes was signed", n)
		}
		if _, err := SignBytes(key, make([]byte, n)); err == nil {
			t.Errorf("a hash of %d bytes was signed", n)
		}
	}
	// A signature of a 32 byte hash must not verify a longer hash which starts with it
	short := strings.Repeat("ab", 32)
	sig, err := SignHexBytes(key, short)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyHexBytes(&key.PublicKey, short+"cd", sig); err == nil {
		t.Error("a signature verified a hash which is longer than 32 bytes")
	}
	if err := VerifyHexBytes(&key.PublicKey, short[:60], sig); err == nil {
		t.Error("a signature verified a hash which is shorter than 32 bytes")
	}
}

// highS makes the other valid signature out of the given one: (r, s) -> (r, n-s)
func highS(t *testing.T, sig []byte) []byte {
	t.Helper()
	var s ecdsaSignature
	if _, err := asn1.Unmarshal(sig, &s); err != nil {
		t.Fatal(err)
	}
	s.S = new(big.Int).Sub(elliptic.P256().Params().N, s.S)
	out, err := asn1.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSignaturesAreLowS(t *testing.T) {
	key := newKey(t)
	hash := strings.Repeat("ab", 32)
	for i := 0; i < 50; i++ { // About half of the plain ECDSA signatures are high-S
		sig, err := SignHexBytes(key, hash)
		if err != nil {
			t.Fatal(err)
		}
		if !IsLowS(sig) {
			t.Fatal("SignBytes produced a high-S signature")
		}
		if err := VerifyHexBytesLowS(&key.PublicKey, hash, sig); err != nil {
			t.Fatal(err)
		}
		malleated := highS(t, sig)
		if IsLowS(malleated) {
			t.Fatal("the malleated signature is low-S")
		}
		if err := VerifyHexBytesLowS(&key.PublicKey, hash, malleated); err == nil {
			t.Fatal("a high-S signature passed the low-S verification")
		}
		// The lax verification (used for the genesis block) still accepts it
		if err := VerifyHexBytes(&key.PublicKey, hash, malleated); err != nil {
			t.Fatalf("the lax verification refused the high-S signature: %v", err)
		}
	}
	if IsLowS([]byte("junk")) || IsLowS(nil) {
		t.Error("junk is low-S")
	}
}

func TestKeyOpSignatures(t *testing.T) {
	signer, subject := newKey(t), newKey(t)
	genesis := strings.Repeat("11", 32)
	otherGenesis := strings.Repeat("22", 32)
	target := MustPublicKeyHash(&subject.PublicKey)
	otherTarget := MustPublicKeyHash(&signer.PublicKey)

	sig, err := SignKeyOp(signer, genesis, "A", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyKeyOp(&signer.PublicKey, genesis, "A", target, sig); err != nil {
		t.Fatalf("valid key op signature rejected: %v", err)
	}
	if VerifyKeyOp(&signer.PublicKey, genesis, "R", target, sig) == nil {
		t.Error("the signature of an add is valid for a revoke")
	}
	if VerifyKeyOp(&signer.PublicKey, otherGenesis, "A", target, sig) == nil {
		t.Error("the signature is valid on another chain")
	}
	if VerifyKeyOp(&signer.PublicKey, genesis, "A", otherTarget, sig) == nil {
		t.Error("the signature is valid for another key")
	}
	if VerifyKeyOp(&signer.PublicKey, genesis, "A", target, highS(t, sig)) == nil {
		t.Error("a high-S key op signature was accepted")
	}
	// It's not the old format, in either direction
	if VerifyPublicKeyHashSignature(&signer.PublicKey, target, sig) == nil {
		t.Error("a key op signature is valid in the old format")
	}
	old, err := SignPublicKeyHash(signer, target)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyKeyOp(&signer.PublicKey, genesis, "A", target, old) == nil {
		t.Error("an old format signature is valid as a key op signature")
	}
	for _, bad := range []struct{ genesis, op, target string }{
		{genesis, "X", target}, {genesis, "", target}, {"11", "A", target}, {genesis, "A", "1:ab"},
		{genesis, "A", "2:" + strings.Repeat("ab", 32)},
	} {
		if _, err := SignKeyOp(signer, bad.genesis, bad.op, bad.target); err == nil {
			t.Errorf("signed a key op with %+v", bad)
		}
	}
}
