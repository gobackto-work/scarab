package bridge

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/gobackto-work/scarab/internal/contract"
)

// AssertionGate verifies the assertion town's ForwardAuth puts on every request
// to a workspace.
//
// It is the bridge's own check, not the edge's. Traefik's forwardauth middleware
// is the gate the browser passes through; this is a second, independent one
// inside the workspace, and it is what makes the workspace endpoint safe when the
// edge is not in front of it -- a middleware dropped from the chain, a tenant
// adding their own ingress, a port-forward during debugging. That is why a
// rejection is a plain 401 and never a redirect: a redirect would hide a missing
// edge behind a login page that always works.
//
// Only the public key is present here, so a compromised bridge still cannot mint
// an assertion for itself. The same property is why the key is not sensitive and
// can live in a ConfigMap rather than a Secret.
type AssertionGate struct {
	pub ed25519.PublicKey

	// audience is this workspace's hostname. town mints one assertion per
	// workspace with the hostname as `aud`, so an assertion minted for one
	// workspace names a different audience and is refused here. That is what stops
	// a token being replayed against a bridge it was not minted for.
	audience string

	now func() time.Time
}

// NewAssertionGate parses a PKIX PEM Ed25519 public key and binds the gate to one
// workspace's hostname.
//
// The hostname is required rather than defaulted: without it the audience check
// cannot run, and a gate that silently skips its most load-bearing check is worse
// than no gate.
func NewAssertionGate(pubPEM []byte, hostname string) (*AssertionGate, error) {
	if hostname == "" {
		return nil, errors.New("assertion: hostname is required, because it is the audience")
	}
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return nil, errors.New("assertion: public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("assertion: parse public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("assertion: public key is %T, want ed25519.PublicKey", key)
	}
	return &AssertionGate{pub: pub, audience: hostname, now: time.Now}, nil
}

// LoadAssertionGate reads the public key from path and builds a gate.
func LoadAssertionGate(path, hostname string) (*AssertionGate, error) {
	pubPEM, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("assertion: read public key: %w", err)
	}
	return NewAssertionGate(pubPEM, hostname)
}

// Verify returns the subject of a valid assertion, or an error.
//
// The claims checked are the ones town mints: `alg` pinned to EdDSA rather than
// inferred from the token, `iss` exactly town, `aud` exactly this workspace, and
// an expiry that must be present. The signature covers all of it, so a caller
// cannot alter the audience or the subject and keep the token valid.
//
// Every rejection returns the same shape to the caller, and the HTTP layer
// collapses them into one response, so a probing client learns nothing about
// which check failed.
func (g *AssertionGate) Verify(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("assertion: missing")
	}
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return g.pub, nil },
		// Pinned, not inferred. Without this a verifier accepts whatever algorithm
		// the token names, which is the shape of the classic JWT confusion bugs --
		// here, an HS256 token signed with the public key bytes as the shared
		// secret would verify.
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(contract.AssertionIssuer),
		jwt.WithAudience(g.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(g.now),
	)
	if err != nil {
		return "", fmt.Errorf("assertion: %w", err)
	}
	if claims.Subject == "" {
		return "", errors.New("assertion: no subject")
	}
	return claims.Subject, nil
}
