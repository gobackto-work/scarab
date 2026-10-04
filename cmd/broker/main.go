// Command broker is the per-workspace agent broker.
//
// It serves exactly one workspace. Its whole configuration is supplied by the
// platform when pestilence applies the Deployment (scarab/docs/architecture.md §5.5,
// §6.4); nothing about the workspace is discovered from the network or from a
// request.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/gobackto-work/scarab/internal/agentpod"
	"github.com/gobackto-work/scarab/internal/broker"
	"github.com/gobackto-work/scarab/internal/contract"
	"github.com/gobackto-work/scarab/internal/report"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scarab-broker:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	settings, err := loadSettings()
	if err != nil {
		return err
	}

	pubPEM, err := os.ReadFile(settings.tokenPublicKeyPath)
	if err != nil {
		return fmt.Errorf("read token public key: %w", err)
	}
	verifier, err := broker.NewVerifier(pubPEM, settings.tokenAudience, settings.slug, settings.namespace)
	if err != nil {
		return err
	}

	client, err := inClusterClient()
	if err != nil {
		return err
	}

	spawner := broker.NewKubeSpawner(client, settings.agentpodConfig(), settings.podBudget, settings.memoryBudget)
	recorder := runStateReporter(settings, logger)
	handler := broker.NewServer(spawner, verifier, recorder, logger).Handler()

	srv, err := newServer(handler, settings, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("broker listening", "addr", srv.Addr, "workspace", settings.slug, "namespace", settings.namespace, "tls", srv.TLSConfig != nil)
		if err := listen(srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go observeRuns(ctx, spawner, recorder, logger)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// reportInterval is how often the broker looks for workers that have reached a terminal
// state.
//
// A poll and not a watch, because the states it needs are already on the objects List
// returns, and because a poll is self-healing: a report lost to a control-plane restart is
// sent again on the next pass, with the same event id, so it cannot become two events.
const reportInterval = 30 * time.Second

// runStateReporter builds the broker's reporter, or a discarding one when the control plane
// is not configured.
//
// A failure here does not stop the broker. Losing notifications is bad; refusing to run
// work because notifications cannot be sent is worse, and it is the failure mode that would
// look like the platform being down.
func runStateReporter(settings settings, logger *slog.Logger) broker.Recorder {
	if settings.platformURL == "" || settings.reportTokenPath == "" {
		logger.Warn("run state reporting is off: SCARAB_PLATFORM_URL or SCARAB_REPORT_TOKEN_PATH is unset")
		return nil
	}
	client, err := report.New(settings.platformURL, settings.reportTokenPath)
	if err != nil {
		logger.Error("run state reporting is off", "err", err)
		return nil
	}
	return broker.NewReporter(client, logger)
}

// observeRuns reports the state of every worker that has reached a terminal state.
func observeRuns(ctx context.Context, spawner broker.Spawner, recorder broker.Recorder, logger *slog.Logger) {
	reporter, ok := recorder.(*broker.Reporter)
	if !ok {
		return
	}
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			agents, err := spawner.List(ctx)
			if err != nil {
				// The API server is unreachable or the Role is wrong. Both are transient
				// from here, and the next pass tries again.
				logger.Warn("could not list workers to report their state", "err", err)
				continue
			}
			reporter.Observe(ctx, agents)

			// The root agent's terminal state comes from its pod, because the bridge cannot
			// report the state it reaches by dying and a killed bridge would report nothing.
			root, err := spawner.Root(ctx)
			if err != nil {
				logger.Warn("could not read the root agent to report its state", "err", err)
				continue
			}
			reporter.ObserveRoot(ctx, root)
		}
	}
}

// newServer builds the broker's HTTP server, over TLS when a certificate is
// configured.
//
// The pair is loaded here, at startup, rather than on the first connection: a
// certificate that cannot be read is a configuration mistake, and finding out then
// is strictly worse than not starting.
func newServer(handler http.Handler, settings settings, logger *slog.Logger) (*http.Server, error) {
	tlsConfig, err := broker.TLSConfig(settings.tlsCertPath, settings.tlsKeyPath)
	if err != nil {
		return nil, err
	}
	if tlsConfig == nil {
		logger.Warn("no broker TLS certificate: serving plaintext, so the NetworkPolicy is the only thing confining this hop")
	}
	return &http.Server{
		Addr:              ":" + strconv.Itoa(settings.port),
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: the logs endpoint streams for as long as the worker
		// runs, and a write deadline would truncate it.
		IdleTimeout: 120 * time.Second,
	}, nil
}

// listen serves the broker, over TLS when a certificate is configured.
//
// ListenAndServeTLS is handed empty file names because the certificate is already
// in srv.TLSConfig. Loading it there is what makes a broken pair fail at startup
// instead of on the first connection.
func listen(srv *http.Server) error {
	if srv.TLSConfig == nil {
		return srv.ListenAndServe()
	}
	return srv.ListenAndServeTLS("", "")
}

// settings is the broker's whole configuration, read from the environment that
// pestilence sets on the Deployment.
type settings struct {
	slug               string
	namespace          string
	port               int
	agentImage         string
	resultRoot         string
	podBudget          int
	memoryBudget       resource.Quantity
	containerCPUMax    resource.Quantity
	containerMemMax    resource.Quantity
	tokenPublicKeyPath string
	tokenAudience      string

	// Reporting. Both are empty when the workspace was provisioned without a control
	// plane address, and the broker then reports nothing rather than failing to start:
	// reporting is a side effect of running work, not a precondition for it.
	platformURL     string
	reportTokenPath string

	// Broker TLS paths, empty when pestilence provisioned the workspace without
	// TLS (handoff §8.5).
	tlsCertPath string
	tlsKeyPath  string
}

func loadSettings() (settings, error) {
	var s settings
	var err error

	s.slug = os.Getenv(contract.EnvWorkspaceSlug)
	s.namespace = os.Getenv(contract.EnvWorkspaceNS)
	s.agentImage = os.Getenv(contract.EnvAgentImage)
	s.tokenPublicKeyPath = os.Getenv(contract.EnvTokenPublicKey)
	s.tokenAudience = os.Getenv(contract.EnvTokenAudience)
	s.platformURL = os.Getenv(contract.EnvPlatformURL)
	s.reportTokenPath = os.Getenv(contract.EnvReportTokenPath)
	s.tlsCertPath = os.Getenv(contract.EnvBrokerTLSCert)
	s.tlsKeyPath = os.Getenv(contract.EnvBrokerTLSKey)

	for name, value := range map[string]string{
		contract.EnvWorkspaceSlug:  s.slug,
		contract.EnvWorkspaceNS:    s.namespace,
		contract.EnvAgentImage:     s.agentImage,
		contract.EnvTokenPublicKey: s.tokenPublicKeyPath,
		contract.EnvTokenAudience:  s.tokenAudience,
	} {
		if value == "" {
			return s, fmt.Errorf("%s is required", name)
		}
	}
	// SCARAB_MODEL_SECRET is optional. The MVP model credential is supplied by the
	// user in the session and held by the bridge, so a worker normally runs without
	// a Secret (handoff §8.4).

	if s.port, err = envInt(contract.EnvBrokerPort, contract.BrokerPort); err != nil {
		return s, err
	}
	if s.podBudget, err = envInt(contract.EnvPodBudget, contract.PodBudget); err != nil {
		return s, err
	}
	s.resultRoot = os.Getenv(contract.EnvResultRoot) // DefaultConfig supplies the fallback
	if s.containerCPUMax, err = envQuantityOptional(contract.EnvContainerCPUMax); err != nil {
		return s, err
	}
	if s.containerMemMax, err = envQuantityOptional(contract.EnvContainerMemMax); err != nil {
		return s, err
	}
	return s, nil
}

func (s settings) agentpodConfig() agentpod.Config {
	// DefaultConfig owns the defaults; the environment only overrides. These
	// defaults used to be written twice -- here and in agentpod -- which is the
	// same failure shape as the contract constants: two copies, nothing checking
	// they agree.
	cfg := agentpod.DefaultConfig(s.namespace, s.slug, s.agentImage)
	if s.resultRoot != "" {
		cfg.ResultRoot = s.resultRoot
	}
	if !s.containerCPUMax.IsZero() {
		cfg.MaxCPU = s.containerCPUMax
	}
	if !s.containerMemMax.IsZero() {
		cfg.MaxMemory = s.containerMemMax
	}
	return cfg
}

func inClusterClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return client, nil
}

func envInt(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// envQuantityOptional parses a quantity, returning the zero value when the
// variable is unset so the caller's own default applies rather than a second copy
// of it living here.
func envQuantityOptional(name string) (resource.Quantity, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return resource.Quantity{}, nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s: %w", name, err)
	}
	return q, nil
}
