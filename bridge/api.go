package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// maxBodyBytes caps request bodies at 64 MiB as specified.
const maxBodyBytes = 64 << 20

// API is the wiring hub for every endpoint.
type API struct {
	cfg      *Config
	logger   *log.Logger
	started  time.Time
	exec     *execManager
	adb      *adbServer
	fs       *fsManager
	serials  *serialManager
	toolsMu  sync.RWMutex
	tools    *platformTools
	toolsErr string
}

func newAPI(cfg *Config, logger *log.Logger, started time.Time) *API {
	a := &API{
		cfg:     cfg,
		logger:  logger,
		started: started,
		exec:    newExecManager(),
		fs:      &fsManager{},
		serials: newSerialManager(),
	}
	a.adb = &adbServer{api: a, port: cfg.AdbPort}
	return a
}

// handler builds the mux and wraps it in the bearer-auth middleware.
func (a *API) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", a.handleHealth)
	mux.HandleFunc("/v1/setup", a.handleSetup)
	mux.HandleFunc("/v1/devices", a.handleDevices)
	mux.HandleFunc("/v1/adb", a.handleAdb)
	mux.HandleFunc("/v1/fastboot", a.handleFastboot)
	mux.HandleFunc("/v1/fs/put", a.handleFsPut)
	mux.HandleFunc("/v1/fs/get", a.handleFsGet)
	mux.HandleFunc("/v1/fs/ls", a.handleFsLs)
	mux.HandleFunc("/v1/serial/open", a.handleSerialOpen)
	mux.HandleFunc("/v1/serial/write", a.handleSerialWrite)
	mux.HandleFunc("/v1/serial/read", a.handleSerialRead)
	mux.HandleFunc("/v1/serial/close", a.handleSerialClose)
	mux.HandleFunc("/v1/serial/ports", a.handleSerialPorts)
	mux.HandleFunc("/", a.handleNotFound)
	return a.withAuth(mux)
}

func (a *API) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.URL.Path)
}

// withAuth enforces `Authorization: Bearer <token>` on every /v1/* request.
func (a *API) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const p = "Bearer "
		if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) ||
			subtle.ConstantTimeCompare([]byte(h[len(p):]), []byte(a.cfg.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// --- JSON helpers --------------------------------------------------------------

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

// writeBadRequest is the generic 400 used for a missing or wrong-typed field.
func writeBadRequest(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, "bad_request", err.Error())
}

// writeFsError maps filesystem and serial-session errors to their documented
// status codes: containment violations are 403, a missing path or a stale
// session id is 404, and everything else (a malformed path, an already-closed
// port) stays a 400.
func writeFsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPathNotAllowed):
		writeError(w, http.StatusForbidden, "path_not_allowed", "path is outside allowedRoots")
	case errors.Is(err, errUnknownSession):
		writeError(w, http.StatusNotFound, "session_unknown", err.Error())
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func (a *API) requireGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return false
	}
	return true
}

func (a *API) requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return false
	}
	return true
}

// --- misc ---------------------------------------------------------------------

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func goosName() string   { return runtime.GOOS }
func goarchName() string { return runtime.GOARCH }
