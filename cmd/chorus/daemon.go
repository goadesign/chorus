// This file implements background service lifecycle management ("chorus up",
// "chorus down", "chorus status"), opening the dashboard ("chorus ui"), and
// zero-token background message watching ("chorus watch"). MCP client
// configuration ("chorus setup") lives in setup.go.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/chorus/chorus"
	genchorus "github.com/goadesign/chorus/gen/chorus"
	genmcpchorusclient "github.com/goadesign/chorus/gen/jsonrpc/mcp_chorus/client"
	genmcpchorus "github.com/goadesign/chorus/gen/mcp_chorus"
	goahttp "goa.design/goa/v3/http"
)

const (
	// launchdLabel is the macOS LaunchAgent identifier for the Chorus background service.
	launchdLabel = "design.goa.chorus"
	// systemdUnitName is the Linux systemd user service unit filename.
	systemdUnitName = "chorus.service"
	// daemonReadyTimeout bounds waiting for GET /healthz after starting the daemon.
	daemonReadyTimeout = 15 * time.Second
	// healthProbeTimeout bounds a single GET /healthz HTTP check.
	healthProbeTimeout = 500 * time.Millisecond
	// defaultWatchWait is the default blocking duration for "chorus watch".
	defaultWatchWait = 24 * time.Hour
	// maxReadWaitSec is the maximum wait_sec accepted by a single MCP read call.
	maxReadWaitSec = 600
	// watchReconnectBackoff is the delay between reconnect attempts when a
	// blocking "chorus watch" stream is interrupted by a daemon restart.
	watchReconnectBackoff = 200 * time.Millisecond
	// watchReconnectWindow bounds how long a blocking "chorus watch" waits for
	// the server to come back up after a transport disconnect before failing.
	// It covers a full restart by "chorus up" or an update: stopping the old
	// server and waiting for the new one to answer can each take up to
	// daemonReadyTimeout.
	watchReconnectWindow = 2 * daemonReadyTimeout
	// serveArgsFile names the file under ~/.chorus that records the server
	// flags "chorus up" last started Chorus with, as a JSON array of strings.
	serveArgsFile = "chorus.args"
)

var (
	// errWatchToolError marks application-level MCP tool errors returned by
	// "read" so runWatch fails fast instead of retrying them as transport drops.
	errWatchToolError = errors.New("watch tool error")
)

// runUp is the one command a new user runs after installing Chorus. It
// registers Chorus with the coding agents on this machine (the same global
// setup as "chorus setup -global", including the launcher when Herdr has a
// settings directory, skipped when CHORUS_NO_SETUP=1), starts
// Chorus in the background (a user service when supported, otherwise a
// detached daemon) unless the current version already answers, waits until
// GET /healthz reports that version, and writes the dashboard and MCP
// addresses to out. A setup error stops runUp before anything is started.
//
// The server flags start from the ones "chorus up" last started Chorus with
// (recorded in ~/.chorus/chorus.args), and flags given in args override them,
// so a bare "chorus up" restarts Chorus with the same address and settings.
// For example, after "chorus up -addr :9550 -swarm-ttl 48h", a later
// "chorus up -swarm-ttl 1h" keeps -addr :9550.
func runUp(args []string, out io.Writer) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	saved, err := loadServeArgs(homeDir)
	if err != nil {
		return err
	}
	cfg, err := parseServerFlags(append(saved, args...))
	if err != nil {
		return err
	}

	baseURL := localBaseURL(cfg.addr)
	if os.Getenv("CHORUS_NO_SETUP") != "1" {
		if err := setupForUp(homeDir, baseURL+"/mcp", out); err != nil {
			return err
		}
	}

	if ok, srvVer := probeServer(baseURL); ok {
		if srvVer == genchorus.APIVersion {
			writeEndpoints(out, srvVer, baseURL)
			return nil
		}
		stopBackgroundService(homeDir)
		stopped := false
		stopDeadline := time.Now().Add(daemonReadyTimeout)
		for time.Now().Before(stopDeadline) {
			if up, _ := probeServer(baseURL); !up {
				stopped = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !stopped {
			return fmt.Errorf("existing chorus server at %s could not be stopped; stop it before running 'chorus up'", baseURL)
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve chorus executable path: %w", err)
	}

	if err := startBackgroundService(homeDir, serviceExecutable(exePath), cfg); err != nil {
		return err
	}
	if err := saveServeArgs(homeDir, cfg); err != nil {
		return err
	}

	deadline := time.Now().Add(daemonReadyTimeout)
	for time.Now().Before(deadline) {
		if ok, srvVer := probeServer(baseURL); ok && srvVer == genchorus.APIVersion {
			writeEndpoints(out, genchorus.APIVersion, baseURL)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("chorus did not become healthy at %s/healthz within %s", baseURL, daemonReadyTimeout)
}

// runDown stops the background Chorus service or detached daemon and forgets
// the server flags it was started with, so the next "chorus up" starts from
// the defaults again.
func runDown(args []string) error {
	fs := flag.NewFlagSet("chorus down", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	stopBackgroundService(homeDir)
	if err := os.Remove(filepath.Join(homeDir, ".chorus", serveArgsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove saved server flags: %w", err)
	}
	fmt.Println("Chorus stopped")
	return nil
}

// runStatus checks whether Chorus is reachable at GET /healthz and writes the
// running server's version and its dashboard and MCP addresses to out. It
// returns an error naming the address when nothing answers there.
func runStatus(args []string, out io.Writer) error {
	var addr string
	fs := flag.NewFlagSet("chorus status", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	baseURL := localBaseURL(addr)
	if ok, srvVer := probeServer(baseURL); ok {
		writeEndpoints(out, srvVer, baseURL)
		return nil
	}
	return fmt.Errorf("chorus is not running at %s", baseURL)
}

// runUI opens the Chorus dashboard in the user's browser with open. It refuses
// an address whose host is not a loopback host, because the dashboard rejects
// requests addressed to any other host. It then checks that Chorus answers:
// when nothing answers, it opens nothing and returns an error telling the
// user to run "chorus up". When
// Chorus answers, it writes the dashboard address to out before opening it,
// so the user can still open the address by hand if the browser cannot be
// launched; that failure is returned with the address.
func runUI(args []string, out io.Writer, open func(string) error) error {
	var addr string
	fs := flag.NewFlagSet("chorus ui", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	baseURL := localBaseURL(addr)
	dashboard, err := dashboardURL(baseURL)
	if err != nil {
		return err
	}
	if ok, _ := probeServer(baseURL); !ok {
		return fmt.Errorf("chorus is not running at %s; run 'chorus up' first", baseURL)
	}
	fmt.Fprintf(out, "Dashboard: %s\n", dashboard)
	if err := open(dashboard); err != nil {
		return fmt.Errorf("open the dashboard in your browser at %s (%w)", dashboard, err)
	}
	return nil
}

// runWatch blocks until unread Chorus messages arrive for the specified agent
// (consuming zero LLM tokens while waiting in a background shell or hook),
// writes the unread wire lines to out, and exits 0. When no matching messages
// arrive before -wait elapses, runWatch writes nothing to out and exits 0.
func runWatch(args []string, out io.Writer) error {
	var (
		addr            string
		agent           string
		positionalAgent string
		from            string
		wait            time.Duration
		mentionsOnly    bool
		limit           int
	)
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positionalAgent = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("chorus watch", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	fs.StringVar(&agent, "agent", "", "Your agent handle (e.g. 'api' or '@api')")
	fs.StringVar(&from, "from", "", "Optional '#channel' filter")
	fs.DurationVar(&wait, "wait", defaultWatchWait, "Maximum duration to wait for unread messages (0s for instant hook check)")
	fs.BoolVar(&mentionsOnly, "mentions", true, "Only wake on direct messages, @agent/@all mentions, or replies to your messages")
	fs.IntVar(&limit, "limit", 20, "Maximum unread messages to drain per target")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if positionalAgent != "" {
		if agent != "" {
			return errors.New("chorus watch cannot specify both positional <agent> and -agent")
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		agent = positionalAgent
	} else if fs.NArg() > 0 {
		if agent != "" || fs.NArg() > 1 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		agent = fs.Arg(0)
	}

	agent = strings.TrimPrefix(strings.TrimSpace(agent), "@")
	if agent == "" {
		return errors.New("chorus watch requires <agent> (e.g. 'chorus watch api')")
	}
	if wait < 0 {
		return errors.New("-wait must not be negative")
	}
	if limit < 1 || limit > 50 {
		return errors.New("-limit must be between 1 and 50")
	}

	parsedURL, err := url.Parse(localBaseURL(addr))
	if err != nil {
		return fmt.Errorf("parse chorus address %q: %w", addr, err)
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	deadline := time.Now().Add(wait)
	var (
		reconnectDeadline time.Time
		watchedOnce       bool
	)
	for {
		chunkSec := 0
		if wait > 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil
			}
			chunkSec = max(1, int(remaining.Round(time.Second).Seconds()))
			if chunkSec > maxReadWaitSec {
				chunkSec = maxReadWaitSec
			}
		}

		text, err := callWatchRead(sigCtx, parsedURL, agent, from, chunkSec, limit, mentionsOnly)
		if err != nil {
			if sigCtx.Err() != nil {
				return nil
			}
			if errors.Is(err, errWatchToolError) && watchedOnce {
				return nil
			}
			if wait == 0 || errors.Is(err, errWatchToolError) {
				return err
			}
			now := time.Now()
			if reconnectDeadline.IsZero() {
				reconnectDeadline = now.Add(watchReconnectWindow)
			}
			retryLimit := reconnectDeadline
			if deadline.Before(retryLimit) {
				retryLimit = deadline
			}
			sleepDur := min(watchReconnectBackoff, time.Until(retryLimit))
			if sleepDur <= 0 {
				return err
			}
			select {
			case <-sigCtx.Done():
				return nil
			case <-time.After(sleepDur):
				if !time.Now().Before(retryLimit) {
					return err
				}
				continue
			}
		}
		reconnectDeadline = time.Time{}
		watchedOnce = true
		if text != "" && text != "none" {
			if wait > 0 {
				text += "\n! watch: " + formatWatchRearmCommand(agent, from, wait, mentionsOnly)
			}
			if _, err := fmt.Fprintln(out, text); err != nil {
				return fmt.Errorf("write watch output: %w", err)
			}
			return nil
		}
		if wait == 0 || !time.Now().Before(deadline) {
			return nil
		}
	}
}

// formatWatchRearmCommand renders the exact "chorus watch" command string an
// agent should run to re-arm its background watcher after a one-shot wakeup.
func formatWatchRearmCommand(agent, from string, wait time.Duration, mentionsOnly bool) string {
	var b strings.Builder
	b.WriteString("chorus watch ")
	b.WriteString(agent)
	if from != "" {
		b.WriteString(" -from ")
		b.WriteString(from)
	}
	if !mentionsOnly {
		b.WriteString(" -mentions=false")
	}
	if wait != defaultWatchWait {
		waitStr := wait.String()
		if wait >= time.Minute && wait%time.Minute == 0 {
			waitStr = strings.TrimSuffix(waitStr, "0s")
		}
		b.WriteString(" -wait ")
		b.WriteString(waitStr)
	}
	return b.String()
}

// startBackgroundService attempts to start Chorus via the OS user service
// manager (launchd on macOS, systemd --user on Linux) when no secret password
// needs to be persisted on disk, falling back to a detached background process.
func startBackgroundService(homeDir, exePath string, cfg serverConfig) error {
	stateDir := filepath.Join(homeDir, ".chorus")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", stateDir, err)
	}

	if cfg.redisPassword == "" {
		switch runtime.GOOS {
		case "darwin":
			if err := startLaunchdService(homeDir, stateDir, exePath, cfg); err == nil {
				return nil
			}
		case "linux":
			if err := startSystemdService(homeDir, exePath, cfg); err == nil {
				return nil
			}
		}
	}
	return startDetachedProcess(homeDir, stateDir, exePath, cfg)
}

// stopBackgroundService stops any active launchd unit, systemd user unit, or
// PID-tracked detached Chorus process, waiting for the process to terminate.
func stopBackgroundService(homeDir string) {
	var targetPid int
	pidFile := filepath.Join(homeDir, ".chorus", "chorus.pid")
	if raw, err := os.ReadFile(pidFile); err == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil && pid > 0 && pid != os.Getpid() {
			targetPid = pid
		}
	}

	switch runtime.GOOS {
	case "darwin":
		if targetPid == 0 {
			targetPid = parseLaunchdPID()
		}
		plistPath := filepath.Join(homeDir, "Library", "LaunchAgents", launchdLabel+".plist")
		if _, err := os.Stat(plistPath); err == nil {
			_ = exec.Command("launchctl", "unload", plistPath).Run()
			_ = os.Remove(plistPath)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := exec.Command("launchctl", "list", launchdLabel).Run(); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	case "linux":
		unitPath := filepath.Join(homeDir, ".config", "systemd", "user", systemdUnitName)
		if _, err := os.Stat(unitPath); err == nil {
			_ = exec.Command("systemctl", "--user", "stop", systemdUnitName).Run()
			_ = exec.Command("systemctl", "--user", "disable", systemdUnitName).Run()
			_ = os.Remove(unitPath)
		}
	}

	if targetPid > 0 {
		if proc, err := os.FindProcess(targetPid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
			waitForProcessExit(proc, 5*time.Second)
		}
		_ = os.Remove(pidFile)
	}
}

// waitForProcessExit waits for proc to exit within timeout, falling back to
// SIGKILL if the process fails to terminate gracefully.
func waitForProcessExit(proc *os.Process, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	killDeadline := time.Now().Add(time.Second)
	for time.Now().Before(killDeadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// parseLaunchdPID queries launchd for the PID of the active Chorus service.
func parseLaunchdPID() int {
	out, err := exec.Command("launchctl", "list", launchdLabel).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `"PID"`) {
			parts := strings.Split(trimmed, "=")
			if len(parts) == 2 {
				val := strings.Trim(strings.TrimSpace(parts[1]), ` ;"`)
				if pid, err := strconv.Atoi(val); err == nil && pid > 0 {
					return pid
				}
			}
		}
	}
	return 0
}

// saveServeArgs records the flags of the server that runUp just started in
// ~/.chorus/chorus.args, so later "chorus up" and "chorus update" runs restart
// Chorus with the same flags. The Redis password is never recorded.
func saveServeArgs(homeDir string, cfg serverConfig) error {
	raw, err := json.Marshal(buildServeArgs(cfg)[1:])
	if err != nil {
		return fmt.Errorf("encode server flags: %w", err)
	}
	path := filepath.Join(homeDir, ".chorus", serveArgsFile)
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("save server flags to %s: %w", path, err)
	}
	return nil
}

// loadServeArgs returns the server flags saveServeArgs recorded (for example
// ["-addr", "127.0.0.1:9440", "-redis-addr", "localhost:6379", ...]), or nil
// when Chorus was never started by "chorus up" or "chorus down" cleared them.
// A file that is not a JSON array of strings is reported with its path.
func loadServeArgs(homeDir string) ([]string, error) {
	path := filepath.Join(homeDir, ".chorus", serveArgsFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read saved server flags %s: %w", path, err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("read saved server flags %s: %w (run 'chorus down' to clear them)", path, err)
	}
	return args, nil
}

// serviceExecutable returns the path runUp records in the launchd plist,
// systemd unit, or detached command line for the binary at exePath. For a
// Homebrew install it is Homebrew's stable bin/chorus link (such as
// /home/linuxbrew/.linuxbrew/bin/chorus) instead of the versioned Cellar path
// os.Executable resolves to on Linux, which "brew upgrade" deletes. A server
// updated in place by POST /update keeps its service file, so the file must
// keep naming a binary that exists after the next crash or reboot.
func serviceExecutable(exePath string) string {
	realExe, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		return exePath
	}
	if prefix, ok := homebrewPrefix(realExe); ok {
		return filepath.Join(prefix, "bin", "chorus")
	}
	return exePath
}

// buildServeArgs constructs the CLI argument list for "chorus serve" from cfg,
// excluding redisPassword so credentials are never exposed in process tables or
// service unit files. Every other setting is listed, even when empty (such as
// "-origins=", one argument so a systemd unit never needs an empty quoted
// argument), so the list fully overrides flags saved by an earlier
// "chorus up": a restart through "up" with these arguments never brings back
// a saved -origins the server no longer uses.
func buildServeArgs(cfg serverConfig) []string {
	return []string{
		"serve",
		"-addr", cfg.addr,
		"-redis-addr", cfg.redisAddr,
		"-redis-db", strconv.Itoa(cfg.redisDB),
		"-swarm-ttl", cfg.swarmTTL.String(),
		"-stream-max-len", strconv.Itoa(cfg.streamMaxLen),
		"-online-window", cfg.onlineWindow.String(),
		"-origins=" + cfg.origins,
	}
}

// daemonPathEnv returns a PATH string that includes standard package-manager
// binary directories so auto-started redis-server or docker binaries are found.
func daemonPathEnv() string {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	}
	if !strings.Contains(pathEnv, "/opt/homebrew/bin") {
		pathEnv = pathEnv + ":/opt/homebrew/bin:/usr/local/bin"
	}
	return pathEnv
}

// xmlEscape returns the XML-escaped representation of s for plist generation.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// startLaunchdService writes and loads a macOS LaunchAgent plist for Chorus.
func startLaunchdService(homeDir, stateDir, exePath string, cfg serverConfig) error {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return err
	}
	stopBackgroundService(homeDir)
	agentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return err
	}
	plistPath := filepath.Join(agentsDir, launchdLabel+".plist")
	logPath := filepath.Join(stateDir, "chorus.log")

	var argItems strings.Builder
	argItems.WriteString("\t\t<string>" + xmlEscape(exePath) + "</string>\n")
	for _, arg := range buildServeArgs(cfg) {
		argItems.WriteString("\t\t<string>" + xmlEscape(arg) + "</string>\n")
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, xmlEscape(launchdLabel), argItems.String(), xmlEscape(daemonPathEnv()), xmlEscape(logPath), xmlEscape(logPath))

	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startSystemdService writes and enables a Linux systemd user unit for Chorus.
func startSystemdService(homeDir, exePath string, cfg serverConfig) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return err
	}
	stopBackgroundService(homeDir)
	unitDir := filepath.Join(homeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, systemdUnitName)

	quotedArgs := make([]string, 0, 12)
	quotedArgs = append(quotedArgs, strconv.Quote(exePath))
	for _, arg := range buildServeArgs(cfg) {
		quotedArgs = append(quotedArgs, strconv.Quote(arg))
	}

	unit := fmt.Sprintf(`[Unit]
Description=Chorus Multi-Agent Coordination MCP Server
After=network.target

[Service]
Environment="PATH=%s"
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, daemonPathEnv(), strings.Join(quotedArgs, " "))

	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return err
	}
	if err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnitName).Run(); err != nil {
		return err
	}
	return nil
}

// startDetachedProcess spawns "chorus serve" in a new process group in the
// background and records its PID in ~/.chorus/chorus.pid.
func startDetachedProcess(homeDir, stateDir, exePath string, cfg serverConfig) error {
	stopBackgroundService(homeDir)
	logPath := filepath.Join(stateDir, "chorus.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logPath, err)
	}
	defer func() {
		_ = logFile.Close()
	}()

	cmd := exec.Command(exePath, buildServeArgs(cfg)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := os.Environ()
	if cfg.redisPassword != "" {
		env = append(env, "REDIS_PASSWORD="+cfg.redisPassword)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background chorus process: %w", err)
	}
	if cmd.Process == nil {
		return errors.New("background chorus process has no PID")
	}

	pidFile := filepath.Join(stateDir, "chorus.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write pid file %s: %w", pidFile, err)
	}
	_ = cmd.Process.Release()
	return nil
}

// callWatchRead invokes the Chorus MCP "read" tool over HTTP JSON-RPC and
// returns its plain-text wire result.
func callWatchRead(
	ctx context.Context,
	baseURL *url.URL,
	agent string,
	from string,
	waitSec int,
	limit int,
	mentionsOnly bool,
) (string, error) {
	toolArgs := map[string]any{
		"agent":         agent,
		"wait_sec":      waitSec,
		"limit":         limit,
		"mentions_only": mentionsOnly,
	}
	if from != "" {
		toolArgs["from"] = from
	}
	rawArgs, err := json.Marshal(toolArgs)
	if err != nil {
		return "", fmt.Errorf("marshal read arguments: %w", err)
	}

	httpClient := &http.Client{
		Timeout: time.Duration(waitSec)*time.Second + 10*time.Second,
	}
	mcpClient := genmcpchorusclient.NewClient(
		baseURL.Scheme,
		baseURL.Host,
		httpClient,
		goahttp.RequestEncoder,
		goahttp.ResponseDecoder,
		false,
	)
	resAny, err := mcpClient.ToolsCall()(ctx, &genmcpchorus.ToolsCallPayload{
		Name:      "read",
		Arguments: rawArgs,
	})
	if err != nil {
		return "", fmt.Errorf("call chorus read at %s/mcp: %w", baseURL.String(), err)
	}
	res := resAny.(*genmcpchorus.ToolsCallResult)
	complete, ok := res.Outcome.AsComplete()
	if !ok {
		return "", errors.New("unexpected streaming outcome from chorus read")
	}
	if complete.IsError != nil && *complete.IsError {
		errText := "tool call failed"
		if len(complete.Content) > 0 && complete.Content[0].Text != nil {
			errText = *complete.Content[0].Text
		}
		if strings.Contains(errText, context.Canceled.Error()) {
			return "", errors.New(errText)
		}
		return "", fmt.Errorf("%w: %s", errWatchToolError, errText)
	}
	if len(complete.Content) == 0 || complete.Content[0].Text == nil {
		return "", nil
	}
	return *complete.Content[0].Text, nil
}

// localBaseURL normalizes a listen address (such as ":9440" or "127.0.0.1:9440")
// into an HTTP base URL ("http://localhost:9440").
func localBaseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "http://localhost:9440"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "127.0.0.1" || host == "::1" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

// probeServer queries GET <baseURL>/healthz and returns whether the server
// responded with HTTP 200 OK along with its reported X-Chorus-Version header.
func probeServer(baseURL string) (bool, string) {
	client := &http.Client{Timeout: healthProbeTimeout}
	resp, err := client.Get(baseURL + "/healthz")
	if err != nil {
		return false, ""
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, ""
	}
	return true, strings.TrimSpace(resp.Header.Get("X-Chorus-Version"))
}

// writeEndpoints writes the running Chorus version and the two addresses a
// user needs: the dashboard to open in a browser and the MCP endpoint that
// agents use. An unknown version (an older server without the version
// header) is omitted. The dashboard answers only requests addressed to a
// loopback host, so for any other host it says the dashboard is unavailable
// there instead of printing an address that would be rejected.
func writeEndpoints(out io.Writer, version, baseURL string) {
	if version != "" {
		fmt.Fprintf(out, "Chorus v%s is running\n", version)
	} else {
		fmt.Fprintln(out, "Chorus is running")
	}
	if dashboard, err := dashboardURL(baseURL); err == nil {
		fmt.Fprintf(out, "  Dashboard: %s\n", dashboard)
	} else {
		fmt.Fprintf(out, "  Dashboard: unavailable (%v)\n", err)
	}
	fmt.Fprintf(out, "  MCP:       %s/mcp\n", baseURL)
}

// dashboardURL returns the dashboard address for a Chorus base URL built by
// localBaseURL. It returns an error when the host is not a loopback host,
// because the dashboard rejects requests addressed to any other host.
func dashboardURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", baseURL, err)
	}
	if !chorus.IsLoopbackHost(u.Host) {
		return "", fmt.Errorf("the dashboard only accepts localhost; it is not reachable at %s", u.Host)
	}
	return baseURL + "/", nil
}

// openBrowser opens url in the default browser with the platform's own opener
// ("open" on macOS, "xdg-open" on Linux). Other platforms return an error so
// the caller can tell the user to open the printed address by hand.
func openBrowser(url string) error {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux":
		name = "xdg-open"
	default:
		return fmt.Errorf("no browser opener for %s", runtime.GOOS)
	}
	if out, err := exec.Command(name, url).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
