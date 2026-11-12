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

	// updateResult is the JSON body of a successful POST /update.
	updateResult struct {
		// Status is "restarting" when a newer release was installed and the
		// server is about to restart on it, or "up_to_date" when the server
		// already runs the latest release.
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

	// updateHandler serves GET and POST /update for the dashboard of the
	// server runServe started. A POST installs the latest release and
	// restarts this server on it; only one runs at a time.
	updateHandler struct {
		// releases finds and downloads releases.
		releases releaseSource
		// exe is the path this server was started from (os.Executable).
		exe string
		// cfg holds this server's flags; the restarted server keeps them.
		cfg serverConfig
		// restart starts "<exe> up <args>" in a process that outlives this
		// server; startRestart in production, a recorder in tests.
		restart func(exe string, args []string) error
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
)

var (
	// errReleaseUnavailable marks failures to reach GitHub, download a
	// release, or verify its checksum, as opposed to failures installing it
	// on this machine. POST /update answers the former with 502.
	errReleaseUnavailable = errors.New("release unavailable")
)

// ServeHTTP answers GET /update with an updateStatus and POST /update by
// installing the latest release and restarting this server on it. Other
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
//	Chorus v0.10.0 is up to date
//
// With -check it stops there. Otherwise it asks "Update Chorus to v0.10.0?
// [y/N]: " (or whether to restart) on in unless -yes is given, installs the
// release over exe or through Homebrew, refreshes the project's Chorus files
// when the working directory has .agents/skills/chorus/SKILL.md, and then
// either restarts the running server with "<new binary> up" and its saved
// flags, or, when no server answers, runs "<new binary> setup -global". The
// new binary's output is written to out. The whole update is bounded by
// updateTimeout.
func runUpdate(args []string, in io.Reader, out io.Writer, releases releaseSource, exe string) error {
	var (
		addr  string
		check bool
		yes   bool
	)
	fs := flag.NewFlagSet("chorus update", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", "", "Chorus HTTP listen address (default: the address 'chorus up' last used, else "+defaultListenAddr+")")
	fs.BoolVar(&check, "check", false, "Only report whether an update or restart is available")
	fs.BoolVar(&yes, "yes", false, "Update without asking for confirmation")
	fs.BoolVar(&yes, "y", false, "Shorthand for -yes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	saved, err := loadServeArgs(homeDir)
	if err != nil {
		return err
	}
	// The new binary's "up" reads the saved flags itself, so only an explicit
	// -addr is passed on to it.
	var upArgs []string
	if addr != "" {
		upArgs = []string{"-addr", addr}
	}
	cfg, err := parseServerFlags(append(saved, upArgs...))
	if err != nil {
		return err
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(sigCtx, updateTimeout)
	defer cancel()

	running, serverVersion := probeServer(localBaseURL(cfg.addr))
	rel, err := releases.latest(ctx)
	if err != nil {
		return err
	}
	newer, err := newerVersion(rel.version, genchorus.APIVersion)
	if err != nil {
		return err
	}
	restart := running && !newer && serverOlder(serverVersion)
	switch {
	case newer:
		fmt.Fprintf(out, "Update available: chorus v%s -> v%s (%s)\n", genchorus.APIVersion, rel.version, rel.url)
	case restart:
		fmt.Fprintf(out, "Restart needed: chorus binary is v%s, running server is v%s\n", genchorus.APIVersion, serverVersion)
	default:
		fmt.Fprintf(out, "Chorus v%s is up to date\n", genchorus.APIVersion)
		return nil
	}
	if check {
		return nil
	}

	question := fmt.Sprintf("Update Chorus to v%s? [y/N]: ", rel.version)
	if !newer {
		question = fmt.Sprintf("Restart Chorus on v%s? [y/N]: ", genchorus.APIVersion)
	}
	if !yes && !confirm(in, out, question) {
		fmt.Fprintln(out, "Update cancelled")
		return nil
	}

	next := exe
	if newer {
		if next, err = installRelease(ctx, releases, rel, exe, out); err != nil {
			return timeoutHint(ctx, err)
		}
		fmt.Fprintf(out, "Installed chorus v%s at %s\n", rel.version, next)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".agents", "skills", "chorus", "SKILL.md")); err == nil {
		if err := runChorus(ctx, next, out, "setup", "-addr", cfg.addr); err != nil {
			return timeoutHint(ctx, err)
		}
	}
	if running {
		return timeoutHint(ctx, runChorus(ctx, next, out, append([]string{"up"}, upArgs...)...))
	}
	return timeoutHint(ctx, runChorus(ctx, next, out, "setup", "-global", "-addr", cfg.addr))
}

// installRelease makes rel the chorus binary run next and returns its path.
// A binary inside a Homebrew Cellar is upgraded with "brew upgrade", writing
// Homebrew's output to out, and the returned path is Homebrew's bin/chorus.
// Any other binary is replaced in place by the downloaded release. Nothing is
// downloaded when the binary on disk already reports rel's version, for
// example after an earlier update whose restart did not happen.
func installRelease(ctx context.Context, releases releaseSource, rel release, exe string, out io.Writer) (string, error) {
	realExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		realExe = exe
	}
	if prefix, ok := homebrewPrefix(realExe); ok {
		next := filepath.Join(prefix, "bin", "chorus")
		if binaryVersion(ctx, next) == rel.version {
			return next, nil
		}
		cmd := exec.CommandContext(ctx, "brew", "upgrade", homebrewFormula)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("brew upgrade %s: %w", homebrewFormula, err)
		}
		if got := binaryVersion(ctx, next); got != rel.version {
			return "", fmt.Errorf("brew upgrade %s installed %q at %s, not v%s; run 'brew update' and try again", homebrewFormula, got, next, rel.version)
		}
		return next, nil
	}

	if binaryVersion(ctx, realExe) == rel.version {
		return exe, nil
	}
	bin, err := releases.download(ctx, rel.version)
	if err != nil {
		return "", err
	}
	if err := replaceExecutable(realExe, bin); err != nil {
		return "", fmt.Errorf("install chorus v%s at %s: %w (rerun with write access to %s)", rel.version, realExe, err, filepath.Dir(realExe))
	}
	if got := binaryVersion(ctx, realExe); got != rel.version {
		return "", fmt.Errorf("installed chorus at %s reports %q, not v%s", realExe, got, rel.version)
	}
	return exe, nil
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

// replaceExecutable atomically replaces the file at target with an
// executable holding content. The new file is written next to target and
// renamed over it, so a running server keeps executing the old file and a
// failure leaves target unchanged.
func replaceExecutable(target string, content []byte) (err error) {
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

// serverOlder reports whether a running server's X-Chorus-Version is older
// than this binary. An unknown or malformed version is not reported as older.
func serverOlder(serverVersion string) bool {
	older, err := newerVersion(genchorus.APIVersion, serverVersion)
	return err == nil && older
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

// startRestart starts "<exe> up <args>" so that it outlives this server,
// which "up" stops before starting the new one. Under systemd, stopping
// chorus.service ends every process in the service, including children in a
// new session, so the restart runs as its own transient unit through
// systemd-run. Otherwise it runs in a new session, which launchd does not
// stop with the job and which survives the server's exit. Its output is
// appended to ~/.chorus/chorus.log.
func startRestart(exe string, args []string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	logPath := filepath.Join(homeDir, ".chorus", "chorus.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logPath, err)
	}
	defer func() {
		_ = logFile.Close()
	}()

	upArgs := append([]string{"up"}, args...)
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" && os.Getenv("INVOCATION_ID") != "" {
		cmd = exec.Command("systemd-run", append([]string{"--user", "--collect", "--quiet", "--setenv=PATH=" + daemonPathEnv(), "--", exe}, upArgs...)...)
	} else {
		cmd = exec.Command(exe, upArgs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s up: %w", exe, err)
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
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
		_ = resp.Body.Close()
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
		_ = resp.Body.Close()
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

// update answers POST /update. When a newer release exists it installs it,
// runs the new binary's "setup -global" so skills, MCP registrations, and the
// Herdr launcher match it, answers 202 with the new version, and only then
// starts the restart, so the response reaches the dashboard before this
// server stops. It answers 200 "up_to_date" when this server already runs the
// latest release, 409 while another update runs, 502 when the release cannot
// be found, downloaded, or verified, and 500 when installing or setup fails.
// The server keeps running after every error.
func (h *updateHandler) update(w http.ResponseWriter, r *http.Request) {
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

	rel, err := h.releases.latest(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, updateError{Error: err.Error()})
		return
	}
	newer, err := newerVersion(rel.version, genchorus.APIVersion)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, updateError{Error: err.Error()})
		return
	}
	if !newer {
		writeJSON(w, http.StatusOK, updateResult{Status: "up_to_date", Version: genchorus.APIVersion})
		return
	}

	var output bytes.Buffer
	next, err := installRelease(ctx, h.releases, rel, h.exe, &output)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errReleaseUnavailable) {
			status = http.StatusBadGateway
		}
		slog.Error("update chorus", "version", rel.version, "error", err, "output", output.String())
		writeJSON(w, status, updateError{Error: timeoutHint(ctx, err).Error()})
		return
	}
	if err := runChorus(ctx, next, &output, "setup", "-global", "-addr", h.cfg.addr); err != nil {
		slog.Error("set up updated chorus", "version", rel.version, "error", err, "output", output.String())
		writeJSON(w, http.StatusInternalServerError, updateError{Error: fmt.Sprintf("%v: %s", timeoutHint(ctx, err), strings.TrimSpace(output.String()))})
		return
	}

	writeJSON(w, http.StatusAccepted, updateResult{Status: "restarting", Version: rel.version})
	if err := http.NewResponseController(w).Flush(); err != nil {
		slog.Error("flush /update response", "error", err)
	}
	if err := h.restart(next, buildServeArgs(h.cfg)[1:]); err != nil {
		slog.Error("restart chorus on the updated binary", "version", rel.version, "error", err)
		return
	}
	restarting = true
	slog.Info("restarting chorus on the updated binary", "version", rel.version, "exe", next)
}
