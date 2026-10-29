// This file tests the Chorus CLI helpers, local Redis auto-start, and MCP
// configuration file merging.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestLoopbackPort verifies detection of loopback host:port addresses.
func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		addr string
		port string
		ok   bool
	}{
		{addr: "localhost:6379", port: "6379", ok: true},
		{addr: "127.0.0.1:6380", port: "6380", ok: true},
		{addr: "[::1]:6381", port: "6381", ok: true},
		{addr: ":6379", port: "6379", ok: true},
		{addr: "redis.internal:6379", port: "", ok: false},
		{addr: "10.0.0.5:6379", port: "", ok: false},
		{addr: "invalid", port: "", ok: false},
	}
	for _, tc := range cases {
		port, ok := loopbackPort(tc.addr)
		if ok != tc.ok || port != tc.port {
			t.Fatalf("loopbackPort(%q) = (%q, %v), want (%q, %v)", tc.addr, port, ok, tc.port, tc.ok)
		}
	}
}

// TestConnectOrStartRedis verifies that connectOrStartRedis automatically boots
// a local Redis instance when targeting an unused loopback port and cleans it
// up when the returned stop function is called.
func TestConnectOrStartRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer func() {
		_ = rdb.Close()
	}()

	stopFn, err := connectOrStartRedis(context.Background(), rdb, addr)
	if err != nil {
		t.Fatalf("connectOrStartRedis(%q) failed: %v", addr, err)
	}
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		stopFn()
		t.Fatalf("expected auto-started Redis at %s to respond to PING: %v", addr, err)
	}

	stopFn()
	if err := pingRedis(context.Background(), rdb, initialPingTimeout); err == nil {
		t.Fatalf("expected auto-started Redis at %s to stop after stopFn()", addr)
	}
}

// TestSetupMCPConfigs verifies that setupMCPConfigs creates and idempotently
// merges the Chorus MCP server entry into project and global MCP JSON files
// while preserving existing settings and peer MCP servers.
func TestSetupMCPConfigs(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()

	// Pre-populate ~/.cursor/mcp.json with an existing server entry and top-level setting.
	cursorDir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatalf("mkdir .cursor: %v", err)
	}
	existingJSON := `{
  "customKey": "keep-me",
  "mcpServers": {
    "other": {
      "type": "http",
      "url": "http://localhost:9000/mcp"
    }
  }
}`
	cursorConfig := filepath.Join(cursorDir, "mcp.json")
	if err := os.WriteFile(cursorConfig, []byte(existingJSON), 0o600); err != nil {
		t.Fatalf("write cursor mcp.json: %v", err)
	}

	// Pre-populate ~/.gemini/config/mcp_config.json with existing Antigravity servers.
	agyDir := filepath.Join(home, ".gemini", "config")
	if err := os.MkdirAll(agyDir, 0o755); err != nil {
		t.Fatalf("mkdir .gemini/config: %v", err)
	}
	existingAgyJSON := `{
  "mcpServers": {
    "slack": {
      "disabled": false,
      "serverUrl": "https://mcp.slack.com/mcp"
    }
  }
}`
	agyConfig := filepath.Join(agyDir, "mcp_config.json")
	if err := os.WriteFile(agyConfig, []byte(existingAgyJSON), 0o600); err != nil {
		t.Fatalf("write agy mcp_config.json: %v", err)
	}

	// Pre-populate ~/.codex/config.toml with existing model settings and another MCP server.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	existingTOML := "model = \"o3\"\n\n[mcp_servers.other]\nurl = \"http://localhost:9000/mcp\"\n"
	codexConfig := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(codexConfig, []byte(existingTOML), 0o600); err != nil {
		t.Fatalf("write codex config.toml: %v", err)
	}

	// 1. Project-local setup (.mcp.json).
	projPaths, err := setupMCPConfigs(cwd, home, "http://localhost:9440/mcp", false)
	if err != nil {
		t.Fatalf("setupMCPConfigs(project) failed: %v", err)
	}
	if len(projPaths) != 1 || projPaths[0] != filepath.Join(cwd, ".mcp.json") {
		t.Fatalf("unexpected project paths: %v", projPaths)
	}

	// 2. Global setup (preserves existing keys and servers in ~/.cursor/mcp.json, ~/.gemini/config/mcp_config.json, and ~/.codex/config.toml).
	globalPaths, err := setupMCPConfigs(cwd, home, "http://localhost:9440/mcp", true)
	if err != nil {
		t.Fatalf("setupMCPConfigs(global) failed: %v", err)
	}
	if len(globalPaths) != 3 || globalPaths[0] != cursorConfig || globalPaths[1] != agyConfig || globalPaths[2] != codexConfig {
		t.Fatalf("unexpected global paths: %v", globalPaths)
	}

	agyRaw, err := os.ReadFile(agyConfig)
	if err != nil {
		t.Fatalf("read updated agy mcp_config.json: %v", err)
	}
	var agyParsed map[string]any
	if err := json.Unmarshal(agyRaw, &agyParsed); err != nil {
		t.Fatalf("unmarshal updated agy mcp_config.json: %v", err)
	}
	agyServers := agyParsed["mcpServers"].(map[string]any)
	if _, ok := agyServers["slack"]; !ok {
		t.Fatalf("expected peer server 'slack' to be preserved in mcp_config.json, got %v", agyServers)
	}
	agyChorus := agyServers["chorus"].(map[string]any)
	if agyChorus["url"] != "http://localhost:9440/mcp" || len(agyChorus) != 1 {
		t.Fatalf("expected Antigravity chorus entry {\"url\": \"http://localhost:9440/mcp\"}, got %v", agyChorus)
	}

	raw, err := os.ReadFile(cursorConfig)
	if err != nil {
		t.Fatalf("read updated cursor mcp.json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal updated cursor mcp.json: %v", err)
	}
	if parsed["customKey"] != "keep-me" {
		t.Fatalf("expected customKey to be preserved, got %v", parsed["customKey"])
	}
	servers := parsed["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("expected peer server 'other' to be preserved, got %v", servers)
	}
	chorusEntry := servers["chorus"].(map[string]any)
	if chorusEntry["url"] != "http://localhost:9440/mcp" || chorusEntry["type"] != "http" {
		t.Fatalf("unexpected chorus MCP entry: %v", chorusEntry)
	}

	codexRaw, err := os.ReadFile(codexConfig)
	if err != nil {
		t.Fatalf("read updated codex config.toml: %v", err)
	}
	codexStr := string(codexRaw)
	if !strings.Contains(codexStr, "model = \"o3\"") || !strings.Contains(codexStr, "[mcp_servers.other]") {
		t.Fatalf("expected existing TOML settings to be preserved, got:\n%s", codexStr)
	}
	if !strings.Contains(codexStr, "[mcp_servers.chorus]\nurl = \"http://localhost:9440/mcp\"") {
		t.Fatalf("expected chorus section in codex config.toml, got:\n%s", codexStr)
	}

	// 3. Re-running with a new port updates the existing [mcp_servers.chorus] section in place without duplication.
	if _, err := setupMCPConfigs(cwd, home, "http://localhost:9550/mcp", true); err != nil {
		t.Fatalf("setupMCPConfigs(global update) failed: %v", err)
	}
	codexUpdatedRaw, err := os.ReadFile(codexConfig)
	if err != nil {
		t.Fatalf("read re-updated codex config.toml: %v", err)
	}
	codexUpdated := string(codexUpdatedRaw)
	if strings.Count(codexUpdated, "[mcp_servers.chorus]") != 1 {
		t.Fatalf("expected exactly one [mcp_servers.chorus] section, got:\n%s", codexUpdated)
	}
	if !strings.Contains(codexUpdated, "url = \"http://localhost:9550/mcp\"") {
		t.Fatalf("expected updated chorus URL in codex config.toml, got:\n%s", codexUpdated)
	}
}

// TestLocalBaseURL verifies normalization of listen addresses to local HTTP URLs.
func TestLocalBaseURL(t *testing.T) {
	if got := localBaseURL(":9440"); got != "http://localhost:9440" {
		t.Fatalf("localBaseURL(:9440) = %q", got)
	}
	if got := localBaseURL("127.0.0.1:9090"); got != "http://localhost:9090" {
		t.Fatalf("localBaseURL(127.0.0.1:9090) = %q", got)
	}
}

// TestBuildServeArgs verifies that buildServeArgs forwards all non-secret
// serverConfig fields and never leaks redisPassword onto the command line.
func TestBuildServeArgs(t *testing.T) {
	cfg, err := parseServerFlags([]string{
		"-addr", "127.0.0.1:9091",
		"-redis-addr", "127.0.0.1:6380",
		"-redis-password", "super-secret",
		"-redis-db", "2",
		"-origins", "https://example.com",
	})
	if err != nil {
		t.Fatalf("parseServerFlags failed: %v", err)
	}
	args := buildServeArgs(cfg)
	for _, a := range args {
		if a == "super-secret" || a == "-redis-password" {
			t.Fatalf("buildServeArgs leaked redisPassword in argv: %v", args)
		}
	}
	wantPairs := map[string]string{
		"-addr":       "127.0.0.1:9091",
		"-redis-addr": "127.0.0.1:6380",
		"-redis-db":   "2",
		"-origins":    "https://example.com",
	}
	for flagName, wantVal := range wantPairs {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flagName && args[i+1] == wantVal {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected %s %s in buildServeArgs output %v", flagName, wantVal, args)
		}
	}
}
