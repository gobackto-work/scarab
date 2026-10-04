// Package report posts a run's state transitions to the control plane.
//
// The broker is the only component that reports, because it is the only one that outlives
// the agent processes in the workspace. An agent cannot report the state it reaches by
// dying, and a worker that is killed reports nothing at all, which is the case that
// matters most: a parent waiting on that worker would wait for ever.
//
// The client posts once. It classifies a failure as retryable or final and leaves the
// retry to the caller, because the caller owns the queue and knows whether it has one.
//
// See pestilence/docs/event-record.md for the contract.
package report

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
)

// maxBody bounds what is read from a response. A report is small, and a reply that is not
// small is not a reply.
const maxBody = 8 << 10

var (
	// ErrTransient means the report did not arrive. The same event id may be posted
	// again, and the control plane will record one event.
	ErrTransient = errors.New("the control plane is not reachable")

	// ErrRejected means the control plane refused the report. Posting it again with the
	// same event id will be refused again.
	ErrRejected = errors.New("the control plane rejected the report")

	// ErrConflict means the record holds a state this report cannot follow, or the run's
	// mode disagrees with the one it was recorded with. The caller asserts the run's
	// current state instead of retrying.
	ErrConflict = errors.New("the run cannot move to that state")
)

// Report is one state transition to post.
type Report struct {
	// EventID is the idempotency key. A retry carries the same value, so a report that
	// arrived but whose answer was lost does not become a second event.
	EventID string

	RunID string

	// State is the state the run entered, and Mode is how the run was started. The
	// control plane derives the event from the state the run is already in, so neither
	// the caller nor a compromised agent can misreport what came before.
	State string
	Mode  string

	// At is when the transition was observed, which is not when it was posted.
	At time.Time

	// Attributes carries counts, durations and identifiers under a closed schema. The
	// control plane refuses anything else.
	Attributes map[string]any
}

// Result is what the control plane did with a report.
type Result struct {
	// Sequence is the sequence of the event that records the state.
	Sequence int64 `json:"sequence"`

	// Appended is false when the report added no event, because the run was already in
	// that state. It is a success for the caller either way: it has a sequence.
	Appended bool `json:"appended"`
}

// Client posts reports for the workspace its token was minted for.
type Client struct {
	http      *http.Client
	url       string
	tokenPath string
}

// New returns a Client. The base URL is the control plane's root, and the token path is a
// file whose contents are the reporting token.
//
// There is no workspace argument. The token names the workspace and the control plane
// resolves it from there, so a caller has nothing to get wrong and nothing to spoof.
func New(baseURL, tokenPath string) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("platform url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("platform url %q is not absolute", baseURL)
	}
	if tokenPath == "" {
		return nil, errors.New("the report token path is required")
	}
	return &Client{
		http:      &http.Client{Timeout: 10 * time.Second},
		url:       strings.TrimSuffix(baseURL, "/") + contract.ReportPath,
		tokenPath: tokenPath,
	}, nil
}

// Post sends one report and returns what the control plane did with it.
func (c *Client) Post(ctx context.Context, r Report) (Result, error) {
	token, err := c.token()
	if err != nil {
		return Result{}, err
	}

	body, err := json.Marshal(map[string]any{
		"event_id":    r.EventID,
		"run_id":      r.RunID,
		"state":       r.State,
		"mode":        r.Mode,
		"occurred_at": r.At.UTC().Format(time.RFC3339Nano),
		"attributes":  r.Attributes,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrRejected, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure says nothing about whether the report arrived, which is
		// why the same event id is what makes the retry safe.
		return Result{}, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply := readBounded(resp.Body)

	switch {
	case resp.StatusCode == http.StatusAccepted:
		var out Result
		if err := json.Unmarshal(reply, &out); err != nil {
			// Accepted but unreadable. The event is recorded, and the caller cannot
			// tell which one, so this is worth retrying with the same id.
			return Result{}, fmt.Errorf("%w: unreadable answer: %v", ErrTransient, err)
		}
		return out, nil
	case resp.StatusCode == http.StatusConflict:
		return Result{}, fmt.Errorf("%w: %s", ErrConflict, truncate(reply))
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		return Result{}, fmt.Errorf("%w: %d %s", ErrTransient, resp.StatusCode, truncate(reply))
	default:
		return Result{}, fmt.Errorf("%w: %d %s", ErrRejected, resp.StatusCode, truncate(reply))
	}
}

// NewEventID returns an idempotency key. It is random rather than derived, because two
// genuine transitions of one run can enter the same state, and a derived key would make
// the second one look like a retry of the first.
func NewEventID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// token reads the token for this call.
//
// It is read every time and never cached. The control plane rotates the token at half its
// TTL and the kubelet replaces the mounted file in place, so a cached token turns into a
// 401 that looks like a bug in the caller. The bridge reads its token the same way for the
// same reason.
func (c *Client) token() (string, error) {
	raw, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("read the report token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("the report token file is empty")
	}
	return token, nil
}

func readBounded(r io.Reader) []byte {
	body, err := io.ReadAll(io.LimitReader(r, maxBody))
	if err != nil {
		return nil
	}
	return body
}

// truncate shortens a reply for an error message, so a large body cannot fill a log.
func truncate(body []byte) string {
	const limit = 200
	if len(body) > limit {
		return string(body[:limit]) + "..."
	}
	return string(body)
}
