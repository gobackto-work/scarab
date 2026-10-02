package bridge

import (
	"context"
	"embed"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/gobackto-work/scarab/internal/contract"
)

//go:embed index.html app.mjs
var static embed.FS

// Policy is what the bridge requires of a request before it serves it.
//
// It is a struct rather than a parameter list so that adding a check is a visible
// change at every call site, rather than a new zero value that silently disables
// something.
type Policy struct {
	// Hostname is this workspace's public hostname. Empty disables the Host and
	// Origin checks, which is what a dev run without SCARAB_HOSTNAME gets; the
	// startup log says so rather than leaving it implicit.
	Hostname string

	// Gate verifies the assertion town's ForwardAuth puts on the request. Nil
	// disables the check, which leaves the edge as the only gate.
	Gate *AssertionGate
}

// Server exposes the bridge: a health probe, a WebSocket, and a minimal
// frontend. It is the default landing page for the workspace endpoint and the
// tenant may replace it (handoff §4.6, §8.2).
type Server struct {
	session *Session
	policy  Policy
	logger  *slog.Logger
}

// NewServer returns a Server. A nil logger discards.
func NewServer(session *Session, policy Policy, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Server{session: session, policy: policy, logger: logger}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWS)
	// The rendering logic is a module so it can be unit-tested without a browser.
	mux.HandleFunc("GET /app.mjs", func(w http.ResponseWriter, _ *http.Request) {
		s.serveAsset(w, "app.mjs", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("GET /", s.handleIndex)
	return s.guard(mux)
}

// guard rejects a request that is not addressed to this workspace, that does not
// come from it, or that the edge did not authenticate.
//
// The workspace has exactly one hostname, so a request carrying another one is not
// for us. That blunts DNS rebinding, where an attacker's name resolves to our
// address and the browser then treats our responses as same-origin.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The kubelet's probe sends the pod IP as Host, and readiness has no user
		// to authenticate as.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hostAllowed(r.Host) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if !s.originAllowed(r.Header.Get("Origin")) {
			http.Error(w, "cross-origin requests are not accepted", http.StatusForbidden)
			return
		}
		if s.policy.Gate != nil {
			if _, err := s.policy.Gate.Verify(r.Header.Get(contract.HeaderAssertion)); err != nil {
				// A warning, not a debug line: reaching here means a request arrived
				// at the workspace without passing the edge, which is a
				// misconfiguration worth seeing in the log rather than a user error.
				s.logger.Warn("rejected: no valid workspace assertion", "path", r.URL.Path, "err", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether the request's Host is this workspace's. An empty
// configured hostname disables the check.
func (s *Server) hostAllowed(host string) bool {
	if s.policy.Hostname == "" {
		return true
	}
	return stripPort(host) == s.policy.Hostname
}

// originAllowed reports whether an Origin header may drive this bridge.
//
// A browser always sends Origin on a WebSocket upgrade, so an absent one is a
// non-browser client -- a probe or a test -- and is allowed, because it is not the
// cross-site case. That case is a page on ANOTHER origin opening the socket, so
// only this workspace's own origin is accepted.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" || s.policy.Hostname == "" {
		return true
	}
	return origin == "https://"+s.policy.Hostname || origin == "http://"+s.policy.Hostname
}

// stripPort reduces a Host header to its hostname, tolerating IPv6 literals and a
// missing port.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// handleHealth backs both the readiness and the liveness probe.
//
// It stays 200 while the bridge can serve, and reports Pi's state in the body.
// Failing the liveness probe while Pi is briefly restarting would make the kubelet
// kill the whole pod, which is a heavier recovery than the supervisor restarting
// the child.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	state := "restarting"
	if s.session.Running() {
		state = "running"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "pi": state})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, "index.html", "text/html; charset=utf-8")
}

// serveAsset serves one embedded file.
func (s *Server) serveAsset(w http.ResponseWriter, name, contentType string) {
	body, err := static.ReadFile(name)
	if err != nil {
		http.Error(w, "bridge: missing "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}

// clientMessage is the only thing a browser may ask for. It is deliberately tiny:
// the browser drives the root agent, it does not shape a PodSpec or reach the
// broker.
type clientMessage struct {
	Type     string `json:"type"`
	Message  string `json:"message"`
	Provider string `json:"provider"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
}

// bridgeError is a bridge-level message, not a Pi event. It never carries
// internal detail.
type bridgeError struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

func newBridgeError(msg string) bridgeError {
	return bridgeError{Type: "bridge_error", Error: msg}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The Origin header was checked in guard, which is what decides whether a
		// page may open this socket. Skipping the library's own check avoids a
		// second, differently-shaped answer to the same question.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxRecordBytes)

	ctx := r.Context()
	c := &wsConn{conn: conn, ctx: ctx}

	// The capability token rotates at half its TTL, so re-read what must stay out of
	// the browser on every connect.
	s.session.RefreshSecrets()

	sub, cancel := s.session.Subscribe()
	defer cancel()

	if err := c.writeJSON(map[string]any{
		"type":          "bridge",
		"pi":            s.session.Running(),
		"model_key_set": s.session.HasModelKey(),
		"providers":     s.session.ConfiguredProviders(),
		"model":         s.session.Model(),
	}); err != nil {
		return
	}

	// Ask Pi for the transcript and the catalog, so a reload is not a blank page.
	s.session.RequestState()

	// Fan-out. The deferred cancel closes sub, which ends this goroutine.
	go func() {
		for record := range sub {
			if err := c.write(websocket.MessageText, record); err != nil {
				return
			}
		}
	}()

	for {
		var msg clientMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if err := s.dispatch(c, msg); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(c *wsConn, msg clientMessage) error {
	switch msg.Type {
	case "prompt":
		if msg.Message == "" {
			return c.writeJSON(newBridgeError("empty prompt"))
		}
		if err := s.session.Prompt(msg.Message); err != nil {
			return c.writeJSON(newBridgeError(err.Error()))
		}
	case "abort":
		if err := s.session.Abort(); err != nil {
			return c.writeJSON(newBridgeError(err.Error()))
		}
	case "model_key":
		// The key is user-supplied for this session. It is written to Pi's
		// credential store on the workspace volume, so it survives a restart, and
		// then Pi is restarted to read it.
		if err := s.session.SetModelKey(msg.Provider, msg.APIKey, msg.Model); err != nil {
			return c.writeJSON(newBridgeError(err.Error()))
		}
		return c.writeJSON(map[string]any{"type": "model_key_ok", "provider": msg.Provider, "model": s.session.Model()})
	default:
		return c.writeJSON(newBridgeError("unknown command: " + msg.Type))
	}
	return nil
}

// wsConn serialises writes. coder/websocket permits one concurrent writer, and
// both the fan-out goroutine and the command replies write.
type wsConn struct {
	conn *websocket.Conn
	ctx  context.Context
	mu   sync.Mutex
}

func (c *wsConn) write(typ websocket.MessageType, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Write(c.ctx, typ, data)
}

func (c *wsConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsjson.Write(c.ctx, c.conn, v)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
