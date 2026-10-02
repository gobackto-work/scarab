package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// settingsFileName is Pi's user-level settings file inside the agent directory.
//
// The bridge writes defaultProvider/defaultModel here rather than keeping its own
// file, for one important reason: the agent directory is shared with this
// workspace's workers, so a worker inherits the provider and model the user chose
// instead of falling back to Pi's own default.
//
// That fallback was a real bug. A user running the root agent on a free
// OpenRouter model spawned workers that quietly ran on the provider's default
// model — a Gemini model their free tier allowed at limit 0 — so every worker
// failed on a quota error for a model they were not even using.
const settingsFileName = "settings.json"

type defaultSettings struct {
	DefaultProvider string `json:"defaultProvider"`
	DefaultModel    string `json:"defaultModel"`
}

// readDefaults returns the provider and model Pi will start with, or empty
// strings when nothing is set.
func readDefaults(dir string) (provider, model string) {
	if dir == "" {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, settingsFileName))
	if err != nil || len(raw) == 0 {
		return "", ""
	}
	var s defaultSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", ""
	}
	return s.DefaultProvider, s.DefaultModel
}

// writeDefaults merges the chosen provider and model into settings.json, leaving
// every other key — Pi's or the tenant's — untouched. An empty value removes the
// key, so Pi falls back to its own default.
func writeDefaults(dir, provider, model string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, settingsFileName)

	// Everything else in the file is preserved verbatim.
	rest := map[string]json.RawMessage{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil && len(raw) > 0:
		if err := json.Unmarshal(raw, &rest); err != nil {
			// Refuse rather than clobber a file someone else owns.
			return fmt.Errorf("bridge: %s is not valid JSON; refusing to overwrite it: %w", settingsFileName, err)
		}
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("bridge: read %s: %w", settingsFileName, err)
	}

	if err := setString(rest, "defaultProvider", provider); err != nil {
		return err
	}
	if err := setString(rest, "defaultModel", model); err != nil {
		return err
	}

	payload, err := json.MarshalIndent(rest, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	// Atomic replace, so a reader never sees a partial file.
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("bridge: stage %s: %w", settingsFileName, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	// Not a secret; 0644 matches how a settings file is normally written.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func setString(m map[string]json.RawMessage, key, value string) error {
	if value == "" {
		delete(m, key)
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m[key] = encoded
	return nil
}
