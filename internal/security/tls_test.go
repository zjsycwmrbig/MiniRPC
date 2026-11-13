package security_test

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/zjsycwmrbig/mini-rpc/internal/security"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
)

func TestLoadServerAndClientConfigsForMutualTLS(t *testing.T) {
	t.Parallel()

	material, err := testkit.NewTLSMaterial("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("NewTLSMaterial() error = %v", err)
	}
	caFile := writePEMFile(t, "ca.pem", material.CAPEM)
	serverCertFile := writePEMFile(t, "server.pem", material.ServerCertPEM)
	serverKeyFile := writePEMFile(t, "server-key.pem", material.ServerKeyPEM)
	clientCertFile := writePEMFile(t, "client.pem", material.ClientCertPEM)
	clientKeyFile := writePEMFile(t, "client-key.pem", material.ClientKeyPEM)

	serverConfig, err := security.LoadServerConfig(
		serverCertFile,
		serverKeyFile,
		caFile,
		tls.VersionTLS12,
	)
	if err != nil {
		t.Fatalf("LoadServerConfig() error = %v", err)
	}
	if serverConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", serverConfig.ClientAuth)
	}
	if serverConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS 1.2", serverConfig.MinVersion)
	}

	clientConfig, err := security.LoadClientConfig(
		caFile,
		clientCertFile,
		clientKeyFile,
		"",
		tls.VersionTLS13,
	)
	if err != nil {
		t.Fatalf("LoadClientConfig() error = %v", err)
	}
	if clientConfig.RootCAs == nil || len(clientConfig.Certificates) != 1 {
		t.Fatalf("unexpected client TLS material: %+v", clientConfig)
	}
	if clientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d, want TLS 1.3", clientConfig.MinVersion)
	}
}

func TestTLSConfigValidation(t *testing.T) {
	t.Parallel()

	if _, err := security.LoadServerConfig("server.pem", "", "", security.DefaultMinVersion); err == nil {
		t.Fatal("LoadServerConfig accepted a certificate without a key")
	}
	if _, err := security.LoadClientConfig("", "client.pem", "", "", security.DefaultMinVersion); err == nil {
		t.Fatal("LoadClientConfig accepted a certificate without a key")
	}
	if _, err := security.ParseMinVersion("1.1"); err == nil {
		t.Fatal("ParseMinVersion accepted TLS 1.1")
	}
	if version, err := security.ParseMinVersion("TLS1.3"); err != nil || version != tls.VersionTLS13 {
		t.Fatalf("ParseMinVersion(TLS1.3) = %d, %v", version, err)
	}
}

func writePEMFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
