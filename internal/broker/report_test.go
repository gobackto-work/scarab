package broker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
	"github.com/gobackto-work/scarab/internal/report"
)

// controlPlane is a stand-in for the ingest endpoint. It records what it was sent and
// answers with whatever the test sets.
type controlPlane struct {
	*httptest.Server
	posts []map[string]any
	code  int
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{code: http.StatusAccepted}
	cp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.posts = append(cp.posts, body)
		w.WriteHeader(cp.code)
		_, _ = w.Write([]byte(`{"sequence":1,"appended":true}`))
	}))
	t.Cleanup(cp.Close)
	return cp
}

func reporterFor(t *testing.T, cp *controlPlane) *Reporter {
	t.Helper()
	token := filepath.Join(t.TempDir(), "report-token")
	if err := os.WriteFile(token, []byte("a-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	client, err := report.New(cp.URL, token)
	if err != nil {
		t.Fatalf("report.New: %v", err)
	}
	return NewReporter(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestStartedRecordsARunningBatchRun(t *testing.T) {
	cp := newControlPlane(t)
	reporterFor(t, cp).Started(context.Background(), "01M3ZX0X9EBQ25P2PC5BK63C5B")

	if len(cp.posts) != 1 {
		t.Fatalf("the control plane saw %d posts, want 1", len(cp.posts))
	}
	got := cp.posts[0]
	if got["state"] != contract.StateRunning {
		t.Errorf("state = %v, want %q", got["state"], contract.StateRunning)
	}
	// A worker is a job. The control plane refuses waiting for a batch run, so reporting
	// anything else here would be a state it could never leave.
	if got["mode"] != contract.ModeBatch {
		t.Errorf("mode = %v, want %q", got["mode"], contract.ModeBatch)
	}
	if got["run_id"] != "01M3ZX0X9EBQ25P2PC5BK63C5B" {
		t.Errorf("run_id = %v", got["run_id"])
	}
}

func TestStoppedRecordsACancelledRun(t *testing.T) {
	cp := newControlPlane(t)
	reporterFor(t, cp).Stopped(context.Background(), "01M3ZX0X9EBQ25P2PC5BK63C5C")

	if len(cp.posts) != 1 || cp.posts[0]["state"] != contract.StateCancelled {
		t.Fatalf("posts = %+v, want one cancellation", cp.posts)
	}
}

// The poll reports every worker that has a state worth recording, and skips the ones that
// do not.
func TestObserveReportsEveryWorkerWithAState(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)

	reporter.Observe(context.Background(), []Agent{
		{ID: "a", State: StatePending},
		{ID: "b", State: StateRunning},
		{ID: "c", State: StateSucceeded},
		{ID: "d", State: StateFailed},
		{ID: "e", State: "something-new"},
	})

	// Three, from four states: pending is not a run state and an unknown one is not ours to
	// interpret.
	if len(cp.posts) != 3 {
		t.Fatalf("the control plane saw %d posts, want 3: %+v", len(cp.posts), cp.posts)
	}
	want := map[string]string{"b": contract.StateRunning, "c": contract.StateSucceeded, "d": contract.StateFailed}
	for _, p := range cp.posts {
		runID, _ := p["run_id"].(string)
		if want[runID] != p["state"] {
			t.Errorf("run %s was reported as %v, want %q", runID, p["state"], want[runID])
		}
	}
}

// The event id is derived from the run and the state, which is what makes a repeated poll
// idempotent instead of a source of duplicate events.
func TestRepeatedObservationsCarryTheSameEventID(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)
	agents := []Agent{{ID: "a", State: StateSucceeded}}

	reporter.Observe(context.Background(), agents)
	reporter.Observe(context.Background(), agents)

	if len(cp.posts) != 2 {
		t.Fatalf("the control plane saw %d posts, want 2", len(cp.posts))
	}
	if cp.posts[0]["event_id"] != cp.posts[1]["event_id"] {
		t.Errorf("the second poll carried a different event id (%v vs %v), so the record would hold two events for one run",
			cp.posts[0]["event_id"], cp.posts[1]["event_id"])
	}
	if id, _ := cp.posts[0]["event_id"].(string); !strings.HasPrefix(id, "a:") {
		t.Errorf("event id %q is not derived from the run and the state", id)
	}
}

// One refused report must not stop the others: a run the record already holds a terminal
// state for is expected, and the rest of the workspace still needs reporting.
func TestOneRefusedReportDoesNotStopTheOthers(t *testing.T) {
	cp := newControlPlane(t)
	cp.code = http.StatusConflict
	reporter := reporterFor(t, cp)

	reporter.Observe(context.Background(), []Agent{
		{ID: "a", State: StateSucceeded},
		{ID: "b", State: StateFailed},
	})

	if len(cp.posts) != 2 {
		t.Fatalf("the control plane saw %d posts, want 2: a conflict stopped the loop", len(cp.posts))
	}
}

// Reporting is a side effect and never a precondition, so a control plane that is down must
// not make Spawn or Stop fail. The Reporter returns nothing for exactly this reason.
func TestAnUnreachableControlPlaneIsSurvivable(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)
	cp.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		reporter.Started(context.Background(), "a")
		reporter.Stopped(context.Background(), "b")
		reporter.Observe(context.Background(), []Agent{{ID: "c", State: StateSucceeded}})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reporting blocked against a control plane that is down")
	}
}
