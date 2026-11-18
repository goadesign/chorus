// This file tests "chorus stdio": relaying newline-delimited MCP JSON-RPC over
// stdin/stdout to both a running Chorus HTTP server and an in-process embedded
// Chorus fallback, covering legacy initialize/ping/tools/list/tools/call, silent
// notifications, concurrent dispatch and notifications/cancelled watch release,
// MCP 2026-07-28 server/discover and Mcp-Name encoding, saved-address lookup,
// and transport error handling.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// stdioTestResponse decodes one single-line JSON-RPC frame from stdout.
	stdioTestResponse struct {
		// ID is the raw JSON-RPC request identifier.
		ID json.RawMessage `json:"id"`
		// Result holds the raw JSON-RPC success payload, if any.
		Result json.RawMessage `json:"result"`
		// Error holds the raw JSON-RPC error object, if any.
		Error json.RawMessage `json:"error"`
	}
)

// TestRunStdioRelaysHTTPAndEmbedded verifies that "chorus stdio" answers
// standard MCP stdio handshakes, silent notifications, concurrent dispatch and
// notifications/cancelled cancellation, MCP 2026-07-28 discovery and header
// encoding, saved-address resolution, and transport errors both when relaying
// to a running Chorus server and when starting in-process with no HTTP server.
func TestRunStdioRelaysHTTPAndEmbedded(t *testing.T) {
	hasRuntime := false
	for _, bin := range []string{"redis-server", "valkey-server", "docker", "podman"} {
		if _, err := exec.LookPath(bin); err == nil {
			hasRuntime = true
			break
		}
	}
	if !hasRuntime {
		t.Skip("no local Redis/Valkey/Docker/Podman runtime installed")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	redisAddr := fmt.Sprintf("127.0.0.1:%d", port)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() {
		stopTestRedis(t, rdb, port)
		_ = rdb.Close()
	}()
	require.NoError(t, connectOrStartRedis(context.Background(), rdb, redisAddr, t.TempDir()))

	// 1. Relay mode against a running Chorus HTTP server.
	t.Run("relay to running server", func(t *testing.T) {
		svc, err := chorus.New(rdb)
		require.NoError(t, err)
		defer func() { _ = svc.Close() }()
		srv := httptest.NewServer(chorus.NewHTTPHandler(svc))
		defer srv.Close()

		input := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"glama-test","version":"1.0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`,
			`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"who","arguments":{"swarm":"stdio-test","agent":"probe","model":"Claude Opus 5.5"}}}`,
			`{"jsonrpc":"2.0","id":5,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"stdio-2026","version":"1"}}}}`,
			`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":" who ","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":123,"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			`{not-json`,
			"",
		}, "\n")

		var out bytes.Buffer
		require.NoError(t, runStdio([]string{"-addr", srv.Listener.Addr().String()}, strings.NewReader(input), &out))

		responses := parseStdioResponses(t, out.String())
		// 9 requests (ids 1..8 + malformed JSON parse error); both notifications emit nothing.
		require.Len(t, responses, 9)

		assert.NotNil(t, responses["1"].Result, "initialize must succeed")
		assert.NotNil(t, responses["2"].Result, "ping must succeed")

		var listRes struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(responses["3"].Result, &listRes))
		names := make([]string, 0, len(listRes.Tools))
		for _, tool := range listRes.Tools {
			names = append(names, tool.Name)
		}
		assert.ElementsMatch(t, []string{"who", "join", "leave", "send", "read"}, names)
		assert.Contains(t, string(responses["4"].Result), "@probe")
		assert.NotNil(t, responses["5"].Result, "MCP 2026-07-28 server/discover must succeed")
		// Empty and whitespace-padded tool names must pass Mcp-Name header validation
		// and reach tool dispatch rather than failing with -32000 HeaderMismatch.
		assert.NotContains(t, string(responses["6"].Error), `-32000`)
		assert.NotNil(t, responses["6"].Error, "empty tool name must return tool error")
		assert.NotContains(t, string(responses["7"].Error), `-32000`)
		assert.NotNil(t, responses["7"].Error, "padded tool name must return tool error")
		// Non-string params.name must not block _meta decoding, so unsupported modern
		// protocol versions still route through ValidateHTTPRequest.
		assert.Contains(t, string(responses["8"].Error), `Unsupported protocol version`)
		assert.NotNil(t, responses["null"].Error, "malformed JSON must return JSON-RPC parse error")
	})

	// 2. Concurrent dispatch and notifications/cancelled aborting a blocking read.
	t.Run("concurrent dispatch and notifications/cancelled", func(t *testing.T) {
		svc, err := chorus.New(rdb)
		require.NoError(t, err)
		defer func() { _ = svc.Close() }()
		srv := httptest.NewServer(chorus.NewHTTPHandler(svc))
		defer srv.Close()

		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		errCh := make(chan error, 1)
		go func() {
			errCh <- runStdio([]string{"-addr", srv.Listener.Addr().String()}, inR, outW)
			_ = outW.Close()
		}()

		outLines := make(chan string, 16)
		go func() {
			scanner := bufio.NewReader(outR)
			for {
				line, readErr := scanner.ReadString('\n')
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					outLines <- trimmed
				}
				if readErr != nil {
					close(outLines)
					return
				}
			}
		}()

		writeFrame := func(frame string) {
			t.Helper()
			_, writeErr := io.WriteString(inW, frame+"\n")
			require.NoError(t, writeErr)
		}
		readFrame := func() stdioTestResponse {
			t.Helper()
			select {
			case line, ok := <-outLines:
				require.True(t, ok, "stdout closed before expected response")
				var resp stdioTestResponse
				require.NoError(t, json.Unmarshal([]byte(line), &resp))
				return resp
			case <-time.After(4 * time.Second):
				t.Fatal("timed out waiting for stdio response")
				return stdioTestResponse{}
			}
		}

		// Join watcher so read(wait_sec=30) acquires an active watch on #cancel-test.
		writeFrame(`{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"join","arguments":{"swarm":"stdio-cancel","agent":"watcher","channels":["#cancel-test"],"model":"Claude Opus 5.5"}}}`)
		joinResp := readFrame()
		assert.Equal(t, "19", string(joinResp.ID))
		assert.NotNil(t, joinResp.Result)
		assert.NotContains(t, string(joinResp.Result), `"isError":true`)

		// Start a 30s blocking read on id 20, then confirm it actually acquired the
		// server-side active watch by probing with wait_sec=1.
		writeFrame(`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"read","arguments":{"agent":"watcher","wait_sec":30}}}`)
		watchAcquired := false
		for probeID := 200; probeID < 220; probeID++ {
			writeFrame(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"read","arguments":{"agent":"watcher","wait_sec":1}}}`,
				probeID,
			))
			probeResp := readFrame()
			assert.Equal(t, fmt.Sprintf("%d", probeID), string(probeResp.ID))
			if strings.Contains(string(probeResp.Result), "already has an active watch") {
				watchAcquired = true
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		require.True(t, watchAcquired, "blocking read(wait_sec=30) must hold active watch on server")

		// Issue ping while id 20 is confirmed blocked to prove non-blocking concurrency.
		writeFrame(`{"jsonrpc":"2.0","id":21,"method":"ping"}`)
		pingResp := readFrame()
		assert.Equal(t, "21", string(pingResp.ID), "ping must return immediately while read blocks")
		assert.NotNil(t, pingResp.Result)

		// Cancel the blocking read via notifications/cancelled and verify zero error output.
		writeFrame(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":20,"reason":"user cancelled"}}`)

		// Probe with wait_sec=1 (which requires acquiring the server watch) until the
		// cancelled read releases its watch cleanly.
		var followUp stdioTestResponse
		watchReleased := false
		deadline := time.Now().Add(4 * time.Second)
		for attempt := 0; time.Now().Before(deadline); attempt++ {
			id := 30 + attempt
			writeFrame(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"read","arguments":{"agent":"watcher","wait_sec":1}}}`,
				id,
			))
			followUp = readFrame()
			assert.NotEqual(t, "20", string(followUp.ID), "cancelled request must not emit a response")
			if followUp.Result != nil && !strings.Contains(string(followUp.Result), `"isError":true`) {
				watchReleased = true
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		assert.True(t, watchReleased, "follow-up blocking read must acquire watch after cancellation")
		assert.NotContains(t, string(followUp.Result), "already has an active watch")

		require.NoError(t, inW.Close())
		select {
		case runErr := <-errCh:
			require.NoError(t, runErr)
		case <-time.After(3 * time.Second):
			t.Fatal("runStdio did not exit promptly after stdin EOF")
		}
		for extra := range outLines {
			t.Fatalf("unexpected trailing stdout frame: %s", extra)
		}
	})

	// 3. Embedded in-process fallback when no Chorus HTTP server is listening.
	t.Run("embedded fallback when server offline", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("REDIS_PASSWORD", "")
		input := strings.Join([]string{
			`{"jsonrpc":"2.0","id":10,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"glama-sandbox","version":"1.0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{}}`,
			`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"who","arguments":{"swarm":"stdio-embedded","agent":"solo","model":"Claude Opus 5.5"}}}`,
		}, "\n")

		var out bytes.Buffer
		require.NoError(t, runStdio([]string{"-addr", "127.0.0.1:1", "-redis-addr", redisAddr}, strings.NewReader(input), &out))

		responses := parseStdioResponses(t, out.String())
		require.Len(t, responses, 3)
		assert.NotNil(t, responses["10"].Result)
		assert.Contains(t, string(responses["11"].Result), `"name":"join"`)
		assert.Contains(t, string(responses["12"].Result), "@solo")
	})

	// 4. Bare runStdio resolves a valid saved -addr from ~/.chorus/chorus.args.
	t.Run("saved address resolution", func(t *testing.T) {
		svc, err := chorus.New(rdb)
		require.NoError(t, err)
		defer func() { _ = svc.Close() }()
		srv := httptest.NewServer(chorus.NewHTTPHandler(svc))
		defer srv.Close()

		home := t.TempDir()
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
		savedArgs, err := json.Marshal([]string{"-addr", srv.Listener.Addr().String()})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(home, ".chorus", serveArgsFile), savedArgs, 0o644))

		var out bytes.Buffer
		require.NoError(t, runStdio(nil, strings.NewReader(`{"jsonrpc":"2.0","id":40,"method":"ping"}`+"\n"), &out))
		responses := parseStdioResponses(t, out.String())
		require.Len(t, responses, 1)
		assert.NotNil(t, responses["40"].Result)
	})

	// 5. Relayed empty 200, empty/non-empty 202, non-JSON 502, and invalid-envelope
	// requests carrying an id must all yield JSON-RPC -32603 errors instead of
	// corrupting stdout or hanging the caller.
	t.Run("transport errors and invalid envelopes return JSON-RPC internal error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"ok"}`))
				return
			}
			body, _ := io.ReadAll(r.Body)
			switch {
			case strings.Contains(string(body), `"id":96`):
				w.WriteHeader(http.StatusOK)
			case strings.Contains(string(body), `"id":97`):
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte("accepted"))
			case strings.Contains(string(body), `"id":98`):
				w.WriteHeader(http.StatusAccepted)
			default:
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
			}
		}))
		defer srv.Close()

		var out bytes.Buffer
		input := strings.Join([]string{
			`{"jsonrpc":"2.0","id":96,"method":"tools/list","params":{}}`,
			`{"jsonrpc":"2.0","id":97,"method":"tools/list","params":{}}`,
			`{"jsonrpc":"2.0","id":98,"method":"tools/list","params":{}}`,
			`{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}`,
			`{"jsonrpc":"2.0","id":42,"method":"tools/list","params":"bad"}`,
		}, "\n")
		require.NoError(t, runStdio([]string{"-addr", srv.Listener.Addr().String()}, strings.NewReader(input), &out))

		responses := parseStdioResponses(t, out.String())
		require.Len(t, responses, 5)
		assert.Contains(t, string(responses["96"].Error), `-32603`)
		assert.Contains(t, string(responses["96"].Error), `unexpected empty HTTP 200`)
		assert.Contains(t, string(responses["97"].Error), `-32603`)
		assert.Contains(t, string(responses["97"].Error), `unexpected HTTP 202`)
		assert.Contains(t, string(responses["98"].Error), `-32603`)
		assert.Contains(t, string(responses["98"].Error), `unexpected HTTP 202`)
		assert.Contains(t, string(responses["99"].Error), `-32603`)
		assert.Contains(t, string(responses["99"].Error), `unexpected HTTP 502`)
		assert.Contains(t, string(responses["42"].Error), `-32603`)
		assert.Contains(t, string(responses["42"].Error), `unexpected HTTP 502`)
	})

	// 6. Dispatch routes "stdio" and rejects unexpected positional arguments.
	err = dispatch([]string{"stdio", "extra-arg"}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected argument")
}

// parseStdioResponses parses newline-delimited JSON-RPC responses indexed by ID,
// asserting that every line is valid JSON and no response ID is duplicated.
func parseStdioResponses(t *testing.T, raw string) map[string]stdioTestResponse {
	t.Helper()
	out := make(map[string]stdioTestResponse)
	lineCount := 0
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineCount++
		var resp stdioTestResponse
		require.NoError(t, json.Unmarshal([]byte(line), &resp), "response line must be valid JSON: %s", line)
		key := string(resp.ID)
		if key == "" {
			key = "null"
		}
		out[key] = resp
	}
	require.Len(t, out, lineCount, "duplicate response IDs in stdio output")
	return out
}
