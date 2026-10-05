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
	if got["runId"] != "01M3ZX0X9EBQ25P2PC5BK63C5B" {
		t.Errorf("run_id = %v", got["runId"])
	}
}

func TestStoppedRecordsACancelledRun(t *testing.T) {
	cp := newControlPlane(t)
	reporterFor(t, cp).Stopped(context.Background(), "01M3ZX0X9EBQ25P2PC5BK63C5C")

	if len(cp.posts) != 1 || cp.posts[0]["state"] != contract.StateCancelled {
		t.Fatalf("posts = %+v, want one cancellation", cp.posts)
	}
}

// Every worker's START is reported, whatever its phase, so that a run which begins and ends
// between two polls still records both events.
func TestObserveReportsTheStartBeforeTheEnd(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)

	reporter.Observe(context.Background(), []Agent{
		{ID: "a", State: StatePending},
		{ID: "b", State: StateSucceeded},
		{ID: "c", State: StateFailed},
		{ID: "d", State: "something-new"},
	})

	if len(cp.posts) != 6 {
		t.Fatalf("the control plane saw %d posts, want 6: %+v", len(cp.posts), cp.posts)
	}

	// Every run has a start, whatever the job's phase.
	starts := map[string]int{}
	for i, post := range cp.posts {
		if post["state"] == contract.StateRunning {
			starts[post["runId"].(string)] = i
		}
	}
	for _, run := range []string{"a", "b", "c", "d"} {
		if _, ok := starts[run]; !ok {
			t.Errorf("run %s has no start, so its end would be refused", run)
		}
	}

	// And each end follows its own start. That is the order the record needs; the order
	// BETWEEN runs is not something it cares about.
	ends := 0
	for i, post := range cp.posts {
		switch post["state"] {
		case contract.StateSucceeded, contract.StateFailed:
			ends++
			if starts[post["runId"].(string)] > i {
				t.Errorf("run %v ended before it started", post["runId"])
			}
		}
	}
	if ends != 2 {
		t.Errorf("got %d ends, want 2", ends)
	}
}

// The case this exists for. A worker began and ended inside one poll interval, so its running
// phase was never observed -- and a terminal state on its own is refused, because a run
// cannot end without having begun. The poll therefore asserts the start it knows happened.
func TestAWorkerThatEndedBeforeThePollStillRecordsBothEvents(t *testing.T) {
	cp := newControlPlane(t)
	reporterFor(t, cp).Observe(context.Background(), []Agent{{ID: "a", State: StateFailed}})

	if len(cp.posts) != 2 {
		t.Fatalf("the control plane saw %d posts, want a start and an end", len(cp.posts))
	}
	if cp.posts[0]["state"] != contract.StateRunning || cp.posts[1]["state"] != contract.StateFailed {
		t.Errorf("posts = %v, want running then failed", cp.posts)
	}
	if cp.posts[0]["eventId"] == cp.posts[1]["eventId"] {
		t.Error("the start and the end share an event id, so one would be dropped as a retry")
	}
}

// The event id is derived from the run and the state, which is what makes a repeated poll
// idempotent instead of a source of duplicate events.
func TestRepeatedObservationsCarryTheSameEventIDs(t *testing.T) {
	cp := newControlPlane(t)
	reporter := reporterFor(t, cp)
	agents := []Agent{{ID: "a", State: StateSucceeded}}

	reporter.Observe(context.Background(), agents)
	reporter.Observe(context.Background(), agents)

	if len(cp.posts) != 4 {
		t.Fatalf("the control plane saw %d posts, want 4 (a start and an end, twice)", len(cp.posts))
	}
	for i := range 2 {
		if cp.posts[i]["eventId"] != cp.posts[i+2]["eventId"] {
			t.Errorf("the second poll carried a different event id for %v (%v vs %v)",
				cp.posts[i]["state"], cp.posts[i]["eventId"], cp.posts[i+2]["eventId"])
		}
	}
	if id, _ := cp.posts[0]["eventId"].(string); !strings.HasPrefix(id, "a:") {
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

	if len(cp.posts) != 4 {
		t.Fatalf("the control plane saw %d posts, want 4: a conflict stopped the loop", len(cp.posts))
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
	if got["runId"] != "uid-1" {
		t.Errorf("run_id = %v, want the pod uid", got["runId"])
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
	if got["eventId"] != "an-event-id" {
		t.Errorf("event_id = %v, want the id the bridge supplied", got["eventId"])
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
