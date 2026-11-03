// Package broker implements the agent broker: the only privileged interface a
// Pi agent has.
//
// It is semi-trusted. It holds a namespace-scoped Role inside one tenant
// namespace and nothing else, and it serves exactly one workspace. Two
// properties are the point of the design:
//
//   - The workspace is derived from the authenticated capability token and from
//     this process's own configuration, never from a request field. There is no
//     endpoint that names a namespace.
//   - Every PodSpec it creates is built by internal/agentpod from task-level
//     input, so no tenant-supplied field can reach the API server.
//
// The HTTP surface and its authorization are specified in
// scarab/docs/architecture.md §6.1–§6.2, and they are the highest-value attack
// surface, so the tests in this package attack them directly.
package broker

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"net/http"

	"github.com/gobackto-work/scarab/internal/agentpod"
)

// Role values carried by a capability token. Only RoleRoot may orchestrate.
const (
	RoleRoot   = "root-agent"
	RoleWorker = "worker"
)

// AgentIDLen is the length of a generated agent id. It sits inside the 8–26
// bound that internal/agentpod enforces, and keeps "worker-<id>" within the
// 63-character object-name limit.
const AgentIDLen = 16

// NewAgentID returns a random agent id: lowercase alphanumerics, no dashes, so
// it is a legal DNS label and a legal object-name fragment.
//
// crypto/rand is deliberate. The id is not a secret, but a guessable id would
// let one agent address another's logs or stop another's worker, so it is drawn
// from the same source as a secret.
func NewAgentID() (string, error) {
	raw := make([]byte, 10) // 10 bytes -> 16 base32 characters
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("broker: generate agent id: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return lowerASCII(enc)[:AgentIDLen], nil
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Agent is the broker's view of one worker. It is deliberately free of any
// Kubernetes vocabulary: the Pi-facing surface must not mention pods, jobs or
// namespaces (handoff §6.3).
type Agent struct {
	ID         string `json:"id"`
	Task       string `json:"task,omitempty"`
	State      string `json:"state"`
	CreatedAt  string `json:"createdAt,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Agent states.
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
)

// Spawner creates and inspects the workspace's agents. It is an interface so the HTTP
// layer can be tested without a cluster.
type Spawner interface {
	// Spawn creates a worker Job named from agentID.
	Spawn(ctx context.Context, agentID string, req agentpod.Request) error
	// List returns every worker in this workspace.
	List(ctx context.Context) ([]Agent, error)
	// Get returns one worker, or an *Error with status 404.
	Get(ctx context.Context, agentID string) (Agent, error)
	// Logs returns the worker's output stream.
	Logs(ctx context.Context, agentID string) (io.ReadCloser, error)
	// Stop terminates the worker.
	Stop(ctx context.Context, agentID string) error
	// Root returns the workspace's root agent. Its id is its pod's UID.
	Root(ctx context.Context) (Agent, error)
}

// Error is an error that carries the HTTP status and a message safe to show the
// agent. Internal failures must never be returned as an *Error: the agent sees
// only stable codes and human-readable messages (handoff §6.5).
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Error codes. These are a contract with the Pi extension, so they are stable.
const (
	CodeUnauthenticated = "unauthenticated"
	CodeInvalidRequest  = "invalid_request"
	CodeNotFound        = "not_found"
	CodeQuotaExceeded   = "quota_exceeded"
	CodeInternal        = "internal"
)

func errNotFound(agentID string) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: "no such agent: " + agentID}
}
