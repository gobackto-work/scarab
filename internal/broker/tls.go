package broker

import (
	"crypto/tls"
	"errors"
	"fmt"
)

// TLSConfig builds the broker's TLS configuration from the paths pestilence
// supplies, or returns nil when none is configured (handoff §8.5).
//
// Half a configuration is an error rather than a fallback to plaintext. A broker
// told to use a certificate but given no key is a mistake, and serving plaintext
// anyway would look exactly like it had worked.
//
// The certificate is self-signed and its own CA, so there is nothing to chain to
// and no platform-wide key anywhere. Confidentiality is the whole of what this
// buys: the authorization is still the capability token, which rotates, while the
// certificate deliberately does not.
func TLSConfig(certPath, keyPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("broker: TLS certificate and key must be set together")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("broker: load TLS key pair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Explicit rather than inherited: the floor is 1.2 everywhere else in the
		// platform, and pinning it here means a future Go default cannot lower it
		// without a test noticing.
		MinVersion: tls.VersionTLS12,
	}, nil
}
