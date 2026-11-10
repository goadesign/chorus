package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/chorus/chorus"
	genchorus "github.com/goadesign/chorus/gen/chorus"
	"github.com/goadesign/chorus/skills"
)

// fakeChorus starts an HTTP server that answers GET /healthz like a running
// Chorus of the given version and returns its host:port. It lets "chorus up"
// and "chorus ui" tests find an already-running server without starting a
// launchd or systemd service.
func fakeChorus(t *testing.T, version string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Chorus-Version", version)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host
}

// TestRunUpSetsUpAgentsAndPrintsEndpoints verifies that "chorus up" registers
// Chorus with the coding agents found in HOME before confirming the server,
// prints the dashboard and MCP addresses, skips setup when CHORUS_NO_SETUP=1,
// and stops with an actionable error when a configuration cannot be updated.
// Every case finds an already-running server of the current version, so no OS
// service is started.
func TestRunUpSetsUpAgentsAndPrintsEndpoints(t *testing.T) {
	host := fakeChorus(t, genchorus.APIVersion)
	port := host[strings.LastIndex(host, ":")+1:]
	endpoints := fmt.Sprintf("Chorus v%s is running\n  Dashboard: http://localhost:%s/\n  MCP:       http://localhost:%s/mcp\n", genchorus.APIVersion, port, port)

	cases := []struct {
		name      string
		noSetup   bool
		claudeCfg string
		wantErr   string
		wantSetup bool
		// herdr detects a Herdr installation inside the temporary home.
		herdr bool
		// herdrBlocked makes the install destination a file to exercise recovery.
		herdrBlocked bool
	}{
		{name: "setup then endpoints", wantSetup: true},
		{name: "setup includes Herdr launcher", wantSetup: true, herdr: true},
		{name: "Herdr install failure stops up", herdr: true, herdrBlocked: true, wantErr: "install Chorus launcher in Herdr"},
		{name: "CHORUS_NO_SETUP skips setup", noSetup: true},
		{name: "unreadable config stops up", claudeCfg: "{not json", wantErr: "CHORUS_NO_SETUP=1"},
		{name: "null config stops up", claudeCfg: "null", wantErr: "CHORUS_NO_SETUP=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("HERDR_APP_HOME", "")
			t.Setenv("PATH", t.TempDir())
			if tc.noSetup {
				t.Setenv("CHORUS_NO_SETUP", "1")
			} else {
				t.Setenv("CHORUS_NO_SETUP", "")
			}
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
			if tc.herdr {
				require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "herdr-app"), 0o755))
				if tc.herdrBlocked {
					require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "herdr-app", "mods"), 0o755))
					require.NoError(t, os.WriteFile(filepath.Join(home, ".config", "herdr-app", "mods", "chorus-launcher"), []byte("blocked"), 0o600))
				}
			}
			if tc.claudeCfg != "" {
				require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(tc.claudeCfg), 0o644))
			}

			var out bytes.Buffer
			err := runUp([]string{"-addr", host}, &out)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.NotContains(t, out.String(), "Dashboard:")
				if tc.herdrBlocked {
					assert.Contains(t, err.Error(), filepath.Join(home, ".config", "herdr-app", "mods", "chorus-launcher"))
					assert.Contains(t, err.Error(), "rerun chorus up")
					assert.NotContains(t, err.Error(), "register Chorus with your coding agents:")
				}
				return
			}
			require.NoError(t, err)
			skill := filepath.Join(home, ".claude", "skills", "chorus", "SKILL.md")
			if tc.wantSetup {
				var launcherHint string
				if tc.herdr {
					launcherHint = fmt.Sprintf("Installed Chorus launcher in %s\nOpen it from a project terminal: herdr-app do mod.open --name chorus-launcher\n", filepath.Join(home, ".config", "herdr-app", "mods", "chorus-launcher"))
					if runtime.GOOS == "darwin" {
						launcherHint = "Herdr keyboard file is missing; open Herdr and rerun chorus setup -global to add the shortcut.\n" + launcherHint
					}
					assert.FileExists(t, filepath.Join(home, ".config", "herdr-app", "mods", "chorus-launcher", "main.js"))
					assert.NoFileExists(t, filepath.Join(home, ".config", "herdr-app", "keys.json"))
				}
				assert.Regexp(t, `^Registered Chorus with your coding agents \(\d+ files\)\n`+regexp.QuoteMeta(launcherHint+endpoints)+`$`, out.String())
				raw, readErr := os.ReadFile(skill)
				require.NoError(t, readErr)
				assert.Equal(t, skills.ChorusSkillMD, raw)
				return
			}
			assert.Equal(t, endpoints, out.String())
			assert.NoFileExists(t, skill)
		})
	}
}

// TestRunUI verifies that "chorus ui" opens the dashboard address of a running
// Chorus, tells the user to run "chorus up" without opening anything when no
// server answers, and still prints the address when the browser cannot open.
func TestRunUI(t *testing.T) {
	host := fakeChorus(t, genchorus.APIVersion)
	port := host[strings.LastIndex(host, ":")+1:]
	dashboard := fmt.Sprintf("http://localhost:%s/", port)

	var opened []string
	open := func(u string) error {
		opened = append(opened, u)
		return nil
	}
	var out bytes.Buffer
	require.NoError(t, runUI([]string{"-addr", host}, &out, open))
	assert.Equal(t, []string{dashboard}, opened)
	assert.Equal(t, "Dashboard: "+dashboard+"\n", out.String())

	opened = nil
	err := runUI([]string{"-addr", "127.0.0.1:1"}, &bytes.Buffer{}, open)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run 'chorus up'")
	assert.Empty(t, opened)

	out.Reset()
	err = runUI([]string{"-addr", host}, &out, func(string) error { return errors.New("no display") })
	require.Error(t, err)
	assert.Contains(t, err.Error(), dashboard)
	assert.Contains(t, out.String(), dashboard)

	err = runUI([]string{"-addr", host, "extra"}, &bytes.Buffer{}, open)
	require.Error(t, err)

	opened = nil
	err = runUI([]string{"-addr", "192.0.2.10:9440"}, &bytes.Buffer{}, open)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only accepts localhost")
	assert.Empty(t, opened)
}

// TestWriteEndpoints verifies that the dashboard address is printed only for
// hosts the dashboard accepts (localhost and loopback IPs, which is where a
// wildcard listener such as ":9440" is reached) and reported unavailable for
// any other host, while the MCP address is always printed.
func TestWriteEndpoints(t *testing.T) {
	cases := []struct {
		addr, wantDashboard string
	}{
		{addr: ":9550", wantDashboard: "http://localhost:9550/"},
		{addr: "0.0.0.0:9550", wantDashboard: "http://localhost:9550/"},
		{addr: "127.0.0.2:9550", wantDashboard: "http://127.0.0.2:9550/"},
		{addr: "192.0.2.10:9550", wantDashboard: "unavailable (the dashboard only accepts localhost; it is not reachable at 192.0.2.10:9550)"},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			var out bytes.Buffer
			baseURL := localBaseURL(tc.addr)
			writeEndpoints(&out, "1.2.3", baseURL)
			assert.Equal(t, "Chorus v1.2.3 is running\n  Dashboard: "+tc.wantDashboard+"\n  MCP:       "+baseURL+"/mcp\n", out.String())
		})
	}
}

// TestLoopbackListenOriginReachesDashboardAndMCP verifies that a dashboard
// page served from a loopback listen host other than 127.0.0.1 (such as
// 127.0.0.2) can call both the dashboard API and POST /mcp, the requests a
// browser makes to join and send, with the origins runServe builds, while an
// external origin is still rejected.
func TestLoopbackListenOriginReachesDashboardAndMCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	redisPort := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	redisAddr := fmt.Sprintf("127.0.0.1:%d", redisPort)
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() {
		stopTestRedis(t, rdb, redisPort)
		_ = rdb.Close()
	})
	require.NoError(t, connectOrStartRedis(context.Background(), rdb, redisAddr, t.TempDir()))
	svc, err := chorus.New(rdb)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	srv := httptest.NewUnstartedServer(nil)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	listenHost := net.JoinHostPort("127.0.0.2", port)
	srv.Config.Handler = chorus.NewHTTPHandler(svc, loopbackOrigins(listenHost)...)
	srv.Start()
	t.Cleanup(srv.Close)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{origin: "http://" + listenHost, want: http.StatusOK},
		{origin: "http://evil.example", want: http.StatusForbidden},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			view, err := http.NewRequest(http.MethodGet, srv.URL+"/swarms", nil)
			require.NoError(t, err)
			view.Host = listenHost
			view.Header.Set("Origin", tc.origin)
			resp, err := http.DefaultClient.Do(view)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, tc.want, resp.StatusCode, "GET /swarms")

			mcp, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(initialize))
			require.NoError(t, err)
			mcp.Host = listenHost
			mcp.Header.Set("Origin", tc.origin)
			mcp.Header.Set("Content-Type", "application/json")
			mcp.Header.Set("Accept", "application/json, text/event-stream")
			resp, err = http.DefaultClient.Do(mcp)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, tc.want, resp.StatusCode, "POST /mcp")
		})
	}
}
