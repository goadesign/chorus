// This file tests installRelease, the step of "chorus update" and
// POST /update that writes a release to disk: Homebrew upgrades bound to the
// approved version, and the directory lock that keeps concurrent updates of
// one binary from downgrading it or replacing it before setup and restart.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genchorus "github.com/goadesign/chorus/gen/chorus"
)

// TestInstallReleaseHomebrew verifies that a binary inside a Homebrew Cellar
// is upgraded through Homebrew instead of being replaced: after
// "brew update", the tap must offer exactly the approved version before
// "brew upgrade goadesign/tap/chorus" runs, every brew command runs with
// HOMEBREW_NO_AUTO_UPDATE=1, the returned binary is Homebrew's bin/chorus,
// and an upgrade that does not reach the version is reported.
func TestInstallReleaseHomebrew(t *testing.T) {
	next := nextMinorVersion(t)
	cases := []struct {
		name string
		// offered is the version "brew info" reports for the tap's formula.
		offered string
		// brewInstalls is the version the fake "brew upgrade" links into bin/chorus.
		brewInstalls string
		wantCalls    string
		wantErr      string
	}{
		{
			name:         "tap offers the approved version",
			offered:      next,
			brewInstalls: next,
			wantCalls:    "update\ninfo --json=v2 goadesign/tap/chorus\nupgrade goadesign/tap/chorus\n",
		},
		{
			name:         "tap offering another version is refused before upgrading",
			offered:      "0.0.2",
			brewInstalls: "0.0.2",
			wantCalls:    "update\ninfo --json=v2 goadesign/tap/chorus\n",
			wantErr:      fmt.Sprintf("Homebrew offers chorus v0.0.2, not the approved v%s", next),
		},
		{
			name:         "formula changed during the upgrade is reported",
			offered:      next,
			brewInstalls: "0.0.2",
			wantCalls:    "update\ninfo --json=v2 goadesign/tap/chorus\nupgrade goadesign/tap/chorus\n",
			wantErr:      `installed "0.0.2"`,
		},
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
			writeExecutable(t, filepath.Join(brewDir, "brew"), []byte(fmt.Sprintf(`#!/bin/sh
[ "$HOMEBREW_NO_AUTO_UPDATE" = 1 ] || { echo "auto-update not disabled" >&2; exit 1; }
echo "$*" >> %q
case "$1" in
info) echo '{"formulae":[{"versions":{"stable":%q}}]}' ;;
upgrade) mkdir -p %q && cp %q %q && ln -sf %q %q ;;
esac
`, brewLog, tc.offered, filepath.Dir(newExe), newSource, newExe, newExe, link)))
			t.Setenv("PATH", brewDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("HOMEBREW_NO_AUTO_UPDATE", "")

			unused := releaseSource{client: http.DefaultClient, latestURL: "http://127.0.0.1:1/latest", downloadURL: "http://127.0.0.1:1/download"}
			var out bytes.Buffer
			got, unlock, err := installRelease(context.Background(), unused, release{version: next}, link, &out)
			assert.Equal(t, tc.wantCalls, string(mustReadFile(t, brewLog)))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				if tc.offered != next {
					assert.Equal(t, "0.0.1", binaryVersion(context.Background(), link), "a refused upgrade changes nothing")
				}
				return
			}
			require.NoError(t, err)
			assert.NoError(t, unlock())
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
		_, _, err := installRelease(context.Background(), releases, release{version: next}, exe, io.Discard)
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
			_, _, err := installRelease(context.Background(), releases, release{version: next}, exe, io.Discard)
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

	t.Run("lock stays held until the installer releases it", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "chorus")
		writeExecutable(t, exe, fakeChorusBinary(genchorus.APIVersion))
		_, unlock, err := installRelease(context.Background(), releases, release{version: next}, exe, io.Discard)
		require.NoError(t, err)

		// While the first installer still holds the lock (for example during
		// setup), a second install of the same path must wait.
		done := make(chan error, 1)
		go func() {
			_, secondUnlock, err := installRelease(context.Background(), releases, release{version: next}, exe, io.Discard)
			if err == nil {
				err = secondUnlock()
			}
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("second install finished while the first held the lock: %v", err)
		case <-time.After(3 * installLockPoll):
		}
		require.NoError(t, unlock())
		select {
		case err := <-done:
			require.NoError(t, err, "the second install finds the version already installed")
		case <-time.After(10 * time.Second):
			t.Fatal("second install did not resume after the lock was released")
		}
		assert.Equal(t, next, binaryVersion(context.Background(), exe))
	})
}

// assertLocked fails unless another holder has the install lock on dir.
func assertLocked(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := lockDir(ctx, dir)
	if err == nil {
		assert.NoError(t, unlock())
		t.Errorf("expected the install lock on %s to be held", dir)
	}
}
