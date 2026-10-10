//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr hides the console window of child processes on Windows.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// processExitCode extracts the exit status of a finished child process.
func processExitCode(ee *exec.ExitError) (int, bool) {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
		return ws.ExitStatus(), true
	}
	return 0, false
}
