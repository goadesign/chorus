// This file implements updating Chorus to its latest release: the
// "chorus update" command and the GET and POST /update routes the dashboard
// calls. Both find the latest release on GitHub, install it over the running
// executable (or through Homebrew when Homebrew installed Chorus), refresh the
// skills, MCP registrations, and Herdr launcher with the new binary's setup,
// and restart the server on the new binary with its current flags.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	genchorus "github.com/goadesign/chorus/gen/chorus"
)

type (
	// releaseSource finds and downloads published Chorus releases. dispatch
	// builds it with defaultReleaseSource; tests point it at a local server.
	releaseSource struct {
		// client performs the GitHub API and download requests.
		client *http.Client
		// latestURL returns the latest release as JSON with tag_name and html_url.
		latestURL string
		// downloadURL is the base URL under which each release tag publishes
		// its archives and checksums.txt, as <downloadURL>/v<version>/<file>.
		downloadURL string
	}

	// release identifies one published Chorus release.
	release struct {
		// version has no leading "v" (for example "0.10.0"), the same form as
		// the X-Chorus-Version header.
		version string
		// url is the release's GitHub page.
		url string
	}

	// updateStatus is the JSON body of a successful GET /update.
	updateStatus struct {
		// CurrentVersion is the version of the server answering the request.
		CurrentVersion string `json:"current_version"`
		// LatestVersion is the latest published release.
		LatestVersion string `json:"latest_version"`
		// UpdateAvailable is true only when LatestVersion is newer than
		// CurrentVersion, so the dashboard never offers a downgrade.
		UpdateAvailable bool `json:"update_available"`
		// ReleaseURL is the latest release's GitHub page.
		ReleaseURL string `json:"release_url"`
	}

	// updateRequest is the JSON body of POST /update: the version the user
	// approved, as shown by GET /update or "chorus update". The server
	// installs exactly this version, so a release published after the
	// approval is never installed by it.
	updateRequest struct {
		// Version is a release version without the leading "v", such as "0.10.0".
		Version string `json:"version"`
	}

	// updateResult is the JSON body of a successful POST /update.
	updateResult struct {
		// Status is "restarting" when the requested version was installed and
		// the server is about to restart on it, or "up_to_date" when the
		// server already runs the requested version.
		Status string `json:"status"`
		// Version is the version the dashboard should wait for in the
		// X-Chorus-Version header of GET /healthz.
		Version string `json:"version"`
	}

	// updateError is the JSON body of a failed GET or POST /update.
	updateError struct {
		// Error says what failed and what the user can do about it.
		Error string `json:"error"`
	}

	// updateHandler serves GET and POST /update for the server runServe
	// started: the dashboard and "chorus update" call it. A POST installs the
	// requested release and restarts this server process in place on it;
	// only one runs at a time.
	updateHandler struct {
		// releases finds and downloads releases.
		releases releaseSource
		// exe is the path this server was started from (os.Executable).
		exe string
		// addr is this server's listen address, which the new binary's setup
		// registers with the coding agents.
		addr string
		// restart asks runServe to shut down and restart this process on the
		// binary at exe, handing over the install lock that unlock releases;
		// a recorder in tests.
		restart func(exe string, unlock func() error)
		// busy is held from the start of a POST until the restart begins, or
		// until the POST fails, so concurrent POSTs answer 409.
		busy sync.Mutex
	}
)

const (
	// homebrewFormula is the Homebrew formula "chorus update" upgrades when
	// Homebrew installed the running binary.
	homebrewFormula = "goadesign/tap/chorus"
	// updateTimeout bounds a whole update: the release check, download or
	// Homebrew upgrade, setup, and restart.
	updateTimeout = 10 * time.Minute
	// releaseCheckTimeout bounds one request for the latest release.
	releaseCheckTimeout = 15 * time.Second
	// installLockPoll is how often an install retries the lock on the
	// binary's directory while another install holds it.
	installLockPoll = 100 * time.Millisecond
)

var (
	// errReleaseUnavailable marks failures to reach GitHub, download a
	// release, or verify its checksum, as opposed to failures installing it
	// on this machine. POST /update answers the former with 502.
	errReleaseUnavailable = errors.New("release unavailable")
	// errWouldDowngrade marks an install refused because the binary on disk
	// already reports a newer version than the one requested, for example
	// after another update finished while this one waited for approval.
	// POST /update answers it with 409.
	errWouldDowngrade = errors.New("would downgrade")
)

// ServeHTTP answers GET /update with an updateStatus and POST /update by
// installing the requested release and restarting this server on it. Other
// methods get 405. runServe mounts it behind chorus.LocalOnly and
// loopbackPeer, so only the dashboard and processes on this machine reach it.
func (h *updateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.status(w, r)
	case http.MethodPost:
		h.update(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, updateError{Error: "use GET or POST"})
	}
}

// defaultReleaseSource returns the source for releases published on GitHub
// at github.com/goadesign/chorus.
func defaultReleaseSource() releaseSource {
	return releaseSource{
		client:      &http.Client{},
		latestURL:   "https://api.github.com/repos/goadesign/chorus/releases/latest",
		downloadURL: "https://github.com/goadesign/chorus/releases/download",
	}
}

// runUpdate implements "chorus update [-addr] [-check] [-yes]". It compares
// this binary and the server running at the address "chorus up" last used
// (or -addr) with the latest release and writes one of:
//
//	Update available: chorus v0.9.0 -> v0.10.0 (https://github.com/...)
//	Restart needed: chorus binary is v0.10.0, running server is v0.9.0
//	Chorus v0.11.0 is running, newer than the latest release v0.10.0
//	Chorus v0.10.0 is up to date
//
// The first two lines are a contract for callers such as the Herdr
// launcher: the version after "->" or after "chorus binary is" is the one to
// pass to -version. With -version, runUpdate skips the latest-release check
// and updates to exactly that version, refusing one older than this binary
// or the running server, so a caller's approval cannot be widened to a
// release published after it showed the line.
//
// With -check it stops there. Otherwise it asks "Update Chorus to v0.10.0?
// [y/N]: " (or whether to restart) on in unless -yes is given. Ctrl-C at the
// prompt ends the command as usual; the update itself, which starts after
// the answer, is bounded by updateTimeout and stops on SIGINT or SIGTERM.
//
// A running server is asked to update itself with POST /update and the
// approved version: it installs the release into its own executable and
// restarts in place, because only that process knows the flags and Redis
// password it runs with. runUpdate waits for GET /healthz to report the
// version and writes the server's addresses. Otherwise runUpdate installs
// the release over exe or through Homebrew itself. A server older than POST /update is left
// running, and runUpdate returns an error with instructions to restart it,
// so callers such as the Herdr launcher see the update as unfinished. With
// no server running, the
// new binary's "setup -global" refreshes the skills, MCP registrations, and
// Herdr launcher. Output of the commands it runs is written to out.
func runUpdate(args []string, in io.Reader, out io.Writer, releases releaseSource, exe string) error {
	var (
		addr    string
		check   bool
		yes     bool
		version string
	)
	fs := flag.NewFlagSet("chorus update", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", "", "Chorus HTTP listen address (default: the address 'chorus up' last used, else "+defaultListenAddr+")")
	fs.BoolVar(&check, "check", false, "Only report whether an update or restart is available")
	fs.BoolVar(&yes, "yes", false, "Update without asking for confirmation")
	fs.BoolVar(&yes, "y", false, "Shorthand for -yes")
	fs.StringVar(&version, "version", "", "Update to exactly this version (MAJOR.MINOR.PATCH, as reported by -check) instead of the latest release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if version != "" && check {
		return errors.New("-check looks up the latest release, so it cannot be combined with -version")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	serveArgs, err := loadServeArgs(homeDir)
	if err != nil {
		return err
	}
	if addr != "" {
		serveArgs = append(serveArgs, "-addr", addr)
	}
	cfg, err := parseServerFlags(serveArgs)
	if err != nil {
		return err
	}
	baseURL := localBaseURL(cfg.addr)

	running, serverVersion := probeServer(baseURL)
	var rel release
	if version == "" {
		if rel, err = releases.latest(context.Background()); err != nil {
			return err
		}
	} else {
		if _, err := parseVersion(version); err != nil {
			return err
		}
		if versionBefore(version, genchorus.APIVersion) {
			return fmt.Errorf("v%s is older than this chorus binary v%s", version, genchorus.APIVersion)
		}
		if running && versionBefore(version, serverVersion) {
			return fmt.Errorf("v%s is older than the running server v%s", version, serverVersion)
		}
		rel = release{version: version}
	}
	newer, err := newerVersion(rel.version, genchorus.APIVersion)
	if err != nil {
		return err
	}
	// target is the version a running server should end up on: the release
	// when it is newer than this binary, otherwise this binary's version.
	target := genchorus.APIVersion
	if newer {
		target = rel.version
	}
	switch {
	case running && versionBefore(rel.version, serverVersion):
		fmt.Fprintf(out, "Chorus v%s is running, newer than the latest release v%s\n", serverVersion, rel.version)
		return nil
	case newer && rel.url != "":
		fmt.Fprintf(out, "Update available: chorus v%s -> v%s (%s)\n", genchorus.APIVersion, rel.version, rel.url)
	case newer:
		fmt.Fprintf(out, "Update available: chorus v%s -> v%s\n", genchorus.APIVersion, rel.version)
	case running && versionBefore(serverVersion, target):
		fmt.Fprintf(out, "Restart needed: chorus binary is v%s, running server is v%s\n", genchorus.APIVersion, serverVersion)
	default:
		fmt.Fprintf(out, "Chorus v%s is up to date\n", genchorus.APIVersion)
		return nil
	}
	if check {
		return nil
	}

	question := fmt.Sprintf("Update Chorus to v%s? [y/N]: ", target)
	if !newer {
		question = fmt.Sprintf("Restart Chorus on v%s? [y/N]: ", target)
	}
	if !yes && !confirm(in, out, question) {
		fmt.Fprintln(out, "Update cancelled")
		return nil
	}

	// From here on the update changes files and processes, so it gets its
	// own deadline and stops cleanly on SIGINT or SIGTERM.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(sigCtx, updateTimeout)
	defer cancel()

	// install makes target the local binary to run next. A running server
	// installs into its own executable instead, so this runs only when no
	// server answers, the server already runs target, or it predates
	// POST /update.
	install := func() (string, func() error, error) {
		if !newer {
			return exe, func() error { return nil }, nil
		}
		next, unlock, err := installRelease(ctx, releases, rel, exe, out)
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(out, "Installed chorus v%s at %s\n", rel.version, next)
		return next, unlock, nil
	}
	if running {
		return timeoutHint(ctx, updateServer(ctx, out, cfg.addr, serverVersion, target, install))
	}
	_, err = installAndSetUp(ctx, out, cfg.addr, install)
	return timeoutHint(ctx, err)
}

// installAndSetUp runs install and then the installed binary's
// "setup -global", so the skills, MCP registrations, and Herdr launcher
// match it, and returns that binary's path. The install lock stays held
// until setup finishes, so another update cannot replace the binary between
// the two steps.
func installAndSetUp(ctx context.Context, out io.Writer, addr string, install func() (string, func() error, error)) (string, error) {
	next, unlock, err := install()
	if err != nil {
		return "", err
	}
	defer releaseInstallLock(unlock)
	if err := runChorus(ctx, next, out, "setup", "-global", "-addr", addr); err != nil {
		return "", err
	}
	return next, nil
}

// updateServer asks the server at addr to update itself to version with
// POST /update and waits up to 2*daemonReadyTimeout for GET /healthz to
// report that version, then writes the server's addresses to out. The
// server installs the release and runs setup itself. When the server
// already runs version, install provides the local binary whose
// "setup -global" refreshes the skills and MCP registrations. A server
// without POST /update (v0.9.0 and earlier) is left running after the same
// install and setup, and the returned error says how to restart it, so the
// command exits nonzero while that server still runs the old version: its
// Redis password and flags are unknown here, so stopping it could leave
// nothing running.
func updateServer(ctx context.Context, out io.Writer, addr, serverVersion, version string, install func() (string, func() error, error)) error {
	baseURL := localBaseURL(addr)
	body, err := json.Marshal(updateRequest{Version: version})
	if err != nil {
		return fmt.Errorf("encode update request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/update", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ask chorus at %s to update: %w", baseURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ask chorus at %s to update: %w", baseURL, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("close update response", "error", err)
		}
	}()

	switch resp.StatusCode {
	case http.StatusAccepted:
		fmt.Fprintf(out, "Chorus at %s is installing v%s and restarting\n", baseURL, version)
	case http.StatusOK, http.StatusNotFound:
		next, err := installAndSetUp(ctx, out, addr, install)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("installed chorus v%s and refreshed integrations, but the running server (v%s at %s) cannot update itself; finish the update by running '%s up' with the flags and REDIS_PASSWORD it was started with", version, serverVersion, baseURL, next)
		}
		writeEndpoints(out, version, baseURL)
		return nil
	default:
		var failure updateError
		if err := json.NewDecoder(resp.Body).Decode(&failure); err != nil || failure.Error == "" {
			return fmt.Errorf("chorus at %s could not update: %s", baseURL, resp.Status)
		}
		return fmt.Errorf("chorus at %s could not update: %s", baseURL, failure.Error)
	}

	deadline := time.Now().Add(2 * daemonReadyTimeout)
	for time.Now().Before(deadline) {
		if ok, got := probeServer(baseURL); ok && got == version {
			writeEndpoints(out, version, baseURL)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("chorus v%s did not become healthy at %s/healthz within %s; check 'chorus status' and ~/.chorus/chorus.log", version, baseURL, 2*daemonReadyTimeout)
}

// installRelease makes rel the chorus binary run next and returns its path
// with the function that releases the install lock, which the caller holds
// until it no longer needs that exact file: through setup, and for a server
// until it has replaced itself with the file. Installs take an exclusive
// lock on the directory of the file they write and read the version on disk
// under it, so concurrent updates of one binary, including from two servers
// sharing it, run one at a time: a binary already at rel's version is kept,
// and one reporting a newer version is left alone with an error wrapping
// errWouldDowngrade. A binary inside a Homebrew Cellar is upgraded through
// Homebrew (see installHomebrew), and the returned path is Homebrew's
// bin/chorus; any other binary is replaced in place (see installDirect).
func installRelease(ctx context.Context, releases releaseSource, rel release, exe string, out io.Writer) (string, func() error, error) {
	realExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		realExe = exe
	}
	next, dir := exe, filepath.Dir(realExe)
	prefix, homebrew := homebrewPrefix(realExe)
	if homebrew {
		next = filepath.Join(prefix, "bin", "chorus")
		dir = filepath.Dir(next)
	}
	unlock, err := lockDir(ctx, dir)
	if err != nil {
		return "", nil, fmt.Errorf("install chorus v%s: %w", rel.version, err)
	}
	if homebrew {
		err = installHomebrew(ctx, rel, next, out)
	} else {
		err = installDirect(ctx, releases, rel, realExe)
	}
	if err != nil {
		releaseInstallLock(unlock)
		return "", nil, err
	}
	return next, unlock, nil
}

// installHomebrew makes Homebrew's bin/chorus at next report rel's version.
// A Homebrew tap only offers its latest formula, so after "brew update" it
// checks that the tap offers exactly rel's version and otherwise refuses,
// before changing anything, with an error wrapping errReleaseUnavailable.
// Every brew command runs with HOMEBREW_NO_AUTO_UPDATE=1, so "brew upgrade"
// does not refresh the tap itself; a "brew update" run by another process
// in between can still change the formula, which the version check after
// the upgrade reports. Homebrew's output is written to out. The caller holds
// the install lock.
func installHomebrew(ctx context.Context, rel release, next string, out io.Writer) error {
	installed := binaryVersion(ctx, next)
	if installed == rel.version {
		return nil
	}
	if versionBefore(rel.version, installed) {
		return fmt.Errorf("%w: %s is already chorus v%s, newer than v%s", errWouldDowngrade, next, installed, rel.version)
	}
	if err := runBrew(ctx, out, "update"); err != nil {
		return err
	}
	offered, err := homebrewStableVersion(ctx)
	if err != nil {
		return err
	}
	if offered != rel.version {
		return fmt.Errorf("%w: Homebrew offers chorus v%s, not the approved v%s; check for updates again", errReleaseUnavailable, offered, rel.version)
	}
	if err := runBrew(ctx, out, "upgrade", homebrewFormula); err != nil {
		return err
	}
	if got := binaryVersion(ctx, next); got != rel.version {
		return fmt.Errorf("brew upgrade %s installed %q at %s, not v%s", homebrewFormula, got, next, rel.version)
	}
	return nil
}

// installDirect replaces the binary at realExe with rel's downloaded
// release, but only after the downloaded file has run and reported rel's
// version, so a broken or mislabeled release never overwrites a working
// binary. The caller holds the install lock.
func installDirect(ctx context.Context, releases releaseSource, rel release, realExe string) error {
	installed := binaryVersion(ctx, realExe)
	if installed == rel.version {
		return nil
	}
	if versionBefore(rel.version, installed) {
		return fmt.Errorf("%w: %s is already chorus v%s, newer than v%s", errWouldDowngrade, realExe, installed, rel.version)
	}
	bin, err := releases.download(ctx, rel.version)
	if err != nil {
		return err
	}
	err = replaceExecutable(realExe, bin, func(candidate string) error {
		if got := binaryVersion(ctx, candidate); got != rel.version {
			return fmt.Errorf("%w: the v%s release binary reports %q, not v%s", errReleaseUnavailable, rel.version, got, rel.version)
		}
		return nil
	})
	if errors.Is(err, errReleaseUnavailable) {
		return err
	}
	if err != nil {
		return fmt.Errorf("install chorus v%s at %s: %w (rerun with write access to %s)", rel.version, realExe, err, filepath.Dir(realExe))
	}
	return nil
}

// runBrew runs "brew args..." with HOMEBREW_NO_AUTO_UPDATE=1, writing its
// output to out, so only an explicit "brew update" refreshes the taps.
func runBrew(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "brew", args...)
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("brew %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// homebrewStableVersion returns the version the goadesign/tap/chorus formula
// offers, from "brew info --json=v2".
func homebrewStableVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "brew", "info", "--json=v2", homebrewFormula)
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("brew info %s: %w", homebrewFormula, err)
	}
	var info struct {
		// Formulae lists the requested formula.
		Formulae []struct {
			// Versions holds the formula's offered versions.
			Versions struct {
				// Stable is the version "brew upgrade" installs.
				Stable string `json:"stable"`
			} `json:"versions"`
		} `json:"formulae"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || len(info.Formulae) != 1 || info.Formulae[0].Versions.Stable == "" {
		return "", fmt.Errorf("%w: brew info %s did not report a stable version", errReleaseUnavailable, homebrewFormula)
	}
	return info.Formulae[0].Versions.Stable, nil
}

// releaseInstallLock releases an install lock, logging a failure: the lock
// is advisory and also ends when this process exits or replaces itself.
func releaseInstallLock(unlock func() error) {
	if err := unlock(); err != nil {
		slog.Debug("release chorus install lock", "error", err)
	}
}

// extractBinary returns the "chorus" file of a release .tar.gz archive.
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no chorus binary")
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && path.Clean(hdr.Name) == "chorus" {
			bin, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read chorus from archive: %w", err)
			}
			return bin, nil
		}
	}
}

// lockDir takes an exclusive advisory lock (flock) on dir, waiting while
// another process holds it until ctx ends, and returns the function that
// releases it. Locking the directory rather than the binary keeps the lock
// on one stable inode while the binary is renamed over, and leaves no lock
// file behind.
func lockDir(ctx context.Context, dir string) (func() error, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("lock %s: %w", dir, err), f.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("wait for another chorus install in %s: %w", dir, ctx.Err()), f.Close())
		case <-time.After(installLockPoll):
		}
	}
}

// replaceExecutable atomically replaces the file at target with an
// executable holding content. The new file is written next to target, passed
// to check, and renamed over target only when check accepts it, so a running
// server keeps executing the old file and any failure, including check's,
// leaves target unchanged and removes the new file.
func replaceExecutable(target string, content []byte, check func(candidate string) error) (err error) {
	file, err := os.CreateTemp(filepath.Dir(target), ".chorus-update-*")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.Remove(file.Name()); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	writeErr := file.Chmod(0o755)
	if writeErr == nil {
		_, writeErr = file.Write(content)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := check(file.Name()); err != nil {
		return err
	}
	return os.Rename(file.Name(), target)
}

// homebrewPrefix returns the Homebrew prefix (such as /opt/homebrew) when
// realExe lies in Homebrew's Cellar (such as
// /opt/homebrew/Cellar/chorus/0.9.0/bin/chorus).
func homebrewPrefix(realExe string) (string, bool) {
	i := strings.Index(realExe, "/Cellar/chorus/")
	if i < 0 {
		return "", false
	}
	return realExe[:i], true
}

// binaryVersion returns the version "<exe> version" reports without its
// leading "v" (for example "0.10.0"), or "" when exe cannot run.
func binaryVersion(ctx context.Context, exe string) string {
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "chorus v")
}

// parseVersion splits a MAJOR.MINOR.PATCH version such as "0.10.0".
func parseVersion(version string) ([3]int, error) {
	var parsed [3]int
	parts := strings.Split(version, ".")
	if len(parts) != len(parsed) {
		return parsed, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", version)
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return parsed, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", version)
		}
		parsed[i] = n
	}
	return parsed, nil
}

// newerVersion reports whether version a is strictly newer than version b.
func newerVersion(a, b string) (bool, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	return slices.Compare(pa[:], pb[:]) > 0, nil
}

// versionBefore reports whether version a is older than version b. A
// malformed or unknown version (such as the empty X-Chorus-Version of a
// server that predates the header) is never reported as older or newer.
func versionBefore(a, b string) bool {
	newer, err := newerVersion(b, a)
	return err == nil && newer
}

// confirm writes question to out and reports whether the line read from in
// is "y" or "yes" (any case). End of input counts as no.
func confirm(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprint(out, question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// runChorus runs "<exe> args..." with its output written to out and returns
// an error naming the command when it fails.
func runChorus(ctx context.Context, exe string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", exe, strings.Join(args, " "), err)
	}
	return nil
}

// timeoutHint adds that the update timed out to err when ctx expired.
func timeoutHint(ctx context.Context, err error) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: timed out after %s", err, updateTimeout)
	}
	return err
}

// loopbackPeer returns middleware that answers 403 unless the request's TCP
// peer is on this machine. chorus.LocalOnly checks headers a non-browser
// client can set freely; the peer address cannot be forged, so a server
// listening on all interfaces (-addr :9440) still updates only for local
// callers.
func loopbackPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			writeJSON(w, http.StatusForbidden, updateError{Error: "updates are only accepted from this machine"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON writes v as a JSON response with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write /update response", "error", err)
	}
}

// latest returns the latest published release. Failures to reach GitHub or
// decode its answer wrap errReleaseUnavailable.
func (s releaseSource) latest(ctx context.Context) (release, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.latestURL, nil)
	if err != nil {
		return release{}, fmt.Errorf("check latest chorus release: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("%w: check latest chorus release: %w", errReleaseUnavailable, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("close release response", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("%w: check latest chorus release at %s: %s", errReleaseUnavailable, s.latestURL, resp.Status)
	}
	var body struct {
		// TagName is the release tag, such as "v0.10.0".
		TagName string `json:"tag_name"`
		// HTMLURL is the release's GitHub page.
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return release{}, fmt.Errorf("%w: decode latest chorus release: %w", errReleaseUnavailable, err)
	}
	version := strings.TrimPrefix(body.TagName, "v")
	if _, err := parseVersion(version); err != nil {
		return release{}, fmt.Errorf("%w: latest chorus release tag %q: %w", errReleaseUnavailable, body.TagName, err)
	}
	return release{version: version, url: body.HTMLURL}, nil
}

// download returns the chorus binary of version for this OS and CPU. It
// fetches the release archive and checksums.txt, rejects an archive whose
// SHA-256 differs from the published one, and extracts the "chorus" file.
// Every failure wraps errReleaseUnavailable.
func (s releaseSource) download(ctx context.Context, version string) ([]byte, error) {
	archive := fmt.Sprintf("chorus_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/v%s/", s.downloadURL, version)
	sums, err := s.fetch(ctx, base+"checksums.txt")
	if err != nil {
		return nil, err
	}
	data, err := s.fetch(ctx, base+archive)
	if err != nil {
		return nil, err
	}
	var want string
	for line := range strings.Lines(string(sums)) {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == archive {
			want = fields[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%w: %s is not listed in checksums.txt of v%s", errReleaseUnavailable, archive, version)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%w: SHA-256 mismatch for %s: expected %s, got %s", errReleaseUnavailable, archive, want, got)
	}
	bin, err := extractBinary(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errReleaseUnavailable, archive, err)
	}
	return bin, nil
}

// fetch returns the body of a successful GET of url.
func (s releaseSource) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: download %s: %w", errReleaseUnavailable, url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("close release response", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: download %s: %s", errReleaseUnavailable, url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: download %s: %w", errReleaseUnavailable, url, err)
	}
	return body, nil
}

// status answers GET /update with this server's version and the latest
// release, or 502 when the latest release cannot be found.
func (h *updateHandler) status(w http.ResponseWriter, r *http.Request) {
	rel, err := h.releases.latest(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, updateError{Error: err.Error()})
		return
	}
	newer, err := newerVersion(rel.version, genchorus.APIVersion)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, updateError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, updateStatus{
		CurrentVersion:  genchorus.APIVersion,
		LatestVersion:   rel.version,
		UpdateAvailable: newer,
		ReleaseURL:      rel.url,
	})
}

// update answers POST /update with an updateRequest body. When the
// requested version is newer than this server, it installs exactly that
// version, runs the new binary's "setup -global" so skills, MCP
// registrations, and the Herdr launcher match it, answers 202 with the
// version, and only then asks runServe to restart, so the response reaches
// the caller before this server shuts down and replaces itself with the new
// binary (see serve). It answers 200 "up_to_date" when this
// server already runs the version, 400 for a missing or malformed body
// (including anything after the JSON object), 409
// for an older version than this server or than the binary already on disk
// (a downgrade) or while another update runs, 502 when
// the release cannot be downloaded or verified, and 500 when installing or
// setup fails. The server keeps running after every error.
func (h *updateHandler) update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, updateError{Error: fmt.Sprintf(`send {"version":"MAJOR.MINOR.PATCH"}: %v`, err)})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, updateError{Error: `send only {"version":"MAJOR.MINOR.PATCH"}, with nothing after it`})
		return
	}
	if _, err := parseVersion(req.Version); err != nil {
		writeJSON(w, http.StatusBadRequest, updateError{Error: err.Error()})
		return
	}
	if req.Version == genchorus.APIVersion {
		writeJSON(w, http.StatusOK, updateResult{Status: "up_to_date", Version: genchorus.APIVersion})
		return
	}
	if versionBefore(req.Version, genchorus.APIVersion) {
		writeJSON(w, http.StatusConflict, updateError{Error: fmt.Sprintf("v%s would downgrade the running v%s", req.Version, genchorus.APIVersion)})
		return
	}
	if !h.busy.TryLock() {
		writeJSON(w, http.StatusConflict, updateError{Error: "an update is already in progress"})
		return
	}
	restarting := false
	defer func() {
		if !restarting {
			h.busy.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), updateTimeout)
	defer cancel()

	var output bytes.Buffer
	next, unlock, err := installRelease(ctx, h.releases, release{version: req.Version}, h.exe, &output)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errReleaseUnavailable):
			status = http.StatusBadGateway
		case errors.Is(err, errWouldDowngrade):
			status = http.StatusConflict
		}
		slog.Error("update chorus", "version", req.Version, "error", err, "output", output.String())
		writeJSON(w, status, updateError{Error: timeoutHint(ctx, err).Error()})
		return
	}
	if err := runChorus(ctx, next, &output, "setup", "-global", "-addr", h.addr); err != nil {
		releaseInstallLock(unlock)
		slog.Error("set up updated chorus", "version", req.Version, "error", err, "output", output.String())
		writeJSON(w, http.StatusInternalServerError, updateError{Error: fmt.Sprintf("%v: %s", timeoutHint(ctx, err), strings.TrimSpace(output.String()))})
		return
	}

	writeJSON(w, http.StatusAccepted, updateResult{Status: "restarting", Version: req.Version})
	if err := http.NewResponseController(w).Flush(); err != nil {
		slog.Error("flush /update response", "error", err)
	}
	// Both locks stay held: this server stops accepting updates as it shuts
	// down, and the install lock keeps the installed file in place until
	// this process has replaced itself with it.
	restarting = true
	h.restart(next, unlock)
}
