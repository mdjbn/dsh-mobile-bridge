package main

import (
	"io"
	"runtime"
	"strings"
)

// multiWriter fans a log line out to stdout and the log file.
type multiWriter struct {
	primary   io.Writer
	secondary io.Writer
}

// newMultiWriter returns an io.Writer that writes to both sinks, stopping at
// the first error so a broken log file cannot silently swallow console output.
func newMultiWriter(primary, secondary io.Writer) io.Writer {
	return &multiWriter{primary: primary, secondary: secondary}
}

func (m *multiWriter) Write(p []byte) (int, error) {
	n, err := m.primary.Write(p)
	if err != nil {
		return n, err
	}
	if _, err := m.secondary.Write(p); err != nil {
		return n, err
	}
	return n, nil
}

// pathFold lowercases a path on Windows, where path comparison is
// case-insensitive, and returns it unchanged everywhere else.
func pathFold(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// pathEqualFold compares two cleaned paths under the platform's folding rules.
func pathEqualFold(a, b string) bool { return pathFold(a) == pathFold(b) }
