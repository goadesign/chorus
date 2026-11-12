package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/chorus/chorus"
	genchorus "github.com/goadesign/chorus/gen/chorus"
)

// TestNewerVersion verifies that versions compare numerically per part and
// that anything other than MAJOR.MINOR.PATCH is rejected.
func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b    string
		want    bool
		wantErr bool
	}{
		{a: "0.10.0", b: "0.9.0", want: true},
		{a: "1.0.0", b: "0.99.99", want: true},
		{a: "0.9.1", b: "0.9.0", want: true},
		{a: "0.9.0", b: "0.9.0"},
		{a: "0.9.0", b: "0.10.0"},
		{a: "v0.9.0", b: "0.9.0", wantErr: true},
		{a: "0.9", b: "0.9.0", wantErr: true},
		{a: "0.9.0", b: "", wantErr: true},
		{a: "0.-1.0", b: "0.9.0", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			got, err := newerVersion(tc.a, tc.b)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRunUpdate verifies "chorus update": the -check reports, never offering
// to downgrade a server newer than the release, declining the prompt,
// installing over the executable and then asking the running server to
// update itself to the approved version, leaving a server without
// POST /update running after a global setup and failing with how to restart
// it, running "setup -global" when no
// server answers, reporting a server's update error, never touching project
// files in the working directory, and leaving the executable untouched when
// the checksum does not match.
func TestRunUpdate(t *testing.T) {
	current := genchorus.APIVersion
	next := nextMinorVersion(t)
	cases := []struct {
		name string
		args []string
		// latest is the version the release source publishes.
		latest string
		// serverVersion is the version of a server answering at the saved
		// address; empty means no server answers there.
		serverVersion string
		// legacyServer makes that server answer 404 to POST /update, like v0.9.0.
		legacyServer bool
		// serverFailure makes that server's POST /update fail with this error.
		serverFailure string
		// input is what the user types at the prompt.
		input string
		// projectSkill creates .agents/skills/chorus/SKILL.md in the working
		// directory, as in a repository that checks in the Chorus skill.
		projectSkill bool
		badSum       bool
		// wantOut is the exact output; wantOutParts are lines it must contain.
		wantOut      string
		wantOutParts []string
		wantErr      string
		// wantRuns lists the commands run on the new binary, one per line,
		// with ADDR standing for the saved server address.
		wantRuns string
		// wantPosts lists the versions sent to the server's POST /update.
		wantPosts []string
		installed bool
	}{
		{
			name:          "check reports an update",
			args:          []string{"-check"},
			latest:        next,
			serverVersion: current,
			wantOut:       fmt.Sprintf("Update available: chorus v%s -> v%s (https://example.test/v%s)\n", current, next, next),
		},
		{
			name:          "check reports up to date",
			args:          []string{"-check"},
			latest:        current,
			serverVersion: current,
			wantOut:       fmt.Sprintf("Chorus v%s is up to date\n", current),
		},
		{
			name:          "check reports a needed restart",
			args:          []string{"-check"},
			latest:        current,
			serverVersion: "0.0.1",
			wantOut:       fmt.Sprintf("Restart needed: chorus binary is v%s, running server is v0.0.1\n", current),
		},
		{
			name:          "server newer than the release is left alone",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: "99.0.0",
			wantOut:       fmt.Sprintf("Chorus v99.0.0 is running, newer than the latest release v%s\n", next),
		},
		{
			name:          "declined prompt changes nothing",
			latest:        next,
			serverVersion: current,
			input:         "n\n",
			wantOut:       fmt.Sprintf("Update available: chorus v%s -> v%s (https://example.test/v%s)\nUpdate Chorus to v%s? [y/N]: Update cancelled\n", current, next, next, next),
		},
		{
			name:          "accepted prompt installs and the server updates itself",
			latest:        next,
			serverVersion: current,
			input:         "yes\n",
			wantOutParts:  []string{fmt.Sprintf("Chorus v%s is running\n", next)},
			wantPosts:     []string{next},
			installed:     true,
		},
		{
			name:          "needed restart asks the server for this binary's version",
			args:          []string{"-y"},
			latest:        current,
			serverVersion: "0.0.1",
			wantOutParts:  []string{fmt.Sprintf("Chorus v%s is running\n", current)},
			wantPosts:     []string{current},
		},
		{
			name:          "server without POST /update keeps running",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: current,
			legacyServer:  true,
			wantErr:       fmt.Sprintf("installed chorus v%s and refreshed integrations, but the running server (v%s at http://localhost:", next, current),
			wantRuns:      "setup -global -addr ADDR\n",
			installed:     true,
		},
		{
			name:          "server update error is reported",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: current,
			serverFailure: "setup failed",
			wantErr:       "could not update: setup failed",
			wantPosts:     []string{next},
			installed:     true,
		},
		{
			name:      "no running server runs global setup",
			args:      []string{"-y"},
			latest:    next,
			wantRuns:  "setup -global -addr ADDR\n",
			installed: true,
		},
		{
			name:          "project files in the working directory are left alone",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: current,
			projectSkill:  true,
			wantPosts:     []string{next},
			installed:     true,
		},
		{
			name:          "checksum mismatch leaves the binary",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: current,
			badSum:        true,
			wantErr:       "SHA-256 mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			runLog := filepath.Join(t.TempDir(), "runs.log")
			t.Setenv("CHORUS_FAKE_LOG", runLog)
			cwd := t.TempDir()
			t.Chdir(cwd)
			if tc.projectSkill {
				writeExecutable(t, filepath.Join(cwd, ".agents", "skills", "chorus", "SKILL.md"), []byte("old skill"))
			}

			addr := unusedAddr(t)
			posts := func() []string { return nil }
			if tc.serverVersion != "" {
				addr, posts = fakeUpdatableChorus(t, tc.serverVersion, !tc.legacyServer, tc.serverFailure)
			}
			saved, err := parseServerFlags([]string{"-addr", addr})
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
			require.NoError(t, saveServeArgs(home, saved))

			oldBin := fakeChorusBinary(current)
			newBin := fakeChorusBinary(tc.latest)
			exe := filepath.Join(t.TempDir(), "bin", "chorus")
			writeExecutable(t, exe, oldBin)

			var out bytes.Buffer
			err = runUpdate(tc.args, strings.NewReader(tc.input), &out, fakeReleases(t, tc.latest, newBin, tc.badSum), exe)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tc.wantOut != "" {
				assert.Equal(t, tc.wantOut, out.String())
			}
			for _, part := range tc.wantOutParts {
				assert.Contains(t, out.String(), part)
			}
			assert.Equal(t, tc.wantPosts, posts())

			got, err := os.ReadFile(exe)
			require.NoError(t, err)
			if tc.installed {
				assert.Equal(t, newBin, got)
				assert.Contains(t, out.String(), fmt.Sprintf("Installed chorus v%s at %s\n", tc.latest, exe))
			} else {
				assert.Equal(t, oldBin, got)
			}
			if tc.projectSkill {
				skill, err := os.ReadFile(filepath.Join(cwd, ".agents", "skills", "chorus", "SKILL.md"))
				require.NoError(t, err)
				assert.Equal(t, "old skill", string(skill))
				assert.NoFileExists(t, filepath.Join(cwd, ".mcp.json"))
			}
			runs, err := os.ReadFile(runLog)
			if tc.wantRuns == "" {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, strings.ReplaceAll(tc.wantRuns, "ADDR", addr), string(runs))
		})
	}
}

// TestUpdatePromptInterrupt verifies that Ctrl-C at the "chorus update"
// prompt ends the command. It runs the prompt in a child copy of this test
// binary whose input never ends, sends it SIGINT once the prompt appears,
// and expects the child to exit.
func TestUpdatePromptInterrupt(t *testing.T) {
	if os.Getenv("CHORUS_TEST_UPDATE_PROMPT") == "1" {
		releases := fakeReleases(t, nextMinorVersion(t), fakeChorusBinary("0.0.1"), false)
		err := runUpdate([]string{"-addr", unusedAddr(t)}, os.Stdin, os.Stdout, releases, filepath.Join(t.TempDir(), "chorus"))
		t.Logf("runUpdate returned %v", err)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdatePromptInterrupt$")
	cmd.Env = append(os.Environ(), "CHORUS_TEST_UPDATE_PROMPT=1", "HOME="+t.TempDir())
	// The input pipe stays open and empty, like a terminal nobody types in;
	// cmd.Wait closes it.
	_, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	prompted := make(chan struct{})
	drained := make(chan struct{})
	go func(notify chan struct{}) {
		defer close(drained)
		var seen bytes.Buffer
		buf := make([]byte, 256)
		for {
			n, err := stdout.Read(buf)
			seen.Write(buf[:n])
			if notify != nil && bytes.Contains(seen.Bytes(), []byte("[y/N]: ")) {
				close(notify)
				notify = nil
			}
			if err != nil {
				return
			}
		}
	}(prompted)
	select {
	case <-prompted:
	case <-time.After(10 * time.Second):
		assert.NoError(t, cmd.Process.Kill())
		t.Fatal("the update prompt never appeared")
	}

	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		assert.NoError(t, cmd.Process.Kill())
		<-drained
		t.Fatal("the update prompt ignored SIGINT")
	}
	assert.Error(t, cmd.Wait(), "the interrupted child must not report success")
}

// TestInstallReleaseHomebrew verifies that a binary inside a Homebrew Cellar
// is upgraded with "brew upgrade goadesign/tap/chorus" instead of being
// replaced, that the returned binary is Homebrew's bin/chorus, and that an
// upgrade that does not reach the release is reported.
func TestInstallReleaseHomebrew(t *testing.T) {
	next := nextMinorVersion(t)
	cases := []struct {
		name string
		// brewInstalls is the version the fake brew links into bin/chorus.
		brewInstalls string
		wantErr      string
	}{
		{name: "upgrade reaches the release", brewInstalls: next},
		{name: "stale tap is reported", brewInstalls: "0.0.2", wantErr: "run 'brew update'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefix, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			oldExe := filepath.Join(prefix, "Cellar", "chorus", "0.0.1", "bin", "chorus")
			writeExecutable(t, oldExe, fakeChorusBinary("0.0.1"))
			link := filepath.Join(prefix, "bin", "chorus")
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(oldExe, link))

			newExe := filepath.Join(prefix, "Cellar", "chorus", tc.brewInstalls, "bin", "chorus")
			newSource := filepath.Join(t.TempDir(), "chorus")
			writeExecutable(t, newSource, fakeChorusBinary(tc.brewInstalls))
			brewLog := filepath.Join(t.TempDir(), "brew.log")
			brewDir := t.TempDir()
			writeExecutable(t, filepath.Join(brewDir, "brew"), []byte(fmt.Sprintf(
				"#!/bin/sh\necho \"$*\" >> %q\nmkdir -p %q && cp %q %q && ln -sf %q %q\n",
				brewLog, filepath.Dir(newExe), newSource, newExe, newExe, link,
			)))
			t.Setenv("PATH", brewDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			unused := releaseSource{client: http.DefaultClient, latestURL: "http://127.0.0.1:1/latest", downloadURL: "http://127.0.0.1:1/download"}
			var out bytes.Buffer
			got, err := installRelease(context.Background(), unused, release{version: next}, link, &out)
			calls, readErr := os.ReadFile(brewLog)
			require.NoError(t, readErr)
			assert.Equal(t, "upgrade goadesign/tap/chorus\n", string(calls))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, link, got)
			assert.Equal(t, next, binaryVersion(context.Background(), got))
		})
	}
}

// TestUpdateHandler verifies GET and POST /update behind the same
// middleware runServe uses: requests from another machine or another website
// are refused, GET reports the latest release, and POST installs exactly the
// requested version, runs the new binary's setup, and answers 202 before
// restarting with this server's flags and Redis password. It also verifies
// the answers for the running version, a downgrade, a malformed body, a
// concurrent update, an unavailable release, and an install failure, none
// of which restart the server.
func TestUpdateHandler(t *testing.T) {
	current := genchorus.APIVersion
	next := nextMinorVersion(t)
	cfg, err := parseServerFlags([]string{"-addr", "127.0.0.1:9440", "-swarm-ttl", "48h", "-redis-password", "secret"})
	require.NoError(t, err)
	nextBody := fmt.Sprintf(`{"version":%q}`, next)

	cases := []struct {
		name   string
		method string
		body   string
		// remoteAddr is the TCP peer; empty means 127.0.0.1:50000.
		remoteAddr string
		origin     string
		// published is the release the release source serves.
		published string
		badSum    bool
		// busy holds the update lock as if another POST were running.
		busy bool
		// readOnlyDir makes the executable's directory unwritable.
		readOnlyDir bool
		wantStatus  int
		wantBody    string
		// wantError is a substring of the JSON error message.
		wantError   string
		wantRestart bool
	}{
		{name: "another machine is refused", method: http.MethodPost, body: nextBody, remoteAddr: "192.0.2.7:50000", published: next, wantStatus: http.StatusForbidden, wantBody: `{"error":"updates are only accepted from this machine"}`},
		{name: "another website is refused", method: http.MethodPost, body: nextBody, origin: "https://evil.example", published: next, wantStatus: http.StatusForbidden},
		{name: "GET reports the latest release", method: http.MethodGet, origin: "http://localhost:9440", published: next, wantStatus: http.StatusOK, wantBody: fmt.Sprintf(`{"current_version":%q,"latest_version":%q,"update_available":true,"release_url":"https://example.test/v%s"}`, current, next, next)},
		{name: "GET never offers a downgrade", method: http.MethodGet, published: "0.0.1", wantStatus: http.StatusOK, wantBody: fmt.Sprintf(`{"current_version":%q,"latest_version":"0.0.1","update_available":false,"release_url":"https://example.test/v0.0.1"}`, current)},
		{name: "POST of the running version", method: http.MethodPost, body: fmt.Sprintf(`{"version":%q}`, current), published: next, wantStatus: http.StatusOK, wantBody: fmt.Sprintf(`{"status":"up_to_date","version":%q}`, current)},
		{name: "POST of an older version is a downgrade", method: http.MethodPost, body: `{"version":"0.0.1"}`, published: "0.0.1", wantStatus: http.StatusConflict, wantError: "would downgrade"},
		{name: "POST installs the requested version and restarts", method: http.MethodPost, body: nextBody, origin: "http://localhost:9440", published: next, wantStatus: http.StatusAccepted, wantBody: fmt.Sprintf(`{"status":"restarting","version":%q}`, next), wantRestart: true},
		{name: "POST without a body", method: http.MethodPost, published: next, wantStatus: http.StatusBadRequest, wantError: "send {\"version\""},
		{name: "POST with a malformed version", method: http.MethodPost, body: `{"version":"v0.10"}`, published: next, wantStatus: http.StatusBadRequest, wantError: "MAJOR.MINOR.PATCH"},
		{name: "POST with an unknown field", method: http.MethodPost, body: fmt.Sprintf(`{"version":%q,"force":true}`, next), published: next, wantStatus: http.StatusBadRequest, wantError: "unknown field"},
		{name: "concurrent POST is refused", method: http.MethodPost, body: nextBody, published: next, busy: true, wantStatus: http.StatusConflict, wantBody: `{"error":"an update is already in progress"}`},
		{name: "unpublished version is a bad gateway", method: http.MethodPost, body: nextBody, published: "0.0.1", wantStatus: http.StatusBadGateway, wantError: "404"},
		{name: "checksum mismatch is a bad gateway", method: http.MethodPost, body: nextBody, published: next, badSum: true, wantStatus: http.StatusBadGateway, wantError: "SHA-256 mismatch"},
		{name: "install failure is a server error", method: http.MethodPost, body: nextBody, published: next, readOnlyDir: true, wantStatus: http.StatusInternalServerError, wantError: "rerun with write access"},
		{name: "other methods are not allowed", method: http.MethodPut, published: next, wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLog := filepath.Join(t.TempDir(), "runs.log")
			t.Setenv("CHORUS_FAKE_LOG", runLog)
			exe := filepath.Join(t.TempDir(), "chorus")
			writeExecutable(t, exe, fakeChorusBinary(current))
			if tc.readOnlyDir {
				require.NoError(t, os.Chmod(filepath.Dir(exe), 0o555))
				t.Cleanup(func() {
					assert.NoError(t, os.Chmod(filepath.Dir(exe), 0o755))
				})
			}
			var restarts []string
			h := &updateHandler{
				releases: fakeReleases(t, tc.published, fakeChorusBinary(tc.published), tc.badSum),
				exe:      exe,
				cfg:      cfg,
				restart: func(next string, restartCfg serverConfig) error {
					assert.Equal(t, cfg, restartCfg, "the restart must keep this server's flags and password")
					restarts = append(restarts, next)
					return nil
				},
			}
			if tc.busy {
				h.busy.Lock()
			}
			handler := chorus.LocalOnly(loopbackOrigins(cfg.addr)...)(loopbackPeer(h))

			req := httptest.NewRequest(tc.method, "http://localhost:9440/update", strings.NewReader(tc.body))
			req.RemoteAddr = "127.0.0.1:50000"
			if tc.remoteAddr != "" {
				req.RemoteAddr = tc.remoteAddr
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantBody != "" {
				assert.JSONEq(t, tc.wantBody, rec.Body.String())
			}
			if tc.wantError != "" {
				var body updateError
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Contains(t, body.Error, tc.wantError)
			}
			if !tc.wantRestart {
				assert.Empty(t, restarts)
				if !tc.busy {
					assert.True(t, h.busy.TryLock(), "a failed or finished POST must release the update lock")
				}
				return
			}
			assert.Equal(t, []string{exe}, restarts)
			assert.Equal(t, next, binaryVersion(context.Background(), exe))
			runs, err := os.ReadFile(runLog)
			require.NoError(t, err)
			assert.Equal(t, "setup -global -addr 127.0.0.1:9440\n", string(runs))
			assert.False(t, h.busy.TryLock(), "the lock stays held while the server restarts")
		})
	}
}

// TestStartRestart verifies that the restart runs "<exe> up" with the
// server's flags in a separate process and hands it the Redis password
// through REDIS_PASSWORD, never as an argument.
func TestStartRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("REDIS_PASSWORD", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
	runLog := filepath.Join(t.TempDir(), "runs.log")
	exe := filepath.Join(t.TempDir(), "chorus")
	writeExecutable(t, exe, []byte(fmt.Sprintf("#!/bin/sh\necho \"$* password=$REDIS_PASSWORD\" > %q.tmp && mv %q.tmp %q\n", runLog, runLog, runLog)))
	cfg, err := parseServerFlags([]string{"-addr", "127.0.0.1:9550", "-redis-db", "7", "-redis-password", "secret"})
	require.NoError(t, err)

	require.NoError(t, startRestart(exe, cfg))

	var got []byte
	require.Eventually(t, func() bool {
		got, err = os.ReadFile(runLog)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	want := "up " + strings.Join(buildServeArgs(cfg)[1:], " ") + " password=secret\n"
	assert.Equal(t, want, string(got))
	assert.NotContains(t, strings.TrimSuffix(string(got), " password=secret\n"), "secret")
}

// TestServeArgsPersistence verifies that the flags "chorus up" saved are the
// starting point for the next run, that flags given on the command line
// override them, that a running server's complete flags (as a restart passes
// them) override every saved one including an empty -origins, that the Redis
// password is never saved, and that a damaged file is reported with a way to
// clear it.
func TestServeArgsPersistence(t *testing.T) {
	home := t.TempDir()
	loaded, err := loadServeArgs(home)
	require.NoError(t, err)
	assert.Nil(t, loaded)

	cfg, err := parseServerFlags([]string{"-addr", "127.0.0.1:9550", "-swarm-ttl", "48h", "-redis-password", "secret"})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
	require.NoError(t, saveServeArgs(home, cfg))
	raw, err := os.ReadFile(filepath.Join(home, ".chorus", serveArgsFile))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret")

	loaded, err = loadServeArgs(home)
	require.NoError(t, err)
	restarted, err := parseServerFlags(append(loaded, "-swarm-ttl", "1h"))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9550", restarted.addr)
	assert.Equal(t, "1h0m0s", restarted.swarmTTL.String())

	// A restart passes the running server's complete flags after the saved
	// ones, so a saved -origins does not come back when the server has none.
	withOrigins, err := parseServerFlags([]string{"-addr", "127.0.0.1:9550", "-origins", "https://old.example"})
	require.NoError(t, err)
	require.NoError(t, saveServeArgs(home, withOrigins))
	loaded, err = loadServeArgs(home)
	require.NoError(t, err)
	current, err := parseServerFlags([]string{"-addr", "127.0.0.1:9550"})
	require.NoError(t, err)
	restarted, err = parseServerFlags(append(loaded, buildServeArgs(current)[1:]...))
	require.NoError(t, err)
	assert.Equal(t, current, restarted)

	require.NoError(t, os.WriteFile(filepath.Join(home, ".chorus", serveArgsFile), []byte("{"), 0o644))
	_, err = loadServeArgs(home)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run 'chorus down' to clear them")
}

// fakeUpdatableChorus starts a server that answers GET /healthz like a
// running Chorus of version and returns its host:port with a function
// listing the versions sent to its POST /update. When updatable is false,
// POST /update answers 404 like v0.9.0. Otherwise it fails with failure when
// set, or switches its version to the requested one and answers 202, as a
// server that restarted on it would.
func fakeUpdatableChorus(t *testing.T, version string, updatable bool, failure string) (string, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		posts []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("X-Chorus-Version", version)
	})
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		if !updatable {
			http.NotFound(w, r)
			return
		}
		var req updateRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, req.Version)
		if failure != "" {
			writeJSON(w, http.StatusInternalServerError, updateError{Error: failure})
			return
		}
		version = req.Version
		writeJSON(w, http.StatusAccepted, updateResult{Status: "restarting", Version: req.Version})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(posts)
	}
}

// fakeChorusBinary returns a shell script standing in for a chorus binary of
// version: "version" prints "chorus v<version>", and any other invocation
// appends its arguments as one line to the file named by $CHORUS_FAKE_LOG, so
// tests can see which commands an update ran on the new binary.
func fakeChorusBinary(version string) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then echo \"chorus v%s\"; exit 0; fi\necho \"$*\" >> \"$CHORUS_FAKE_LOG\"\n", version))
}

// nextMinorVersion returns the release after this build's version, such as
// "0.10.0" for "0.9.0".
func nextMinorVersion(t *testing.T) string {
	t.Helper()
	v, err := parseVersion(genchorus.APIVersion)
	require.NoError(t, err)
	return fmt.Sprintf("%d.%d.0", v[0], v[1]+1)
}

// fakeReleases serves a latest release of version, published like a GitHub
// release: an archive for this OS and CPU holding bin as "chorus", and a
// checksums.txt listing it (with a wrong sum when badSum is set). It returns
// a releaseSource reading from that server.
func fakeReleases(t *testing.T, version string, bin []byte, badSum bool) releaseSource {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, content := range map[string][]byte{"chorus": bin, "README.md": []byte("readme")} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	name := fmt.Sprintf("chorus_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive.Bytes())
	if badSum {
		sum = sha256.Sum256([]byte("tampered"))
	}
	checksums := fmt.Sprintf("%s  chorus_%s_other_arch.tar.gz\n%s  %s\n", strings.Repeat("0", 64), version, hex.EncodeToString(sum[:]), name)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /latest", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
		fmt.Fprintf(w, `{"tag_name":"v%s","html_url":"https://example.test/v%s"}`, version, version)
	})
	mux.HandleFunc("GET /download/v"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(checksums))
		assert.NoError(t, err)
	})
	mux.HandleFunc("GET /download/v"+version+"/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write(archive.Bytes())
		assert.NoError(t, err)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return releaseSource{client: srv.Client(), latestURL: srv.URL + "/latest", downloadURL: srv.URL + "/download"}
}

// writeExecutable writes content as an executable file at path.
func writeExecutable(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o755))
}

// unusedAddr returns a loopback address nothing listens on.
func unusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}
