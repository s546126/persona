package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/rpc"
)

const (
	machinesExecDefaultTimeoutMs   = 20000
	machinesExecMaxTimeoutMs       = 60000
	machinesExecDefaultOutputBytes = 16384
	machinesExecMaxOutputBytes     = 65536
	machinesExecMaxArgvItems       = 64
	machinesExecMaxArgvBytes       = 4096
	machinesExecMaxCommandRunes    = 1024
	machinesExecMaxReasonRunes     = 200
)

// limitedWriter captures at most limit bytes and records whether any additional bytes were discarded.
// It always returns len(p), nil so the child process never blocks writing stdout/stderr.
type limitedWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rem := w.limit - w.buf.Len()
	if rem > 0 {
		if len(p) <= rem {
			_, _ = w.buf.Write(p)
		} else {
			_, _ = w.buf.Write(p[:rem])
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *limitedWriter) snapshot() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.buf.Bytes()
	// Avoid splitting a multi-byte UTF-8 rune at the truncation boundary.
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b), w.truncated
}

// ValidateMachinesExecParams checks bounds and mutual exclusivity of machines.exec params.
func ValidateMachinesExecParams(p *wire.MachinesExecParams) error {
	p.MachineID = wire.HubMachineID(strings.TrimSpace(string(p.MachineID)))
	if p.MachineID == "" {
		return &rpc.Error{Code: "invalid_params", Message: "machine_id is required"}
	}
	hasArgv := len(p.Argv) > 0
	hasCmd := p.Command != ""
	if hasArgv && hasCmd {
		return &rpc.Error{Code: "invalid_params", Message: "argv and command are mutually exclusive"}
	}
	if !hasArgv && strings.TrimSpace(p.Command) == "" {
		return &rpc.Error{Code: "invalid_params", Message: "either argv or command is required"}
	}
	if hasArgv {
		if len(p.Argv) > machinesExecMaxArgvItems {
			return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("argv has at most %d items", machinesExecMaxArgvItems)}
		}
		if p.Argv[0] == "" {
			return &rpc.Error{Code: "invalid_params", Message: "argv[0] must not be empty"}
		}
		totalBytes := 0
		for _, a := range p.Argv {
			if strings.IndexByte(a, 0) >= 0 {
				return &rpc.Error{Code: "invalid_params", Message: "argv must not contain NUL bytes"}
			}
			totalBytes += len(a)
		}
		if totalBytes > machinesExecMaxArgvBytes {
			return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("argv total length exceeds %d bytes", machinesExecMaxArgvBytes)}
		}
	}
	if hasCmd {
		if strings.IndexByte(p.Command, 0) >= 0 {
			return &rpc.Error{Code: "invalid_params", Message: "command must not contain NUL bytes"}
		}
		if utf8.RuneCountInString(p.Command) > machinesExecMaxCommandRunes {
			return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("command exceeds %d characters", machinesExecMaxCommandRunes)}
		}
	}
	if strings.IndexByte(p.Cwd, 0) >= 0 || len(p.Cwd) > 4096 {
		return &rpc.Error{Code: "invalid_params", Message: "invalid cwd"}
	}
	if p.TimeoutMs < 0 || p.TimeoutMs > machinesExecMaxTimeoutMs {
		return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("timeout_ms must be between 0 and %d", machinesExecMaxTimeoutMs)}
	}
	if p.MaxOutputBytes < 0 || p.MaxOutputBytes > machinesExecMaxOutputBytes {
		return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("max_output_bytes must be between 0 and %d", machinesExecMaxOutputBytes)}
	}
	if strings.IndexByte(p.Reason, 0) >= 0 || utf8.RuneCountInString(p.Reason) > machinesExecMaxReasonRunes {
		return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("reason exceeds %d characters", machinesExecMaxReasonRunes)}
	}
	return nil
}

// machinesExec runs a single owner-confirmed command on this computer (#16741):
//   - Stdin is nil (/dev/null)
//   - argv runs directly without a shell; command runs via /bin/sh -c
//   - Runs inside an isolated process group so timeout/cancel kills the whole process tree
//   - Combined stdout+stderr is bounded by max_output_bytes and marked truncated on overflow
func (n *Node) machinesExec(ctx context.Context, raw json.RawMessage) (wire.MachinesExecResult, error) {
	var p wire.MachinesExecParams
	if len(bytes.TrimSpace(raw)) == 0 {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "invalid_params", Message: "params are required"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "invalid_params", Message: err.Error()}
	}
	if err := ValidateMachinesExecParams(&p); err != nil {
		return wire.MachinesExecResult{}, err
	}
	timeoutMs := p.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = machinesExecDefaultTimeoutMs
	}
	maxOut := p.MaxOutputBytes
	if maxOut == 0 {
		maxOut = machinesExecDefaultOutputBytes
	}

	dir := strings.TrimSpace(p.Cwd)
	if dir != "" {
		if dir == "~" && n.home != "" {
			dir = n.home
		} else if strings.HasPrefix(dir, "~/") && n.home != "" {
			dir = filepath.Join(n.home, dir[2:])
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return wire.MachinesExecResult{}, &rpc.Error{Code: "invalid_cwd", Message: "cwd does not exist or is not a directory"}
		}
	}

	var cmd *exec.Cmd
	if len(p.Argv) > 0 {
		cmd = exec.Command(p.Argv[0], p.Argv[1:]...)
	} else {
		cmd = exec.Command("/bin/sh", "-c", p.Command)
	}
	cmd.Stdin = nil
	if dir != "" {
		cmd.Dir = dir
	}
	setMachinesExecProcGroup(cmd)

	out := &limitedWriter{limit: maxOut}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "exec_failed", Message: err.Error()}
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	var waitErr error
	var timedOut, cancelled bool
	select {
	case waitErr = <-waitCh:
	case <-execCtx.Done():
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			timedOut = true
		} else {
			cancelled = true
		}
		if cmd.Process != nil {
			killMachinesExecProcGroup(cmd.Process.Pid)
		}
		waitErr = <-waitCh
	}

	durationMs := time.Since(start).Milliseconds()
	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	if timedOut || cancelled {
		if exitCode == 0 {
			exitCode = -1
		}
	}
	text, truncated := out.snapshot()
	machineID := p.MachineID
	if n.machineID != "" {
		machineID = wire.HubMachineID(n.machineID)
	}
	return wire.MachinesExecResult{
		MachineID:  machineID,
		ExitCode:   exitCode,
		Output:     text,
		Truncated:  truncated,
		DurationMs: durationMs,
		TimedOut:   timedOut,
		Cancelled:  cancelled,
	}, nil
}
