package broker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
	"github.com/gobackto-work/scarab/internal/report"
)

// Recorder records a worker's run state. It is an interface so the HTTP layer can be tested
// without a control plane.
//
// A Recorder must never fail a request. Reporting is a side effect of spawning a worker and
// not a precondition for it, because refusing to start work when a notification cannot be
// queued would turn a reporting outage into a work outage.
type Recorder interface {
	// Started records that a worker began.
	Started(ctx context.Context, agentID string)
	// Stopped records that a person ended a worker.
	Stopped(ctx context.Context, agentID string)
	// Observed relays a state the bridge saw for the root run. The bridge sees turn
	// boundaries and the broker cannot, so this is the one signal the broker relays rather
	// than derives.
	Observed(ctx context.Context, runID, eventID, state string)
}

// nopRecorder is what a Server without a control plane gets, so that the HTTP layer needs
// no nil checks and a test needs no control plane.
type nopRecorder struct{}

func (nopRecorder) Started(context.Context, string)                  {}
func (nopRecorder) Stopped(context.Context, string)                  {}
func (nopRecorder) Observed(context.Context, string, string, string) {}

// Reporter records the run state of this workspace's workers.
//
// The broker is the reporter because it is the only component that outlives them. A
// worker's process IS its run, so a worker cannot report the state it reaches by dying, and
// a worker that is killed reports nothing at all -- which is the case that matters most,
// because a parent waiting on that worker would wait for ever.
//
// Every report is best effort and none is retried here. The poll that drives Observe runs
// again and derives the same event id, so a re-report IS the retry. Losing one poll
// therefore costs one interval and never one event.
type Reporter struct {
	client *report.Client
	logger *slog.Logger

	// rootRun is the root pod this reporter last saw. A root run's identity is its pod's
	// UID, so a replaced pod is a different run, and the one before it can never be ended by
	// anybody else. Guarded because the server reports workers while the poll reports the
	// root.
	mu      sync.Mutex
	rootRun string
}

// NewReporter returns a Reporter.
func NewReporter(client *report.Client, logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{client: client, logger: logger}
}

// Started records that a worker began.
//
// The agent id IS the run id. The root agent receives the id from agents_spawn and writes
// its result under `.agents/<agent-id>/`, so a parent waiting for a child already holds the
// identifier of the run it is waiting on. Nothing has to be plumbed to join the two.
func (r *Reporter) Started(ctx context.Context, agentID string) {
	r.send(ctx, agentID+":"+contract.StateRunning, agentID, contract.StateRunning, contract.ModeBatch)
}

// Stopped records that a person ended a worker.
//
// It is reported at the stop, and not left to the poll, because stopping deletes the job
// and a deleted job is indistinguishable from one that never existed.
func (r *Reporter) Stopped(ctx context.Context, agentID string) {
	r.send(ctx, agentID+":"+contract.StateCancelled, agentID, contract.StateCancelled, contract.ModeBatch)
}

// Observe records the state of every worker.
//
// The START is reported for every worker, whatever phase it is in, before anything else.
// A worker can begin and end inside one poll interval, and a terminal state on its own is
// refused: a run cannot end without having begun. So the poll asserts the start it knows
// happened -- the job exists and a pod was created for it -- and then the end, if there is
// one. Both are no-ops once the record holds them, so the poll stays idempotent.
//
// The alternative, deriving the start from the job's phase, is what this replaces: a phase
// that says running can fall entirely between two polls, and then the run is invisible.
func (r *Reporter) Observe(ctx context.Context, agents []Agent) {
	for _, a := range agents {
		r.send(ctx, a.ID+":"+contract.StateRunning, a.ID, contract.StateRunning, contract.ModeBatch)
		if state, ok := terminalState(a.State); ok {
			r.send(ctx, a.ID+":"+state, a.ID, state, contract.ModeBatch)
		}
	}
}

// ObserveRoot reports the root run's terminal state, and closes the run that a replaced
// bridge left behind.
//
// A pod's phase says a running pod's run is somewhere non-terminal, and nothing in the pod
// distinguishes a run that is working from one that is waiting for a person. Reporting
// `running` from here would therefore fabricate a `resumed` every time the bridge had
// recorded a `waiting`, so the broker derives the END of the run and leaves the rest to the
// bridge.
//
// A root run's identity is its pod's UID. So when the pod is replaced -- an upgrade, a crash,
// a moved image -- the run it was serving is a different run from the one now running, and
// the bridge that knew it is gone. Nothing can report that run's end but this, the only
// component that can see a pod go. Without it the run stays `waiting` for ever, which is a
// permanent lie in the log and a false alarm the moment anything notifies on it.
//
// It needs no durable state: the run id is the bridge's pod, and a broker restart does not
// change it, so only a replaced pod looks new.
func (r *Reporter) ObserveRoot(ctx context.Context, root Agent) {
	r.mu.Lock()
	previous := r.rootRun
	r.rootRun = root.ID
	r.mu.Unlock()

	if previous != "" && previous != root.ID {
		r.send(ctx, previous+":"+contract.StateFailed, previous, contract.StateFailed, contract.ModeInteractive)
	}

	switch root.State {
	case StateSucceeded, StateFailed:
		r.send(ctx, root.ID+":"+root.State, root.ID, root.State, contract.ModeInteractive)
	}
}

// Observed relays the root run's state as the bridge reported it.
//
// The bridge holds the session, so it is the only party that can tell a finished turn from
// one that is waiting for a person. It has no Kubernetes access and no control-plane
// credential, so it tells the broker and the broker reports.
//
// Only a non-terminal state is accepted, and the bridge supplies the event id because it
// owns the retry. A terminal state from the bridge would be a guess: the bridge IS the
// process, so it cannot see the process end.
func (r *Reporter) Observed(ctx context.Context, runID, eventID, state string) {
	switch state {
	case contract.StateRunning, contract.StateWaiting:
	default:
		r.logger.Warn("the bridge reported a state only the broker may report",
			"run", runID, "state", state)
		return
	}
	r.send(ctx, eventID, runID, state, contract.ModeInteractive)
}

// terminalState returns the run state for a worker that has ended.
//
// Only the terminal states are here. A worker's running state is NOT derived from the job's
// phase: Observe asserts it for every job, for the reason given there. A phase that says
// running is an observation, and observations can be missed.
func terminalState(agentState string) (string, bool) {
	switch agentState {
	case StateSucceeded:
		return contract.StateSucceeded, true
	case StateFailed:
		return contract.StateFailed, true
	}
	return "", false
}

// send posts one report. It returns nothing, because its callers cannot act on a failure:
// they are either a request that must not fail or a poll that will run again.
func (r *Reporter) send(ctx context.Context, eventID, runID, state, mode string) {
	_, err := r.client.Post(ctx, report.Report{
		EventID: eventID,
		RunID:   runID,
		State:   state,
		Mode:    mode,
		At:      time.Now().UTC(),
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, report.ErrConflict):
		// Logged and NOT swallowed. A conflict is one of two things: a poll that arrives
		// after the run was stopped, which is harmless, or a run the record has never seen
		// begin, which means an event was lost. They are indistinguishable from here, so
		// both are visible -- a refused report is never something to hide in a debug log.
		r.logger.Warn("the record refused a run state", "run", runID, "state", state, "err", err)
	default:
		r.logger.Warn("could not report a run state", "run", runID, "state", state, "err", err)
	}
}
