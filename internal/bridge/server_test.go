package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(NewSession("pi", nil, t.TempDir(), nil), Policy{}, nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The health probe stays 200 while the bridge can serve, even if Pi is
// restarting, so the liveness probe does not kill the pod.
func TestHealthServesWhilePiIsDown(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"pi":"restarting"`) {
		t.Errorf("body = %s, want pi restarting", body)
	}
}

func TestIndexServesTheFrontend(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "scarab workspace") {
		t.Error("frontend does not look like the bridge page")
	}
	if !strings.Contains(string(body), "/app.mjs") {
		t.Error("frontend does not load the rendering module")
	}
}

// The rendering logic is a served module, so it can be unit-tested outside a
// browser. If it stops being served, the page silently loses its transcript.
func TestRenderingModuleIsServed(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/app.mjs")
	if err != nil {
		t.Fatalf("GET /app.mjs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content-type = %q, want text/javascript", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "export function renderRecord") {
		t.Error("module does not export renderRecord")
	}
}

func dialWS(t *testing.T, srv *httptest.Server) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn, ctx
}

func readJSON(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

func TestWebSocketGreetsAndReportsPiState(t *testing.T) {
	srv := newTestServer(t)
	conn, ctx := dialWS(t, srv)

	hello := readJSON(t, ctx, conn)
	if hello["type"] != "bridge" {
		t.Fatalf("first message = %v, want a bridge greeting", hello)
	}
	if hello["pi"] != false {
		t.Errorf("pi = %v, want false with no process", hello["pi"])
	}
	if hello["model_key_set"] != false {
		t.Errorf("model_key_set = %v, want false before one is supplied", hello["model_key_set"])
	}
}

// A prompt with no Pi running must come back as an ordinary bridge error, not a
// dropped connection.
func TestWebSocketPromptWithoutPiIsAnError(t *testing.T) {
	srv := newTestServer(t)
	conn, ctx := dialWS(t, srv)
	readJSON(t, ctx, conn) // greeting

	if err := wsjson.Write(ctx, conn, map[string]any{"type": "prompt", "message": "hi"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readJSON(t, ctx, conn)
	if reply["type"] != "bridge_error" {
		t.Fatalf("reply = %v, want bridge_error", reply)
	}
	if !strings.Contains(reply["error"].(string), "not running") {
		t.Errorf("error = %v, want it to mention pi not running", reply["error"])
	}
}

// The stop control sends this. Pi handles one message at a time, so without an
// abort the only way out of a long tool call is to wait it out.
func TestWebSocketAbortIsDispatched(t *testing.T) {
	srv := newTestServer(t)
	conn, ctx := dialWS(t, srv)
	readJSON(t, ctx, conn) // greeting

	if err := wsjson.Write(ctx, conn, map[string]any{"type": "abort"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readJSON(t, ctx, conn)
	if reply["type"] != "bridge_error" {
		t.Fatalf("reply = %v, want a bridge_error with no Pi running", reply)
	}
	if !strings.Contains(reply["error"].(string), "not running") {
		t.Errorf("error = %v", reply["error"])
	}
}

func TestWebSocketRejectsUnknownCommand(t *testing.T) {
	srv := newTestServer(t)
	conn, ctx := dialWS(t, srv)
	readJSON(t, ctx, conn)

	if err := wsjson.Write(ctx, conn, map[string]any{"type": "spawn_agent", "message": "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readJSON(t, ctx, conn)
	if reply["type"] != "bridge_error" {
		t.Fatalf("reply = %v, want bridge_error", reply)
	}
}

// Events from Pi are forwarded to the browser verbatim.
func TestWebSocketForwardsPiRecords(t *testing.T) {
	session := NewSession("pi", nil, "", nil)
	srv := httptest.NewServer(NewServer(session, Policy{}, nil).Handler())
	t.Cleanup(srv.Close)

	conn, ctx := dialWS(t, srv)
	readJSON(t, ctx, conn) // greeting

	session.broadcast([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`))

	msg := readJSON(t, ctx, conn)
	if msg["type"] != "message_update" {
		t.Fatalf("forwarded record = %v", msg)
	}
}

func TestWebSocketAcceptsModelKey(t *testing.T) {
	srv := newTestServer(t)
	conn, ctx := dialWS(t, srv)
	readJSON(t, ctx, conn) // greeting

	if err := wsjson.Write(ctx, conn, map[string]any{"type": "model_key", "provider": "google", "apiKey": "test-key"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readJSON(t, ctx, conn)
	if reply["type"] != "model_key_ok" {
		t.Fatalf("reply = %v, want model_key_ok", reply)
	}
}

func TestWebSocketRejectsInvalidProviderID(t *testing.T) {
	srv := newTestServer(t)
	conn, ctx := dialWS(t, srv)
	readJSON(t, ctx, conn)

	// The bridge keeps no allowlist -- any provider Pi knows is fine -- but an id
	// that cannot be one is refused.
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "model_key", "provider": "Not A Provider", "apiKey": "k"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readJSON(t, ctx, conn)
	if reply["type"] != "bridge_error" {
		t.Fatalf("reply = %v, want bridge_error", reply)
	}
}

// --- Host and Origin checks ---------------------------------------------------

// A request carrying another hostname is not for this workspace. That blunts DNS
// rebinding, where an attacker's name resolves here and the browser then treats the
// response as same-origin.
func TestGuardRejectsAnotherHost(t *testing.T) {
	h := NewServer(NewSession("pi", nil, t.TempDir(), nil), Policy{Hostname: "demo.gobackto.work"}, nil).Handler()

	r := httptest.NewRequest(http.MethodGet, "http://evil.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGuardAcceptsItsOwnHost(t *testing.T) {
	h := NewServer(NewSession("pi", nil, t.TempDir(), nil), Policy{Hostname: "demo.gobackto.work"}, nil).Handler()

	r := httptest.NewRequest(http.MethodGet, "https://demo.gobackto.work/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// A browser always sends Origin on a WebSocket upgrade, so a foreign one is the
// cross-site case: another page opening the socket to drive this agent.
func TestGuardRejectsCrossOriginWebSocket(t *testing.T) {
	h := NewServer(NewSession("pi", nil, t.TempDir(), nil), Policy{Hostname: "demo.gobackto.work"}, nil).Handler()

	r := httptest.NewRequest(http.MethodGet, "https://demo.gobackto.work/ws", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// The kubelet's probe sends the pod IP as Host, and readiness has no user to
// authenticate as, so the probe is exempt.
func TestHealthzIsExemptFromTheHostCheck(t *testing.T) {
	h := NewServer(NewSession("pi", nil, t.TempDir(), nil), Policy{Hostname: "demo.gobackto.work"}, nil).Handler()

	r := httptest.NewRequest(http.MethodGet, "http://10.244.0.5:8000/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// An unconfigured hostname disables both checks, which is what a dev run gets. The
// startup log says so rather than leaving it implicit.
func TestGuardIsDisabledWithoutAConfiguredHostname(t *testing.T) {
	h := NewServer(NewSession("pi", nil, t.TempDir(), nil), Policy{}, nil).Handler()

	r := httptest.NewRequest(http.MethodGet, "http://anything.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
