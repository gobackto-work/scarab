package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// The tests use the test binary itself as a fake `pi`, re-executed with
// SCARAB_BRIDGE_HELPER set. That avoids depending on a shell or a real Pi, and it
// exercises the real os/exec and pipe path.

func TestMain(m *testing.M) {
	if os.Getenv("SCARAB_BRIDGE_HELPER") != "" {
		helperProcess()
		return
	}
	os.Exit(m.Run())
}

func helperProcess() {
	switch os.Getenv("SCARAB_BRIDGE_HELPER") {
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "emit":
		fmt.Fprint(os.Stdout, os.Getenv("SCARAB_BRIDGE_EMIT"))
	case "printenv":
		// Emit the value of a named environment variable as a record, so a test can
		// observe what the child actually received.
		fmt.Fprintf(os.Stdout, "{\"type\":\"env\",\"value\":%q}\n", os.Getenv(os.Getenv("SCARAB_BRIDGE_EMIT_VAR")))
	case "announce":
		// Announce each start, then stay alive, so a test can observe a restart.
		fmt.Fprintln(os.Stdout, `{"type":"started"}`)
		time.Sleep(time.Hour)
	case "sleep":
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// startHelper launches the fake Pi. mode selects its behaviour.
func startHelper(t *testing.T, mode, emit string) *Process {
	t.Helper()
	t.Setenv("SCARAB_BRIDGE_HELPER", mode)
	if emit != "" {
		t.Setenv("SCARAB_BRIDGE_EMIT", emit)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	proc, err := StartProcess(ctx, StartOptions{Bin: os.Args[0], Args: []string{"-test.run=^$"}})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	return proc
}

func nextRecord(t *testing.T, proc *Process) []byte {
	t.Helper()
	select {
	case record, ok := <-proc.Events():
		if !ok {
			t.Fatal("event channel closed")
		}
		return record
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a record")
		return nil
	}
}

// CRLF is stripped, so a record is exactly the JSON without its terminator.
func TestProcessEmitsOneRecordPerLine(t *testing.T) {
	proc := startHelper(t, "emit", "{\"type\":\"a\"}\r\n{\"type\":\"b\"}\n")

	first := string(nextRecord(t, proc))
	if first != `{"type":"a"}` {
		t.Errorf("first record = %q, want %q", first, `{"type":"a"}`)
	}
	second := string(nextRecord(t, proc))
	if second != `{"type":"b"}` {
		t.Errorf("second record = %q, want %q", second, `{"type":"b"}`)
	}
}

func TestProcessEventsCloseOnExit(t *testing.T) {
	proc := startHelper(t, "emit", "{\"type\":\"only\"}\n")

	select {
	case <-proc.Events():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
	select {
	case _, ok := <-proc.Events():
		if ok {
			t.Fatal("expected the event channel to be closed after exit")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("event channel did not close")
	}
}

// Send writes JSONL, and Pi's records come back unchanged. This is the round trip
// the whole bridge depends on.
func TestProcessSendRoundTrip(t *testing.T) {
	proc := startHelper(t, "echo", "")

	if err := proc.Send(map[string]any{"type": "prompt", "message": "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var got struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(nextRecord(t, proc), &got); err != nil {
		t.Fatalf("decode echoed record: %v", err)
	}
	if got.Type != "prompt" || got.Message != "hello" {
		t.Errorf("echoed %+v, want prompt/hello", got)
	}
}

// Closing stdin asks Pi for an orderly shutdown, so the child should exit.
func TestProcessCloseStdinEndsTheChild(t *testing.T) {
	proc := startHelper(t, "echo", "")

	if err := proc.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}

	// Wait rather than a Done() channel: Wait is what production uses, and a
	// method that exists only for a test is dead code from the binary's point of
	// view (deadcode reports it, correctly).
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit after stdin was closed")
	}
}
