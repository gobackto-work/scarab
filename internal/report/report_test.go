package report

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
)

// tokenFile writes a token and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

// controlPlane is a stand-in for the ingest endpoint.
type controlPlane struct {
	*httptest.Server
	got  []captured
	code int
	body string
}

type captured struct {
	path   string
	method string
	auth   string
	ctype  string
	body   map[string]any
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{code: http.StatusAccepted, body: `{"sequence":41,"appended":true}`}
	cp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.got = append(cp.got, captured{
			path: r.URL.Path, method: r.Method,
			auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"),
			body: body,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cp.code)
		_, _ = w.Write([]byte(cp.body))
	}))
	t.Cleanup(cp.Close)
	return cp
}

// testToken is the token every client below presents. The token is what
// TestTheTokenIsReadPerCall varies, and that test builds its client directly so it can
// rewrite the file underneath it.
const testToken = "a-token"

func clientFor(t *testing.T, cp *controlPlane) *Client {
	t.Helper()
	c, err := New(cp.URL, tokenFile(t, testToken))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func sampleReport() Report {
	return Report{
		EventID: "9f2b1c4d5e6a7b8c9d0e1f2a3b4c5d6e",
		RunID:   "01M3ZX0X9EBQ25P2PC5BK63C5B",
		State:   contract.StateWaiting,
		Mode:    contract.ModeInteractive,
		At:      time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC),
	}
}

func TestPostSendsWhatTheControlPlaneExpects(t *testing.T) {
	cp := newControlPlane(t)
	c := clientFor(t, cp)

	if _, err := c.Post(context.Background(), sampleReport()); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(cp.got) != 1 {
		t.Fatalf("the control plane saw %d requests, want 1", len(cp.got))
	}
	got := cp.got[0]

	// The path names no workspace. The token does, so a caller has nothing to get wrong
	// and nothing to spoof.
	if got.path != contract.ReportPath {
		t.Errorf("path = %q, want %q", got.path, contract.ReportPath)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.auth != "Bearer a-token" {
		t.Errorf("authorization = %q, want a bearer token", got.auth)
	}
	if !strings.HasPrefix(got.ctype, "application/json") {
		t.Errorf("content-type = %q, want json", got.ctype)
	}

	// The body must not name the workspace or the owner: the token carries the first and
	// the record carries the second, so a caller-supplied field would be spoofable.
	for _, forbidden := range []string{"workspaceId", "owner_id"} {
		if _, ok := got.body[forbidden]; ok {
			t.Errorf("the body carries %q", forbidden)
		}
	}
	if got.body["state"] != contract.StateWaiting {
		t.Errorf("state = %v, want %q", got.body["state"], contract.StateWaiting)
	}
	if got.body["mode"] != contract.ModeInteractive {
		t.Errorf("mode = %v, want %q", got.body["mode"], contract.ModeInteractive)
	}
	if got.body["eventId"] != sampleReport().EventID {
		t.Errorf("event_id = %v, want %q", got.body["eventId"], sampleReport().EventID)
	}
	if at, _ := got.body["occurredAt"].(string); !strings.HasPrefix(at, "2026-10-04T09:30:00") {
		t.Errorf("occurred_at = %q, want the observation time", at)
	}
}

func TestTheAnswerIsReturned(t *testing.T) {
	cp := newControlPlane(t)
	c := clientFor(t, cp)

	got, err := c.Post(context.Background(), sampleReport())
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got.Sequence != 41 || !got.Appended {
		t.Errorf("result = %+v, want sequence 41 and appended", got)
	}
}

// The control plane rotates the token and the kubelet replaces the mounted file, so a
// cached token becomes a 401 that looks like a bug in the caller.
func TestTheTokenIsReadPerCall(t *testing.T) {
	cp := newControlPlane(t)
	path := tokenFile(t, "first-token")
	c, err := New(cp.URL, path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Post(context.Background(), sampleReport()); err != nil {
		t.Fatalf("first Post: %v", err)
	}
	if err := os.WriteFile(path, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatalf("rotate the token: %v", err)
	}
	if _, err := c.Post(context.Background(), sampleReport()); err != nil {
		t.Fatalf("second Post: %v", err)
	}

	if len(cp.got) != 2 {
		t.Fatalf("the control plane saw %d requests, want 2", len(cp.got))
	}
	if cp.got[0].auth != "Bearer first-token" {
		t.Errorf("first authorization = %q", cp.got[0].auth)
	}
	if cp.got[1].auth != "Bearer rotated-token" {
		t.Errorf("second authorization = %q, so the token was cached", cp.got[1].auth)
	}
}

// The caller asserts the run's current state instead of retrying, so a conflict must be
// distinguishable from a failure that a retry would fix.
func TestAConflictIsNotRetryable(t *testing.T) {
	cp := newControlPlane(t)
	cp.code, cp.body = http.StatusConflict, `{"error":{"code":"conflict"}}`
	c := clientFor(t, cp)

	_, err := c.Post(context.Background(), sampleReport())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Post = %v, want ErrConflict", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Error("a conflict was reported as transient, so the caller would retry it for ever")
	}
}

func TestARejectionIsFinal(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		cp := newControlPlane(t)
		cp.code, cp.body = code, `{"error":{"code":"nope"}}`
		c := clientFor(t, cp)

		_, err := c.Post(context.Background(), sampleReport())
		if !errors.Is(err, ErrRejected) {
			t.Errorf("status %d gave %v, want ErrRejected", code, err)
		}
		if errors.Is(err, ErrTransient) {
			t.Errorf("status %d was reported as transient", code)
		}
	}
}

func TestAServerErrorIsTransient(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		cp := newControlPlane(t)
		cp.code, cp.body = code, `{"error":{"code":"internal"}}`
		c := clientFor(t, cp)

		_, err := c.Post(context.Background(), sampleReport())
		if !errors.Is(err, ErrTransient) {
			t.Errorf("status %d gave %v, want ErrTransient", code, err)
		}
	}
}

func TestAnUnreachableControlPlaneIsTransient(t *testing.T) {
	cp := newControlPlane(t)
	c := clientFor(t, cp)
	cp.Close()

	if _, err := c.Post(context.Background(), sampleReport()); !errors.Is(err, ErrTransient) {
		t.Fatalf("Post = %v, want ErrTransient", err)
	}
}

func TestAnUnreadableAnswerIsTransient(t *testing.T) {
	cp := newControlPlane(t)
	cp.body = `not json`
	c := clientFor(t, cp)

	// The event may be recorded, and the caller cannot tell which one, so the same id
	// must be safe to post again.
	if _, err := c.Post(context.Background(), sampleReport()); !errors.Is(err, ErrTransient) {
		t.Fatalf("Post = %v, want ErrTransient", err)
	}
}

func TestAMissingTokenFileIsReported(t *testing.T) {
	cp := newControlPlane(t)
	c, err := New(cp.URL, filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Post(context.Background(), sampleReport()); err == nil {
		t.Fatal("Post with no token file succeeded")
	}
}

func TestNewRefusesConfigurationItCannotUse(t *testing.T) {
	token := tokenFile(t, "a-token")
	cases := map[string]struct{ url, token string }{
		"a relative url": {"platform:8080", token},
		"no scheme":      {"//platform", token},
		"no token path":  {"http://platform", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(c.url, c.token); err == nil {
				t.Error("New accepted it")
			}
		})
	}
}
