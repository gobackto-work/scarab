package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// authFileName is Pi's credential store inside the agent directory. It holds API
// keys and OAuth credentials, and it is the persistence mechanism for a
// user-supplied model credential (handoff §8.4).
//
// The agent directory lives on the workspace volume, so a credential the user
// supplies survives a pod restart. It is deliberately readable by every pod in
// the workspace: the tenant namespace is the unit of compromise, and the
// credential belongs to the workspace owner.
const authFileName = "auth.json"

// hasCredential reports whether a credential is already stored for a provider.
func hasCredential(dir, provider string) bool {
	creds, err := readCredentials(dir)
	if err != nil {
		return false
	}
	_, ok := creds[provider]
	return ok
}

// apiKeyCredential is the auth.json entry for an API key, as documented in Pi's
// provider authentication reference.
type apiKeyCredential struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// readCredentials returns the parsed credential store. A missing or empty file is
// an empty store, not an error.
func readCredentials(dir string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(filepath.Join(dir, authFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, fmt.Errorf("bridge: read credentials: %w", err)
	}
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	creds := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("bridge: credentials are not valid JSON: %w", err)
	}
	return creds, nil
}

// configuredProviders lists the providers with a stored credential, sorted.
func configuredProviders(dir string) []string {
	if dir == "" {
		return nil
	}
	creds, err := readCredentials(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(creds))
	for provider := range creds {
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// putAPIKey stores an API key for one provider, preserving every other entry so
// that multiple providers and OAuth logins coexist.
//
// The write is atomic and the file is created 0600: it holds a secret.
func putAPIKey(dir, provider, key string) error {
	if dir == "" {
		return fmt.Errorf("bridge: no agent directory configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("bridge: create agent directory: %w", err)
	}
	creds, err := readCredentials(dir)
	if err != nil {
		return err
	}
	entry, err := json.Marshal(apiKeyCredential{Type: "api_key", Key: key})
	if err != nil {
		return err
	}
	creds[provider] = entry

	payload, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return fmt.Errorf("bridge: stage credentials: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bridge: write credentials: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bridge: chmod credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bridge: close credentials: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, authFileName)); err != nil {
		return fmt.Errorf("bridge: replace credentials: %w", err)
	}
	return nil
}
