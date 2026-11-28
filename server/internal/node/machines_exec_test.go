package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/rpc"
)

func TestNodeMachinesExecArgvAndCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell test")
	}
	n := &Node{}

	// 1. argv runs directly without shell expansion: "$HOME" must stay literal.
	raw, _ := json.Marshal(wire.MachinesExecParams{
		MachineID: "local",
		Argv:      []string{"/bin/echo", "$HOME", "hello"},
	})
	res, err := n.machinesExec(context.Background(), raw)
	if err != nil {
		t.Fatalf("argv machinesExec: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Output) != "$HOME hello" || res.Truncated || res.TimedOut || res.Cancelled {
		t.Fatalf("unexpected argv result: %+v", res)
	}

	// 2. command executes through /bin/sh -c and merges stdout + stderr.
	raw, _ = json.Marshal(wire.MachinesExecParams{
		MachineID: "local",
		Command:   `echo out-line; echo err-line >&2; exit 7`,
	})
	res, err = n.machinesExec(context.Background(), raw)
	if err != nil {
		t.Fatalf("command machinesExec: %v", err)
	}
	if res.ExitCode != 7 || !strings.Contains(res.Output, "out-line") || !strings.Contains(res.Output, "err-line") {
		t.Fatalf("unexpected command result: %+v", res)
	}
}

func TestNodeMachinesExecTruncationAndProcessTreeKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group test")
	}
	n := &Node{}

	// Output truncation at max_output_bytes.
	raw, _ := json.Marshal(wire.MachinesExecParams{
		MachineID:      "local",
		Command:        `head -c 4096 /dev/zero | tr '\0' 'A'`,
		MaxOutputBytes: 128,
	})
	res, err := n.machinesExec(context.Background(), raw)
	if err != nil {
		t.Fatalf("truncation machinesExec: %v", err)
	}
	if res.ExitCode != 0 || !res.Truncated || len(res.Output) != 128 || res.Output != strings.Repeat("A", 128) {
		t.Fatalf("unexpected truncation result: exit=%d truncated=%v len=%d", res.ExitCode, res.Truncated, len(res.Output))
	}

	// Timeout kills the entire process group, including backgrounded child processes (no orphans).
	pidFile := t.TempDir() + "/child.pid"
	raw, _ = json.Marshal(wire.MachinesExecParams{
		MachineID: "local",
		Command:   `sleep 30 & echo $! > "` + pidFile + `"; wait`,
		TimeoutMs: 350,
	})
	start := time.Now()
	res, err = n.machinesExec(context.Background(), raw)
	if err != nil {
		t.Fatalf("timeout machinesExec: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected timed_out=true, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took too long (%v); child likely orphaned", elapsed)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid file: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || childPID <= 0 {
		t.Fatalf("bad child pid %q: %v", string(pidBytes), err)
	}
	// Give the kernel a brief moment to reap the killed process group.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("child pid %d survived process group kill after timeout", childPID)
}

func TestValidateMachinesExecParams(t *testing.T) {
	tooManyArgs := make([]string, 65)
	for i := range tooManyArgs {
		tooManyArgs[i] = "a"
	}
	cases := []struct {
		name string
		p    wire.MachinesExecParams
	}{
		{"missing machine_id", wire.MachinesExecParams{Command: "echo hi"}},
		{"neither argv nor command", wire.MachinesExecParams{MachineID: "m1"}},
		{"both argv and command", wire.MachinesExecParams{MachineID: "m1", Argv: []string{"echo"}, Command: "echo"}},
		{"empty argv[0]", wire.MachinesExecParams{MachineID: "m1", Argv: []string{""}}},
		{"argv > 64", wire.MachinesExecParams{MachineID: "m1", Argv: tooManyArgs}},
		{"argv > 4096 bytes", wire.MachinesExecParams{MachineID: "m1", Argv: []string{"echo", strings.Repeat("x", 4096)}}},
		{"argv NUL", wire.MachinesExecParams{MachineID: "m1", Argv: []string{"echo", "a\x00b"}}},
		{"command > 1024", wire.MachinesExecParams{MachineID: "m1", Command: strings.Repeat("a", 1025)}},
		{"command NUL", wire.MachinesExecParams{MachineID: "m1", Command: "echo \x00"}},
		{"timeout > 60000", wire.MachinesExecParams{MachineID: "m1", Command: "echo hi", TimeoutMs: 60001}},
		{"max_output > 65536", wire.MachinesExecParams{MachineID: "m1", Command: "echo hi", MaxOutputBytes: 65537}},
		{"reason > 200", wire.MachinesExecParams{MachineID: "m1", Command: "echo hi", Reason: strings.Repeat("r", 201)}},
	}
	for _, tc := range cases {
		p := tc.p
		err := ValidateMachinesExecParams(&p)
		var re *rpc.Error
		if !errors.As(err, &re) || re.Code != "invalid_params" {
			t.Errorf("%s: got %v, want invalid_params", tc.name, err)
		}
	}
}
