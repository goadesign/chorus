// This file tests "chorus demo": seeding an isolated multi-agent swarm,
// preserving single-copy direct messages and channel sequences across repeated
// runs, honoring -no-open, and rejecting non-loopback or offline servers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/goadesign/chorus/chorus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunDemoSeedsSwarmAndOpensDashboard verifies that "chorus demo" populates
// #passkeys, #benchmarks, and a direct message inbox on a live Chorus HTTP
// server, resets cleanly on a second run without duplicating direct messages,
// honors -no-open, and rejects non-loopback, unreachable, or invalid targets.
func TestRunDemoSeedsSwarmAndOpensDashboard(t *testing.T) {
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
	svc, err := chorus.New(rdb)
	require.NoError(t, err)
	defer func() { _ = svc.Close() }()

	srv := httptest.NewServer(chorus.NewHTTPHandler(svc))
	defer srv.Close()
	listenAddr := srv.Listener.Addr().String()

	var opened []string
	openStub := func(u string) error {
		opened = append(opened, u)
		return nil
	}

	// First run opens the browser; second run uses -no-open and verifies neither
	// channel messages nor direct messages duplicate on re-runs.
	var out1 bytes.Buffer
	require.NoError(t, runDemo([]string{"-addr", listenAddr, "-swarm", "demo"}, &out1, openStub))
	assert.Contains(t, out1.String(), `Seeded swarm "demo"`)
	assert.Contains(t, out1.String(), "#swarm=demo&target=%23passkeys")
	require.Len(t, opened, 1)

	var out2 bytes.Buffer
	require.NoError(t, runDemo([]string{"-addr", listenAddr, "-swarm", "demo", "-no-open"}, &out2, openStub))
	assert.Contains(t, out2.String(), `Seeded swarm "demo"`)
	require.Len(t, opened, 1, "-no-open must not invoke open")

	resp, err := http.Get(srv.URL + "/swarms/demo/view")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var view struct {
		Swarm    string `json:"swarm"`
		Agents   []any  `json:"agents"`
		Channels []struct {
			Name      string `json:"name"`
			HeadSeq   int    `json:"head_seq"`
			Positions []struct {
				Handle    string `json:"handle"`
				Behind    int    `json:"behind"`
				Mentioned bool   `json:"mentioned"`
			} `json:"positions"`
			Messages []any `json:"messages"`
		} `json:"channels"`
		Inboxes []struct {
			Recipient string `json:"recipient"`
			HeadSeq   int    `json:"head_seq"`
			Messages  []any  `json:"messages"`
		} `json:"inboxes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&view))
	assert.Equal(t, "demo", view.Swarm)
	assert.Len(t, view.Agents, 4)
	require.Len(t, view.Inboxes, 1)
	assert.Equal(t, "claude-demo", view.Inboxes[0].Recipient)
	assert.Equal(t, 1, view.Inboxes[0].HeadSeq)
	assert.Len(t, view.Inboxes[0].Messages, 1, "repeated demo runs must not duplicate direct messages")

	require.Len(t, view.Channels, 2)
	for _, ch := range view.Channels {
		if ch.Name == "#passkeys" {
			assert.Equal(t, 7, ch.HeadSeq)
			assert.Len(t, ch.Messages, 7)
			foundGemini := false
			for _, pos := range ch.Positions {
				if pos.Handle == "gemini-demo" {
					foundGemini = true
					assert.Equal(t, 1, pos.Behind)
					assert.True(t, pos.Mentioned)
				}
			}
			assert.True(t, foundGemini, "expected gemini-demo watermark in #passkeys")
		}
	}

	// Validation, non-loopback, and offline-server error cases.
	var errOut bytes.Buffer
	err = runDemo([]string{"-swarm", "../bad"}, &errOut, openStub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid -swarm")

	err = runDemo([]string{"-addr", "10.0.0.1:9440", "-no-open"}, &errOut, openStub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only accepts localhost")

	err = runDemo([]string{"-addr", "127.0.0.1:1", "-no-open"}, &errOut, openStub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chorus is not running")
}
