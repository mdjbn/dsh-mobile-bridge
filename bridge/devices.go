package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// errMissingField is the 400 used when a required body field is absent.
func errMissingField(name string) error {
	return fmt.Errorf("%s is required", name)
}

// errEmptyArg rejects an empty element inside an argv array, which would make
// the child process see a different command than the caller intended.
func errEmptyArg(index int) error {
	return fmt.Errorf("args[%d] is empty", index)
}

// errUnknownAction lists the accepted values so a typo is self-correcting.
func errUnknownAction(scope, got string, allowed ...string) error {
	return fmt.Errorf("unknown %s action %q — use one of: %s", scope, got, strings.Join(allowed, ", "))
}

// adbDevice is one line of `adb devices -l`.
type adbDevice struct {
	Serial      string `json:"serial"`
	State       string `json:"state"`
	Model       string `json:"model,omitempty"`
	Product     string `json:"product,omitempty"`
	Device      string `json:"device,omitempty"`
	TransportID string `json:"transportId,omitempty"`
	Extra       string `json:"extra,omitempty"`
}

// fastbootDevice is one line of `fastboot devices`.
type fastbootDevice struct {
	Serial string `json:"serial"`
	Mode   string `json:"mode"`
}

type devicesResponse struct {
	Adb           []adbDevice      `json:"adb"`
	Fastboot      []fastbootDevice `json:"fastboot"`
	AdbError      *string          `json:"adbError"`
	FastbootError *string          `json:"fastbootError"`
	SerialPorts   []serialPortInfo `json:"serialPorts"`
}

// handleDevices enumerates everything attached to this PC: adb-visible
// devices, bootloader-mode devices, and COM ports.
func (a *API) handleDevices(w http.ResponseWriter, r *http.Request) {
	if !a.requireGet(w, r) {
		return
	}
	response := devicesResponse{
		Adb:         []adbDevice{},
		Fastboot:    []fastbootDevice{},
		SerialPorts: a.serials.listPorts(),
	}

	res := a.runAdb([]string{"devices", "-l"}, 30*time.Second, "")
	if res.ExitCode == 0 {
		response.Adb = parseAdbDevices(res.Stdout)
	} else {
		response.AdbError = ptr(errorText(res, "adb devices -l"))
	}

	pt := a.platformToolsSnapshot()
	if pt.Fastboot == "" {
		response.FastbootError = ptr("fastboot is unavailable: " + pt.AdbErr)
	} else {
		fb := a.runFastboot([]string{"devices"}, "", 30*time.Second, "")
		if fb.ExitCode == 0 {
			response.Fastboot = parseFastbootDevices(fb.Stdout)
		} else {
			response.FastbootError = ptr(errorText(fb, "fastboot devices"))
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func ptr(s string) *string { return &s }

// errorText renders a failed child as one readable line.
func errorText(res commandResult, label string) string {
	parts := []string{}
	if res.Error != "" {
		parts = append(parts, res.Error)
	}
	if text := strings.TrimSpace(res.Stderr); text != "" {
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("exit %d", res.ExitCode))
	}
	return fmt.Sprintf("%s failed: %s", label, strings.Join(parts, " — "))
}

// parseAdbDevices reads `adb devices -l`. The header, blank lines and the
// trailing `* daemon started successfully *` chatter are skipped; every other
// line's first two fields are serial and state, and `key:value` pairs follow.
func parseAdbDevices(stdout string) []adbDevice {
	out := []adbDevice{}
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		device := adbDevice{Serial: fields[0], State: fields[1]}
		var unknown []string
		for _, field := range fields[2:] {
			key, value, ok := strings.Cut(field, ":")
			if !ok {
				unknown = append(unknown, field)
				continue
			}
			switch key {
			case "model":
				device.Model = value
			case "product":
				device.Product = value
			case "device":
				device.Device = value
			case "transport_id":
				device.TransportID = value
			default:
				unknown = append(unknown, field)
			}
		}
		if len(unknown) > 0 {
			device.Extra = strings.Join(unknown, " ")
		}
		out = append(out, device)
	}
	return out
}

// parseFastbootDevices reads `fastboot devices`, whose columns are
// tab-separated (`<serial>\tfastboot`).
func parseFastbootDevices(stdout string) []fastbootDevice {
	out := []fastbootDevice{}
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		mode := "fastboot"
		if len(fields) > 1 {
			mode = fields[1]
		}
		out = append(out, fastbootDevice{Serial: fields[0], Mode: mode})
	}
	return out
}
