package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
)

func TestSubscribeReceivesBroadcast(t *testing.T) {
	s := NewSession("pi", nil, "", nil)
	ch, cancel := s.Subscribe()
	defer cancel()

	s.broadcast([]byte(`{"type":"x"}`))

	select {
	case got := <-ch:
		if string(got) != `{"type":"x"}` {
			t.Errorf("record = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber received nothing")
	}
}

func TestCancelRemovesSubscriber(t *testing.T) {
	s := NewSession("pi", nil, "", nil)
	ch, cancel := s.Subscribe()
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the subscriber channel to be closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not close the subscriber channel")
	}
	s.broadcast([]byte(`{"type":"x"}`)) // must not panic or block
}

// A client that cannot keep up is dropped rather than allowed to stall Pi's
// stdout. Correctness here matters: blocking broadcast would block the reader.
func TestSlowSubscriberIsDropped(t *testing.T) {
	s := NewSession("pi", nil, "", nil)
	ch, _ := s.Subscribe()

	for i := 0; i < subscriberBuffer+10; i++ {
		s.broadcast([]byte(`{}`))
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // dropped and closed, as intended
			}
		case <-deadline:
			t.Fatal("a subscriber that fell behind was never dropped")
		}
	}
}

func TestPromptAndAbortWithoutPi(t *testing.T) {
	s := NewSession("pi", nil, "", nil)
	if err := s.Prompt("hi"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Prompt err = %v, want ErrNotRunning", err)
	}
	if err := s.Abort(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Abort err = %v, want ErrNotRunning", err)
	}
	if s.Running() {
		t.Error("Running = true with no process")
	}
}

// While Pi is streaming, a bare prompt is rejected, so the session must queue it
// as a steer.
func TestPromptSteersWhileBusy(t *testing.T) {
	s := NewSession("pi", nil, "", nil)
	proc := startHelper(t, "echo", "")
	s.setProc(proc)
	s.setBusy(true)

	if err := s.Prompt("hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var busy map[string]any
	if err := json.Unmarshal(nextRecord(t, proc), &busy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if busy["streamingBehavior"] != "steer" {
		t.Errorf("streamingBehavior = %v, want steer", busy["streamingBehavior"])
	}

	s.setBusy(false)
	if err := s.Prompt("again"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var idle map[string]any
	if err := json.Unmarshal(nextRecord(t, proc), &idle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := idle["streamingBehavior"]; present {
		t.Errorf("streamingBehavior present while idle: %v", idle)
	}
}

func TestRunningReflectsProcess(t *testing.T) {
	s := NewSession("pi", nil, "", nil)
	proc := startHelper(t, "sleep", "")
	s.setProc(proc)
	if !s.Running() {
		t.Error("Running = false with a live process")
	}
	s.setProc(nil)
	if s.Running() {
		t.Error("Running = true after the process was cleared")
	}
}

func TestSetModelKeyRejectsBadInput(t *testing.T) {
	s := NewSession("pi", nil, t.TempDir(), nil)
	if err := s.SetModelKey("google", "", ""); err == nil {
		t.Error("an empty key was accepted")
	}
	if err := s.SetModelKey("Not A Provider", "k", ""); err == nil {
		t.Error("an invalid provider id was accepted")
	}
	if s.HasModelKey() {
		t.Error("HasModelKey = true after a rejected credential")
	}

	// With no agent directory there is nowhere to persist the key.
	if err := NewSession("pi", nil, "", nil).SetModelKey("google", "k", ""); err == nil {
		t.Error("a key was accepted with no agent directory")
	}
}

// The credential is persisted to Pi's credential store on the workspace volume,
// so it survives a pod restart and is shared with the workspace's workers.
func TestSetModelKeyPersistsToAuthJSON(t *testing.T) {
	dir := t.TempDir()
	s := NewSession("pi", nil, dir, nil)

	if err := s.SetModelKey("google", "test-key-123", ""); err != nil {
		t.Fatalf("SetModelKey: %v", err)
	}
	if !s.HasModelKey() {
		t.Error("HasModelKey = false after storing a key")
	}
	if got := s.ConfiguredProviders(); len(got) != 1 || got[0] != "google" {
		t.Fatalf("providers = %v, want [google]", got)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("auth.json was not written: %v", err)
	}
	var creds map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("auth.json is not valid JSON: %v", err)
	}
	if creds["google"].Type != "api_key" || creds["google"].Key != "test-key-123" {
		t.Errorf("stored entry = %+v", creds["google"])
	}
}

// The store holds a secret, so it must not be readable by anyone else.
func TestAuthJSONIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows does not model POSIX file modes; the assertion is meaningful on
		// the Linux target only.
		t.Skip("POSIX permissions are not modelled on Windows")
	}
	dir := t.TempDir()
	s := NewSession("pi", nil, dir, nil)
	if err := s.SetModelKey("google", "k", ""); err != nil {
		t.Fatalf("SetModelKey: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("auth.json mode = %o, want 600", mode)
	}
}

// A second provider must not clobber the first, so multiple keys and logins
// coexist in one store.
func TestSetModelKeyPreservesOtherProviders(t *testing.T) {
	dir := t.TempDir()
	s := NewSession("pi", nil, dir, nil)
	if err := s.SetModelKey("google", "g", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelKey("anthropic", "a", ""); err != nil {
		t.Fatal(err)
	}
	got := s.ConfiguredProviders()
	if len(got) != 2 || got[0] != "anthropic" || got[1] != "google" {
		t.Fatalf("providers = %v, want [anthropic google]", got)
	}
}

// Setting a key must restart Pi: it reads auth.json at startup and cannot be told
// a key over RPC.
func TestSetModelKeyRestartsPi(t *testing.T) {
	t.Setenv("SCARAB_BRIDGE_HELPER", "announce")

	s := NewSession(os.Args[0], []string{"-test.run=^$"}, t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	ch, unsub := s.Subscribe()
	defer unsub()

	if !waitForType(t, ch, "started") {
		t.Fatal("pi never started")
	}
	if err := s.SetModelKey("google", "k", ""); err != nil {
		t.Fatalf("SetModelKey: %v", err)
	}
	if !waitForType(t, ch, "started") {
		t.Fatal("pi was not restarted after the credential was stored")
	}
}

// The model choice is remembered next to the credential, so it survives a pod
// restart. Pi's default Google model is not available on a free tier, which is
// exactly the case this exists for.
func TestSetModelKeyPersistsTheModelChoice(t *testing.T) {
	dir := t.TempDir()
	s := NewSession("pi", nil, dir, nil)

	if err := s.SetModelKey("google", "k", "gemini-flash-latest"); err != nil {
		t.Fatalf("SetModelKey: %v", err)
	}
	if got := s.Model(); got != "gemini-flash-latest" {
		t.Errorf("Model() = %q", got)
	}

	// A fresh session on the same directory must pick it up.
	if got := NewSession("pi", nil, dir, nil).Model(); got != "gemini-flash-latest" {
		t.Errorf("after restart Model() = %q, want the persisted choice", got)
	}
}

// Changing only the model must not require re-entering the key: an empty key
// keeps what is stored.
func TestSetModelKeyKeepsTheStoredKeyWhenKeyIsEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewSession("pi", nil, dir, nil)

	if err := s.SetModelKey("google", "first-key", ""); err != nil {
		t.Fatalf("SetModelKey: %v", err)
	}
	if err := s.SetModelKey("google", "", "gemini-2.5-flash"); err != nil {
		t.Fatalf("changing only the model should be allowed: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("auth.json: %v", err)
	}
	var creds map[string]struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if creds["google"].Key != "first-key" {
		t.Errorf("stored key = %q, want it unchanged", creds["google"].Key)
	}
	if got := s.Model(); got != "gemini-2.5-flash" {
		t.Errorf("Model() = %q", got)
	}
}

// With no stored key and no key supplied there is nothing to use, and saying so is
// better than starting Pi against an unauthenticated provider.
func TestSetModelKeyRequiresAKeyTheFirstTime(t *testing.T) {
	s := NewSession("pi", nil, t.TempDir(), nil)
	if err := s.SetModelKey("google", "", "gemini-flash-latest"); err == nil {
		t.Error("a model-only change was accepted with no stored key")
	}
}

func waitForType(t *testing.T, ch <-chan []byte, want string) bool {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case record, ok := <-ch:
			if !ok {
				return false
			}
			var env struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(record, &env) == nil && env.Type == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// The broker's certificate is self-signed, so trust has to be arranged before Pi
// starts. This is the only place it is arranged, which makes it worth a test: if
// it stops happening, the symptom is a connection error that says nothing about
// trust (handoff §8.5).
func TestChildEnvCarriesTheBrokerCA(t *testing.T) {
	t.Setenv(contract.EnvBrokerCA, contract.BrokerCAFile)

	got := childEnv()
	if len(got) != 1 || got[0] != nodeExtraCACerts+"="+contract.BrokerCAFile {
		t.Fatalf("childEnv() = %v, want NODE_EXTRA_CA_CERTS=%s", got, contract.BrokerCAFile)
	}

	// An unset CA is how "the broker is on plaintext" is expressed, and then there
	// is nothing to trust.
	t.Setenv(contract.EnvBrokerCA, "")
	if got := childEnv(); got != nil {
		t.Fatalf("childEnv() with no CA = %v, want nil", got)
	}
}
