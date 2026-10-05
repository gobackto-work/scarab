// Command bridge serves the root agent's workspace endpoint.
//
// Pi has no HTTP mode, so the bridge runs `pi --mode rpc` as a child process and
// translates its JSONL protocol to a WebSocket for the browser
// (scarab/docs/architecture.md §8.2). It is the default landing page; the tenant may
// replace it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gobackto-work/scarab/internal/bridge"
	"github.com/gobackto-work/scarab/internal/contract"
)

// watchRunTurns attaches the run observer to the session and starts it.
//
// A bridge with no broker is a supported configuration: it serves the workspace and reports
// nothing, which is what a development cluster looks like.
func watchRunTurns(ctx context.Context, session *bridge.Session, logger *slog.Logger) {
	observer := runObserver(logger)
	if observer == nil {
		return
	}
	session.SetRunObserver(observer)
	go observer.Run(ctx)
}

// runObserver builds the root run's observer, or nil when the broker is not configured.
//
// A failure here does not stop the bridge. Losing a run state is bad; refusing to serve a
// workspace because a notification cannot be queued is worse, and it is the failure that
// would look like the platform being down.
func runObserver(logger *slog.Logger) *bridge.BrokerObserver {
	brokerURL := os.Getenv(contract.EnvBrokerURL)
	tokenPath := os.Getenv(contract.EnvTokenPath)
	if brokerURL == "" || tokenPath == "" {
		logger.Warn("run state reporting is off: SCARAB_BROKER_URL or SCARAB_TOKEN_PATH is unset")
		return nil
	}
	// The broker's certificate is signed by its own CA, which is mounted beside the token.
	// Unset means the broker is on plaintext.
	caPath := os.Getenv(contract.EnvBrokerCA)
	if caPath == "" {
		caPath = contract.BrokerCAFile
	}
	observer, err := bridge.NewBrokerObserver(brokerURL, tokenPath, caPath, logger)
	if err != nil {
		logger.Error("run state reporting is off", "err", err)
		return nil
	}
	return observer
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scarab-bridge:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port       = flag.Int("port", envInt(contract.EnvBridgePort, contract.HTTPPort), "HTTP port")
		sessionDir = flag.String("session-dir", envString(contract.EnvSessionDir, contract.SessionDir), "Pi session directory, on the workspace volume")
		agentDir   = flag.String("agent-dir", envString(contract.EnvAgentDir, contract.AgentDir), "Pi agent directory (credential store), on the workspace volume")
		extension  = flag.String("extension", envString(contract.EnvPIExtension, contract.PlatformToolsExtension), "Pi extension giving the root agent its platform tools")
		piBin      = flag.String("pi", "pi", "path to the pi binary")
		piName     = flag.String("name", "scarab-root", "Pi session display name")
		sessionID  = flag.String("session-id", "scarab-root", "stable Pi session id, so restarts continue one conversation instead of starting a new one")
		hostname   = flag.String("hostname", envString(contract.EnvHostname, ""), "this workspace's public hostname; empty disables the Host and Origin checks")
		assertion  = flag.String("assertion-pubkey", envString(contract.EnvAssertionPubkey, ""), "path to town's assertion public key; empty disables the assertion gate")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Private files for everything this process and its children create. Pi's session
	// transcripts are the reason: they were 0644 on a world-writable volume, so every
	// process in the workspace could read them (red-team finding 7). umask is
	// inherited across exec, which is what makes this work without patching Pi.
	setUmask()

	policy, err := requestPolicy(*hostname, *assertion, logger)
	if err != nil {
		return err
	}

	// The session and agent directories live on the workspace PVC, so the root
	// agent's conversation and its credential store survive a pod restart.
	if err := prepareDirs(*sessionDir, *agentDir); err != nil {
		return err
	}

	// The platform tools are loaded for the ROOT agent only; a worker is started by
	// the broker without them, so it has no orchestration capability (§8.3).
	session := bridge.NewSession(*piBin, piArgs(*sessionDir, *sessionID, *piName, *extension, logger), *agentDir, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Reporting the root run's turns. The bridge is the only party that can see a turn
	// boundary, and it reports through the broker because the broker is the only component
	// that reports to the control plane.
	watchRunTurns(ctx, session, logger)

	go session.Run(ctx)

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(*port),
		Handler:           bridge.NewServer(session, policy, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the WebSocket is long-lived.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("bridge listening", "addr", srv.Addr, "sessionDir", *sessionDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		// Ask Pi to exit cleanly before the process is torn down.
		session.Shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// prepareDirs creates the two directories the bridge needs on the workspace
// volume. The modes are the point: the session directory holds transcripts and the
// agent directory holds the credential store, and neither is for other processes in
// the workspace.
func prepareDirs(sessionDir, agentDir string) error {
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("create agent directory: %w", err)
	}
	return nil
}

// requestPolicy assembles what the bridge requires of a request.
//
// It warns about anything that is switched off, so a gap is visible in the log at
// startup rather than implicit in a missing environment variable.
func requestPolicy(hostname, assertionPath string, logger *slog.Logger) (bridge.Policy, error) {
	if hostname == "" {
		logger.Warn("SCARAB_HOSTNAME is not set: Host and Origin checks are disabled")
	}
	gate, err := loadAssertionGate(assertionPath, hostname, logger)
	if err != nil {
		return bridge.Policy{}, err
	}
	return bridge.Policy{Hostname: hostname, Gate: gate}, nil
}

// loadAssertionGate builds the gate that authenticates a request as having come
// through the edge.
//
// It fails closed on a key it cannot use: a bridge that cannot verify assertions
// would serve an unauthenticated workspace while looking gated, which is worse than
// not starting at all. An unset path disables the gate -- a dev run, or a workspace
// created before the key was delivered -- and says so at startup rather than
// leaving the gap implicit.
func loadAssertionGate(path, hostname string, logger *slog.Logger) (*bridge.AssertionGate, error) {
	if path == "" {
		logger.Warn("SCARAB_ASSERTION_PUBKEY is not set: the workspace assertion gate is disabled, so the edge is the only gate")
		return nil, nil
	}
	gate, err := bridge.LoadAssertionGate(path, hostname)
	if err != nil {
		return nil, err
	}
	logger.Info("assertion gate enabled", "audience", hostname, "key", path)
	return gate, nil
}

// piArgs builds the Pi child's arguments.
//
// --session-id keeps ONE conversation across restarts: without it every bridge
// restart -- a credential change, a crash -- starts a fresh session, so the history
// fragments and a reloaded page has nothing coherent to show.
//
// The platform tools are loaded for the ROOT agent only. A worker is started by the
// broker without this flag, so it has no orchestration capability (§8.3).
func piArgs(sessionDir, sessionID, name, extension string, logger *slog.Logger) []string {
	args := []string{"--mode", "rpc", "--session-dir", sessionDir, "--session-id", sessionID, "--name", name}
	if _, err := os.Stat(extension); err == nil {
		args = append(args, "--extension", extension)
	} else {
		logger.Warn("platform tools extension not found; the agent cannot spawn workers", "path", extension)
	}
	return args
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}
