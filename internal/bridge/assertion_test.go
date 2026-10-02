package bridge

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/gobackto-work/scarab/internal/contract"
)

const testHost = "demo.gobackto.work"

// newTestGate returns a gate and the private key that signs for it, so a test can
// mint assertions the way town does rather than against a stub.
func newTestGate(t *testing.T) (*AssertionGate, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	gate, err := NewAssertionGate(publicPEM(t, pub), testHost)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	return gate, priv
}

func publicPEM(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// mint signs an assertion with the claims town mints. mutate, when not nil, edits
// the claims first, which is how each rejection case is built.
func mint(t *testing.T, priv ed25519.PrivateKey, mutate func(*jwt.RegisteredClaims)) string {
	t.Helper()
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    contract.AssertionIssuer,
		Audience:  jwt.ClaimStrings{testHost},
		Subject:   "github#1234",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(120 * time.Second)),
	}
	if mutate != nil {
		mutate(&claims)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestAssertionGateAcceptsWhatTownMints(t *testing.T) {
	gate, priv := newTestGate(t)

	subject, err := gate.Verify(mint(t, priv, nil))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if subject != "github#1234" {
		t.Fatalf("subject = %q, want github#1234", subject)
	}
}

func TestAssertionGateRejects(t *testing.T) {
	gate, priv := newTestGate(t)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate second key: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"not a token", "hello"},
		{"two segments", "aaa.bbb"},
		{"wrong issuer", mint(t, priv, func(c *jwt.RegisteredClaims) { c.Issuer = "pestilence" })},
		{"no issuer", mint(t, priv, func(c *jwt.RegisteredClaims) { c.Issuer = "" })},
		{"another workspace", mint(t, priv, func(c *jwt.RegisteredClaims) {
			c.Audience = jwt.ClaimStrings{"other-workspace.gobackto.work"}
		})},
		{"no audience", mint(t, priv, func(c *jwt.RegisteredClaims) { c.Audience = nil })},
		{"expired", mint(t, priv, func(c *jwt.RegisteredClaims) {
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		})},
		{"no expiry", mint(t, priv, func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil })},
		{"no subject", mint(t, priv, func(c *jwt.RegisteredClaims) { c.Subject = "" })},
		{"signed by another key", mint(t, otherPriv, nil)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if subject, err := gate.Verify(tc.token); err == nil {
				t.Fatalf("Verify accepted it, subject = %q", subject)
			}
		})
	}
}

// The classic JWT confusion bug, aimed at this gate specifically: sign HS256 using
// the PUBLIC KEY BYTES as the shared secret. It only verifies if the algorithm is
// inferred from the token instead of pinned.
func TestAssertionGateRejectsAlgorithmConfusion(t *testing.T) {
	gate, _ := newTestGate(t)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := publicPEM(t, pub)
	// The gate's key is not pemBytes, but the point is that any key material the
	// attacker can read -- and the public key is readable -- must not become an
	// HMAC secret.
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    contract.AssertionIssuer,
		Audience:  jwt.ClaimStrings{testHost},
		Subject:   "github#1234",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}).SignedString(pemBytes)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := gate.Verify(forged); err == nil {
		t.Fatal("Verify accepted an HS256 token; the algorithm is not pinned")
	}
}

func TestNewAssertionGateRejectsBadKeys(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}

	cases := []struct {
		name     string
		pem      []byte
		hostname string
	}{
		{"not PEM", []byte("nonsense"), testHost},
		{"PEM but not a key", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")}), testHost},
		{"not Ed25519", publicPEM(t, &ec.PublicKey), testHost},
		{"no hostname", publicPEM(t, &ec.PublicKey), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAssertionGate(tc.pem, tc.hostname); err == nil {
				t.Fatal("NewAssertionGate accepted it")
			}
		})
	}
}

func TestLoadAssertionGate(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "assertion-key.pub")
	if err := os.WriteFile(path, publicPEM(t, pub), 0o400); err != nil {
		t.Fatalf("write key: %v", err)
	}

	gate, err := LoadAssertionGate(path, testHost)
	if err != nil {
		t.Fatalf("LoadAssertionGate: %v", err)
	}
	if _, err := gate.Verify(mint(t, priv, nil)); err != nil {
		t.Fatalf("Verify with a loaded key: %v", err)
	}

	if _, err := LoadAssertionGate(filepath.Join(t.TempDir(), "absent"), testHost); err == nil {
		t.Fatal("LoadAssertionGate accepted a missing file")
	}
}

// The guard is where the gate is enforced, so these exercise it through the real
// handler rather than calling Verify directly.
func TestGuardRequiresAssertion(t *testing.T) {
	gate, priv := newTestGate(t)
	h := NewServer(NewSession("pi", nil, t.TempDir(), nil),
		Policy{Hostname: testHost, Gate: gate}, nil).Handler()

	cases := []struct {
		name       string
		path       string
		assertion  string
		origin     string
		wantStatus int
	}{
		{"no assertion", "/", "", "", http.StatusUnauthorized},
		{"bad assertion", "/", "not-a-token", "", http.StatusUnauthorized},
		{"valid assertion", "/", mint(t, priv, nil), "", http.StatusOK},
		// /healthz is exempt: the kubelet's probe carries no identity, and a
		// readiness check that needs an assertion would never pass.
		{"healthz is exempt", "/healthz", "", "", http.StatusOK},
		// The host and origin checks run first, so a wrong host is a 404 rather
		// than a 401 and does not advertise that the bridge is here.
		{"wrong host still 404", "/", mint(t, priv, nil), "", http.StatusNotFound},
		{"cross-origin still 403", "/", mint(t, priv, nil), "https://evil.example", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := testHost
			if tc.name == "wrong host still 404" {
				host = "other.gobackto.work"
			}
			req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
			req.URL.Path = tc.path
			if tc.assertion != "" {
				req.Header.Set(contract.HeaderAssertion, tc.assertion)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}
