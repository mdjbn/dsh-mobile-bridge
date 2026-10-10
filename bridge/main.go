// Command dsh-mobile-bridge is the Windows-side bridge that owns adb, fastboot
// and the COM ports for the DSH harness-pc-mobile plugin.
//
// It implements the frozen HTTP API v1 described in docs/BRIDGE-API.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// bridgeVersion is reported by /v1/health.
const bridgeVersion = "1.0.0"

// apiVersion is the frozen contract revision implemented here.
const apiVersion = 1

func main() {
	fs := flag.NewFlagSet("dsh-mobile-bridge", flag.ExitOnError)
	var (
		flagListen    = fs.String("listen", "", "HTTP listen address (default 0.0.0.0:8787)")
		flagToken     = fs.String("token", "", "bearer token (default: env DSH_MOBILE_BRIDGE_TOKEN, then config, then generated)")
		flagConfig    = fs.String("config", "", "path to the JSON config file")
		flagPTHome    = fs.String("platform-tools-dir", "", "directory holding adb/fastboot")
		flagAdbPort   = fs.Int("adb-port", 0, "TCP port for the adb server (default 5037)")
		flagNoAutoAdb = fs.Bool("no-start-adb-server", false, "do not start the adb server on launch")
		flagNoAutoDL  = fs.Bool("no-auto-download", false, "never download platform-tools automatically")
		flagLogFile   = fs.String("log-file", "", "also append log lines to this file")
		flagPrintCfg  = fs.Bool("print-config", false, "print the effective config as JSON and exit")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["token"] {
		tokenFlagValue = *flagToken
	}

	cfgPath := *flagConfig
	if cfgPath == "" {
		cfgPath = defaultConfigPath()
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	applyFlags(&cfg, flagOverrides{
		listen: set["listen"], listenVal: *flagListen,
		token: set["token"], tokenVal: *flagToken,
		ptDir: set["platform-tools-dir"], ptDirVal: *flagPTHome,
		adbPort: set["adb-port"], adbPortVal: *flagAdbPort,
		noAutoAdb: set["no-start-adb-server"], noAutoAdbVal: *flagNoAutoAdb,
		noAutoDL: set["no-auto-download"], noAutoDLVal: *flagNoAutoDL,
		logFile: set["log-file"], logFileVal: *flagLogFile,
	})

	if *flagPrintCfg {
		b, _ := jsonMarshalIndent(cfg)
		fmt.Println(string(b))
		return
	}

	// Persist the effective config, so a generated token stays stable across
	// restarts instead of forcing the DSH plugin to be reconfigured each time.
	if err := saveConfig(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", cfg.path, err)
	}

	logger, logCloser, err := newLogger(cfg.LogFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log file:", err)
		os.Exit(2)
	}
	if logCloser != nil {
		defer logCloser.Close()
	}

	started := time.Now()
	api := newAPI(&cfg, logger, started)

	// Resolving platform-tools is best effort: a failure never aborts startup.
	api.resolveAndStore()

	banner(&cfg, api)

	// ADB server startup is best effort as well.
	if cfg.StartAdbServerOnLaunch {
		if err := api.adb.Start(true); err != nil {
			logger.Printf("adb server: not started: %v", err)
		} else {
			logger.Printf("adb server: listening on %s (pid %d)", api.adb.Snapshot().Addr, api.adb.Snapshot().PID)
		}
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", listenAddr(cfg.Listen))
	if err != nil {
		logger.Printf("FATAL: cannot listen on %s: %v", cfg.Listen, err)
		fmt.Fprintf(os.Stderr, "FATAL: cannot listen on %s: %v\n", cfg.Listen, err)
		os.Exit(1)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("FATAL: http server: %v", err)
			fmt.Fprintf(os.Stderr, "FATAL: http server: %v\n", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	s := <-sig
	logger.Printf("shutting down (signal %s)", s)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	api.serials.CloseAll()
	api.adb.StopIfOwned()
	logger.Printf("stopped")
}

type flagOverrides struct {
	listen       bool
	listenVal    string
	token        bool
	tokenVal     string
	ptDir        bool
	ptDirVal     string
	adbPort      bool
	adbPortVal   int
	noAutoAdb    bool
	noAutoAdbVal bool
	noAutoDL     bool
	noAutoDLVal  bool
	logFile      bool
	logFileVal   string
}

// newLogger returns a logger writing to stdout and, when path is non-empty, to
// that file as well.
func newLogger(path string) (*log.Logger, *os.File, error) {
	if strings.TrimSpace(path) == "" {
		return log.New(os.Stdout, "", log.LstdFlags), nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return log.New(newMultiWriter(os.Stdout, f), "", log.LstdFlags), f, nil
}

// banner prints the human-facing startup summary. The token appears here and in
// the config file, nowhere else.
func banner(cfg *Config, api *API) {
	pt := api.platformToolsSnapshot()
	log.Printf("dsh-mobile-bridge %s (%s/%s)", bridgeVersion, runtime.GOOS, runtime.GOARCH)
	if dual := listenAddr(cfg.Listen); dual != cfg.Listen {
		log.Printf("listen        : %s (dual-stack; config says %s)", dual, cfg.Listen)
	} else {
		log.Printf("listen        : %s", cfg.Listen)
	}
	log.Printf("token         : %s", cfg.Token)
	log.Printf("config file   : %s", cfg.path)
	if pt.Dir != "" {
		log.Printf("platform-tools: %s (source=%s)", pt.Dir, pt.Source)
	} else {
		log.Printf("platform-tools: <not found> (source=%s)", pt.Source)
	}
	if pt.Adb != "" {
		log.Printf("adb           : %s", pt.Adb)
	}
	if pt.AdbVersion != "" {
		log.Printf("adb version   : %s", strings.TrimSpace(pt.AdbVersion))
	} else if pt.AdbErr != "" {
		log.Printf("adb version   : unavailable (%s)", pt.AdbErr)
	}
	if pt.Fastboot != "" {
		log.Printf("fastboot      : %s", pt.Fastboot)
	}
	ports := api.serials.listPorts()
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.Description != "" {
			names = append(names, fmt.Sprintf("%s (%s)", p.Port, p.Description))
		} else {
			names = append(names, p.Port)
		}
	}
	if len(names) == 0 {
		log.Printf("serial ports  : none detected")
	} else {
		log.Printf("serial ports  : %s", strings.Join(names, ", "))
	}
	port := portOf(cfg.Listen)
	usable, linkLocal := addrCandidates()
	host, _, _ := net.SplitHostPort(cfg.Listen)
	if h := strings.TrimSpace(host); h != "" && h != "0.0.0.0" && h != "::" {
		// The listener is already pinned to one address: that is the answer, and the
		// interface scan would only print addresses this process is not listening on.
		log.Printf("bridgeUrl     : %s   (the configured listen address)", "http://"+net.JoinHostPort(h, port))
		return
	}
	if len(usable) == 0 {
		log.Printf("bridgeUrl     : (none) no usable LAN address on this PC — check that an adapter has a DHCP lease")
	} else {
		log.Printf("bridgeUrl     : %s", bridgeURL(usable[0].ip, port))
		shown := usable[1:]
		for i, c := range shown {
			if i == 4 {
				log.Printf("also at       : … and %d more address(es)", len(shown)-i)
				break
			}
			log.Printf("also at       : %s   (%s)", bridgeURL(c.ip, port), c.iface)
		}
	}
	for i, l := range linkLocal {
		if i == 3 {
			log.Printf("skipped       : … and %d more 169.254.x.x address(es)", len(linkLocal)-i)
			break
		}
		if i == 0 {
			log.Printf("skipped       : %s (%s) — 169.254.x.x means this adapter never got a DHCP lease, so a server on your LAN cannot dial it; use one of the addresses above",
				l.ip, l.iface)
			continue
		}
		log.Printf("                %s (%s)", l.ip, l.iface)
	}
}

// addrCandidate is one dialable address plus the interface that carries it.
type addrCandidate struct {
	ip    net.IP
	iface string
	score int
}

// addrCandidates lists the addresses the bridge can be reached at, best first, and
// separately the link-local ones it deliberately ignores.
//
// Picking "the first non-loopback IPv4" is wrong on a real Windows box: it happily
// returns a dormant adapter's 169.254.x.x (no DHCP lease), and the user then copies an
// address nothing can route to. So candidates are ranked — RFC1918 LAN addresses first,
// virtual/VPN adapters last — and link-local addresses are reported as a warning.
func addrCandidates() (usable []addrCandidate, linkLocal []addrCandidate) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	virtualNames := []string{"wsl", "vethernet", "docker", "vmware", "virtual", "vbox", "hyper-v", "tap", "tun", "zerotier", "tailscale", "bluetooth", "npcap", "radmin", "hamachi"}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		name := strings.ToLower(ifc.Name)
		virtual := false
		for _, bad := range virtualNames {
			if strings.Contains(name, bad) {
				virtual = true
				break
			}
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsUnspecified() {
				continue
			}
			ip := ipnet.IP
			if v4 := ip.To4(); v4 != nil {
				// 169.254.0.0/16 means this adapter never got a DHCP lease — the exact
				// trap that made the old "first non-loopback IPv4" print a dead URL.
				if ip.IsLinkLocalUnicast() {
					linkLocal = append(linkLocal, addrCandidate{ip: v4, iface: ifc.Name})
					continue
				}
			} else if !ip.IsGlobalUnicast() {
				continue // fe80::/10 and friends: real on every adapter, but unreachable from the LAN
			}
			score := 0
			if virtual {
				score += 20
			}
			if v4 := ip.To4(); v4 != nil {
				switch {
				case v4[0] == 192 && v4[1] == 168:
					score += 1
				case v4[0] == 10:
					score += 2
				case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
					score += 3
				case v4.IsPrivate():
					score += 3
				default:
					score += 5
				}
				ip = v4
			} else if ip.IsPrivate() {
				score += 4 // ULA fc00::/7
			} else {
				score += 6 // global IPv6
			}
			usable = append(usable, addrCandidate{ip: ip, iface: ifc.Name, score: score})
		}
	}
	sort.SliceStable(usable, func(i, j int) bool { return usable[i].score < usable[j].score })
	return usable, linkLocal
}

// bridgeURL renders a URL for an address, bracketing IPv6 literals.
func bridgeURL(ip net.IP, port string) string {
	return "http://" + net.JoinHostPort(ip.String(), port)
}

// portOf extracts the port from a listen address, defaulting to 8787.
func portOf(listen string) string {
	_, p, err := net.SplitHostPort(listen)
	if err != nil || p == "" {
		return "8787"
	}
	return p
}

// listenAddr turns the configured listen address into the one handed to net.Listen.
// A wildcard IPv4 address ("0.0.0.0:8787", the default) becomes ":8787" so the socket
// is dual-stack: on a network where the PC only has a usable IPv6 address, an
// IPv4-only listener would be unreachable.
func listenAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("", port)
	}
	return listen
}
