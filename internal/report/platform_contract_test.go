package report

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gobackto-work/scarab/internal/contract"
)

// This test is driven by pestilence's cross-repository contract test.
func TestPlatformContractReporter(t *testing.T) {
	url := os.Getenv("PLATFORM_CONTRACT_URL")
	if url == "" {
		t.Skip("run through pestilence's platform contract test")
	}
	client, err := New(url, os.Getenv("PLATFORM_CONTRACT_REPORT_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	r := Report{
		EventID: "contract-start", RunID: os.Getenv("PLATFORM_CONTRACT_RUN_ID"),
		State: contract.StateRunning, Mode: contract.ModeInteractive, At: time.Now().UTC(),
	}
	first, err := client.Post(context.Background(), r)
	if err != nil || !first.Appended || first.Sequence <= 0 {
		t.Fatalf("start: result %+v, error %v", first, err)
	}
	duplicate, err := client.Post(context.Background(), r)
	if err != nil || duplicate.Appended || duplicate.Sequence != first.Sequence {
		t.Fatalf("duplicate: result %+v, error %v", duplicate, err)
	}
	r.EventID = "contract-waiting"
	r.State = contract.StateWaiting
	r.At = time.Now().UTC()
	waiting, err := client.Post(context.Background(), r)
	if err != nil || !waiting.Appended || waiting.Sequence <= first.Sequence {
		t.Fatalf("waiting: result %+v, error %v", waiting, err)
	}
	r.EventID = "contract-invalid"
	r.State = "unknown_state"
	if _, err := client.Post(context.Background(), r); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown state: got %v, want rejected", err)
	}
}
