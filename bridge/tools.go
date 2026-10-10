package main

import (
	"sync"
	"time"
)

// execManager serializes fastboot invocations per serial, because two
// concurrent fastboot calls talking to the same device can interleave badly.
type execManager struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newExecManager() *execManager {
	return &execManager{locks: make(map[string]*sync.Mutex)}
}

// lockSerial returns an unlock function for the given serial key.
func (e *execManager) lockSerial(serial string) func() {
	e.mu.Lock()
	m, ok := e.locks[serial]
	if !ok {
		m = &sync.Mutex{}
		e.locks[serial] = m
	}
	e.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// runAdb executes adb with argv, prepending the resolved binary path.
func (a *API) runAdb(argv []string, timeout time.Duration, stdin string) commandResult {
	pt := a.platformToolsSnapshot()
	if pt == nil || pt.Adb == "" {
		msg := "adb not available"
		if pt != nil && pt.AdbErr != "" {
			msg = pt.AdbErr
		}
		return commandResult{Argv: argv, ExitCode: -1, Error: msg}
	}
	full := append([]string{pt.Adb}, argv...)
	return runCommand(full, timeout, stdin, nil, pt.Dir)
}

// runFastboot executes fastboot under the per-serial mutex.
func (a *API) runFastboot(argv []string, serial string, timeout time.Duration, stdin string) commandResult {
	pt := a.platformToolsSnapshot()
	if pt == nil || pt.Fastboot == "" {
		msg := "fastboot not available"
		if pt != nil && pt.AdbErr != "" {
			msg = pt.AdbErr
		}
		return commandResult{Argv: argv, ExitCode: -1, Error: msg}
	}
	unlock := a.exec.lockSerial("fastboot:" + serial)
	defer unlock()
	full := append([]string{pt.Fastboot}, argv...)
	return runCommand(full, timeout, stdin, nil, pt.Dir)
}

// buildToolArgs inserts the optional `-s <serial>` before args, unless args
// already begins with -s.
func buildToolArgs(args []string, serial string) []string {
	if serial == "" || len(args) == 0 {
		return args
	}
	if args[0] == "-s" || args[0] == "--serial" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "-s", serial)
	out = append(out, args...)
	return out
}

// clampTimeout applies the documented clamps for tool calls.
func clampTimeout(ms int, def int) time.Duration {
	if ms <= 0 {
		ms = def
	}
	if ms < 1000 {
		ms = 1000
	}
	if ms > 1800000 {
		ms = 1800000
	}
	return time.Duration(ms) * time.Millisecond
}

// clampReadTimeout applies the /v1/serial/read clamp of 0..60000 ms.
func clampReadTimeout(ms int, def int) int {
	if ms < 0 {
		ms = def
	}
	if ms > 60000 {
		ms = 60000
	}
	return ms
}
