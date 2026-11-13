// Package security loads TLS material for Mini-RPC client and server commands.
package security

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
)

// DefaultMinVersion is the minimum TLS version used by command-line clients
// and servers unless the operator explicitly selects TLS 1.3.
const DefaultMinVersion = tls.VersionTLS12

// LoadServerConfig loads a server certificate and optional client CA. A
// non-empty client CA enables mutual TLS with RequireAndVerifyClientCert.
func LoadServerConfig(
	certFile string,
	keyFile string,
	clientCAFile string,
	minVersion uint16,
) (*tls.Config, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("TLS certificate and key must be configured together")
	}
	if certFile == "" {
		if clientCAFile != "" {
			return nil, fmt.Errorf("TLS client CA requires a server certificate and key")
		}
		return nil, nil
	}
	minVersion, err := normalizeMinVersion(minVersion)
	if err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS server key pair: %w", err)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   minVersion,
	}
	if clientCAFile != "" {
		clientCAs, err := loadCertPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS client CA: %w", err)
		}
		config.ClientAuth = tls.RequireAndVerifyClientCert
		config.ClientCAs = clientCAs
	}
	return config, nil
}

// LoadClientConfig loads an optional private CA and client key pair. When
// serverName is empty, the client derives it from each dialed RPC address.
func LoadClientConfig(
	caFile string,
	certFile string,
	keyFile string,
	serverName string,
	minVersion uint16,
) (*tls.Config, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("TLS client certificate and key must be configured together")
	}
	if caFile == "" && certFile == "" && serverName == "" {
		return nil, nil
	}
	minVersion, err := normalizeMinVersion(minVersion)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{
		MinVersion: minVersion,
		ServerName: strings.TrimSpace(serverName),
	}
	if caFile != "" {
		rootCAs, err := loadCertPool(caFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS root CA: %w", err)
		}
		config.RootCAs = rootCAs
	}
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS client key pair: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

// ParseMinVersion parses the operator-facing TLS version selector.
func ParseMinVersion(value string) (uint16, error) {
	switch strings.TrimSpace(value) {
	case "", "1.2", "tls1.2", "TLS1.2":
		return tls.VersionTLS12, nil
	case "1.3", "tls1.3", "TLS1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("TLS minimum version must be 1.2 or 1.3")
	}
}

func normalizeMinVersion(value uint16) (uint16, error) {
	if value == 0 {
		return DefaultMinVersion, nil
	}
	if value < tls.VersionTLS12 {
		return 0, fmt.Errorf("TLS minimum version must be 1.2 or newer")
	}
	return value, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("file contains no valid PEM certificates")
	}
	return pool, nil
}
