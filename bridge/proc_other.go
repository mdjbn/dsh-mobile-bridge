//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr is a no-op away from Windows.
func configureSysProcAttr(cmd *exec.Cmd) {}

// processExitCode extracts the exit status of a finished child process.
func processExitCode(ee *exec.ExitError) (int, bool) {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
		return ws.ExitStatus(), true
	}
	return 0, false
}
