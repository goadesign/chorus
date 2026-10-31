// This file tests the Chorus CLI helpers, local Redis auto-start, MCP
// configuration file merging, and the background "chorus watch" listener.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goadesign/chorus/chorus"
	genchorus "github.com/goadesign/chorus/gen/chorus"
	"github.com/goadesign/chorus/skills"
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
// a detached, persistent local Redis instance backed by dataDir when targeting
// an unused loopback port, and preserves persisted state across restarts.
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
		stopTestRedis(t, rdb, port)
		_ = rdb.Close()
	}()

	dataDir := t.TempDir()
	if err := connectOrStartRedis(context.Background(), rdb, addr, dataDir); err != nil {
		t.Fatalf("connectOrStartRedis(%q) failed: %v", addr, err)
	}
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("expected auto-started Redis at %s to respond to PING: %v", addr, err)
	}

	// Verify durability: write a key, stop Redis, and restart against the same dataDir.
	if err := rdb.Set(context.Background(), "test-durability-key", "preserved-value", 0).Err(); err != nil {
		t.Fatalf("set test key: %v", err)
	}
	stopTestRedis(t, rdb, port)

	if err := pingRedis(context.Background(), rdb, initialPingTimeout); err == nil {
		t.Fatalf("expected Redis at %s to be stopped", addr)
	}

	// Re-connect/restart against same dataDir.
	if err := connectOrStartRedis(context.Background(), rdb, addr, dataDir); err != nil {
		t.Fatalf("restart connectOrStartRedis(%q) failed: %v", addr, err)
	}
	got, err := rdb.Get(context.Background(), "test-durability-key").Result()
	if err != nil {
		t.Fatalf("get preserved key after restart: %v", err)
	}
	if got != "preserved-value" {
		t.Fatalf("got key value %q, want %q", got, "preserved-value")
	}
}

// stopTestRedis shuts down the Redis server instance listening on port.
func stopTestRedis(t *testing.T, rdb *redis.Client, port int) {
	t.Helper()
	_ = rdb.ShutdownNoSave(context.Background()).Err()
	if dockerBin, err := exec.LookPath("docker"); err == nil {
		_ = exec.Command(dockerBin, "rm", "-f", fmt.Sprintf("%s-%d", dockerContainerName, port)).Run()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pingRedis(context.Background(), rdb, 100*time.Millisecond) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
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

	// 1. Project-local setup (.mcp.json and .agents/skills/chorus/SKILL.md).
	projPaths, err := setupMCPConfigs(cwd, home, "http://localhost:9440/mcp", false)
	if err != nil {
		t.Fatalf("setupMCPConfigs(project) failed: %v", err)
	}
	projSkill := filepath.Join(cwd, ".agents", "skills", "chorus", "SKILL.md")
	if len(projPaths) != 2 || projPaths[0] != filepath.Join(cwd, ".mcp.json") || projPaths[1] != projSkill {
		t.Fatalf("unexpected project paths: %v", projPaths)
	}
	if skillRaw, err := os.ReadFile(projSkill); err != nil || !bytes.Equal(skillRaw, skills.ChorusSkillMD) {
		t.Fatalf("expected project SKILL.md to match embedded ChorusSkillMD, err=%v", err)
	}

	// 2. Global setup (preserves existing keys and servers in ~/.cursor/mcp.json, ~/.gemini/config/mcp_config.json, and ~/.codex/config.toml, and installs SKILL.md into all detected skill directories).
	globalPaths, err := setupMCPConfigs(cwd, home, "http://localhost:9440/mcp", true)
	if err != nil {
		t.Fatalf("setupMCPConfigs(global) failed: %v", err)
	}
	wantGlobal := []string{
		cursorConfig,
		agyConfig,
		codexConfig,
		filepath.Join(home, ".agents", "skills", "chorus", "SKILL.md"),
		filepath.Join(home, ".cursor", "skills", "chorus", "SKILL.md"),
		filepath.Join(home, ".gemini", "config", "skills", "chorus", "SKILL.md"),
		filepath.Join(home, ".codex", "skills", "chorus", "SKILL.md"),
	}
	if len(globalPaths) != len(wantGlobal) {
		t.Fatalf("globalPaths = %v, want %v", globalPaths, wantGlobal)
	}
	for i, wantPath := range wantGlobal {
		if globalPaths[i] != wantPath {
			t.Fatalf("globalPaths[%d] = %q, want %q", i, globalPaths[i], wantPath)
		}
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

// TestRunWatch verifies that "chorus watch" exits silently (0 bytes on stdout)
// when no unread messages exist under -wait 0s, and blocks until a matching
// mention arrives when -wait > 0 and -mentions are passed.
func TestRunWatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	redisAddr := fmt.Sprintf("127.0.0.1:%d", port)
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() {
		stopTestRedis(t, rdb, port)
		_ = rdb.Close()
	}()
	if err := connectOrStartRedis(context.Background(), rdb, redisAddr, t.TempDir()); err != nil {
		t.Fatalf("connectOrStartRedis: %v", err)
	}

	svc, err := chorus.New(rdb)
	if err != nil {
		t.Fatalf("chorus.New: %v", err)
	}
	defer func() {
		_ = svc.Close()
	}()

	httpSrv := httptest.NewServer(chorus.NewHTTPHandler(svc))
	defer httpSrv.Close()
	parsedURL, err := url.Parse(httpSrv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	ctx := context.Background()
	for _, agent := range []string{"api", "db", "qa"} {
		if _, err := svc.Join(ctx, &genchorus.JoinPayload{
			Agent:    agent,
			Swarm:    "default",
			Channels: []string{"#auth"},
			Tail:     5,
		}); err != nil {
			t.Fatalf("Join(#auth) as %s: %v", agent, err)
		}
	}

	// 0. Unregistered handle in chorus watch must fail immediately naming registered handles.
	var typoOut bytes.Buffer
	typoErr := runWatch([]string{
		"-addr", parsedURL.Host,
		"-agent", "api-typo",
		"-wait", "5s",
	}, &typoOut)
	if typoErr == nil || !strings.Contains(typoErr.Error(), `unknown agent "api-typo"`) || !strings.Contains(typoErr.Error(), "@api") {
		t.Fatalf("expected unregistered agent error listing @api, got: %v", typoErr)
	}

	// 1. Instant hook check (-wait 0s) with no unread messages must write 0 bytes.
	var emptyOut bytes.Buffer
	if err := runWatch([]string{
		"-addr", parsedURL.Host,
		"-agent", "@api",
		"-wait", "0s",
	}, &emptyOut); err != nil {
		t.Fatalf("runWatch(-wait 0s) failed: %v", err)
	}
	if emptyOut.Len() != 0 {
		t.Fatalf("expected 0 bytes from caught-up -wait 0s watch, got %q", emptyOut.String())
	}

	// 2. Blocking watch with positional agent (default -mentions=true) ignores
	// unrelated messages and wakes immediately when @api is mentioned, printing
	// the full unread delta.
	var watchOut bytes.Buffer
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- runWatch([]string{
			"api",
			"-addr", parsedURL.Host,
			"-wait", "5s",
		}, &watchOut)
	}()

	time.Sleep(80 * time.Millisecond)

	// A second concurrent chorus watch for the same agent identity must fail fast.
	var dupOut bytes.Buffer
	dupErr := runWatch([]string{
		"api",
		"-addr", parsedURL.Host,
		"-wait", "5s",
	}, &dupOut)
	if dupErr == nil || !strings.Contains(dupErr.Error(), `agent "api" already has an active watch`) {
		t.Fatalf("expected duplicate active watch error for @api, got: %v", dupErr)
	}

	if _, err := svc.Send(ctx, &genchorus.SendPayload{
		Agent: "db",
		To:    "#auth",
		Text:  "starting passkey schema check",
	}); err != nil {
		t.Fatalf("Send(#auth#1): %v", err)
	}
	if _, err := svc.Send(ctx, &genchorus.SendPayload{
		Agent: "qa",
		To:    "#auth",
		Text:  "hey @api 500 on expired challenge",
	}); err != nil {
		t.Fatalf("Send(#auth#2): %v", err)
	}

	select {
	case watchErr := <-doneCh:
		if watchErr != nil {
			t.Fatalf("runWatch(api) failed: %v", watchErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for chorus watch to wake on @api mention")
	}

	got := watchOut.String()
	want := "#auth#1 @db: starting passkey schema check\n#auth#2 @qa: hey @api 500 on expired challenge\n! watch: chorus watch api -wait 5s\n"
	if got != want {
		t.Fatalf("runWatch output = %q, want %q", got, want)
	}

	// 3. When the server is unreachable and stays down, runWatch must fail
	// bounded by watchReconnectWindow (or -wait when shorter) rather than hanging.
	lnDead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate dead port: %v", err)
	}
	deadAddr := lnDead.Addr().String()
	_ = lnDead.Close()

	var deadOut bytes.Buffer
	deadStart := time.Now()
	if err := runWatch([]string{
		"-addr", deadAddr,
		"-agent", "api",
		"-wait", "450ms",
	}, &deadOut); err == nil {
		t.Fatal("expected runWatch against unreachable server to return an error")
	}
	if elapsed := time.Since(deadStart); elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("expected unreachable runWatch to retry briefly and fail within 450ms..2s, took %v", elapsed)
	}
}

// TestRedisHandover verifies the fixed handover behavior:
//  1. Server A auto-starts persistent Redis and serves an open long-poll read.
//  2. Server A begins graceful shutdown with BaseContext tied to shutdown context,
//     canceling the in-flight long-poll read immediately instead of hanging.
//  3. Server A exits without killing Redis.
//  4. Server B starts, connects to the existing Redis, and successfully serves tool calls.
//  5. When Redis is subsequently stopped, Server B's /healthz truthfully reports unhealthy (503).
func TestRedisHandover(t *testing.T) {
	lnRedis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate redis port: %v", err)
	}
	redisPort := lnRedis.Addr().(*net.TCPAddr).Port
	_ = lnRedis.Close()
	redisAddr := fmt.Sprintf("127.0.0.1:%d", redisPort)

	rdbA := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() {
		stopTestRedis(t, rdbA, redisPort)
		_ = rdbA.Close()
	}()

	dataDir := t.TempDir()

	// 1. Server A auto-starts Redis.
	if err := connectOrStartRedis(context.Background(), rdbA, redisAddr, dataDir); err != nil {
		t.Fatalf("Server A connectOrStartRedis: %v", err)
	}

	svcA, err := chorus.New(rdbA)
	if err != nil {
		t.Fatalf("Server A chorus.New: %v", err)
	}
	defer func() { _ = svcA.Close() }()

	sigCtxA, stopSignalsA := context.WithCancel(context.Background())
	defer stopSignalsA()

	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Server A listen: %v", err)
	}
	httpServerA := &http.Server{
		Handler: chorus.NewHTTPHandler(svcA),
		BaseContext: func(net.Listener) context.Context {
			return sigCtxA
		},
	}
	go func() { _ = httpServerA.Serve(lnA) }()
	baseA, _ := url.Parse("http://" + lnA.Addr().String())

	// Populate initial channel state on Server A.
	for _, agent := range []string{"lead", "watcher", "reviewer"} {
		if _, err := svcA.Join(context.Background(), &genchorus.JoinPayload{
			Agent:    agent,
			Swarm:    "default",
			Channels: []string{"#foundry"},
			Tail:     5,
		}); err != nil {
			t.Fatalf("Server A Join(#foundry) as %s: %v", agent, err)
		}
	}

	// 2. Server A serves an in-flight long-poll request AND a background runWatch
	// that must survive the daemon restart without crashing.
	serverAddr := lnA.Addr().String()
	longPollStarted := make(chan struct{})
	longPollDone := make(chan error, 1)
	go func() {
		close(longPollStarted)
		_, readErr := callWatchRead(context.Background(), baseA, "lead", "", 10, 10, false)
		longPollDone <- readErr
	}()

	var surviveWatchOut bytes.Buffer
	surviveWatchDone := make(chan error, 1)
	go func() {
		surviveWatchDone <- runWatch([]string{
			"watcher",
			"-addr", serverAddr,
			"-mentions=false",
			"-wait", "5s",
		}, &surviveWatchOut)
	}()
	<-longPollStarted
	time.Sleep(100 * time.Millisecond)

	// 3. Server A begins shutdown: canceling sigCtxA cancels in-flight requests immediately.
	shutStart := time.Now()
	stopSignalsA() // trigger BaseContext cancellation

	shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShut()
	if err := httpServerA.Shutdown(shutCtx); err != nil {
		t.Fatalf("httpServerA.Shutdown: %v", err)
	}
	shutDuration := time.Since(shutStart)
	if shutDuration > 2*time.Second {
		t.Fatalf("expected graceful shutdown to complete promptly via BaseContext cancellation, took %v", shutDuration)
	}

	// Verify raw single-request long-poll returned without hanging for 10s.
	select {
	case <-longPollDone:
	case <-time.After(time.Second):
		t.Fatal("long-poll request did not return after server shutdown")
	}

	// 4. Server B starts on the same address against the same Redis and dataDir.
	rdbB := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = rdbB.Close() }()
	if err := connectOrStartRedis(context.Background(), rdbB, redisAddr, dataDir); err != nil {
		t.Fatalf("Server B connectOrStartRedis: %v", err)
	}

	svcB, err := chorus.New(rdbB)
	if err != nil {
		t.Fatalf("Server B chorus.New: %v", err)
	}
	defer func() { _ = svcB.Close() }()

	lnB, err := net.Listen("tcp", serverAddr)
	if err != nil {
		t.Fatalf("Server B listen on %s: %v", serverAddr, err)
	}
	httpServerB := &http.Server{Handler: chorus.NewHTTPHandler(svcB)}
	go func() { _ = httpServerB.Serve(lnB) }()
	defer func() { _ = httpServerB.Close() }()
	baseB, _ := url.Parse("http://" + lnB.Addr().String())

	// Publish a message on Server B and verify the surviving runWatch reconnects and delivers it.
	if _, err := svcB.Send(context.Background(), &genchorus.SendPayload{
		Agent: "reviewer",
		To:    "#foundry",
		Text:  "LGTM on max_tokens fix",
	}); err != nil {
		t.Fatalf("Server B Send(#foundry): %v", err)
	}
	select {
	case watchErr := <-surviveWatchDone:
		if watchErr != nil {
			t.Fatalf("expected runWatch to survive Server A -> Server B restart, got error: %v", watchErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for surviving runWatch to wake on Server B message")
	}
	wantSurvive := "#foundry#1 @reviewer: LGTM on max_tokens fix\n! watch: chorus watch watcher -mentions=false -wait 5s\n"
	if got := surviveWatchOut.String(); got != wantSurvive {
		t.Fatalf("surviving runWatch output = %q, want %q", got, wantSurvive)
	}

	// 5. Server B tool calls succeed and access pre-existing swarm state.
	whoRes, err := svcB.Who(context.Background(), &genchorus.WhoPayload{
		Agent: "lead",
		Swarm: "default",
	})
	if err != nil {
		t.Fatalf("Server B Who tool call failed: %v", err)
	}
	if len(whoRes.Content) == 0 {
		t.Fatalf("Server B Who returned empty content")
	}
	whoBranch, ok := whoRes.Content[0].Content.AsText()
	if !ok || whoBranch == nil {
		t.Fatalf("expected text content branch in who response")
	}
	if !strings.Contains(whoBranch.Text, "#foundry") {
		t.Fatalf("expected Server B directory to contain #foundry, got: %q", whoBranch.Text)
	}
	if !strings.Contains(whoBranch.Text, "@lead") {
		t.Fatalf("expected Server B directory to contain @lead, got: %q", whoBranch.Text)
	}

	if _, err := callWatchRead(context.Background(), baseB, "lead", "", 0, 10, false); err != nil {
		t.Fatalf("Server B read tool call failed: %v", err)
	}

	// 6. /healthz on Server B reports healthy and returns X-Chorus-Version.
	if healthy, ver := probeServer("http://" + lnB.Addr().String()); !healthy || ver != genchorus.APIVersion {
		t.Fatalf("expected Server B to be healthy with version %q, got (%v, %q)", genchorus.APIVersion, healthy, ver)
	}

	// 7. When Redis is stopped, /healthz truthfully reports unhealthy.
	stopTestRedis(t, rdbB, redisPort)
	if healthy, _ := probeServer("http://" + lnB.Addr().String()); healthy {
		t.Fatal("expected Server B to report unhealthy after Redis stopped")
	}
}

// TestStatusAndVersion verifies that runStatus reports the live server version
// from GET /healthz when reachable (and fails when unreachable or Redis is down),
// that dispatch handles version flags and the version subcommand, and that
// invoking an MCP tool name on the CLI returns a self-correcting error.
func TestStatusAndVersion(t *testing.T) {
	lnRedis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate redis port: %v", err)
	}
	redisPort := lnRedis.Addr().(*net.TCPAddr).Port
	_ = lnRedis.Close()
	redisAddr := fmt.Sprintf("127.0.0.1:%d", redisPort)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() {
		stopTestRedis(t, rdb, redisPort)
		_ = rdb.Close()
	}()
	if err := connectOrStartRedis(context.Background(), rdb, redisAddr, t.TempDir()); err != nil {
		t.Fatalf("connectOrStartRedis: %v", err)
	}
	svc, err := chorus.New(rdb)
	if err != nil {
		t.Fatalf("chorus.New: %v", err)
	}
	defer func() {
		_ = svc.Close()
	}()

	srv := httptest.NewServer(chorus.NewHTTPHandler(svc))
	defer srv.Close()

	parsedURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	var statusOut bytes.Buffer
	if err := runStatus([]string{"-addr", parsedURL.Host}, &statusOut); err != nil {
		t.Fatalf("runStatus(running) failed: %v", err)
	}
	wantStatus := fmt.Sprintf("running v%s (http://localhost:%s/mcp)\n", genchorus.APIVersion, parsedURL.Port())
	if statusOut.String() != wantStatus {
		t.Fatalf("runStatus(running) = %q, want %q", statusOut.String(), wantStatus)
	}

	legacySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer legacySrv.Close()
	legacyURL, err := url.Parse(legacySrv.URL)
	if err != nil {
		t.Fatalf("parse legacy server URL: %v", err)
	}
	var legacyOut bytes.Buffer
	if err := runStatus([]string{"-addr", legacyURL.Host}, &legacyOut); err != nil {
		t.Fatalf("runStatus(legacy) failed: %v", err)
	}
	wantLegacy := fmt.Sprintf("running (http://localhost:%s/mcp)\n", legacyURL.Port())
	if legacyOut.String() != wantLegacy {
		t.Fatalf("runStatus(legacy) = %q, want %q", legacyOut.String(), wantLegacy)
	}

	if err := runStatus([]string{"-addr", "127.0.0.1:1"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected runStatus(stopped) to return an error")
	}

	wantVer := fmt.Sprintf("chorus v%s\n", genchorus.APIVersion)
	for _, arg := range []string{"version", "-version", "--version", "-v"} {
		var verOut bytes.Buffer
		if err := dispatch([]string{arg}, &verOut); err != nil {
			t.Fatalf("dispatch(%q) failed: %v", arg, err)
		}
		if verOut.String() != wantVer {
			t.Fatalf("dispatch(%q) = %q, want %q", arg, verOut.String(), wantVer)
		}
	}

	for _, toolName := range []string{"who", "join", "leave", "send", "read"} {
		err := dispatch([]string{toolName}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "is a Chorus MCP tool, not a CLI subcommand") {
			t.Fatalf("expected MCP guidance error from dispatch(%q), got %v", toolName, err)
		}
	}

	repoSkill, err := os.ReadFile(filepath.Join("..", "..", ".agents", "skills", "chorus", "SKILL.md"))
	if err != nil {
		t.Fatalf("read .agents/skills/chorus/SKILL.md: %v", err)
	}
	if !bytes.Equal(repoSkill, skills.ChorusSkillMD) {
		t.Fatal(".agents/skills/chorus/SKILL.md and skills/chorus/SKILL.md are out of sync")
	}
}
