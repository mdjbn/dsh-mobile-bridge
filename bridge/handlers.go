package main

import (
	"net/http"
	"strings"
)

// handleHealth reports the bridge's whole state in one call.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !a.requireGet(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.health())
}

type setupRequest struct {
	Action string `json:"action"`
}

type setupResponse struct {
	OK      bool           `json:"ok"`
	Message string         `json:"message"`
	Health  healthResponse `json:"health"`
}

// handleSetup runs one preparation action. Every action is idempotent, so a
// caller can retry without reasoning about prior state.
func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	var req setupRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		writeBadRequest(w, errMissingField("action"))
		return
	}

	ok := true
	var message string
	switch action {
	case "install_platform_tools":
		a.toolsMu.Lock()
		info := a.ensurePlatformTools(true)
		a.tools = info
		a.toolsErr = info.AdbErr
		a.toolsMu.Unlock()
		if info.Adb == "" {
			ok = false
			message = "platform-tools could not be installed: " + info.AdbErr
		} else {
			message = "platform-tools are ready in " + info.Dir
		}
	case "start_adb_server":
		if err := a.adb.Start(true); err != nil {
			ok = false
			message = err.Error()
		} else {
			snap := a.adb.Snapshot()
			message = "the adb server is listening on " + snap.Addr
		}
	case "stop_adb_server":
		if err := a.adb.Stop(); err != nil {
			ok = false
			message = err.Error()
		} else {
			message = "the adb server was stopped"
		}
	case "refresh":
		a.resolveAndStore()
		info := a.platformToolsSnapshot()
		if info.Adb == "" {
			ok = false
			message = "adb is still unavailable: " + info.AdbErr
		} else {
			message = "resolved platform-tools in " + info.Dir
		}
	default:
		writeBadRequest(w, errUnknownAction("setup", action,
			"install_platform_tools", "start_adb_server", "stop_adb_server", "refresh"))
		return
	}

	writeJSON(w, http.StatusOK, setupResponse{OK: ok, Message: message, Health: a.health()})
}

// toolRequest is the shared body of /v1/adb and /v1/fastboot.
type toolRequest struct {
	Args      []string `json:"args"`
	Serial    string   `json:"serial"`
	TimeoutMs int      `json:"timeoutMs"`
	Stdin     string   `json:"stdin"`
}

// handleAdb runs adb on this PC with a literal argv.
func (a *API) handleAdb(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	req, ok := decodeToolRequest(w, r)
	if !ok {
		return
	}
	full := buildToolArgs(req.Args, req.Serial)
	res := a.runAdb(full, clampTimeout(req.TimeoutMs, 60000), req.Stdin)
	res.Argv = full
	writeJSON(w, http.StatusOK, res)
}

// handleFastboot runs fastboot against an attached device, serialized per serial.
func (a *API) handleFastboot(w http.ResponseWriter, r *http.Request) {
	if !a.requirePost(w, r) {
		return
	}
	req, ok := decodeToolRequest(w, r)
	if !ok {
		return
	}
	full := buildToolArgs(req.Args, req.Serial)
	res := a.runFastboot(full, req.Serial, clampTimeout(req.TimeoutMs, 60000), "")
	res.Argv = full
	writeJSON(w, http.StatusOK, res)
}

func decodeToolRequest(w http.ResponseWriter, r *http.Request) (toolRequest, bool) {
	var req toolRequest
	if err := decodeBody(r.Body, &req); err != nil {
		writeBadRequest(w, err)
		return req, false
	}
	if len(req.Args) == 0 {
		writeBadRequest(w, errMissingField("args"))
		return req, false
	}
	for i, arg := range req.Args {
		if strings.TrimSpace(arg) == "" {
			writeBadRequest(w, errEmptyArg(i))
			return req, false
		}
	}
	return req, true
}
