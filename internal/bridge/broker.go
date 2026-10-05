package bridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
	"github.com/gobackto-work/scarab/internal/report"
)

const (
	// runQueueDepth bounds the observations waiting to be posted. A run settles a handful of
	// times a minute at most, so a deeper queue buys nothing and a full one means the broker
	// is gone.
	runQueueDepth = 32

	// runPostTimeout bounds one post.
	runPostTimeout = 5 * time.Second

	// runAttempts and runRetryDelay bound the retry for one observation. The event id is
	// stable across the attempts, so a retry of a report the broker already recorded is a
	// no-op rather than a second event.
	runAttempts   = 3
	runRetryDelay = 500 * time.Millisecond
)

// RunObserver is told when a run starts working and when it settles.
//
// The bridge is the only party that can tell those apart. Neither is visible from outside
// the pod: a pod's Running phase covers both a run that is working and one that is waiting
// for a person, which is why the bridge reports and the broker relays.
type RunObserver interface {
	// RunStarted is called when the agent begins a turn.
	RunStarted()
	// RunSettled is called when the agent finishes a turn and waits for the next prompt.
	RunSettled()
}

// BrokerObserver posts the root run's state to the broker, which is the only component that
// reports to the control plane.
//
// It is asynchronous and bounded, and that is the whole point. Reporting must never stall the
// session's event loop, so a full queue is dropped and logged rather than waited on. The cost
// of a drop is one missed state and not a stalled agent, and the next turn boundary reports
// the state again anyway.
type BrokerObserver struct {
	url       string
	tokenPath string
	http      *http.Client
	queue     chan observation
	logger    *slog.Logger
}

type observation struct {
	eventID string
	state   string
}

// NewBrokerObserver returns an observer that posts to the broker's run endpoint.
//
// caPath is the broker's own certificate authority. The broker serves TLS with a
// per-workspace certificate that it signed itself, so the system trust store rejects it.
// A report that fails to verify never arrives, which is a failure that looks exactly like
// an agent nobody prompted. Empty means the broker is on plaintext, and then there is
// nothing to trust.
func NewBrokerObserver(brokerURL, tokenPath, caPath string, logger *slog.Logger) (*BrokerObserver, error) {
	if !strings.HasPrefix(brokerURL, "http://") && !strings.HasPrefix(brokerURL, "https://") {
		return nil, fmt.Errorf("broker url %q is not absolute", brokerURL)
	}
	if tokenPath == "" {
		return nil, errors.New("the capability token path is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	client := &http.Client{Timeout: runPostTimeout}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read the broker CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the broker CA at %s holds no certificate", caPath)
		}
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}
	}

	return &BrokerObserver{
		url:       strings.TrimSuffix(brokerURL, "/") + "/run",
		tokenPath: tokenPath,
		http:      client,
		queue:     make(chan observation, runQueueDepth),
		logger:    logger,
	}, nil
}

// RunStarted reports that the run began working.
//
// It is reported at every turn start and not only at the first. The record derives the event
// from the state the run is already in, so a turn after a wait becomes `run.resumed` and the
// first becomes `run.started`, and neither the bridge nor the record has to guess which.
func (o *BrokerObserver) RunStarted() { o.enqueue(contract.StateRunning) }

// RunSettled reports that the run finished a turn and is waiting for a person.
func (o *BrokerObserver) RunSettled() { o.enqueue(contract.StateWaiting) }

// Run posts observations until the context is cancelled.
func (o *BrokerObserver) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case obs := <-o.queue:
			o.post(ctx, obs)
		}
	}
}

// enqueue adds an observation, and gives up rather than waits when the queue is full.
func (o *BrokerObserver) enqueue(state string) {
	// Random and not derived: a run enters these states repeatedly, so a derived key would
	// make the second visit look like a retry of the first.
	eventID, err := report.NewEventID()
	if err != nil {
		o.logger.Warn("could not make an event id, so the run state was not reported", "err", err)
		return
	}
	select {
	case o.queue <- observation{eventID: eventID, state: state}:
	default:
		o.logger.Warn("dropping a run state: the broker is not keeping up", "state", state)
	}
}

// post sends one observation, retrying with the same event id.
func (o *BrokerObserver) post(ctx context.Context, obs observation) {
	for attempt := 1; ; attempt++ {
		err := o.attempt(ctx, obs)
		if err == nil {
			return
		}
		if attempt >= runAttempts {
			o.logger.Warn("could not report a run state", "state", obs.state, "attempts", attempt, "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(runRetryDelay):
		}
	}
}

func (o *BrokerObserver) attempt(ctx context.Context, obs observation) error {
	body, err := json.Marshal(map[string]string{"event_id": obs.eventID, "state": obs.state})
	if err != nil {
		return err
	}
	// Read per call and never cached: pestilence rotates the token at half its TTL and the
	// kubelet replaces the mounted file in place.
	token, err := os.ReadFile(o.tokenPath)
	if err != nil {
		return fmt.Errorf("read the capability token: %w", err)
	}
	trimmed := strings.TrimSpace(string(token))
	if trimmed == "" {
		return errors.New("the capability token file is empty")
	}

	postCtx, cancel := context.WithTimeout(ctx, runPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+trimmed)

	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("the broker answered %d", resp.StatusCode)
	}
	return nil
}
