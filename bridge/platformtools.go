package main

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// platformTools is the resolved toolchain: where adb and fastboot live, where
// they came from, and what `adb version` said. It aliases the JSON shape the
// health endpoint reports so there is one struct, not two.
type platformTools = platformToolsInfo

// resolveAndStore resolves the toolchain once and records it for every later
// request. It never fails: an unresolvable toolchain is a reported state.
func (a *API) resolveAndStore() {
	info := a.ensurePlatformTools(false)
	a.toolsMu.Lock()
	a.tools = info
	a.toolsErr = info.AdbErr
	a.toolsMu.Unlock()
}

// platformToolsSnapshot returns the current toolchain. It never returns nil, so
// callers (the startup banner) can read fields without a nil check.
func (a *API) platformToolsSnapshot() *platformToolsInfo {
	a.toolsMu.RLock()
	defer a.toolsMu.RUnlock()
	if a.tools == nil {
		return &platformToolsInfo{Source: "missing"}
	}
	return a.tools
}

// executableName appends the platform's executable suffix.
func executableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// platformToolsSubdir is the directory name both the download and the
// config-less default resolve to.
const platformToolsSubdir = "platform-tools"

// defaultPlatformToolsDir is where platform-tools are downloaded to when the
// config names no directory: %LOCALAPPDATA%\dsh-mobile-bridge\platform-tools on
// Windows, ~/.dsh-mobile-bridge/platform-tools elsewhere.
func defaultPlatformToolsDir() string {
	if runtime.GOOS == "windows" {
		if base := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); base != "" {
			return filepath.Join(base, "dsh-mobile-bridge", platformToolsSubdir)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".dsh-mobile-bridge", platformToolsSubdir)
}

// platformToolsArchiveURL is Google's "latest" archive for this OS.
func platformToolsArchiveURL() string {
	switch runtime.GOOS {
	case "windows":
		return "https://dl.google.com/android/repository/platform-tools-latest-windows.zip"
	case "darwin":
		return "https://dl.google.com/android/repository/platform-tools-latest-darwin.zip"
	default:
		return "https://dl.google.com/android/repository/platform-tools-latest-linux.zip"
	}
}

// ensurePlatformTools resolves adb/fastboot from, in order: the configured
// directory, the download directory, PATH, and finally a download when one is
// permitted.
func (a *API) ensurePlatformTools(download bool) *platformToolsInfo {
	adbName := executableName("adb")
	for _, candidate := range a.platformToolsDirCandidates() {
		if isFile(filepath.Join(candidate.dir, adbName)) {
			return a.probePlatformTools(candidate.dir, candidate.source)
		}
	}
	if found, err := exec.LookPath(adbName); err == nil {
		return a.probePlatformTools(filepath.Dir(found), "path")
	}
	if download && a.cfg.AutoDownloadPlatformTools {
		dir := strings.TrimSpace(a.cfg.PlatformToolsDir)
		if dir == "" {
			dir = defaultPlatformToolsDir()
		}
		if dir == "" {
			return &platformToolsInfo{Source: "missing", AdbErr: "platform-tools were not found and no download directory is available"}
		}
		if err := downloadPlatformTools(dir, a.logger); err != nil {
			a.logger.Printf("platform-tools: download failed: %v", err)
			return &platformToolsInfo{Source: "missing", AdbErr: fmt.Sprintf("platform-tools are missing and the download failed: %v", err)}
		}
		if isFile(filepath.Join(dir, adbName)) {
			return a.probePlatformTools(dir, "autodownload")
		}
	}
	return &platformToolsInfo{
		Source: "missing",
		AdbErr: "adb was not found in the configured platformToolsDir, the download directory, or PATH — " +
			"run the install_platform_tools setup action to fetch Google's platform-tools",
	}
}

type ptCandidate struct {
	dir    string
	source string
}

func (a *API) platformToolsDirCandidates() []ptCandidate {
	out := make([]ptCandidate, 0, 2)
	if dir := strings.TrimSpace(a.cfg.PlatformToolsDir); dir != "" {
		out = append(out, ptCandidate{dir: dir, source: "config"})
	}
	if dir := defaultPlatformToolsDir(); dir != "" && dir != strings.TrimSpace(a.cfg.PlatformToolsDir) {
		out = append(out, ptCandidate{dir: dir, source: "autodownload"})
	}
	return out
}

// probePlatformTools fills in the health-shaped record for a directory and
// records the adb version banner when adb runs.
func (a *API) probePlatformTools(dir, source string) *platformToolsInfo {
	info := &platformToolsInfo{
		Dir:    dir,
		Source: source,
		Adb:    filepath.Join(dir, executableName("adb")),
	}
	if fastboot := filepath.Join(dir, executableName("fastboot")); isFile(fastboot) {
		info.Fastboot = fastboot
	}
	res := runCommand([]string{info.Adb, "version"}, 20*time.Second, "", nil, dir)
	if res.ExitCode == 0 {
		text := strings.TrimSpace(res.Stdout)
		if line, _, ok := strings.Cut(text, "\n"); ok {
			info.AdbVersion = strings.TrimSpace(line)
		} else {
			info.AdbVersion = text
		}
	} else {
		info.AdbErr = strings.TrimSpace(res.Error + " " + res.Stderr)
		if strings.TrimSpace(info.AdbErr) == "" {
			info.AdbErr = fmt.Sprintf("adb version exited with %d", res.ExitCode)
		}
	}
	return info
}

// downloadPlatformTools fetches Google's archive and extracts it into dir,
// replacing whatever was there. It is safe to run twice.
func downloadPlatformTools(dir string, logger *log.Logger) error {
	url := platformToolsArchiveURL()
	if logger != nil {
		logger.Printf("platform-tools: downloading %s", url)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", response.StatusCode, url)
	}
	tmp, err := os.CreateTemp("", "platform-tools-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	written, err := io.Copy(tmp, response.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if written < 1<<20 {
		return fmt.Errorf("archive looks truncated (%d bytes)", written)
	}
	if err := extractPlatformToolsZip(tmpName, dir); err != nil {
		return err
	}
	if logger != nil {
		logger.Printf("platform-tools: extracted %d bytes into %s", written, dir)
	}
	return nil
}

// extractPlatformToolsZip unpacks an archive whose entries live under the
// top-level "platform-tools/" directory, stripping that prefix. Zip-slip is
// rejected rather than sanitized.
func extractPlatformToolsZip(archive, destination string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer reader.Close()

	const prefix = platformToolsSubdir + "/"
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	found := false
	for _, entry := range reader.File {
		name := filepath.ToSlash(entry.Name)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			continue
		}
		found = true
		target := filepath.Join(root, filepath.FromSlash(rel))
		if !withinRoot(filepath.Clean(target), root) {
			return fmt.Errorf("archive entry escapes the destination: %s", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(entry, target); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("archive %s contains no %s/ directory", archive, platformToolsSubdir)
	}
	return nil
}

func writeZipEntry(entry *zip.File, target string) error {
	src, err := entry.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	mode := entry.Mode()
	if mode&0o111 != 0 || mode == 0 {
		mode = 0o755
	} else {
		mode = 0o644
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
