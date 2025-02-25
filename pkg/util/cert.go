package util

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"time"
)

func LoadCertificates(serverCertPath, serverKeyPath string) (tls.Certificate, error) {
	var certPEM, keyPEM []byte

	// If server cert or key dont exist on filesystem, use ephemeral cert
	if _, err := os.Stat(serverCertPath); os.IsNotExist(err) {
		log.Println("Server cert not found, generating ephemeral cert")
		certPEM, keyPEM, err = GenerateEphemeralCert("RSA")
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to generate ephemeral cert: %w", err)
		}

		// Write cert and key to filesystem
		if err := os.WriteFile(serverCertPath, certPEM, 0644); err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to write cert: %w", err)
		}

		if err := os.WriteFile(serverKeyPath, keyPEM, 0644); err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to write key: %w", err)
		}
	} else {
		// Load cert and key from filesystem
		certPEM, err = os.ReadFile(serverCertPath)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to read cert: %w", err)
		}

		keyPEM, err = os.ReadFile(serverKeyPath)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to read key: %w", err)
		}
	}

	// Load into tls.Certificate
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load certificate: %w", err)
	}

	return tlsCert, nil
}

// GenerateEphemeralCert generates an in-memory TLS certificate with either RSA or ECC
func GenerateEphemeralCert(keyType string) (certPem []byte, keyPem []byte, err error) {
	var priv interface{}

	switch keyType {
	case "RSA":
		priv, err = rsa.GenerateKey(rand.Reader, 2048) // 2048-bit RSA key
	case "ECC":
		priv, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader) // P-384 ECC key
	default:
		return nil, nil, fmt.Errorf("unsupported key type: %s (use 'RSA' or 'ECC')", keyType)
	}

	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %v", err)
	}

	// Create a unique serial number
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %v", err)
	}

	// Define certificate template
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"Ephemeral Inc."},
		},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, // Removed "0.0.0.0"
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour), // Valid for 24 hours
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// Self-sign the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey(priv), priv)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %v", err)
	}

	// Encode the cert & key to PEM format
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := encodePrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal private key: %v", err)
	}
	return certPEM, keyPEM, nil
}

// publicKey extracts the public key from a given private key
func publicKey(priv interface{}) interface{} {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	default:
		return nil
	}
}

// encodePrivateKey encodes a private key to PEM format
func encodePrivateKey(priv interface{}) ([]byte, error) {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
	case *ecdsa.PrivateKey:
		ecKey, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecKey}), nil
	default:
		return nil, fmt.Errorf("unsupported key type")
	}
}

// Convert tls.Certificate to PEM format (full chain)
func CertificateChainToPEM(cert tls.Certificate) string {
	var pemData []byte

	for _, certBytes := range cert.Certificate {
		pemBlock := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: certBytes,
		}
		pemData = append(pemData, pem.EncodeToMemory(pemBlock)...)
	}

	return string(pemData)
}

// ExtractCertSignature takes a tls.Certificate and returns the signature of the first cert in the chain
func ExtractCertSignature(der []byte) ([]byte, error) {
	// If data is PEM encoded, decode it
	block, _ := pem.Decode(der)
	if block != nil {
		der = block.Bytes
	}

	// Parse the first certificate in the chain
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return cert.Signature, nil
}
