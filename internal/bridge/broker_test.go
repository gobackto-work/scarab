package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
)

// fakeBroker records what it was sent and answers with whatever the test sets.
type fakeBroker struct {
	*httptest.Server
	bodies []map[string]string
	code   int
	fail   int
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{code: http.StatusAccepted}
	fb.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.bodies = append(fb.bodies, body)
		if fb.fail > 0 {
			fb.fail--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(fb.code)
	}))
	t.Cleanup(fb.Close)
	return fb
}

func observerFor(t *testing.T, fb *fakeBroker) *BrokerObserver {
	return observerWithCA(t, fb, "")
}

// observerWithCA builds an observer, optionally trusting a CA in PEM. ca is what the real
// broker needs: it serves TLS with a per-workspace certificate it signed itself.
func observerWithCA(t *testing.T, fb *fakeBroker, ca string) *BrokerObserver {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("a-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	caPath := ""
	if ca != "" {
		caPath = filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(caPath, []byte(ca), 0o600); err != nil {
			t.Fatalf("write ca: %v", err)
		}
	}
	obs, err := NewBrokerObserver(fb.URL, token, caPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewBrokerObserver: %v", err)
	}
	return obs
}

func TestRunStartedAndSettledEnqueueTheRightState(t *testing.T) {
	obs := observerFor(t, newFakeBroker(t))

	obs.RunStarted()
	obs.RunSettled()

	for _, want := range []string{contract.StateRunning, contract.StateWaiting} {
		select {
		case got := <-obs.queue:
			if got.state != want {
				t.Errorf("state = %q, want %q", got.state, want)
			}
			if got.eventID == "" {
				t.Error("the observation carries no event id")
			}
		default:
			t.Fatalf("nothing was enqueued for %q", want)
		}
	}
}

// The two states are entered repeatedly, so the same state twice must be two events. A
// derived id would make the second visit look like a retry of the first, and the record
// would drop it.
func TestRepeatedSettlesCarryDifferentEventIDs(t *testing.T) {
	obs := observerFor(t, newFakeBroker(t))

	obs.RunSettled()
	obs.RunSettled()

	first := <-obs.queue
	second := <-obs.queue
	if first.eventID == second.eventID {
		t.Errorf("both settles carried %q, so the record would drop the second", first.eventID)
	}
}

func TestPostSendsWhatTheBrokerExpects(t *testing.T) {
	fb := newFakeBroker(t)
	obs := observerFor(t, fb)

	obs.post(context.Background(), observation{eventID: "e1", state: contract.StateWaiting})

	if len(fb.bodies) != 1 {
		t.Fatalf("the broker saw %d requests, want 1", len(fb.bodies))
	}
	got := fb.bodies[0]
	if got["event_id"] != "e1" || got["state"] != contract.StateWaiting {
		t.Errorf("body = %+v", got)
	}
	// No run id, because the broker attaches it from the pod. A body that named one would
	// be a field the bridge could get wrong.
	if _, ok := got["run_id"]; ok {
		t.Error("the body names a run")
	}
}

// A retry must carry the same event id, or a report that arrived without its answer becomes
// a second event.
func TestARetryCarriesTheSameEventID(t *testing.T) {
	fb := newFakeBroker(t)
	fb.fail = 2 // fail twice, succeed on the third attempt
	obs := observerFor(t, fb)

	obs.post(context.Background(), observation{eventID: "e1", state: contract.StateWaiting})

	if len(fb.bodies) != 3 {
		t.Fatalf("the broker saw %d requests, want 3", len(fb.bodies))
	}
	for i, body := range fb.bodies {
		if body["event_id"] != "e1" {
			t.Errorf("attempt %d carried event id %q, want e1", i+1, body["event_id"])
		}
	}
}

func TestAPostThatKeepsFailingGivesUp(t *testing.T) {
	fb := newFakeBroker(t)
	fb.fail = 100
	obs := observerFor(t, fb)

	done := make(chan struct{})
	go func() {
		defer close(done)
		obs.post(context.Background(), observation{eventID: "e1", state: contract.StateWaiting})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("post never gave up")
	}
	if len(fb.bodies) != runAttempts {
		t.Errorf("the broker saw %d attempts, want %d", len(fb.bodies), runAttempts)
	}
}

// A full queue is dropped, not waited on. Blocking here would stall the session's event loop,
// and a stalled agent is a worse outcome than a missed state.
func TestAFullQueueDropsRatherThanBlocks(t *testing.T) {
	obs := observerFor(t, newFakeBroker(t))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// One more than the queue holds, with no Run draining it.
		for range runQueueDepth + 5 {
			obs.RunSettled()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	if len(obs.queue) != runQueueDepth {
		t.Errorf("the queue holds %d, want %d", len(obs.queue), runQueueDepth)
	}
}

func TestNewBrokerObserverRefusesConfigurationItCannotUse(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	cases := map[string]struct{ url, token string }{
		"a relative url": {"broker:8080", token},
		"no url":         {"", token},
		"no token path":  {"http://broker", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewBrokerObserver(c.url, c.token, "", nil); err == nil {
				t.Error("NewBrokerObserver accepted it")
			}
		})
	}

	// A CA path that is set but unreadable is a startup failure and not a silent fallback to
	// the system trust store, because that fallback is the bug this parameter exists to fix.
	t.Run("an unreadable broker CA", func(t *testing.T) {
		if _, err := NewBrokerObserver("https://broker", token, filepath.Join(t.TempDir(), "absent"), nil); err == nil {
			t.Error("NewBrokerObserver accepted an unreadable CA")
		}
	})
}

// fakeRunObserver records the turn boundaries it was told about.
type fakeRunObserver struct{ calls []string }

func (f *fakeRunObserver) RunStarted() { f.calls = append(f.calls, "started") }
func (f *fakeRunObserver) RunSettled() { f.calls = append(f.calls, "settled") }

// The session's own view of a turn boundary is exactly what the record needs: Pi says when an
// agent starts working and when it settles.
func TestTheSessionTellsTheObserverAboutTurnBoundaries(t *testing.T) {
	session := NewSession("pi", nil, t.TempDir(), nil)
	observer := &fakeRunObserver{}
	session.SetRunObserver(observer)

	session.observe([]byte(`{"type":"agent_start","payload":{}}`))
	session.observe([]byte(`{"type":"agent_settled","payload":{}}`))
	// Not a turn boundary, and must not be reported as one.
	session.observe([]byte(`{"type":"text_delta","payload":{"text":"hi"}}`))

	want := []string{"started", "settled"}
	if strings.Join(observer.calls, ",") != strings.Join(want, ",") {
		t.Errorf("the observer saw %v, want %v", observer.calls, want)
	}
}

// A session with no observer reports nothing and does not panic, because a bridge without a
// broker is a supported configuration.
func TestASessionWithoutAnObserverIsFine(t *testing.T) {
	session := NewSession("pi", nil, t.TempDir(), nil)
	session.observe([]byte(`{"type":"agent_start"}`))
	session.observe([]byte(`{"type":"agent_settled"}`))
	session.observe([]byte(`not json`))
}

// newTLSBroker starts an HTTPS broker whose certificate is signed by its own CA, which is
// what the real one does. It returns the CA in PEM.
func newTLSBroker(t *testing.T) (*fakeBroker, string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "scarab-broker-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create the CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse the CA: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the leaf key: %v", err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "broker"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create the leaf: %v", err)
	}

	fb := &fakeBroker{code: http.StatusAccepted}
	fb.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.bodies = append(fb.bodies, body)
		w.WriteHeader(fb.code)
	}))
	fb.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}},
		MinVersion:   tls.VersionTLS12,
	}
	fb.StartTLS()
	t.Cleanup(fb.Close)

	return fb, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

// The broker's certificate is signed by a CA this process is given and the system does not
// know. A client on the default trust store rejects it, and the failure looks exactly like an
// agent nobody prompted: nothing arrives and nothing errors where anyone is looking.
func TestTheBrokerCAIsTrusted(t *testing.T) {
	fb, ca := newTLSBroker(t)

	trusting := observerWithCA(t, fb, ca)
	trusting.post(context.Background(), observation{eventID: "e1", state: contract.StateWaiting})
	if len(fb.bodies) != 1 {
		t.Fatalf("the broker saw %d requests with its CA trusted, want 1", len(fb.bodies))
	}

	distrusting := observerFor(t, fb)
	distrusting.post(context.Background(), observation{eventID: "e2", state: contract.StateWaiting})
	if len(fb.bodies) != 1 {
		t.Errorf("the broker saw %d requests with the default trust store, want still 1: an untrusted certificate was accepted", len(fb.bodies))
	}
}
