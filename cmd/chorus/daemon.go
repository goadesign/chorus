// This file implements background service lifecycle management ("chorus up",
// "chorus down", "chorus status") and automatic MCP client configuration
// ("chorus setup").
package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
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
)

// runUp starts Chorus in the background (installing a user service when
// supported or spawning a detached background daemon) and waits until
// GET /healthz responds with 200 OK.
func runUp(args []string) error {
	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}

	baseURL := localBaseURL(cfg.addr)
	if isHealthy(baseURL) {
		fmt.Printf("Chorus is running at %s/mcp\n", baseURL)
		return nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
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
		if isHealthy(baseURL) {
			fmt.Printf("Chorus is running at %s/mcp\n", baseURL)
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

// runStatus checks whether Chorus is reachable at GET /healthz.
func runStatus(args []string) error {
	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}
	baseURL := localBaseURL(cfg.addr)
	if isHealthy(baseURL) {
		fmt.Printf("running (%s/mcp)\n", baseURL)
		return nil
	}
	return fmt.Errorf("chorus is not running at %s", baseURL)
}

// runSetup registers the Chorus MCP server URL in project and/or global MCP
// configuration files.
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
		fmt.Printf("Registered Chorus MCP (%s) in %s\n", mcpURL, p)
	}
	return nil
}

// setupMCPConfigs merges the Chorus MCP server entry into the project's
// .mcp.json (when global is false) or into detected global agent MCP config
// files under homeDir (when global is true).
func setupMCPConfigs(cwd, homeDir, mcpURL string, global bool) ([]string, error) {
	var targets []string
	if !global {
		targets = append(targets, filepath.Join(cwd, ".mcp.json"))
	} else {
		candidates := []struct {
			parentDir string
			config    string
		}{
			{
				parentDir: filepath.Join(homeDir, ".cursor"),
				config:    filepath.Join(homeDir, ".cursor", "mcp.json"),
			},
			{
				parentDir: filepath.Join(homeDir, ".gemini", "antigravity"),
				config:    filepath.Join(homeDir, ".gemini", "antigravity", "mcp_config.json"),
			},
		}
		for _, c := range candidates {
			if info, err := os.Stat(c.parentDir); err == nil && info.IsDir() {
				targets = append(targets, c.config)
			}
		}
		claudeConfig := filepath.Join(homeDir, ".claude.json")
		if _, err := os.Stat(claudeConfig); err == nil {
			targets = append(targets, claudeConfig)
		}
		if len(targets) == 0 {
			targets = append(targets, filepath.Join(homeDir, ".chorus", "mcp.json"))
		}
	}

	updated := make([]string, 0, len(targets))
	for _, target := range targets {
		if err := mergeMCPConfigFile(target, mcpURL); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	return updated, nil
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
	servers["chorus"] = map[string]any{
		"type": "http",
		"url":  mcpURL,
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
	return startDetachedProcess(stateDir, exePath, cfg)
}

// stopBackgroundService stops any active launchd unit, systemd user unit, or
// PID-tracked detached Chorus process.
func stopBackgroundService(homeDir string) {
	switch runtime.GOOS {
	case "darwin":
		plistPath := filepath.Join(homeDir, "Library", "LaunchAgents", launchdLabel+".plist")
		if _, err := os.Stat(plistPath); err == nil {
			_ = exec.Command("launchctl", "unload", plistPath).Run()
			_ = os.Remove(plistPath)
		}
	case "linux":
		unitPath := filepath.Join(homeDir, ".config", "systemd", "user", systemdUnitName)
		if _, err := os.Stat(unitPath); err == nil {
			_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).Run()
			_ = os.Remove(unitPath)
		}
	}

	pidFile := filepath.Join(homeDir, ".chorus", "chorus.pid")
	raw, err := os.ReadFile(pidFile)
	if err == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil && pid > 0 {
			if proc, findErr := os.FindProcess(pid); findErr == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
		}
		_ = os.Remove(pidFile)
	}
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
	_ = exec.Command("launchctl", "unload", plistPath).Run()
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
func startDetachedProcess(stateDir, exePath string, cfg serverConfig) error {
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

// isHealthy returns true if GET <baseURL>/healthz responds with HTTP 200 OK.
func isHealthy(baseURL string) bool {
	client := &http.Client{Timeout: healthProbeTimeout}
	resp, err := client.Get(baseURL + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
