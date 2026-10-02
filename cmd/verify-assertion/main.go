// Command verify-assertion checks a workspace assertion against a public key, the
// way the bridge does.
//
// It exists because the interesting failures here are cross-repository. town signs,
// the bridge verifies, and the only way to learn whether the two agree about the
// audience, the issuer, or the key encoding is to run one against the other. A
// bridge that answers 401 says only that something disagreed; this says which.
//
// The key is the one the cluster delivered, so this is also the check to run after
// touching either side of the contract, and the first thing to reach for when a
// workspace that should be reachable returns 401.
//
// Usage:
//
//	verify-assertion -key <pub.pem> -audience <hostname> [-token <jwt>]
//
// With no -token it reads the assertion from stdin, which keeps it out of the
// process list and out of shell history.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gobackto-work/scarab/internal/bridge"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "verify-assertion:", err)
		os.Exit(1)
	}
}

func run() error {
	key := flag.String("key", "", "path to the PEM public key, e.g. SCARAB_ASSERTION_PUBKEY")
	audience := flag.String("audience", "", "the workspace hostname the assertion must have been minted for")
	token := flag.String("token", "", "the assertion; empty reads it from stdin")
	flag.Parse()

	if *key == "" || *audience == "" {
		return errors.New("-key and -audience are both required")
	}

	raw := *token
	if raw == "" {
		read, err := io.ReadAll(io.LimitReader(os.Stdin, maxTokenBytes))
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		raw = strings.TrimSpace(string(read))
	}

	gate, err := bridge.LoadAssertionGate(*key, *audience)
	if err != nil {
		return err
	}
	subject, err := gate.Verify(raw)
	if err != nil {
		return err
	}
	fmt.Printf("accepted: subject=%s audience=%s\n", subject, *audience)
	return nil
}

// maxTokenBytes bounds what is read from stdin. An assertion is a few hundred
// bytes; this is generous and still refuses to buffer a pipe that never ends.
const maxTokenBytes = 8192
