package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/gwest/fastregistry/config"
)

// Manager handles CA certificate generation and loading
type Manager struct {
	cfg    config.CertsConfig
	caCert []byte // PEM-encoded CA certificate
	caKey  []byte // PEM-encoded CA private key
}

// NewManager creates a new certificate manager
func NewManager(cfg config.CertsConfig) (*Manager, error) {
	m := &Manager{cfg: cfg}

	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("creating cert output dir: %w", err)
	}

	if cfg.CACert != "" && cfg.CAKey != "" {
		// Load from configured paths
		cert, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert: %w", err)
		}
		key, err := os.ReadFile(cfg.CAKey)
		if err != nil {
			return nil, fmt.Errorf("reading CA key: %w", err)
		}
		m.caCert = cert
		m.caKey = key
		log.Printf("Loaded CA certificate from %s", cfg.CACert)
		return m, nil
	}

	if !cfg.AutoGenerate {
		log.Printf("Certificate auto-generation disabled, no CA configured")
		return m, nil
	}

	// Check for existing cert in output dir
	certPath := filepath.Join(cfg.OutputDir, "ca.crt")
	keyPath := filepath.Join(cfg.OutputDir, "ca.key")

	if certData, err := os.ReadFile(certPath); err == nil {
		if keyData, err := os.ReadFile(keyPath); err == nil {
			m.caCert = certData
			m.caKey = keyData
			log.Printf("Loaded existing CA certificate from %s", certPath)
			return m, nil
		}
	}

	// Generate new CA
	if err := m.generateCA(certPath, keyPath); err != nil {
		return nil, fmt.Errorf("generating CA: %w", err)
	}

	log.Printf("Generated new CA certificate at %s", certPath)
	return m, nil
}

func (m *Manager) generateCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generating serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "FastRegistry CA",
			Organization: []string{"FastRegistry"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("creating certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshaling key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("writing cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("writing key: %w", err)
	}

	m.caCert = certPEM
	m.caKey = keyPEM
	return nil
}

// CACertPEM returns the PEM-encoded CA certificate
func (m *Manager) CACertPEM() []byte {
	return m.caCert
}

// HasCA returns true if a CA certificate is available
func (m *Manager) HasCA() bool {
	return len(m.caCert) > 0
}
