package testkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// TLSMaterial contains a disposable CA, server certificate, and client
// certificate for local TLS and mTLS integration tests.
type TLSMaterial struct {
	CAPEM         []byte
	ServerCertPEM []byte
	ServerKeyPEM  []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
}

// NewTLSMaterial issues short-lived test certificates. Server SANs contain
// every supplied host; when none are supplied, localhost and 127.0.0.1 are used.
func NewTLSMaterial(hosts ...string) (*TLSMaterial, error) {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate test CA key: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Mini-RPC Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create test CA: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse test CA: %w", err)
	}

	serverCertPEM, serverKeyPEM, err := issueTestCertificate(
		caCert,
		caKey,
		big.NewInt(2),
		"Mini-RPC Test Server",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		func(template *x509.Certificate) {
			for _, host := range hosts {
				if ip := net.ParseIP(host); ip != nil {
					template.IPAddresses = append(template.IPAddresses, ip)
				} else if host != "" {
					template.DNSNames = append(template.DNSNames, host)
				}
			}
		},
		now,
	)
	if err != nil {
		return nil, err
	}
	clientCertPEM, clientKeyPEM, err := issueTestCertificate(
		caCert,
		caKey,
		big.NewInt(3),
		"Mini-RPC Test Client",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		nil,
		now,
	)
	if err != nil {
		return nil, err
	}
	return &TLSMaterial{
		CAPEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		ServerCertPEM: serverCertPEM,
		ServerKeyPEM:  serverKeyPEM,
		ClientCertPEM: clientCertPEM,
		ClientKeyPEM:  clientKeyPEM,
	}, nil
}

func issueTestCertificate(
	caCert *x509.Certificate,
	caKey *ecdsa.PrivateKey,
	serial *big.Int,
	commonName string,
	extKeyUsage []x509.ExtKeyUsage,
	mutate func(*x509.Certificate),
	now time.Time,
) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate test key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  extKeyUsage,
	}
	if mutate != nil {
		mutate(template)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create test certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal test key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}
