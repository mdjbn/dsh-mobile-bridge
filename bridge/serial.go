package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// errPortBusy maps to HTTP 409 {"code":"port_busy"}.
var errPortBusy = errors.New("port_busy")

// errUnknownSession maps to HTTP 404 {"code":"session_unknown"}: the client is
// holding a session id the bridge no longer has, which is a different problem
// from a malformed request.
var errUnknownSession = errors.New("session_unknown")

// serialPortInfo is one COM port as reported to clients.
type serialPortInfo struct {
	Port        string `json:"port"`
	Description string `json:"description,omitempty"`
	InUse       bool   `json:"inUse"`
}

// serialSession is one open port plus the locks that keep concurrent reads and
// writes from interleaving on the same handle.
type serialSession struct {
	id   string
	port string

	mu     sync.Mutex
	handle serial.Port
	closed bool
}

// serialManager owns every open serial session.
type serialManager struct {
	mu       sync.Mutex
	nextID   int
	sessions map[string]*serialSession
	byPort   map[string]*serialSession
}

func newSerialManager() *serialManager {
	return &serialManager{sessions: map[string]*serialSession{}, byPort: map[string]*serialSession{}}
}

// Count reports how many sessions are open.
func (m *serialManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// CloseAll closes every session; used on shutdown.
func (m *serialManager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*serialSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = map[string]*serialSession{}
	m.byPort = map[string]*serialSession{}
	m.mu.Unlock()
	for _, session := range sessions {
		session.mu.Lock()
		if session.handle != nil {
			_ = session.handle.Close()
			session.closed = true
		}
		session.mu.Unlock()
	}
}

// listPorts enumerates the machine's serial ports, tagging the ones this bridge
// already holds open.
func (m *serialManager) listPorts() []serialPortInfo {
	descriptions := map[string]string{}
	names := []string{}
	if detailed, err := enumerator.GetDetailedPortsList(); err == nil {
		for _, detail := range detailed {
			names = append(names, detail.Name)
			label := detail.Product
			if strings.TrimSpace(label) == "" {
				label = detail.Manufacturer
			}
			if strings.TrimSpace(label) == "" && detail.IsUSB {
				label = fmt.Sprintf("USB serial %s:%s", detail.VID, detail.PID)
			}
			if strings.TrimSpace(label) != "" {
				descriptions[detail.Name] = label
			}
		}
	} else if plain, err := serial.GetPortsList(); err == nil {
		names = plain
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]serialPortInfo, 0, len(names))
	for _, name := range names {
		_, open := m.byPort[name]
		out = append(out, serialPortInfo{Port: name, Description: descriptions[name], InUse: open})
	}
	return out
}

// open opens a port and returns its session id.
func (m *serialManager) open(options serialOpenRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byPort[options.Port]; exists {
		return "", fmt.Errorf("%w: %s is already open in this bridge", errPortBusy, options.Port)
	}
	mode := &serial.Mode{
		BaudRate: options.BaudRate,
		DataBits: options.DataBits,
		Parity:   parseParity(options.Parity),
		StopBits: parseStopBits(options.StopBits),
	}
	handle, err := serial.Open(options.Port, mode)
	if err != nil {
		return "", fmt.Errorf("cannot open %s: %v", options.Port, err)
	}
	if err := handle.SetReadTimeout(time.Duration(options.ReadTimeoutMs) * time.Millisecond); err != nil {
		_ = handle.Close()
		return "", fmt.Errorf("cannot set the read timeout on %s: %v", options.Port, err)
	}
	m.nextID++
	id := "s" + strconv.Itoa(m.nextID)
	session := &serialSession{id: id, port: options.Port, handle: handle}
	m.sessions[id] = session
	m.byPort[options.Port] = session
	return id, nil
}

func (m *serialManager) lookup(id string) (*serialSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: unknown sessionId %q", errUnknownSession, id)
	}
	return session, nil
}

// close closes one session and frees its port.
func (m *serialManager) close(id string) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		delete(m.byPort, session.port)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: unknown sessionId %q", errUnknownSession, id)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil
	}
	session.closed = true
	return session.handle.Close()
}

// write sends bytes to one session.
func (m *serialManager) write(id string, payload []byte, echoNewline bool) (int, error) {
	session, err := m.lookup(id)
	if err != nil {
		return 0, err
	}
	if echoNewline {
		payload = append(payload, '\r', '\n')
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return 0, fmt.Errorf("session %s is closed", id)
	}
	written, err := session.handle.Write(payload)
	if err != nil {
		return written, fmt.Errorf("write to %s failed: %v", session.port, err)
	}
	return written, nil
}

// read drains the port until `maxBytes` arrive, the port falls quiet after
// receiving something, or `timeoutMs` elapses without any byte.
//
// "Falls quiet" is the useful part: a console that prints one line should
// return that line immediately rather than holding the caller for the whole
// timeout. `timedOut` is therefore true only when nothing at all arrived.
func (m *serialManager) read(id string, timeoutMs int, maxBytes int) ([]byte, bool, error) {
	session, err := m.lookup(id)
	if err != nil {
		return nil, false, err
	}
	if maxBytes <= 0 {
		maxBytes = 65536
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil, false, fmt.Errorf("session %s is closed", id)
	}

	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	collected := make([]byte, 0, 4096)
	scratch := make([]byte, 4096)
	for len(collected) < maxBytes {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		if err := session.handle.SetReadTimeout(remaining); err != nil {
			return collected, false, fmt.Errorf("cannot set the read timeout: %v", err)
		}
		read, err := session.handle.Read(scratch)
		if err != nil {
			return collected, false, fmt.Errorf("read from %s failed: %v", session.port, err)
		}
		if read == 0 {
			// Either the timeout elapsed, or (with a zero remaining budget)
			// the port is momentarily quiet.
			if len(collected) > 0 && time.Now().Before(deadline) {
				break // drained what was pending; do not burn the whole budget
			}
			return collected, len(collected) == 0, nil
		}
		collected = append(collected, scratch[:read]...)
		if time.Now().After(deadline) {
			break
		}
	}
	if err := session.handle.SetReadTimeout(0); err != nil {
		return collected, false, fmt.Errorf("cannot restore the read timeout: %v", err)
	}
	return collected, false, nil
}

func parseParity(value string) serial.Parity {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "odd":
		return serial.OddParity
	case "even":
		return serial.EvenParity
	case "mark":
		return serial.MarkParity
	case "space":
		return serial.SpaceParity
	default:
		return serial.NoParity
	}
}

func parseStopBits(value float64) serial.StopBits {
	switch value {
	case 1.5:
		return serial.OnePointFiveStopBits
	case 2:
		return serial.TwoStopBits
	default:
		return serial.OneStopBit
	}
}

// --- requests and handlers ----------------------------------------------------

type serialOpenRequest struct {
	Port          string  `json:"port"`
	BaudRate      int     `json:"baudRate"`
	DataBits      int     `json:"dataBits"`
	StopBits      float64 `json:"stopBits"`
	Parity        string  `json:"parity"`
	ReadTimeoutMs int     `json:"readTimeoutMs"`
}

type serialOpenResponse struct {
	SessionID string `json:"sessionId"`
	Port      string `json:"port"`
	BaudRate  int    `json:"baudRate"`
}

type serialWriteRequest struct {
	SessionID   string `json:"sessionId"`
	Data        string `json:"data"`
	Encoding    string `json:"encoding"`
	EchoNewline bool   `json:"echoNewline"`
}

type serialWriteResponse struct {
	SessionID    string `json:"sessionId"`
	BytesWritten int    `json:"bytesWritten"`
}

type serialReadRequest struct {
	SessionID string `json:"sessionId"`
	TimeoutMs int    `json:"timeoutMs"`
	MaxBytes  int    `json:"maxBytes"`
}

type serialReadResponse struct {
	SessionID string `json:"sessionId"`
	Data      string `json:"data"`
	Bytes     int    `json:"bytes"`
	Text      string `json:"text"`
	TimedOut  bool   `json:"timedOut"`
}

type serialCloseRequest struct {
	SessionID string `json:"sessionId"`
}

func (a *API) handleSerialPorts(w http.ResponseWriter, r *http.Request) {
	if !a.requireGet(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ports": a.serials.listPorts()})
}

func (a *API) handleSerialOpen(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req serialOpenRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	req.Port = strings.TrimSpace(req.Port)
	if req.Port == "" {
		writeBadRequest(w, errMissingField("port"))
		return
	}
	if req.BaudRate <= 0 {
		req.BaudRate = 115200
	}
	if req.DataBits <= 0 {
		req.DataBits = 8
	}
	if strings.TrimSpace(req.Parity) == "" {
		req.Parity = "none"
	}
	if req.StopBits == 0 {
		req.StopBits = 1
	}
	if req.ReadTimeoutMs <= 0 {
		req.ReadTimeoutMs = 500
	}
	id, err := a.serials.open(req)
	if err != nil {
		if errors.Is(err, errPortBusy) {
			writeError(w, http.StatusConflict, "port_busy", err.Error())
			return
		}
		writeBadRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, serialOpenResponse{SessionID: id, Port: req.Port, BaudRate: req.BaudRate})
}

func (a *API) handleSerialWrite(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req serialWriteRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeBadRequest(w, errMissingField("sessionId"))
		return
	}
	payload, err := decodeSerialData(req.Data, req.Encoding)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	written, err := a.serials.write(req.SessionID, payload, req.EchoNewline)
	if err != nil {
		writeFsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, serialWriteResponse{SessionID: req.SessionID, BytesWritten: written})
}

func (a *API) handleSerialRead(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req serialReadRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeBadRequest(w, errMissingField("sessionId"))
		return
	}
	timeout := req.TimeoutMs
	if timeout < 0 {
		timeout = 0
	}
	if timeout > 60000 {
		timeout = 60000
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 65536
	}
	if maxBytes > 4<<20 {
		maxBytes = 4 << 20
	}
	data, timedOut, err := a.serials.read(req.SessionID, timeout, maxBytes)
	if err != nil {
		writeFsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, serialReadResponse{
		SessionID: req.SessionID,
		Data:      base64.StdEncoding.EncodeToString(data),
		Bytes:     len(data),
		Text:      strings.ToValidUTF8(string(data), "\uFFFD"),
		TimedOut:  timedOut,
	})
}

func (a *API) handleSerialClose(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req serialCloseRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeBadRequest(w, errMissingField("sessionId"))
		return
	}
	if err := a.serials.close(req.SessionID); err != nil {
		writeFsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": true})
}

// decodeSerialData turns the request's `data` into bytes according to `encoding`.
func decodeSerialData(data, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "base64":
		out, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("data is not valid base64: %v", err)
		}
		return out, nil
	case "utf8", "text":
		return []byte(data), nil
	case "hex":
		cleaned := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", "0x", "", "0X", "").Replace(data)
		out, err := hex.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("data is not valid hex: %v", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q — use base64, utf8, or hex", encoding)
	}
}
