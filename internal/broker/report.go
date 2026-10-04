package broker

import (
	"context"
	"errors"
	"log/slog"
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
}

// nopRecorder is what a Server without a control plane gets, so that the HTTP layer needs
// no nil checks and a test needs no control plane.
type nopRecorder struct{}

func (nopRecorder) Started(context.Context, string) {}
func (nopRecorder) Stopped(context.Context, string) {}

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
	r.post(ctx, agentID, contract.StateRunning)
}

// Stopped records that a person ended a worker.
//
// It is reported at the stop, and not left to the poll, because stopping deletes the job
// and a deleted job is indistinguishable from one that never existed.
func (r *Reporter) Stopped(ctx context.Context, agentID string) {
	r.post(ctx, agentID, contract.StateCancelled)
}

// Observe records the state of every worker that has one worth recording.
//
// It is called on every poll, and it is idempotent by construction: the event id is derived
// from the run and the state, and a run reaches each of these states once. So a worker that
// is still running, or one whose terminal state was already recorded, costs one refused
// report rather than a duplicate event.
func (r *Reporter) Observe(ctx context.Context, agents []Agent) {
	for _, a := range agents {
		state, ok := runStateFor(a.State)
		if !ok {
			continue
		}
		r.post(ctx, a.ID, state)
	}
}

// runStateFor maps a worker's state onto a run state.
//
// Pending is absent on purpose: a run that has not started has no state to record, and the
// record refuses a run that begins anywhere but running.
func runStateFor(agentState string) (string, bool) {
	switch agentState {
	case StateRunning:
		return contract.StateRunning, true
	case StateSucceeded:
		return contract.StateSucceeded, true
	case StateFailed:
		return contract.StateFailed, true
	}
	return "", false
}

// post sends one report. It returns nothing, because its callers cannot act on a failure:
// they are either a request that must not fail or a poll that will run again.
func (r *Reporter) post(ctx context.Context, runID, state string) {
	_, err := r.client.Post(ctx, report.Report{
		// Derived, not random, and safe because a batch run reaches each of these states
		// once. It is what makes a repeated poll idempotent and a lost report
		// self-healing.
		EventID: runID + ":" + state,
		RunID:   runID,
		State:   state,
		Mode:    contract.ModeBatch,
		At:      time.Now().UTC(),
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, report.ErrConflict):
		// Expected, and not a fault. The record already holds this state, or it holds a
		// state this run cannot leave -- for instance a poll that arrives after the run was
		// stopped. The record is the authority and there is nothing to do about it.
		r.logger.Debug("run state not recorded", "run", runID, "state", state, "err", err)
	default:
		r.logger.Warn("could not report a run state", "run", runID, "state", state, "err", err)
	}
}
