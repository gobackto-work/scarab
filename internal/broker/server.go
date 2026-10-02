package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gobackto-work/scarab/internal/agentpod"
)

// Server is the broker's HTTP surface. It is the only thing a Pi agent talks to.
//
// The server never learns a namespace from a request: the workspace is fixed by
// the Verifier and by the Spawner it was constructed with. That is what makes
// "POST /agents" safe where "POST /namespace/foo/spawn" would not be.
type Server struct {
	spawner  Spawner
	verifier *Verifier
	// maxTaskBytes bounds the decoded task so a hostile agent cannot make the
	// broker hold an unbounded string.
	maxTaskBytes int64
	logger       *slog.Logger
}

// NewServer returns a Server. A nil logger discards.
func NewServer(spawner Spawner, verifier *Verifier, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		spawner:      spawner,
		verifier:     verifier,
		maxTaskBytes: agentpod.MaxTaskBytes,
		logger:       logger,
	}
}

// Handler returns the routed, authenticated handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /agents", s.handleSpawn)
	mux.HandleFunc("GET /agents", s.handleList)
	mux.HandleFunc("GET /agents/{id}", s.handleGet)
	mux.HandleFunc("GET /agents/{id}/logs", s.handleLogs)
	mux.HandleFunc("DELETE /agents/{id}", s.handleStop)
	return s.authenticate(mux)
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

type claimsCtxKey struct{}

// authenticate requires a valid capability token on everything except the health
// probe. The token is never logged and never echoed in an error.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeJSONError(w, &Error{
				Status:  http.StatusUnauthorized,
				Code:    CodeUnauthenticated,
				Message: "missing bearer token",
			})
			return
		}
		claims, err := s.verifier.Verify(raw)
		if err != nil {
			// Collapse every rejection into one response: a probing client must
			// not learn which check failed.
			writeJSONError(w, &Error{
				Status:  http.StatusUnauthorized,
				Code:    CodeUnauthenticated,
				Message: "invalid capability token",
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsCtxKey{}, claims)))
	})
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// ---------------------------------------------------------------------------
// request and response types
// ---------------------------------------------------------------------------

// spawnRequest is the full set of things an agent may ask for. Every field is
// task-level; there is no ServiceAccount, volume, securityContext, node selector
// or label. Unknown fields are rejected rather than ignored (handoff §6.1).
type spawnRequest struct {
	Task           string         `json:"task"`
	ModelProfile   string         `json:"modelProfile"`
	Resources      resourceHints  `json:"resources"`
	Workspace      workspaceHints `json:"workspace"`
	TimeoutSeconds int32          `json:"timeoutSeconds"`
}

type resourceHints struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type workspaceHints struct {
	Mount string `json:"mount"`
}

type spawnResponse struct {
	AgentID string `json:"agentId"`
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var body spawnRequest
	if err := decodeJSON(w, r, s.maxTaskBytes, &body); err != nil {
		writeJSONError(w, err)
		return
	}
	if strings.TrimSpace(body.Task) == "" {
		writeJSONError(w, &Error{
			Status:  http.StatusBadRequest,
			Code:    CodeInvalidRequest,
			Message: "task is required",
		})
		return
	}

	id, err := NewAgentID()
	if err != nil {
		s.internal(w, "generate agent id", err)
		return
	}

	req := agentpod.Request{
		Task:           body.Task,
		ModelProfile:   body.ModelProfile,
		Resources:      agentpod.ResourceHints{CPU: body.Resources.CPU, Memory: body.Resources.Memory},
		Workspace:      agentpod.WorkspaceHints{Mount: body.Workspace.Mount},
		TimeoutSeconds: body.TimeoutSeconds,
	}
	if err := s.spawner.Spawn(r.Context(), id, req); err != nil {
		s.fail(w, "spawn", err)
		return
	}
	writeJSON(w, http.StatusCreated, spawnResponse{AgentID: id})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	agents, err := s.spawner.List(r.Context())
	if err != nil {
		s.fail(w, "list", err)
		return
	}
	if agents == nil {
		agents = []Agent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	agent, err := s.spawner.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	rc, err := s.spawner.Logs(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, "logs", err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// The status line is already sent; all we can do is stop.
		s.logger.Warn("streaming agent logs failed", "err", err)
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.spawner.Stop(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, "stop", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// errors and encoding
// ---------------------------------------------------------------------------

// fail writes an *Error as-is, and everything else as a generic 500. Internal
// detail is logged, never returned: a Kubernetes error can name resources the
// agent has no business seeing (handoff §11.4).
func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		writeJSONError(w, apiErr)
		return
	}
	s.internal(w, op, err)
}

func (s *Server) internal(w http.ResponseWriter, op string, err error) {
	s.logger.Error("broker request failed", "op", op, "err", err)
	writeJSONError(w, &Error{
		Status:  http.StatusInternalServerError,
		Code:    CodeInternal,
		Message: "internal error",
	})
}

// decodeJSON enforces a size limit and rejects unknown fields, so a request that
// tries to smuggle a PodSpec-shaped field is refused rather than silently
// stripped.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) *Error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &Error{Status: http.StatusRequestEntityTooLarge, Code: CodeInvalidRequest, Message: "request body too large"}
		}
		return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "malformed request body"}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "unexpected trailing data"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, err *Error) {
	writeJSON(w, err.Status, err)
}
