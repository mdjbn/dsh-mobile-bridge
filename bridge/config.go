package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config mirrors dsh-mobile-bridge.json.
type Config struct {
	Listen                    string   `json:"listen"`
	Token                     string   `json:"token"`
	PlatformToolsDir          string   `json:"platformToolsDir"`
	AdbPort                   int      `json:"adbPort"`
	StartAdbServerOnLaunch    bool     `json:"startAdbServerOnLaunch"`
	AutoDownloadPlatformTools bool     `json:"autoDownloadPlatformTools"`
	AllowedRoots              []string `json:"allowedRoots"`
	LogFile                   string   `json:"logFile"`

	// path records where this config was loaded from; not part of the JSON.
	path string
}

// defaultConfig returns the documented defaults.
func defaultConfig() Config {
	return Config{
		Listen:                    "0.0.0.0:8787",
		AdbPort:                   5037,
		StartAdbServerOnLaunch:    true,
		AutoDownloadPlatformTools: true,
		AllowedRoots:              []string{},
	}
}

// defaultConfigPath is dsh-mobile-bridge.json next to the executable, falling
// back to the working directory.
func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "dsh-mobile-bridge.json")
	}
	wd, err := os.Getwd()
	if err != nil {
		return "dsh-mobile-bridge.json"
	}
	return filepath.Join(wd, "dsh-mobile-bridge.json")
}

// loadConfig reads path when it exists, otherwise returns defaults. Unknown
// JSON fields are ignored.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	cfg.path = path
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.Token = resolveToken("")
			return cfg, nil
		}
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.path = path
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:8787"
	}
	if cfg.AdbPort == 0 {
		cfg.AdbPort = 5037
	}
	if cfg.AllowedRoots == nil {
		cfg.AllowedRoots = []string{}
	}
	cfg.Token = resolveToken(cfg.Token)
	return cfg, nil
}

// resolveToken applies the documented order: --token, env, config, generated.
func resolveToken(fromConfig string) string {
	if v := strings.TrimSpace(lookupTokenFlag()); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("DSH_MOBILE_BRIDGE_TOKEN")); v != "" {
		return v
	}
	if v := strings.TrimSpace(fromConfig); v != "" {
		return v
	}
	return randomToken()
}

// lookupTokenFlag is replaced by main once flags are parsed.
var tokenFlagValue string

func lookupTokenFlag() string { return tokenFlagValue }

// randomToken returns 32 lowercase hex characters (16 random bytes).
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fall back to something deterministic-but-unlikely rather than panic.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// saveConfig persists cfg to its config file. It is used after generating a
// token so that restarts are stable.
func saveConfig(cfg *Config) error {
	if cfg.path == "" {
		return nil
	}
	type out struct {
		Listen                    string   `json:"listen"`
		Token                     string   `json:"token"`
		PlatformToolsDir          string   `json:"platformToolsDir"`
		AdbPort                   int      `json:"adbPort"`
		StartAdbServerOnLaunch    bool     `json:"startAdbServerOnLaunch"`
		AutoDownloadPlatformTools bool     `json:"autoDownloadPlatformTools"`
		AllowedRoots              []string `json:"allowedRoots"`
		LogFile                   string   `json:"logFile"`
	}
	o := out{
		Listen:                    cfg.Listen,
		Token:                     cfg.Token,
		PlatformToolsDir:          cfg.PlatformToolsDir,
		AdbPort:                   cfg.AdbPort,
		StartAdbServerOnLaunch:    cfg.StartAdbServerOnLaunch,
		AutoDownloadPlatformTools: cfg.AutoDownloadPlatformTools,
		AllowedRoots:              cfg.AllowedRoots,
		LogFile:                   cfg.LogFile,
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(cfg.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(cfg.path, b, 0o600)
}

// applyFlags overlays explicitly-set flags on top of the config file.
func applyFlags(cfg *Config, ov flagOverrides) {
	if ov.listen {
		cfg.Listen = ov.listenVal
	}
	if ov.token {
		cfg.Token = ov.tokenVal
		tokenFlagValue = ov.tokenVal
	}
	if ov.ptDir {
		cfg.PlatformToolsDir = ov.ptDirVal
	}
	if ov.adbPort {
		cfg.AdbPort = ov.adbPortVal
	}
	if ov.noAutoAdb {
		cfg.StartAdbServerOnLaunch = !ov.noAutoAdbVal
	}
	if ov.noAutoDL {
		cfg.AutoDownloadPlatformTools = !ov.noAutoDLVal
	}
	if ov.logFile {
		cfg.LogFile = ov.logFileVal
	}
	if cfg.AdbPort <= 0 || cfg.AdbPort > 65535 {
		cfg.AdbPort = 5037
	}
}

// jsonMarshalIndent is a small helper used by --print-config.
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// --- allowedRoots enforcement -------------------------------------------------

// checkAllowed validates a client-supplied path against allowedRoots. An empty
// allowedRoots list means unrestricted.
func (c *Config) checkAllowed(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("bad path: %v", err)
	}
	abs = filepath.Clean(abs)
	if len(c.AllowedRoots) == 0 {
		return nil
	}
	for _, root := range c.AllowedRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		rabs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rabs = filepath.Clean(rabs)
		if withinRoot(abs, rabs) {
			return nil
		}
	}
	return errPathNotAllowed
}

// errPathNotAllowed maps to HTTP 403 {"code":"path_not_allowed"}.
var errPathNotAllowed = errors.New("path_not_allowed")

func withinRoot(abs, root string) bool {
	if pathEqualFold(abs, root) {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(pathFold(abs), pathFold(root))
}
