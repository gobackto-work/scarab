package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// redacted replaces any known secret on its way to a browser.
const redacted = "[redacted]"

// Redactor removes known secrets from anything the bridge sends to a browser.
//
// It exists because the bridge is a display path with no business leaking a
// credential: the model key and the capability token are both in the pod, and both
// can end up in tool output, in a transcript, or in an error the agent chose to
// print. Redaction is applied to every record broadcast to a subscriber, which
// covers live events AND the transcript replayed on connect -- the replay arrives
// as an ordinary response record.
//
// What it does NOT do: remove the secret from the session file Pi wrote on disk. Any
// process in the workspace can read that, and the tenant brought its own key to a
// namespace that is already the unit of compromise. This protects the browser and
// the audit trail, not the tenant from itself.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// NewRedactor returns an empty Redactor, which passes everything through.
func NewRedactor() *Redactor { return &Redactor{} }

// Set replaces the known secrets.
//
// Short values are ignored: a one- or two-character "secret" would match
// everywhere and turn the transcript into noise. Eight is arbitrary but well below
// any real key and well above an accidental match.
func (r *Redactor) Set(secrets ...string) {
	kept := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if s := strings.TrimSpace(secret); len(s) >= 8 {
			kept = append(kept, s)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })

	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = kept
}

// Apply returns the record with every known secret replaced.
func (r *Redactor) Apply(record []byte) []byte {
	r.mu.RLock()
	secrets := r.secrets
	r.mu.RUnlock()
	if len(secrets) == 0 {
		return record
	}

	text := string(record)
	changed := false
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			text = strings.ReplaceAll(text, secret, redacted)
			changed = true
		}
	}
	if !changed {
		return record
	}
	return []byte(text)
}

// knownSecrets gathers the values the bridge must never send onward: the model
// provider keys from Pi's credential store, and the workspace capability token.
//
// The capability token is read from the file rather than cached, because pestilence
// rotates it at half its TTL and the mount is refreshed underneath us.
func knownSecrets(agentDir, tokenPath string) []string {
	var secrets []string

	if raw, err := os.ReadFile(filepath.Join(agentDir, authFileName)); err == nil {
		var creds map[string]struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(raw, &creds) == nil {
			for _, credential := range creds {
				secrets = append(secrets, credential.Key)
			}
		}
	}

	if tokenPath != "" {
		if raw, err := os.ReadFile(tokenPath); err == nil {
			secrets = append(secrets, strings.TrimSpace(string(raw)))
		}
	}
	return secrets
}
