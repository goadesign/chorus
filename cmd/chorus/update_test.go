package main

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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
// letting a running server install and restart itself on the approved
// version, installing locally and running setup for a server already on it, leaving a server without
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
		// releasedBin replaces the published binary, which otherwise reports latest.
		releasedBin []byte
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
			name:          "accepted prompt lets the server install and restart itself",
			latest:        next,
			serverVersion: current,
			input:         "yes\n",
			wantOutParts:  []string{fmt.Sprintf("is installing v%s and restarting\n", next), fmt.Sprintf("Chorus v%s is running\n", next)},
			wantPosts:     []string{next},
		},
		{
			name:          "server already on the release gets the local binary and setup",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: next,
			wantOutParts:  []string{fmt.Sprintf("Chorus v%s is running\n", next)},
			wantPosts:     []string{next},
			wantRuns:      "setup -global -addr ADDR\n",
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
		},
		{
			name:        "release reporting another version leaves the binary",
			args:        []string{"-yes"},
			latest:      next,
			releasedBin: fakeChorusBinary("0.0.7"),
			wantErr:     fmt.Sprintf(`the v%s release binary reports "0.0.7"`, next),
		},
		{
			name:        "release that cannot run leaves the binary",
			args:        []string{"-yes"},
			latest:      next,
			releasedBin: []byte("not a program"),
			wantErr:     fmt.Sprintf(`the v%s release binary reports ""`, next),
		},
		{
			name:    "checksum mismatch leaves the binary",
			args:    []string{"-yes"},
			latest:  next,
			badSum:  true,
			wantErr: "SHA-256 mismatch",
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
			if tc.releasedBin != nil {
				newBin = tc.releasedBin
			}
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
			entries, err := os.ReadDir(filepath.Dir(exe))
			require.NoError(t, err)
			assert.Len(t, entries, 1, "no staged release file may be left next to the binary")
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

// TestInstallReleaseNeverDowngrades verifies the check at the binary's
// directory: an install whose release is older than the binary on disk is
// refused and leaves the file alone, and an install waiting for another
// install's lock rereads the version once it gets the lock, so a stale
// approval cannot overwrite the newer binary the other install wrote.
func TestInstallReleaseNeverDowngrades(t *testing.T) {
	next := nextMinorVersion(t)
	releases := fakeReleases(t, next, fakeChorusBinary(next), false)

	t.Run("newer binary on disk", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "chorus")
		writeExecutable(t, exe, fakeChorusBinary("99.0.0"))
		_, err := installRelease(context.Background(), releases, release{version: next}, exe, io.Discard)
		require.ErrorIs(t, err, errWouldDowngrade)
		assert.Equal(t, fakeChorusBinary("99.0.0"), mustReadFile(t, exe))
	})

	t.Run("newer binary installed while waiting for the lock", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "chorus")
		writeExecutable(t, exe, fakeChorusBinary(genchorus.APIVersion))
		unlock, err := lockDir(context.Background(), filepath.Dir(exe))
		require.NoError(t, err)

		done := make(chan error, 1)
		go func() {
			_, err := installRelease(context.Background(), releases, release{version: next}, exe, io.Discard)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("install finished while another install held the lock: %v", err)
		case <-time.After(3 * installLockPoll):
		}
		writeExecutable(t, exe, fakeChorusBinary("99.0.0"))
		require.NoError(t, unlock())

		select {
		case err := <-done:
			require.ErrorIs(t, err, errWouldDowngrade)
		case <-time.After(10 * time.Second):
			t.Fatal("install did not resume after the lock was released")
		}
		assert.Equal(t, fakeChorusBinary("99.0.0"), mustReadFile(t, exe))
	})
}

// TestUpdateHandler verifies GET and POST /update behind the same
// middleware runServe uses: requests from another machine or another website
// are refused, GET reports the latest release, and POST installs exactly the
// requested version, runs the new binary's setup, and answers 202 before
// asking for the restart on the new binary. It also verifies
// the answers for the running version, a downgrade, a malformed body, a
// concurrent update, an unavailable release, and an install failure, none
// of which restart the server.
func TestUpdateHandler(t *testing.T) {
	current := genchorus.APIVersion
	next := nextMinorVersion(t)
	cfg, err := parseServerFlags([]string{"-addr", "127.0.0.1:9440"})
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
		// diskVersion is the version the executable on disk reports; empty
		// means this server's own version.
		diskVersion string
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
		{name: "POST of the running version with trailing data", method: http.MethodPost, body: fmt.Sprintf(`{"version":%q} trailing-garbage`, current), published: next, wantStatus: http.StatusBadRequest, wantError: "nothing after it"},
		{name: "POST of a newer version with trailing data", method: http.MethodPost, body: nextBody + `{"version":"99.0.0"}`, published: next, wantStatus: http.StatusBadRequest, wantError: "nothing after it"},
		{name: "POST with an unknown field", method: http.MethodPost, body: fmt.Sprintf(`{"version":%q,"force":true}`, next), published: next, wantStatus: http.StatusBadRequest, wantError: "unknown field"},
		{name: "concurrent POST is refused", method: http.MethodPost, body: nextBody, published: next, busy: true, wantStatus: http.StatusConflict, wantBody: `{"error":"an update is already in progress"}`},
		{name: "unpublished version is a bad gateway", method: http.MethodPost, body: nextBody, published: "0.0.1", wantStatus: http.StatusBadGateway, wantError: "404"},
		{name: "checksum mismatch is a bad gateway", method: http.MethodPost, body: nextBody, published: next, badSum: true, wantStatus: http.StatusBadGateway, wantError: "SHA-256 mismatch"},
		{name: "POST never downgrades a newer binary on disk", method: http.MethodPost, body: nextBody, published: next, diskVersion: "99.0.0", wantStatus: http.StatusConflict, wantError: "is already chorus v99.0.0, newer than v" + next},
		{name: "install failure is a server error", method: http.MethodPost, body: nextBody, published: next, readOnlyDir: true, wantStatus: http.StatusInternalServerError, wantError: "rerun with write access"},
		{name: "other methods are not allowed", method: http.MethodPut, published: next, wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runLog := filepath.Join(t.TempDir(), "runs.log")
			t.Setenv("CHORUS_FAKE_LOG", runLog)
			exe := filepath.Join(t.TempDir(), "chorus")
			diskVersion := cmp.Or(tc.diskVersion, current)
			writeExecutable(t, exe, fakeChorusBinary(diskVersion))
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
				addr:     cfg.addr,
				restart: func(next string) {
					restarts = append(restarts, next)
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
				assert.Equal(t, fakeChorusBinary(diskVersion), mustReadFile(t, exe), "a refused or failed POST leaves the binary on disk")
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

// TestServeRestartsInPlace verifies the restart behind POST /update with two
// real servers sharing one home directory and Redis: the update of server A
// installs the release over A's binary, shuts down only A exactly as on
// SIGTERM, and returns A's binary for serve to exec, while server B keeps
// answering on its own address with its own version. Nothing reads the
// shared ~/.chorus/chorus.pid to find the server to stop.
func TestServeRestartsInPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHORUS_FAKE_LOG", filepath.Join(t.TempDir(), "runs.log"))
	t.Setenv("REDIS_PASSWORD", "")

	redisAddr := unusedAddr(t)
	_, redisPort, err := net.SplitHostPort(redisAddr)
	require.NoError(t, err)
	port, err := strconv.Atoi(redisPort)
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() {
		stopTestRedis(t, rdb, port)
		assert.NoError(t, rdb.Close())
	})
	require.NoError(t, connectOrStartRedis(context.Background(), rdb, redisAddr, t.TempDir()))

	current := genchorus.APIVersion
	next := nextMinorVersion(t)
	releases := fakeReleases(t, next, fakeChorusBinary(next), false)
	type served struct {
		next string
		err  error
	}
	start := func(addr, exe string) chan served {
		done := make(chan served, 1)
		go func() {
			restartOn, err := runServe([]string{"-addr", addr, "-redis-addr", redisAddr}, releases, exe)
			done <- served{next: restartOn, err: err}
		}()
		require.Eventually(t, func() bool {
			ok, _ := probeServer(localBaseURL(addr))
			return ok
		}, 15*time.Second, 50*time.Millisecond)
		return done
	}
	update := func(addr string) int {
		resp, err := http.Post(localBaseURL(addr)+"/update", "application/json", strings.NewReader(fmt.Sprintf(`{"version":%q}`, next)))
		require.NoError(t, err)
		assert.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}

	exeA := filepath.Join(t.TempDir(), "chorus")
	writeExecutable(t, exeA, fakeChorusBinary(current))
	exeB := filepath.Join(t.TempDir(), "chorus")
	writeExecutable(t, exeB, fakeChorusBinary(current))
	addrA, addrB := unusedAddr(t), unusedAddr(t)
	doneA := start(addrA, exeA)
	doneB := start(addrB, exeB)

	assert.Equal(t, http.StatusAccepted, update(addrA))
	select {
	case got := <-doneA:
		require.NoError(t, got.err)
		assert.Equal(t, exeA, got.next, "runServe returns the installed binary for serve to exec")
	case <-time.After(15 * time.Second):
		t.Fatal("server A did not shut down for its restart")
	}
	assert.Equal(t, next, binaryVersion(context.Background(), exeA))
	upA, _ := probeServer(localBaseURL(addrA))
	assert.False(t, upA, "server A stopped listening before its restart")
	upB, versionB := probeServer(localBaseURL(addrB))
	assert.True(t, upB, "updating server A must not stop server B")
	assert.Equal(t, current, versionB)
	assert.Equal(t, fakeChorusBinary(current), mustReadFile(t, exeB))

	assert.Equal(t, http.StatusAccepted, update(addrB))
	select {
	case got := <-doneB:
		require.NoError(t, got.err)
		assert.Equal(t, exeB, got.next)
	case <-time.After(15 * time.Second):
		t.Fatal("server B did not shut down for its restart")
	}
}

// TestRestartProcessKeepsArgsAndEnvironment verifies the real process
// replacement behind serve's restart. A child copy of this test binary,
// started with server flags that include explicit empty values and with
// REDIS_PASSWORD in its environment, calls restartProcess on a script that
// records its process ID, arguments, and REDIS_PASSWORD. The script must run
// in the same process with exactly the child's arguments and environment.
func TestRestartProcessKeepsArgsAndEnvironment(t *testing.T) {
	if record := os.Getenv("CHORUS_TEST_RESTART_RECORD"); record != "" {
		pidFile := record + ".pid"
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
			t.Fatalf("write pid: %v", err)
		}
		err := restartProcess(os.Getenv("CHORUS_TEST_RESTART_EXE"))
		t.Fatalf("restartProcess returned instead of replacing the process: %v", err)
	}

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	exe := filepath.Join(dir, "chorus")
	writeExecutable(t, exe, []byte(fmt.Sprintf("#!/bin/sh\n{ echo \"$$\"; printf '%%s\\n' \"$@\"; echo \"REDIS_PASSWORD=$REDIS_PASSWORD\"; } > %q\n", record)))
	serverArgs := []string{"--", "serve", "-addr", "127.0.0.1:9550", "-redis-password=", "-origins="}
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestRestartProcessKeepsArgsAndEnvironment$"}, serverArgs...)...)
	cmd.Env = append(os.Environ(),
		"CHORUS_TEST_RESTART_RECORD="+record,
		"CHORUS_TEST_RESTART_EXE="+exe,
		"REDIS_PASSWORD=from-environment",
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	pid := strings.TrimSpace(string(mustReadFile(t, record+".pid")))
	want := append([]string{pid, "-test.run=^TestRestartProcessKeepsArgsAndEnvironment$"}, serverArgs...)
	want = append(want, "REDIS_PASSWORD=from-environment")
	assert.Equal(t, strings.Join(want, "\n")+"\n", string(mustReadFile(t, record)))
}

// TestServiceExecutable verifies that service files name Homebrew's stable
// bin/chorus link rather than a versioned Cellar path, given either path, and
// keep any other executable path as it is.
func TestServiceExecutable(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cellar := filepath.Join(prefix, "Cellar", "chorus", "0.9.0", "bin", "chorus")
	writeExecutable(t, cellar, fakeChorusBinary("0.9.0"))
	link := filepath.Join(prefix, "bin", "chorus")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(cellar, link))
	plain := filepath.Join(t.TempDir(), "chorus")
	writeExecutable(t, plain, fakeChorusBinary("0.9.0"))

	assert.Equal(t, link, serviceExecutable(cellar))
	assert.Equal(t, link, serviceExecutable(link))
	assert.Equal(t, plain, serviceExecutable(plain))
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
// POST /update answers 404 like v0.9.0. Otherwise it answers 200 for its own
// version, fails with failure when set, or switches its version to the
// requested one and answers 202, as a server that restarted on it would.
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
		if req.Version == version {
			writeJSON(w, http.StatusOK, updateResult{Status: "up_to_date", Version: version})
			return
		}
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

// mustReadFile returns the content of path.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
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
