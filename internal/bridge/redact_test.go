package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

// Redaction exists because the bridge is a display path with no business leaking a
// credential. It covers live events and the transcript replayed on connect alike,
// because both go through broadcast.

func TestRedactorReplacesKnownSecrets(t *testing.T) {
	r := NewRedactor()
	r.Set("sk-live-abcdefghijklmnop")

	got := string(r.Apply([]byte(`{"delta":"the key is sk-live-abcdefghijklmnop!"}`)))
	want := `{"delta":"the key is [redacted]!"}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// A short "secret" would match everywhere and turn the transcript into noise.
func TestRedactorIgnoresShortValues(t *testing.T) {
	r := NewRedactor()
	r.Set("ab", "")

	in := "ab appears in every other word, ab."
	if got := string(r.Apply([]byte(in))); got != in {
		t.Errorf("got %q, want it unchanged", got)
	}
}

func TestRedactorPassesThroughWithNoSecrets(t *testing.T) {
	r := NewRedactor()
	in := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`)
	if got := r.Apply(in); string(got) != string(in) {
		t.Errorf("got %s, want it unchanged", got)
	}
}

func TestRedactorHandlesSeveralSecrets(t *testing.T) {
	r := NewRedactor()
	r.Set("gemini-key-abcdefgh", "capability-token-ijklmnop")

	got := string(r.Apply([]byte("gemini-key-abcdefgh and capability-token-ijklmnop")))
	want := "[redacted] and [redacted]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The two values that must never reach a browser: the provider key Pi stores, and
// the workspace capability token, which pestilence rotates at half its TTL.
func TestKnownSecretsReadsTheStoreAndTheToken(t *testing.T) {
	dir := t.TempDir()
	store := `{"google":{"type":"api_key","key":"gemini-key-abcdefgh"}}`
	if err := os.WriteFile(filepath.Join(dir, authFileName), []byte(store), 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("capability-token-ijklmnop\n"), 0o400); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secrets := knownSecrets(dir, tokenPath)
	if len(secrets) != 2 {
		t.Fatalf("secrets = %v, want two", secrets)
	}

	r := NewRedactor()
	r.Set(secrets...)
	for _, secret := range []string{"gemini-key-abcdefgh", "capability-token-ijklmnop"} {
		if got := string(r.Apply([]byte(secret))); got != redacted {
			t.Errorf("%q was not redacted: got %q", secret, got)
		}
	}
}

// A missing store or token is normal, not an error: a workspace with no credential
// yet has no key to redact.
func TestKnownSecretsToleratesMissingFiles(t *testing.T) {
	if got := knownSecrets(t.TempDir(), filepath.Join(t.TempDir(), "absent")); len(got) != 0 {
		t.Errorf("secrets = %v, want none", got)
	}
}
