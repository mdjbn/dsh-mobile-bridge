package main

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// adbServer supervises the adb *server* on this PC.
//
// The reason the bridge cares at all: a remote adb client (the one the DSH
// server runs for the `android_*` tools) can be pointed at this machine with
// `ADB_SERVER_SOCKET=tcp:<pc>:<port>`, and that only works when the server
// listens on a routable interface. A plain `adb start-server` binds 127.0.0.1,
// so the bridge starts its own with `-a`, after killing a loopback-only server
// it did not start.
type adbServer struct {
	api  *API
	port int

	mu    sync.Mutex
	owned bool
	cmd   *exec.Cmd
	pid   int
}

// Snapshot reports what is listening right now, from the bridge's point of view.
func (s *adbServer) Snapshot() adbServerInfo {
	s.mu.Lock()
	owned, pid := s.owned, s.pid
	s.mu.Unlock()
	if owned {
		return adbServerInfo{Running: true, Addr: fmt.Sprintf("0.0.0.0:%d", s.port), PID: pid}
	}
	if portIsListening(s.port) {
		return adbServerInfo{Running: true, Addr: fmt.Sprintf("127.0.0.1:%d", s.port)}
	}
	return adbServerInfo{Running: false, Addr: fmt.Sprintf("127.0.0.1:%d", s.port)}
}

// Start brings up an all-interfaces adb server. With force, a pre-existing
// loopback-only server is killed and replaced; without it, an existing server
// is adopted as-is.
func (s *adbServer) Start(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owned {
		return nil
	}
	if portIsListening(s.port) {
		if !force {
			return nil
		}
		if err := s.killServerLocked(); err != nil {
			return fmt.Errorf("an adb server is already listening on port %d and could not be replaced: %w", s.port, err)
		}
	}

	pt := s.api.platformToolsSnapshot()
	if pt.Adb == "" {
		return fmt.Errorf("adb is not available: %s", pt.AdbErr)
	}
	cmd := exec.Command(pt.Adb, "-a", "-P", strconv.Itoa(s.port), "nodaemon", "server")
	configureSysProcAttr(cmd)
	cmd.Dir = pt.Dir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start `adb -a -P %d nodaemon server`: %w", s.port, err)
	}
	s.cmd = cmd
	s.owned = true
	s.pid = cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if portIsListening(s.port) {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("`adb -a -P %d nodaemon server` started (pid %d) but nothing is listening on port %d yet", s.port, s.pid, s.port)
}

// Stop kills the server the bridge started, and any other one adb knows about.
func (s *adbServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killServerLocked()
}

func (s *adbServer) killServerLocked() error {
	var firstErr error
	if s.owned && s.cmd != nil && s.cmd.Process != nil {
		if err := s.cmd.Process.Kill(); err != nil {
			firstErr = err
		}
		s.owned, s.cmd, s.pid = false, nil, 0
	}
	if pt := s.api.platformToolsSnapshot(); pt.Adb != "" {
		if res := runCommand([]string{pt.Adb, "kill-server"}, 20*time.Second, "", nil, pt.Dir); res.ExitCode != 0 && firstErr == nil {
			firstErr = fmt.Errorf("adb kill-server: %s%s", res.Error, res.Stderr)
		}
	}
	return firstErr
}

// StopIfOwned is the shutdown path: it only tears down what this process owns.
func (s *adbServer) StopIfOwned() {
	s.mu.Lock()
	owned := s.owned
	s.mu.Unlock()
	if owned {
		_ = s.Stop()
	}
}

// portIsListening reports whether anything accepts TCP connections on the port.
func portIsListening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
