package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// maxStreamBytes caps each captured child stream at 1 MiB, keeping the tail.
const maxStreamBytes = 1 << 20

// capWriter is an io.Writer that keeps at most max bytes and, once full, retains
// the most recent ones (the tail), like `tail -c`.
type capWriter struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
}

func newCapWriter(max int) *capWriter {
	return &capWriter{max: max, buf: make([]byte, 0, max)}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if n >= w.max {
		w.buf = append(w.buf[:0], p[n-w.max:]...)
		w.truncated = true
		return n, nil
	}
	if len(w.buf)+n > w.max {
		drop := len(w.buf) + n - w.max
		w.buf = append(w.buf[:0], w.buf[drop:]...)
		w.truncated = true
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

func (w *capWriter) Result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return sanitizeUTF8(w.buf), w.truncated
}

// sanitizeUTF8 returns b as valid UTF-8 with invalid sequences replaced.
func sanitizeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// commandResult is the "command execution result" of docs/BRIDGE-API.md.
type commandResult struct {
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exitCode"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	Truncated  bool     `json:"truncated"`
	DurationMs int64    `json:"durationMs"`
	Error      string   `json:"error,omitempty"`
}

// runCommand executes argv with a timeout, optional stdin, and tail-capped
// stdout/stderr.
func runCommand(argv []string, timeout time.Duration, stdin string, extraEnv []string, dir string) commandResult {
	res := commandResult{Argv: argv, ExitCode: -1}
	if len(argv) == 0 {
		res.Error = "empty argv"
		return res
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(cmd)
	if dir != "" {
		cmd.Dir = dir
	}
	if extraEnv != nil {
		cmd.Env = extraEnv
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	outCap := newCapWriter(maxStreamBytes)
	errCap := newCapWriter(maxStreamBytes)
	cmd.Stdout = outCap
	cmd.Stderr = errCap

	runErr := cmd.Run()
	res.DurationMs = time.Since(start).Milliseconds()
	stdout, t1 := outCap.Result()
	stderr, t2 := errCap.Result()
	res.Stdout, res.Stderr, res.Truncated = stdout, stderr, t1 || t2

	if runErr == nil {
		res.ExitCode = 0
		return res
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.ExitCode = -1
		res.Error = fmt.Sprintf("timeout after %s", timeout)
		return res
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		if code, ok := processExitCode(ee); ok {
			res.ExitCode = code
			return res
		}
		res.ExitCode = -1
		res.Error = runErr.Error()
		return res
	}
	res.ExitCode = -1
	res.Error = runErr.Error()
	return res
}

// --- health ------------------------------------------------------------------

type platformToolsInfo struct {
	Dir        string `json:"dir"`
	Adb        string `json:"adb"`
	Fastboot   string `json:"fastboot"`
	AdbVersion string `json:"adbVersion"`
	Source     string `json:"source"`
	AdbErr     string `json:"adbError,omitempty"`
}

type adbServerInfo struct {
	Running bool   `json:"running"`
	Addr    string `json:"addr"`
	PID     int    `json:"pid,omitempty"`
}

type healthResponse struct {
	APIVersion           int                `json:"apiVersion"`
	OK                   bool               `json:"ok"`
	BridgeVersion        string             `json:"bridgeVersion"`
	Hostname             string             `json:"hostname"`
	OS                   string             `json:"os"`
	Arch                 string             `json:"arch"`
	Listen               string             `json:"listen"`
	UptimeSec            int64              `json:"uptimeSec"`
	PlatformTools        *platformToolsInfo `json:"platformTools"`
	AdbServer            adbServerInfo      `json:"adbServer"`
	SerialPorts          []string           `json:"serialPorts"`
	ActiveSerialSessions int                `json:"activeSerialSessions"`
}

func (a *API) health() healthResponse {
	ports := a.serials.listPorts()
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, p.Port)
	}
	return healthResponse{
		APIVersion:           apiVersion,
		OK:                   true,
		BridgeVersion:        bridgeVersion,
		Hostname:             hostname(),
		OS:                   goosName(),
		Arch:                 goarchName(),
		Listen:               a.cfg.Listen,
		UptimeSec:            int64(time.Since(a.started).Seconds()),
		PlatformTools:        a.platformToolsSnapshot(),
		AdbServer:            a.adb.Snapshot(),
		SerialPorts:          names,
		ActiveSerialSessions: a.serials.Count(),
	}
}

// --- small JSON helpers -------------------------------------------------------

// decodeBody decodes a JSON request body, rejecting a missing or wrongly typed
// required field with a 400 handled by the caller. Unknown fields are ignored.
func decodeBody(r io.Reader, v any) error {
	body, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maxBodyBytes {
		return errBodyTooLarge
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	return nil
}

var errBodyTooLarge = errors.New("request body exceeds 64 MiB")
