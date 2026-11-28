//go:build windows

package node

import (
	"os"
	"os/exec"
)

func setMachinesExecProcGroup(_ *exec.Cmd) {}

func killMachinesExecProcGroup(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
