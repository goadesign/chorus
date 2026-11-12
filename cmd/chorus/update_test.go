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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/chorus/chorus"
	genchorus "github.com/goadesign/chorus/gen/chorus"
)

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
		_, _ = w.Write([]byte(checksums))
	})
	mux.HandleFunc("GET /download/v"+version+"/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive.Bytes())
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

// TestRunUpdate verifies "chorus update": the three -check reports, declining
// the prompt, installing over the executable and then restarting a running
// server with "up" (which reads the saved flags itself) or running
// "setup -global" when no server answers, refreshing a project's skill, and
// leaving the executable untouched when the checksum does not match.
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
		// input is what the user types at the prompt.
		input string
		// projectSkill creates .agents/skills/chorus/SKILL.md in the working directory.
		projectSkill bool
		badSum       bool
		wantOut      string
		wantErr      string
		// wantRuns lists the commands run on the new binary, one per line,
		// with ADDR standing for the saved server address.
		wantRuns  string
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
			name:          "newer server is not restarted",
			args:          []string{"-check"},
			latest:        current,
			serverVersion: "99.0.0",
			wantOut:       fmt.Sprintf("Chorus v%s is up to date\n", current),
		},
		{
			name:          "declined prompt changes nothing",
			latest:        next,
			serverVersion: current,
			input:         "n\n",
			wantOut:       fmt.Sprintf("Update available: chorus v%s -> v%s (https://example.test/v%s)\nUpdate Chorus to v%s? [y/N]: Update cancelled\n", current, next, next, next),
		},
		{
			name:          "accepted prompt installs and restarts the running server",
			latest:        next,
			serverVersion: current,
			input:         "yes\n",
			wantRuns:      "up\n",
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
			name:          "project skill is refreshed before the restart",
			args:          []string{"-yes"},
			latest:        next,
			serverVersion: current,
			projectSkill:  true,
			wantRuns:      "setup -addr ADDR\nup\n",
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
			if tc.serverVersion != "" {
				addr = fakeChorus(t, tc.serverVersion)
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

			got, err := os.ReadFile(exe)
			require.NoError(t, err)
			if tc.installed {
				assert.Equal(t, newBin, got)
				assert.Contains(t, out.String(), fmt.Sprintf("Installed chorus v%s at %s\n", tc.latest, exe))
			} else {
				assert.Equal(t, oldBin, got)
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
// are refused, GET reports the latest release, POST installs it, runs the new
// binary's setup, answers 202 before restarting with this server's flags, and
// reports up-to-date servers, concurrent updates, unavailable releases, and
// install failures without restarting.
func TestUpdateHandler(t *testing.T) {
	current := genchorus.APIVersion
	next := nextMinorVersion(t)
	cfg, err := parseServerFlags([]string{"-addr", "127.0.0.1:9440", "-swarm-ttl", "48h"})
	require.NoError(t, err)

	cases := []struct {
		name   string
		method string
		// remoteAddr is the TCP peer; empty means 127.0.0.1:50000.
		remoteAddr string
		origin     string
		latest     string
		badSum     bool
		// busy holds the update lock as if another POST were running.
		busy bool
		// readOnlyDir makes the executable's directory unwritable.
		readOnlyDir bool
		wantStatus  int
		wantBody    string
		wantRestart bool
	}{
		{name: "another machine is refused", method: http.MethodPost, remoteAddr: "192.0.2.7:50000", latest: next, wantStatus: http.StatusForbidden, wantBody: `{"error":"updates are only accepted from this machine"}`},
		{name: "another website is refused", method: http.MethodPost, origin: "https://evil.example", latest: next, wantStatus: http.StatusForbidden},
		{name: "GET reports the latest release", method: http.MethodGet, origin: "http://localhost:9440", latest: next, wantStatus: http.StatusOK, wantBody: fmt.Sprintf(`{"current_version":%q,"latest_version":%q,"update_available":true,"release_url":"https://example.test/v%s"}`, current, next, next)},
		{name: "GET never offers a downgrade", method: http.MethodGet, latest: "0.0.1", wantStatus: http.StatusOK, wantBody: fmt.Sprintf(`{"current_version":%q,"latest_version":"0.0.1","update_available":false,"release_url":"https://example.test/v0.0.1"}`, current)},
		{name: "POST on the latest release", method: http.MethodPost, latest: current, wantStatus: http.StatusOK, wantBody: fmt.Sprintf(`{"status":"up_to_date","version":%q}`, current)},
		{name: "POST installs and restarts", method: http.MethodPost, origin: "http://localhost:9440", latest: next, wantStatus: http.StatusAccepted, wantBody: fmt.Sprintf(`{"status":"restarting","version":%q}`, next), wantRestart: true},
		{name: "concurrent POST is refused", method: http.MethodPost, latest: next, busy: true, wantStatus: http.StatusConflict, wantBody: `{"error":"an update is already in progress"}`},
		{name: "checksum mismatch is a bad gateway", method: http.MethodPost, latest: next, badSum: true, wantStatus: http.StatusBadGateway},
		{name: "install failure is a server error", method: http.MethodPost, latest: next, readOnlyDir: true, wantStatus: http.StatusInternalServerError},
		{name: "other methods are not allowed", method: http.MethodPut, latest: next, wantStatus: http.StatusMethodNotAllowed},
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
					_ = os.Chmod(filepath.Dir(exe), 0o755)
				})
			}
			var restarts []string
			h := &updateHandler{
				releases: fakeReleases(t, tc.latest, fakeChorusBinary(tc.latest), tc.badSum),
				exe:      exe,
				cfg:      cfg,
				restart: func(next string, args []string) error {
					restarts = append(restarts, next+" up "+strings.Join(args, " "))
					return nil
				},
			}
			if tc.busy {
				h.busy.Lock()
			}
			handler := chorus.LocalOnly(loopbackOrigins(cfg.addr)...)(loopbackPeer(h))

			req := httptest.NewRequest(tc.method, "http://localhost:9440/update", nil)
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
			if rec.Code >= http.StatusBadRequest && tc.wantBody == "" && rec.Code != http.StatusForbidden {
				var body updateError
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.NotEmpty(t, body.Error)
			}
			if !tc.wantRestart {
				assert.Empty(t, restarts)
				if !tc.busy {
					assert.True(t, h.busy.TryLock(), "a failed or finished POST must release the update lock")
				}
				return
			}
			assert.Equal(t, []string{exe + " up " + strings.Join(buildServeArgs(cfg)[1:], " ")}, restarts)
			assert.Equal(t, next, binaryVersion(context.Background(), exe))
			runs, err := os.ReadFile(runLog)
			require.NoError(t, err)
			assert.Equal(t, "setup -global -addr 127.0.0.1:9440\n", string(runs))
			assert.False(t, h.busy.TryLock(), "the lock stays held while the server restarts")
		})
	}
}

// TestServeArgsPersistence verifies that the flags "chorus up" saved are the
// starting point for the next run, that flags given on the command line
// override them, that the Redis password is never saved, and that a damaged
// file is reported with a way to clear it.
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

	require.NoError(t, os.WriteFile(filepath.Join(home, ".chorus", serveArgsFile), []byte("{"), 0o644))
	_, err = loadServeArgs(home)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run 'chorus down' to clear them")
}
