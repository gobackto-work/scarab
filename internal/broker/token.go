package broker

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer is the only acceptable `iss` value: the trusted platform plane mints
// every capability token (handoff §6.2).
const Issuer = "pestilence"

// Claims are the capability-token claims. The workspace is carried in the token,
// not in the request, and the broker additionally checks it against its own
// configuration, so a token minted for one workspace cannot be replayed against
// another workspace's broker even if the audience check were bypassed.
type Claims struct {
	Workspace string `json:"workspace"`
	Namespace string `json:"namespace"`
	Role      string `json:"role"`

	jwt.RegisteredClaims
}

// Verifier validates capability tokens against one workspace's Ed25519 public
// key.
//
// Only the public key is present here. The broker therefore cannot mint a token
// even if it is fully compromised, which is what keeps a compromised broker
// inside its own workspace (handoff §11.6).
type Verifier struct {
	pub       ed25519.PublicKey
	issuer    string
	audience  string
	workspace string
	namespace string
	now       func() time.Time
}

// NewVerifier parses a PKIX PEM Ed25519 public key and binds the verifier to one
// workspace. The public key is published by pestilence as
// ConfigMap/broker-<slug>-token-pubkey in the scarab namespace (handoff §5.5).
func NewVerifier(pubPEM []byte, audience, workspace, namespace string) (*Verifier, error) {
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return nil, errors.New("token: public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("token: parse public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("token: public key is %T, want ed25519.PublicKey", key)
	}
	if audience == "" || workspace == "" || namespace == "" {
		return nil, errors.New("token: audience, workspace and namespace are required")
	}
	return &Verifier{
		pub:       pub,
		issuer:    Issuer,
		audience:  audience,
		workspace: workspace,
		namespace: namespace,
		now:       time.Now,
	}, nil
}

// Verify parses and validates a token, returning its claims.
//
// Every rejection returns the same shape of error to the caller; the HTTP layer
// deliberately collapses them into one "invalid capability token" response so a
// probing client learns nothing about which check failed.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return v.pub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if claims.Workspace != v.workspace {
		return nil, errors.New("token: workspace does not match this broker")
	}
	if claims.Namespace != v.namespace {
		return nil, errors.New("token: namespace does not match this broker")
	}
	if claims.Role != RoleRoot {
		return nil, errors.New("token: role is not root-agent")
	}
	return &claims, nil
}
