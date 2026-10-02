package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gobackto-work/scarab/internal/agentpod"
)

// stubSpawner records what the HTTP layer asked for, so the tests can assert
// that a request cannot influence the workspace or the PodSpec.
type stubSpawner struct {
	spawnErr error
	agents   map[string]Agent
	lastID   string
	lastReq  agentpod.Request
	spawned  int
	stopped  []string
	logs     string
}

func (s *stubSpawner) Spawn(_ context.Context, id string, req agentpod.Request) error {
	if s.spawnErr != nil {
		return s.spawnErr
	}
	s.spawned++
	s.lastID = id
	s.lastReq = req
	if s.agents == nil {
		s.agents = map[string]Agent{}
	}
	s.agents[id] = Agent{ID: id, Task: req.Task, State: StatePending}
	return nil
}

func (s *stubSpawner) List(context.Context) ([]Agent, error) {
	out := make([]Agent, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, a)
	}
	return out, nil
}

func (s *stubSpawner) Get(_ context.Context, id string) (Agent, error) {
	a, ok := s.agents[id]
	if !ok {
		return Agent{}, errNotFound(id)
	}
	return a, nil
}

func (s *stubSpawner) Logs(_ context.Context, id string) (io.ReadCloser, error) {
	if _, ok := s.agents[id]; !ok {
		return nil, errNotFound(id)
	}
	return io.NopCloser(strings.NewReader(s.logs)), nil
}

func (s *stubSpawner) Stop(_ context.Context, id string) error {
	if _, ok := s.agents[id]; !ok {
		return errNotFound(id)
	}
	s.stopped = append(s.stopped, id)
	delete(s.agents, id)
	return nil
}

func testServer(t *testing.T, sp *stubSpawner) (http.Handler, string) {
	t.Helper()
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }
	return NewServer(sp, v, nil).Handler(), signEdDSA(t, priv, validClaims(now))
}

func do(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

func TestServerRejectsMissingToken(t *testing.T) {
	h, _ := testServer(t, &stubSpawner{})
	rec := do(t, h, http.MethodGet, "/agents", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestServerRejectsInvalidToken(t *testing.T) {
	h, _ := testServer(t, &stubSpawner{})
	rec := do(t, h, http.MethodGet, "/agents", "not-a-token", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A valid token for a different workspace must not reach the handler. The
// verifier is bound to this broker's workspace, so this is the cross-tenant
// boundary at the HTTP layer.
func TestServerRejectsTokenForAnotherWorkspace(t *testing.T) {
	pub, priv := testKey(t)
	v := testVerifier(t, pub)
	now := time.Now()
	v.now = func() time.Time { return now }
	h := NewServer(&stubSpawner{}, v, nil).Handler()

	claims := validClaims(now)
	claims.Workspace = "other"
	tok := signEdDSA(t, priv, claims)

	rec := do(t, h, http.MethodGet, "/agents", tok, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a token from another workspace", rec.Code)
	}
}

func TestServerHealthIsUnauthenticated(t *testing.T) {
	h, _ := testServer(t, &stubSpawner{})
	rec := do(t, h, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// spawn
// ---------------------------------------------------------------------------

func TestServerSpawn(t *testing.T) {
	sp := &stubSpawner{}
	h, tok := testServer(t, sp)
	body := `{"task":"inspect the repo","modelProfile":"coding","resources":{"cpu":"300m","memory":"512Mi"},"timeoutSeconds":600}`
	rec := do(t, h, http.MethodPost, "/agents", tok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var resp spawnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AgentID == "" {
		t.Fatal("response has no agentId")
	}
	if sp.lastReq.Task != "inspect the repo" {
		t.Errorf("task = %q", sp.lastReq.Task)
	}
	if sp.lastReq.Resources.CPU != "300m" {
		t.Errorf("cpu = %q", sp.lastReq.Resources.CPU)
	}
	if sp.lastReq.TimeoutSeconds != 600 {
		t.Errorf("timeout = %d", sp.lastReq.TimeoutSeconds)
	}
}

// A request that tries to smuggle a PodSpec-shaped field must be refused, not
// silently stripped. This is the "intent APIs, not Kubernetes" boundary.
func TestServerSpawnRejectsUnknownFields(t *testing.T) {
	sp := &stubSpawner{}
	h, tok := testServer(t, sp)
	for _, field := range []string{
		`"serviceAccountName":"pi-root"`,
		`"securityContext":{"privileged":true}`,
		`"volumes":[{"hostPath":{"path":"/"}}]`,
		`"namespace":"ws-other"`,
		`"nodeName":"worker-node"`,
	} {
		body := `{"task":"x",` + field + `}`
		rec := do(t, h, http.MethodPost, "/agents", tok, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("field %s: status = %d, want 400", field, rec.Code)
		}
	}
	if sp.spawned != 0 {
		t.Errorf("spawner was called %d times for rejected requests", sp.spawned)
	}
}

func TestServerSpawnRequiresTask(t *testing.T) {
	h, tok := testServer(t, &stubSpawner{})
	rec := do(t, h, http.MethodPost, "/agents", tok, `{"task":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// A quota rejection must read as an ordinary, actionable broker error.
func TestServerMapsQuotaError(t *testing.T) {
	sp := &stubSpawner{spawnErr: &Error{Status: http.StatusConflict, Code: CodeQuotaExceeded, Message: "workspace pod quota reached (6/6)"}}
	h, tok := testServer(t, sp)
	rec := do(t, h, http.MethodPost, "/agents", tok, `{"task":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var e Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != CodeQuotaExceeded {
		t.Errorf("code = %q, want %q", e.Code, CodeQuotaExceeded)
	}
	if !strings.Contains(e.Message, "quota") {
		t.Errorf("message = %q, want it to mention quota", e.Message)
	}
}

// An internal error must not leak its detail to the agent.
func TestServerDoesNotLeakInternalErrors(t *testing.T) {
	secret := "kubeconfig bearer token abc123"
	sp := &stubSpawner{spawnErr: errors.New(secret)}
	h, tok := testServer(t, sp)
	rec := do(t, h, http.MethodPost, "/agents", tok, `{"task":"x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response leaked internal error detail: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// read and control
// ---------------------------------------------------------------------------

func TestServerListEmptyIsArray(t *testing.T) {
	h, tok := testServer(t, &stubSpawner{})
	rec := do(t, h, http.MethodGet, "/agents", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", rec.Body.String())
	}
}

func TestServerGetNotFound(t *testing.T) {
	h, tok := testServer(t, &stubSpawner{})
	rec := do(t, h, http.MethodGet, "/agents/deadbeefdeadbeef", tok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServerLogs(t *testing.T) {
	sp := &stubSpawner{agents: map[string]Agent{"abcd1234abcd1234": {ID: "abcd1234abcd1234"}}, logs: "hello from the worker"}
	h, tok := testServer(t, sp)
	rec := do(t, h, http.MethodGet, "/agents/abcd1234abcd1234/logs", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello from the worker" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestServerStop(t *testing.T) {
	sp := &stubSpawner{agents: map[string]Agent{"abcd1234abcd1234": {ID: "abcd1234abcd1234"}}}
	h, tok := testServer(t, sp)
	rec := do(t, h, http.MethodDelete, "/agents/abcd1234abcd1234", tok, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(sp.stopped) != 1 {
		t.Fatalf("stopped = %v, want one entry", sp.stopped)
	}
}
