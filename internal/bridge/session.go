package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
)

// ErrNotRunning is returned when Pi is not currently running, for example while
// the supervisor is between restarts.
var ErrNotRunning = errors.New("bridge: pi is not running")

// subscriberBuffer bounds a client's backlog. A client that falls further behind
// than this is dropped rather than allowed to stall Pi's stdout.
const subscriberBuffer = 256

// providerIDRE accepts a plausible provider id.
//
// The bridge deliberately keeps NO allowlist of providers. Pi knows its own
// providers, a Pi upgrade adds more, and a hardcoded list silently rejects a
// provider the user can legitimately authenticate to -- which is exactly what
// happened: nine providers were listed, and every token-auth provider was
// missing. The value only ever becomes an auth.json key and a --provider argument,
// so a wrong one fails in Pi with Pi's own error.
var providerIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Session supervises one `pi --mode rpc` child and fans its event stream out to
// connected browsers.
//
// If Pi exits, the supervisor restarts it. The conversation survives because
// sessions are persisted with --session-dir onto the workspace volume
// (handoff §7).
type Session struct {
	piBin  string
	piArgs []string
	logger *slog.Logger

	// agentDir is Pi's agent directory (PI_CODING_AGENT_DIR). It lives on the
	// workspace volume, so a credential the user supplies persists there
	// (handoff §8.4).
	agentDir string

	mu      sync.Mutex
	proc    *Process
	subs    map[int]*subscriber
	nextSub int
	busy    bool
	// observer is told about each turn boundary. Nil reports nothing.
	observer RunObserver

	// provider is the provider the user most recently supplied a key for. It is
	// passed to Pi as --provider so the stored credential is the one used.
	provider string

	// model is the model the user chose, passed to Pi as --model. Empty means Pi's
	// own default, which on a free provider tier may be a model the key cannot use.
	model string

	// restart is closed when a credential arrives, so the supervisor replaces the
	// child and it picks the new credential up.
	restart chan struct{}

	// redactor keeps known secrets out of everything sent to a browser.
	redactor *Redactor
}

type subscriber struct {
	ch   chan []byte
	once sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

// childEnv is the extra environment Pi needs.
//
// NODE_EXTRA_CA_CERTS is how a Node process is told to trust an additional
// certificate, and Node reads it when the process starts -- so it has to be set by
// whatever starts Pi, which is this bridge. Without it the broker's self-signed
// certificate is rejected, and the failure arrives as a connection error that says
// nothing at all about trust (handoff §8.5).
//
// The value is a path, never a secret, so it is safe to pass to a child. Nothing is
// set when the broker is on plaintext, which is what an unset SCARAB_BROKER_CA
// means.
func childEnv() []string {
	ca := os.Getenv(contract.EnvBrokerCA)
	if ca == "" {
		return nil
	}
	return []string{nodeExtraCACerts + "=" + ca}
}

// nodeExtraCACerts is Node's trust-anchor variable. It is a Node detail, which is
// why it lives at the boundary where Node is launched rather than in the contract
// pestilence writes to.
const nodeExtraCACerts = "NODE_EXTRA_CA_CERTS"

// NewSession returns a Session. agentDir may be empty, in which case credentials
// cannot be supplied. A nil logger discards.
func NewSession(piBin string, piArgs []string, agentDir string, logger *slog.Logger) *Session {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	provider, model := readDefaults(agentDir)
	s := &Session{
		piBin:    piBin,
		piArgs:   piArgs,
		agentDir: agentDir,
		logger:   logger,
		subs:     map[int]*subscriber{},
		provider: provider,
		model:    model,
		redactor: NewRedactor(),
	}
	s.RefreshSecrets()
	return s
}

// RefreshSecrets re-reads the values that must never reach a browser.
//
// Called at startup, whenever a credential changes, and on each client connect,
// because pestilence rotates the capability token at half its TTL and the mounted
// file changes underneath us.
func (s *Session) RefreshSecrets() {
	s.redactor.Set(knownSecrets(s.agentDir, os.Getenv(contract.EnvTokenPath))...)
}

// Run supervises Pi until ctx is cancelled. It blocks, so callers usually run it
// in a goroutine.
func (s *Session) Run(ctx context.Context) {
	const restartDelay = 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		s.mu.Lock()
		opts := StartOptions{Bin: s.piBin, Args: s.buildArgsLocked(), Env: childEnv()}
		restart := s.restartChanLocked()
		s.mu.Unlock()

		proc, err := StartProcess(ctx, opts)
		if err != nil {
			s.logger.Error("cannot start pi", "bin", s.piBin, "err", err)
			if !sleepCtx(ctx, restartDelay) {
				return
			}
			continue
		}
		s.setProc(proc)
		s.logger.Info("pi started", "pid", proc.cmd.Process.Pid)

		if !s.drain(ctx, proc, restart) {
			return
		}

		err = proc.Wait()
		s.setProc(nil)
		s.logger.Warn("pi exited", "err", err)

		if !sleepCtx(ctx, restartDelay) {
			return
		}
	}
}

// drain forwards Pi's records until it exits, the credential changes, or ctx is
// cancelled. It reports whether the supervisor should carry on.
//
// Extracted from Run: the select is the part with the most branching, and keeping
// it separate keeps the supervisor loop readable.
func (s *Session) drain(ctx context.Context, proc *Process, restart <-chan struct{}) bool {
	// Drain until stdout closes. Pi honours stdout backpressure, so a stalled
	// reader stalls the agent; that is the correct behaviour for a local child.
	for {
		select {
		case record, ok := <-proc.Events():
			if !ok {
				return true
			}
			s.observe(record)
			s.broadcast(record)
		case <-restart:
			// A credential arrived: replace the child so it reads the new auth.json.
			// The Pi session on the volume is preserved.
			s.logger.Info("credential stored; restarting pi")
			proc.Kill()
			return true
		case <-ctx.Done():
			proc.Kill()
			return false
		}
	}
}

// SetModelKey persists an API key and/or a model choice, then restarts Pi so they
// take effect.
//
// An empty key means "keep the stored credential and just select the provider or
// model", which is the common case when the only problem is that Pi's default
// model is not available on the user's plan. An empty model means "use Pi's
// default".
//
// The key is written to auth.json, never to the bridge's own state and never to a
// log. Persisting it is the point: the user supplies their own credential once,
// and it survives a pod restart (handoff §8.4).
func (s *Session) SetModelKey(provider, key, model string) error {
	provider = strings.TrimSpace(provider)
	key = strings.TrimSpace(key)
	model = strings.TrimSpace(model)

	if !providerIDRE.MatchString(provider) {
		return fmt.Errorf("bridge: %q is not a provider id", provider)
	}
	switch {
	case key != "":
		if err := putAPIKey(s.agentDir, provider, key); err != nil {
			return err
		}
	case hasCredential(s.agentDir, provider):
		// Keep what is stored.
	default:
		return fmt.Errorf("bridge: no stored key for %q; supply one", provider)
	}
	if err := writeDefaults(s.agentDir, provider, model); err != nil {
		return err
	}
	// A new key is a new secret to keep out of the browser. Safe to call before
	// taking s.mu: the redactor has its own lock.
	s.RefreshSecrets()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = provider
	s.model = model
	if s.restart != nil {
		close(s.restart)
		s.restart = nil
	}
	return nil
}

// Model reports the selected model, or "" for Pi's default.
func (s *Session) Model() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// RequestState asks Pi for what a freshly connected page needs: the running
// model, the transcript, and the model catalog. Without this a reloaded page
// shows a blank transcript and an empty provider list, because the bridge only
// forwards live events.
//
// The responses go to every subscriber, which is why the page replaces rather
// than appends when it renders the history.
func (s *Session) RequestState() {
	proc := s.current()
	if proc == nil {
		return
	}
	stamp := time.Now().UnixNano()
	for _, command := range []string{"get_state", "get_messages", "get_available_models"} {
		_ = proc.Send(map[string]any{
			"id":   fmt.Sprintf("%s-%d", command, stamp),
			"type": command,
		})
	}
}

// HasModelKey reports whether any model credential is stored. It backs the
// frontend's prompt.
func (s *Session) HasModelKey() bool {
	return len(configuredProviders(s.agentDir)) > 0
}

// ConfiguredProviders lists the providers with a stored credential.
func (s *Session) ConfiguredProviders() []string {
	return configuredProviders(s.agentDir)
}

// Subscribe returns a channel of raw Pi records and a cancel function. The
// channel closes on cancel or when the subscriber is dropped for falling behind.
func (s *Session) Subscribe() (<-chan []byte, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.nextSub
	s.nextSub++
	sub := &subscriber{ch: make(chan []byte, subscriberBuffer)}
	s.subs[id] = sub

	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[id]; ok {
			delete(s.subs, id)
			sub.close()
		}
	}
	return sub.ch, cancel
}

// Running reports whether Pi is currently up. It backs the /healthz probe.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// Prompt sends a user message. While Pi is streaming, the message is queued as a
// steer, because a bare prompt during streaming is rejected by Pi.
func (s *Session) Prompt(message string) error {
	s.mu.Lock()
	proc, busy := s.proc, s.busy
	s.mu.Unlock()
	if proc == nil {
		return ErrNotRunning
	}
	command := map[string]any{"type": "prompt", "message": message}
	if busy {
		command["streamingBehavior"] = "steer"
	}
	return proc.Send(command)
}

// Abort cancels the current run.
func (s *Session) Abort() error {
	proc := s.current()
	if proc == nil {
		return ErrNotRunning
	}
	return proc.Send(map[string]any{"type": "abort"})
}

// Shutdown asks Pi to exit cleanly by closing its stdin, as rpc.md prescribes.
func (s *Session) Shutdown() {
	if proc := s.current(); proc != nil {
		_ = proc.CloseStdin()
	}
}

func (s *Session) current() *Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

// buildArgsLocked appends --provider when one can be determined, so Pi uses the
// stored credential rather than its default provider. Callers must hold s.mu.
func (s *Session) buildArgsLocked() []string {
	args := append([]string{}, s.piArgs...)
	if p := s.activeProviderLocked(); p != "" {
		args = append(args, "--provider", p)
	}
	if s.model != "" {
		args = append(args, "--model", s.model)
	}
	return args
}

// activeProviderLocked prefers the provider the user chose this session, and
// falls back to the only stored provider when there is exactly one. Callers must
// hold s.mu.
func (s *Session) activeProviderLocked() string {
	if s.provider != "" {
		return s.provider
	}
	configured := configuredProviders(s.agentDir)
	if len(configured) == 1 {
		return configured[0]
	}
	return ""
}

// restartChanLocked returns the channel closed when a credential arrives.
// Callers must hold s.mu.
func (s *Session) restartChanLocked() chan struct{} {
	if s.restart == nil {
		s.restart = make(chan struct{})
	}
	return s.restart
}

func (s *Session) setProc(p *Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proc = p
	if p == nil {
		s.busy = false
	}
}

func (s *Session) setBusy(busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = busy
}

// envelope is the only part of a record the session inspects: the type, for run
// state. Everything else is forwarded verbatim.
type envelope struct {
	Type string `json:"type"`
}

// observe tracks whether a run is active, so Prompt knows to steer rather than
// start a new turn, and tells the run observer about the turn boundary.
// prompt. agent_settled, not agent_end, means Pi has no automatic work left.
func (s *Session) observe(record []byte) {
	var env envelope
	if err := json.Unmarshal(record, &env); err != nil {
		return
	}
	switch env.Type {
	case "agent_start":
		s.setBusy(true)
		s.notifyRunObserver(true)
	case "agent_settled":
		s.setBusy(false)
		s.notifyRunObserver(false)
	}
}

// SetRunObserver attaches an observer, which is told when the run begins a turn and when it
// settles.
//
// A setter and not a constructor parameter, so that a session can be built and tested
// without a broker. It must be called before Run, because a session with no observer simply
// reports nothing and there is no later point at which that changes.
func (s *Session) SetRunObserver(obs RunObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = obs
}

// notifyRunObserver tells the observer about a turn boundary, if one is attached.
func (s *Session) notifyRunObserver(started bool) {
	s.mu.Lock()
	obs := s.observer
	s.mu.Unlock()
	if obs == nil {
		return
	}
	// Called with the lock RELEASED. The observer enqueues, and holding the session lock
	// across that would let a broker that is not keeping up stall the event loop.
	if started {
		obs.RunStarted()
		return
	}
	obs.RunSettled()
}

func (s *Session) broadcast(record []byte) {
	// Everything a browser receives passes through here -- live events and the
	// transcript replayed on connect alike -- so this is the one place redaction
	// has to happen.
	record = s.redactor.Apply(record)

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sub := range s.subs {
		select {
		case sub.ch <- record:
		default:
			delete(s.subs, id)
			sub.close()
			s.logger.Warn("dropped a bridge client that fell behind", "id", id)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
