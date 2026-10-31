// This file implements background service lifecycle management ("chorus up",
// "chorus down", "chorus status"), automatic MCP client configuration
// ("chorus setup"), and zero-token background message watching ("chorus watch").
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

	genchorus "github.com/goadesign/chorus/gen/chorus"
	genmcpchorusclient "github.com/goadesign/chorus/gen/jsonrpc/mcp_chorus/client"
	genmcpchorus "github.com/goadesign/chorus/gen/mcp_chorus"
	"github.com/goadesign/chorus/skills"
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
	defaultWatchWait = 5 * time.Minute
	// maxReadWaitSec is the maximum wait_sec accepted by a single MCP read call.
	maxReadWaitSec = 600
	// watchReconnectBackoff is the delay between reconnect attempts when a
	// blocking "chorus watch" stream is interrupted by a daemon restart.
	watchReconnectBackoff = 200 * time.Millisecond
	// watchReconnectWindow bounds how long a blocking "chorus watch" waits for
	// the server to come back up after a transport disconnect before failing.
	watchReconnectWindow = 3 * time.Second
)

var (
	// errWatchToolError marks application-level MCP tool errors returned by
	// "read" so runWatch fails fast instead of retrying them as transport drops.
	errWatchToolError = errors.New("watch tool error")
)

// runUp starts Chorus in the background (installing a user service when
// supported or spawning a detached background daemon) and waits until
// GET /healthz responds with 200 OK for the current version.
func runUp(args []string) error {
	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}

	baseURL := localBaseURL(cfg.addr)
	if ok, srvVer := probeServer(baseURL); ok {
		if srvVer == genchorus.APIVersion {
			fmt.Printf("Chorus v%s is running at %s/mcp\n", srvVer, baseURL)
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

	if err := startBackgroundService(homeDir, exePath, cfg); err != nil {
		return err
	}

	deadline := time.Now().Add(daemonReadyTimeout)
	for time.Now().Before(deadline) {
		if ok, srvVer := probeServer(baseURL); ok && srvVer == genchorus.APIVersion {
			fmt.Printf("Chorus v%s is running at %s/mcp\n", genchorus.APIVersion, baseURL)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("chorus did not become healthy at %s/healthz within %s", baseURL, daemonReadyTimeout)
}

// runDown stops the background Chorus service or detached daemon.
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
	fmt.Println("Chorus stopped")
	return nil
}

// runStatus checks whether Chorus is reachable at GET /healthz and writes the
// running server's version to out.
func runStatus(args []string, out io.Writer) error {
	var addr string
	fs := flag.NewFlagSet("chorus status", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	baseURL := localBaseURL(addr)
	if ok, srvVer := probeServer(baseURL); ok {
		if srvVer != "" {
			fmt.Fprintf(out, "running v%s (%s/mcp)\n", srvVer, baseURL)
		} else {
			fmt.Fprintf(out, "running (%s/mcp)\n", baseURL)
		}
		return nil
	}
	return fmt.Errorf("chorus is not running at %s", baseURL)
}

// runSetup registers the Chorus MCP server URL in project and/or global MCP
// configuration files and installs the Chorus Agent Skill (SKILL.md).
func runSetup(args []string) error {
	var (
		addr   string
		global bool
	)
	fs := flag.NewFlagSet("chorus setup", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	fs.BoolVar(&global, "global", false, "Register Chorus in global coding-agent MCP configuration files")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mcpURL := localBaseURL(addr) + "/mcp"
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}

	updated, err := setupMCPConfigs(cwd, homeDir, mcpURL, global)
	if err != nil {
		return err
	}
	for _, p := range updated {
		if strings.HasSuffix(p, ".md") {
			fmt.Printf("Installed Chorus skill in %s\n", p)
		} else {
			fmt.Printf("Registered Chorus MCP (%s) in %s\n", mcpURL, p)
		}
	}
	return nil
}

// runWatch blocks until unread Chorus messages arrive for the specified agent
// (consuming zero LLM tokens while waiting in a background shell or hook),
// writes the unread wire lines to out, and exits 0. When no matching messages
// arrive before -wait elapses, runWatch writes nothing to out and exits 0.
func runWatch(args []string, out io.Writer) error {
	var (
		addr         string
		agent        string
		swarm        string
		from         string
		wait         time.Duration
		mentionsOnly bool
		limit        int
	)
	fs := flag.NewFlagSet("chorus watch", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	fs.StringVar(&agent, "agent", "", "Your agent handle (e.g. 'api' or '@api')")
	fs.StringVar(&swarm, "swarm", "default", "Swarm namespace")
	fs.StringVar(&from, "from", "", "Optional '#channel' filter")
	fs.DurationVar(&wait, "wait", defaultWatchWait, "Maximum duration to wait for unread messages (0s for instant hook check)")
	fs.BoolVar(&mentionsOnly, "mentions", false, "Only wake on direct messages, @agent/@all mentions, or replies to your messages")
	fs.IntVar(&limit, "limit", 20, "Maximum unread messages to drain per target")
	if err := fs.Parse(args); err != nil {
		return err
	}

	agent = strings.TrimPrefix(strings.TrimSpace(agent), "@")
	if agent == "" {
		return errors.New("chorus watch requires -agent <handle>")
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
	var reconnectDeadline time.Time
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

		text, err := callWatchRead(sigCtx, parsedURL, agent, swarm, from, chunkSec, limit, mentionsOnly)
		if err != nil {
			if sigCtx.Err() != nil {
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
		if text != "" && text != "none" {
			if wait > 0 {
				text += "\n[watch exited — re-arm in background: " + formatWatchRearmCommand(agent, swarm, from, wait, mentionsOnly) + "]"
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
func formatWatchRearmCommand(agent, swarm, from string, wait time.Duration, mentionsOnly bool) string {
	var b strings.Builder
	b.WriteString("chorus watch -agent ")
	b.WriteString(agent)
	if swarm != "" && swarm != "default" {
		b.WriteString(" -swarm ")
		b.WriteString(swarm)
	}
	if from != "" {
		b.WriteString(" -from ")
		b.WriteString(from)
	}
	if mentionsOnly {
		b.WriteString(" -mentions")
	}
	waitStr := wait.String()
	if wait >= time.Minute && wait%time.Minute == 0 {
		waitStr = strings.TrimSuffix(waitStr, "0s")
	}
	b.WriteString(" -wait ")
	b.WriteString(waitStr)
	return b.String()
}

// setupMCPConfigs merges the Chorus MCP server entry into the project's
// .mcp.json (and .codex/config.toml if .codex exists) and installs
// .agents/skills/chorus/SKILL.md when global is false, or updates detected
// global agent MCP config files and skill directories under homeDir (Universal
// .agents, Cursor, Antigravity, Claude Code, and OpenAI Codex) when global is
// true.
func setupMCPConfigs(cwd, homeDir, mcpURL string, global bool) ([]string, error) {
	var (
		jsonTargets  []string
		tomlTargets  []string
		skillTargets []string
	)
	if !global {
		jsonTargets = append(jsonTargets, filepath.Join(cwd, ".mcp.json"))
		if info, err := os.Stat(filepath.Join(cwd, ".codex")); err == nil && info.IsDir() {
			tomlTargets = append(tomlTargets, filepath.Join(cwd, ".codex", "config.toml"))
		}
		skillTargets = append(skillTargets, filepath.Join(cwd, ".agents", "skills", "chorus", "SKILL.md"))
	} else {
		skillTargets = append(skillTargets, filepath.Join(homeDir, ".agents", "skills", "chorus", "SKILL.md"))
		candidates := []struct {
			parentDir string
			config    string
			skill     string
		}{
			{
				parentDir: filepath.Join(homeDir, ".cursor"),
				config:    filepath.Join(homeDir, ".cursor", "mcp.json"),
				skill:     filepath.Join(homeDir, ".cursor", "skills", "chorus", "SKILL.md"),
			},
			{
				parentDir: filepath.Join(homeDir, ".gemini", "config"),
				config:    filepath.Join(homeDir, ".gemini", "config", "mcp_config.json"),
				skill:     filepath.Join(homeDir, ".gemini", "config", "skills", "chorus", "SKILL.md"),
			},
		}
		for _, c := range candidates {
			if info, err := os.Stat(c.parentDir); err == nil && info.IsDir() {
				jsonTargets = append(jsonTargets, c.config)
				skillTargets = append(skillTargets, c.skill)
			}
		}
		claudeConfig := filepath.Join(homeDir, ".claude.json")
		if _, err := os.Stat(claudeConfig); err == nil {
			jsonTargets = append(jsonTargets, claudeConfig)
		}
		claudeDir := filepath.Join(homeDir, ".claude")
		if info, err := os.Stat(claudeDir); err == nil && info.IsDir() {
			skillTargets = append(skillTargets, filepath.Join(claudeDir, "skills", "chorus", "SKILL.md"))
		}
		codexDir := filepath.Join(homeDir, ".codex")
		if info, err := os.Stat(codexDir); err == nil && info.IsDir() {
			tomlTargets = append(tomlTargets, filepath.Join(codexDir, "config.toml"))
			skillTargets = append(skillTargets, filepath.Join(codexDir, "skills", "chorus", "SKILL.md"))
		} else if realHome, homeErr := os.UserHomeDir(); homeErr == nil && realHome == homeDir {
			if _, lookErr := exec.LookPath("codex"); lookErr == nil {
				tomlTargets = append(tomlTargets, filepath.Join(codexDir, "config.toml"))
				skillTargets = append(skillTargets, filepath.Join(codexDir, "skills", "chorus", "SKILL.md"))
			}
		}
		if len(jsonTargets) == 0 && len(tomlTargets) == 0 {
			jsonTargets = append(jsonTargets, filepath.Join(homeDir, ".chorus", "mcp.json"))
		}
	}

	updated := make([]string, 0, len(jsonTargets)+len(tomlTargets)+len(skillTargets))
	for _, target := range jsonTargets {
		if err := mergeMCPConfigFile(target, mcpURL); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	for _, target := range tomlTargets {
		if err := mergeCodexConfigFile(target, mcpURL); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	for _, target := range skillTargets {
		if err := writeSkillFile(target); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	return updated, nil
}

// writeSkillFile writes the embedded Chorus SKILL.md to path, creating parent
// directories as needed.
func writeSkillFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create skill directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, skills.ChorusSkillMD, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// mergeMCPConfigFile idempotently inserts or updates "mcpServers.chorus" in a
// JSON configuration file while preserving all existing top-level keys and peer
// MCP server entries.
func mergeMCPConfigFile(path, mcpURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory for %s: %w", path, err)
	}

	root := make(map[string]any)
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if unmarshalErr := json.Unmarshal(raw, &root); unmarshalErr != nil {
				return fmt.Errorf("parse JSON in %s: %w", path, unmarshalErr)
			}
		}
	}

	servers, ok := root["mcpServers"].(map[string]any)
	if !ok || servers == nil {
		servers = make(map[string]any)
	}
	if filepath.Base(path) == "mcp_config.json" {
		servers["chorus"] = map[string]any{
			"url": mcpURL,
		}
	} else {
		servers["chorus"] = map[string]any{
			"type": "http",
			"url":  mcpURL,
		}
	}
	root["mcpServers"] = servers

	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON for %s: %w", path, err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// mergeCodexConfigFile idempotently inserts or updates the
// "[mcp_servers.chorus]" section in an OpenAI Codex TOML configuration file
// while preserving all existing tables, keys, and comments.
func mergeCodexConfigFile(path, mcpURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory for %s: %w", path, err)
	}

	perm := os.FileMode(0o600)
	var content string
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		content = string(raw)
	}

	urlLine := fmt.Sprintf("url = %q", mcpURL)
	if strings.TrimSpace(content) == "" {
		out := fmt.Sprintf("[mcp_servers.chorus]\n%s\n", urlLine)
		if err := os.WriteFile(path, []byte(out), perm); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}

	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var (
		out         []string
		inChorusSec bool
		foundSec    bool
		wroteURL    bool
	)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inChorusSec && !wroteURL {
				out = append(out, urlLine)
				wroteURL = true
			}
			inChorusSec = trimmed == "[mcp_servers.chorus]"
			if inChorusSec {
				foundSec = true
			}
			out = append(out, line)
			continue
		}
		if inChorusSec {
			if key, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(key) == "url" {
				if !wroteURL {
					out = append(out, urlLine)
					wroteURL = true
				}
				continue
			}
		}
		out = append(out, line)
	}
	if inChorusSec && !wroteURL {
		out = append(out, urlLine)
	}
	if !foundSec {
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "", "[mcp_servers.chorus]", urlLine, "")
	}

	rendered := strings.Join(out, "\n")
	if !strings.HasSuffix(rendered, "\n") {
		rendered += "\n"
	}
	if err := os.WriteFile(path, []byte(rendered), perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
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

// buildServeArgs constructs the CLI argument list for "chorus serve" from cfg,
// excluding redisPassword so credentials are never exposed in process tables or
// service unit files.
func buildServeArgs(cfg serverConfig) []string {
	args := []string{
		"serve",
		"-addr", cfg.addr,
		"-redis-addr", cfg.redisAddr,
		"-redis-db", strconv.Itoa(cfg.redisDB),
		"-swarm-ttl", cfg.swarmTTL.String(),
		"-stream-max-len", strconv.Itoa(cfg.streamMaxLen),
		"-online-window", cfg.onlineWindow.String(),
	}
	if cfg.origins != "" {
		args = append(args, "-origins", cfg.origins)
	}
	return args
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
	swarm string,
	from string,
	waitSec int,
	limit int,
	mentionsOnly bool,
) (string, error) {
	toolArgs := map[string]any{
		"agent":         agent,
		"swarm":         swarm,
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
