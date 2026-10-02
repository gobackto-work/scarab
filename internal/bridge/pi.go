// Package bridge is the root agent's HTTP endpoint.
//
// Pi has no HTTP mode, so the bridge runs `pi --mode rpc` as a child process and
// translates between Pi's JSONL protocol on the child's stdio and a WebSocket to
// the browser. It is deliberately small: the bridge is the reusable asset, and a
// richer chat UI is purely frontend work behind the same contract
// (scarab/docs/architecture.md §8.2).
//
// One container: the bridge and Pi share a pod, so the root agent costs one pod
// and there is no internal RPC port to secure.
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// maxRecordBytes bounds a single JSONL record from Pi. Tool results can be
// large, and bufio.Scanner's default 64 KiB limit would truncate them into a
// protocol error.
const maxRecordBytes = 16 << 20

// Process is one running `pi --mode rpc` child.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan []byte

	writeMu sync.Mutex
	done    chan struct{}
	waitErr error
}

// StartOptions describe one `pi --mode rpc` child.
type StartOptions struct {
	Bin  string
	Args []string
	// Env is appended to the parent environment. It carries process-level settings
	// Pi cannot be given as arguments -- the trust anchor for the broker's TLS
	// certificate (handoff §8.5), and a user-supplied model credential without it
	// ever reaching disk (handoff §8.4).
	Env []string
}

// StartProcess launches Pi and begins reading protocol records.
//
// ctx is the process's lifetime: cancelling it kills the child. The caller must
// drain Events() until it closes, or Pi will block on stdout backpressure.
func StartProcess(ctx context.Context, opts StartOptions) (*Process, error) {
	cmd := exec.CommandContext(ctx, opts.Bin, opts.Args...)
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge: pi stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge: pi stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge: pi stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("bridge: start %s: %w", opts.Bin, err)
	}

	p := &Process{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	go p.readStdout(stdout)
	// stderr must be drained continuously or a chatty Pi can block on it. It is
	// diagnostics, never protocol.
	go drain(stderr)
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Events yields one raw JSONL record per item, without its trailing newline. The
// channel closes when Pi's stdout reaches EOF.
func (p *Process) Events() <-chan []byte { return p.events }

// Wait blocks until the child exits and returns its error, if any.
func (p *Process) Wait() error {
	<-p.done
	return p.waitErr
}

// Send writes one command as a JSONL record. It honors stdin backpressure.
func (p *Process) Send(record any) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("bridge: encode command: %w", err)
	}
	payload = append(payload, '\n')

	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(payload); err != nil {
		return fmt.Errorf("bridge: write command: %w", err)
	}
	return nil
}

// CloseStdin asks Pi for an orderly shutdown, as rpc.md prescribes.
func (p *Process) CloseStdin() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.stdin.Close()
}

// Kill terminates the child immediately. It is used when a credential changes and
// the child must be replaced rather than asked to finish.
func (p *Process) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *Process) readStdout(r io.Reader) {
	defer close(p.events)

	// ScanLines splits only on LF and strips a trailing CR. It does not treat
	// U+2028/U+2029 as record boundaries, which Node's readline would.
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		record := make([]byte, len(line))
		copy(record, line)
		p.events <- record
	}
	// A scanner error (an oversized record) is reported through the process exit
	// path: closing the channel ends the supervisor's read loop.
}

func drain(r io.Reader) {
	_, _ = io.Copy(io.Discard, r)
}
