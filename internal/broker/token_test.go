package broker

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The token path is the highest-value red-team target (handoff §11.1). These
// tests are written as attacks: each one is a way a caller might try to make the
// broker act as a workspace it was not issued for, or to present a token that is
// valid for something else.

const (
	testWorkspace = "demo"
	testNamespace = "ws-demo"
	testAudience  = "broker-demo"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func publicKeyPEM(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func testVerifier(t *testing.T, pub ed25519.PublicKey) *Verifier {
	t.Helper()
	v, err := NewVerifier(publicKeyPEM(t, pub), testAudience, testWorkspace, testNamespace)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// validClaims is a token that should be accepted.
func validClaims(now time.Time) Claims {
	return Claims{
		Workspace: testWorkspace,
		Namespace: testNamespace,
		Role:      RoleRoot,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   "pi-root@ws-demo",
			Audience:  jwt.ClaimStrings{testAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			ID:        "test-jti",
		},
	}
}

func signEdDSA(t *testing.T, priv ed25519.PrivateKey, claims Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func TestVerifierAcceptsValidToken(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims, err := v.Verify(signEdDSA(t, priv, validClaims(now)))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Workspace != testWorkspace {
		t.Errorf("workspace = %q, want %q", claims.Workspace, testWorkspace)
	}
	if claims.Role != RoleRoot {
		t.Errorf("role = %q, want %q", claims.Role, RoleRoot)
	}
}

// A token minted for another workspace must not be accepted, even though it is
// correctly signed and correctly addressed to a broker of the same shape. This
// is the cross-tenant attack in its most direct form.
func TestVerifierRejectsWrongWorkspace(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.Workspace = "other"
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("a token for another workspace was accepted")
	}
}

func TestVerifierRejectsWrongNamespace(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.Namespace = "ws-other"
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("a token for another namespace was accepted")
	}
}

// A worker token must not be able to orchestrate (handoff §8.3).
func TestVerifierRejectsWorkerRole(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.Role = RoleWorker
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("a worker-role token was accepted for orchestration")
	}
}

func TestVerifierRejectsWrongAudience(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.Audience = jwt.ClaimStrings{"broker-other"}
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("a token addressed to another broker was accepted")
	}
}

func TestVerifierRejectsWrongIssuer(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.Issuer = "attacker"
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("a token with the wrong issuer was accepted")
	}
}

func TestVerifierRejectsExpiredToken(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute))
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

// A token with no expiry is rejected outright: revocation is bounded by the
// token's own lifetime, so an eternal token cannot be revoked (handoff §8.7).
func TestVerifierRejectsTokenWithoutExpiry(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	claims := validClaims(now)
	claims.ExpiresAt = nil
	if _, err := v.Verify(signEdDSA(t, priv, claims)); err == nil {
		t.Fatal("a token with no expiry was accepted")
	}
}

// The classic JWT attack: present an HS256 token and hope the verifier treats the
// public key as an HMAC secret. WithValidMethods must refuse it.
func TestVerifierRejectsAlgorithmConfusion(t *testing.T) {
	pub, _ := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	// Sign with HMAC using the public key bytes as the secret.
	claims := validClaims(now)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(publicKeyPEM(t, pub))
	if err != nil {
		t.Fatalf("sign hs256: %v", err)
	}
	if _, err := v.Verify(tok); err == nil {
		t.Fatal("an HS256 token was accepted by an EdDSA verifier")
	}
}

// A token signed by a different key must be refused, and the broker must not
// accept the `none` algorithm.
func TestVerifierRejectsForeignSignature(t *testing.T) {
	pub, _ := testKey(t)
	_, otherPriv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }

	if _, err := v.Verify(signEdDSA(t, otherPriv, validClaims(now))); err == nil {
		t.Fatal("a token signed by a foreign key was accepted")
	}
}

func TestVerifierRejectsGarbage(t *testing.T) {
	pub, _ := testKey(t)
	v := testVerifier(t, pub)
	for _, tok := range []string{"", "not.a.jwt", "a.b.c", "Bearer x"} {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("garbage token %q was accepted", tok)
		}
	}
}

func TestNewVerifierRejectsBadPublicKey(t *testing.T) {
	cases := map[string][]byte{
		"not pem":    []byte("hello"),
		"bad der":    pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")}),
		"rsa not ed": rsaPEM(t),
	}
	for name, pemBytes := range cases {
		if _, err := NewVerifier(pemBytes, testAudience, testWorkspace, testNamespace); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func rsaPEM(t *testing.T) []byte {
	t.Helper()
	// A real RSA public key: enough to prove the type check rejects it.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal rsa key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
