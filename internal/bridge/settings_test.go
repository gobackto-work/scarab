package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The provider and model are written to Pi's own settings file because the agent
// directory is shared with this workspace's workers. That is what stops a spawned
// worker from silently running on Pi's default model — which, for a user on a free
// OpenRouter model, meant every worker failed on a Gemini quota error.

func TestWriteDefaultsCreatesSettings(t *testing.T) {
	dir := t.TempDir()
	if err := writeDefaults(dir, "openrouter", "thinkingmachines/inkling:free"); err != nil {
		t.Fatalf("writeDefaults: %v", err)
	}

	provider, model := readDefaults(dir)
	if provider != "openrouter" {
		t.Errorf("provider = %q", provider)
	}
	if model != "thinkingmachines/inkling:free" {
		t.Errorf("model = %q", model)
	}

	// It must be a settings.json a worker will actually read.
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	if s["defaultModel"] != "thinkingmachines/inkling:free" {
		t.Errorf("settings.json = %v", s)
	}
}

// Anything else in the file — Pi's or the tenant's — must survive.
func TestWriteDefaultsPreservesOtherSettings(t *testing.T) {
	dir := t.TempDir()
	existing := `{"theme":"dark","compaction":{"reserveTokens":4096}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := writeDefaults(dir, "google", "gemini-flash-latest"); err != nil {
		t.Fatalf("writeDefaults: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s["theme"] != "dark" {
		t.Errorf("theme was lost: %v", s)
	}
	if s["compaction"] == nil {
		t.Errorf("compaction was lost: %v", s)
	}
	if s["defaultProvider"] != "google" {
		t.Errorf("defaultProvider = %v", s["defaultProvider"])
	}
}

// An empty value removes the key, so Pi falls back to its own default.
func TestWriteDefaultsClearsEmptyValues(t *testing.T) {
	dir := t.TempDir()
	if err := writeDefaults(dir, "google", "gemini-flash-latest"); err != nil {
		t.Fatalf("writeDefaults: %v", err)
	}
	if err := writeDefaults(dir, "", ""); err != nil {
		t.Fatalf("writeDefaults: %v", err)
	}
	provider, model := readDefaults(dir)
	if provider != "" || model != "" {
		t.Errorf("defaults = %q/%q, want both cleared", provider, model)
	}
}

// A settings file someone else owns is never clobbered.
func TestWriteDefaultsRefusesToOverwriteInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeDefaults(dir, "google", "m"); err == nil {
		t.Fatal("expected an error rather than overwriting a file we do not understand")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{ not json" {
		t.Errorf("file was modified: %q", raw)
	}
}

func TestReadDefaultsOnMissingFile(t *testing.T) {
	provider, model := readDefaults(t.TempDir())
	if provider != "" || model != "" {
		t.Errorf("defaults = %q/%q, want empty", provider, model)
	}
}
