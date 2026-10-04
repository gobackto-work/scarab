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

// recordingRecorder captures what the broker told it, so a test can assert that a run
// identity came from the pod and not from a request.
type recordingRecorder struct {
	observed []string
	started  []string
	stopped  []string
}

func (r *recordingRecorder) Started(_ context.Context, id string) { r.started = append(r.started, id) }
func (r *recordingRecorder) Stopped(_ context.Context, id string) { r.stopped = append(r.stopped, id) }
func (r *recordingRecorder) Observed(_ context.Context, runID, eventID, state string) {
	r.observed = append(r.observed, runID+"|"+eventID+"|"+state)
}

// The broker derives the END of the root run and nothing else. A pod's Running phase cannot
// tell a run that is working from one that is waiting for a person, so reporting it would
// fabricate a resumed every time the bridge had recorded a waiting.
func TestObserveRootReportsOnlyTheEnd(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)

	reporter.ObserveRoot(context.Background(), Agent{ID: "uid-1", State: StateRunning})
	if len(cp.posts) != 0 {
		t.Errorf("a running root was reported as %+v, want nothing", cp.posts)
	}

	reporter.ObserveRoot(context.Background(), Agent{ID: "uid-1", State: StateSucceeded})
	if len(cp.posts) != 1 {
		t.Fatalf("the control plane saw %d posts, want 1", len(cp.posts))
	}
	got := cp.posts[0]
	if got["state"] != contract.StateSucceeded {
		t.Errorf("state = %v, want %q", got["state"], contract.StateSucceeded)
	}
	// The root run is the interactive one. Reporting batch here would make the record
	// refuse the waiting states the bridge sends for the same run.
	if got["mode"] != contract.ModeInteractive {
		t.Errorf("mode = %v, want %q", got["mode"], contract.ModeInteractive)
	}
	if got["run_id"] != "uid-1" {
		t.Errorf("run_id = %v, want the pod uid", got["run_id"])
	}
}

func TestTheBridgeRelaysAWaitingState(t *testing.T) {
	cp := newControlPlane(t)
	reporterFor(t, cp).Observed(context.Background(), "uid-1", "an-event-id", contract.StateWaiting)

	if len(cp.posts) != 1 {
		t.Fatalf("the control plane saw %d posts, want 1", len(cp.posts))
	}
	got := cp.posts[0]
	if got["state"] != contract.StateWaiting {
		t.Errorf("state = %v, want %q", got["state"], contract.StateWaiting)
	}
	if got["mode"] != contract.ModeInteractive {
		t.Errorf("mode = %v, want %q, so the record would refuse a waiting run", got["mode"], contract.ModeInteractive)
	}
	// The bridge supplies the event id because it owns the retry. Deriving one here would
	// make a second visit to waiting look like a retry of the first.
	if got["event_id"] != "an-event-id" {
		t.Errorf("event_id = %v, want the id the bridge supplied", got["event_id"])
	}
}

// The bridge IS the process, so it cannot see the process end. A terminal state from it
// would be a guess, and the broker derives those from the pod instead.
func TestTheBridgeCannotReportATerminalState(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)

	for _, state := range []string{contract.StateSucceeded, contract.StateFailed, contract.StateCancelled} {
		reporter.Observed(context.Background(), "uid-1", "an-event-id", state)
	}
	if len(cp.posts) != 0 {
		t.Errorf("the bridge reported %+v, want nothing accepted", cp.posts)
	}
}
