package broker

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCert mints the same shape of certificate pestilence does: self-signed,
// its own CA, with the address the test will dial.
func writeTestCert(t *testing.T) (certPath, keyPath string, cert *x509.Certificate) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "broker-test.scarab.svc.cluster.local"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"broker-test.scarab.svc.cluster.local"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	write(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath, cert
}

func write(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestTLSConfigDisabled(t *testing.T) {
	cfg, err := TLSConfig("", "")
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if cfg != nil {
		t.Fatal("TLSConfig returned a config with no certificate; plaintext must stay possible")
	}
}

func TestTLSConfigRejectsHalfAConfiguration(t *testing.T) {
	certPath, keyPath, _ := writeTestCert(t)

	cases := []struct {
		name     string
		certPath string
		keyPath  string
	}{
		{"cert only", certPath, ""},
		{"key only", "", keyPath},
		{"cert missing", filepath.Join(t.TempDir(), "absent.crt"), keyPath},
		{"key missing", certPath, filepath.Join(t.TempDir(), "absent.key")},
		{"both missing", "/nope/tls.crt", "/nope/tls.key"},
		{"not a certificate", keyPath, keyPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if cfg, err := TLSConfig(tc.certPath, tc.keyPath); err == nil {
				t.Fatalf("TLSConfig accepted it and returned %v", cfg)
			}
		})
	}
}

func TestTLSConfigLoads(t *testing.T) {
	certPath, keyPath, _ := writeTestCert(t)

	cfg, err := TLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
}

// The configuration has to actually serve, and the certificate has to actually be
// the one a client must trust -- so this dials a real TLS listener rather than
// inspecting the struct.
func TestTLSConfigServesAndRequiresTheCA(t *testing.T) {
	certPath, keyPath, cert := writeTestCert(t)
	cfg, err := TLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}),
		TLSConfig: cfg,
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer func() { _ = srv.Close() }()

	url := "https://" + ln.Addr().String() + "/"

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	trusting := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	res, err := trusting.Get(url)
	if err != nil {
		t.Fatalf("GET with the CA: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	// Without the CA the handshake must fail. A self-signed certificate that
	// anything accepts is not confidentiality, it is a formality.
	distrusting := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}}
	if res, err := distrusting.Get(url); err == nil {
		_ = res.Body.Close()
		t.Fatal("GET without the CA succeeded; the certificate is not being verified")
	}
}
