package util

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

func SecureRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		return nil, fmt.Errorf("failed to generate secure random bytes: %w", err)
	}
	return b, nil
}

func Hash(datas ...[]byte) []byte {
	hash := sha256.New()
	for _, data := range datas {
		hash.Write(data)
	}
	return hash.Sum(nil)
}

// VerifySignature verifies a signature using an x509.Certificate's public key.
func VerifySignature(pubKey any, message, signature []byte) error {
	if pubKey == nil {
		return errors.New("pubKey is nil")
	}

	// Hash the message (SHA-256)
	hash := sha256.Sum256(message)

	switch key := pubKey.(type) {
	case *rsa.PublicKey:
		// Verify using RSA-PSS with SHA-256
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], signature)

	case *ecdsa.PublicKey:
		// Verify using ECDSA
		var r, s = new(big.Int), new(big.Int)
		sigLen := len(signature) / 2
		r.SetBytes(signature[:sigLen])
		s.SetBytes(signature[sigLen:])
		if ecdsa.Verify(key, hash[:], r, s) {
			return nil
		}
		return errors.New("ECDSA signature verification failed")

	case ed25519.PublicKey:
		// Verify using Ed25519
		if ed25519.Verify(key, message, signature) {
			return nil
		}
		return errors.New("Ed25519 signature verification failed")

	default:
		return errors.New("unsupported public key type")
	}
}
