// This file implements "chorus stdio": a newline-delimited JSON-RPC MCP
// transport over stdin and stdout for stdio-only MCP hosts and container
// registries (such as Glama's mcp-proxy harness or Claude Desktop). When a
// Chorus HTTP server is already running at the configured address, requests
// relay to POST /mcp so stdio clients share the live swarms and web dashboard.
// Otherwise, it starts an in-process Chorus service backed by local Redis or
// Valkey and answers requests directly without binding a TCP port.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/redis/go-redis/v9"
	mcpruntime "goa.design/goa-ai/runtime/mcp"
	"goa.design/goa/v3/jsonrpc"
)

type (
	// stdioDispatcher sends one raw JSON-RPC payload to Chorus's MCP handler
	// and returns the JSON-RPC response bytes.
	stdioDispatcher func(ctx context.Context, payload []byte, raw *jsonrpc.RawRequest) ([]byte, error)

	// stdioFrame carries one raw line or read error from the stdin reader.
	stdioFrame struct {
		// payload is one trimmed non-empty JSON-RPC frame line.
		payload []byte
		// err is a non-EOF read error from the underlying input stream.
		err error
	}

	// stdioWriter serializes single-line JSON-RPC responses onto out.
	stdioWriter struct {
		// mu guards writes to out and updates to err.
		mu sync.Mutex
		// out receives newline-terminated JSON-RPC frames.
		out io.Writer
		// err records the first write failure, stopping subsequent writes.
		err error
	}

	// stdioCancelEntry binds one active request cancellation callback to its
	// registration sequence so release never removes a newer request reusing an id.
	stdioCancelEntry struct {
		// seq is the monotonic registration sequence assigned by track.
		seq uint64
		// cancel aborts the in-flight request's context.
		cancel context.CancelFunc
	}

	// stdioCancels tracks cancel functions for in-flight JSON-RPC requests
	// keyed by their canonical JSON id so notifications/cancelled can abort
	// blocking read or send calls promptly.
	stdioCancels struct {
		// mu guards next and active.
		mu sync.Mutex
		// next assigns monotonic sequence numbers to tracked requests.
		next uint64
		// active maps canonical JSON request id strings to cancel entries.
		active map[string]stdioCancelEntry
	}

	// memoryResponseWriter captures an in-process http.Handler response without
	// opening a loopback TCP socket.
	memoryResponseWriter struct {
		// header holds response headers written by the handler.
		header http.Header
		// status records the first HTTP status code written by the handler.
		status int
		// body buffers the response body bytes.
		body bytes.Buffer
	}
)

const (
	// stdioRequestTimeout bounds a single forwarded MCP request over HTTP.
	// Exceeds the maximum 600s read/send wait_sec so long blocking reads finish.
	stdioRequestTimeout = 630 * time.Second
)

// Header returns the buffered HTTP response headers.
func (w *memoryResponseWriter) Header() http.Header {
	return w.header
}

// Write appends response bytes to the in-memory buffer.
func (w *memoryResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

// WriteHeader records the first HTTP response status code written.
func (w *memoryResponseWriter) WriteHeader(statusCode int) {
	if w.status == 0 {
		w.status = statusCode
	}
}

// runStdio reads newline-delimited MCP JSON-RPC frames from in and writes
// JSON-RPC responses to out until in reaches EOF or the process receives
// SIGINT/SIGTERM.
func runStdio(args []string, in io.Reader, out io.Writer) error {
	var cfg serverConfig
	defaultRedis := os.Getenv("REDIS_ADDR")
	if defaultRedis == "" {
		defaultRedis = defaultRedisAddr
	}
	fs := flag.NewFlagSet("chorus stdio", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", defaultListenAddr, "Chorus HTTP listen address to probe before starting in-process")
	fs.StringVar(&cfg.redisAddr, "redis-addr", defaultRedis, "Redis server address (host:port) for in-process fallback")
	fs.StringVar(&cfg.redisPassword, "redis-password", os.Getenv("REDIS_PASSWORD"), "Redis password for in-process fallback")
	fs.IntVar(&cfg.redisDB, "redis-db", 0, "Redis database index for in-process fallback")
	fs.DurationVar(&cfg.swarmTTL, "swarm-ttl", defaultSwarmTTL, "Sliding TTL for swarm state and streams")
	fs.IntVar(&cfg.streamMaxLen, "stream-max-len", defaultStreamMaxLen, "Maximum retained messages per channel or DM stream")
	fs.DurationVar(&cfg.onlineWindow, "online-window", defaultOnlineWindow, "Inactivity window before agent registration expires")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	baseURL, err := clientBaseURL(fs, cfg.addr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dispatch, cleanup, err := openStdioDispatcher(ctx, baseURL, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return serveStdioLoop(ctx, in, out, dispatch)
}

// openStdioDispatcher connects to an already-running Chorus server at baseURL
// when reachable, or starts an in-process Chorus service backed by Redis.
func openStdioDispatcher(
	ctx context.Context,
	baseURL string,
	cfg serverConfig,
) (stdioDispatcher, func(), error) {
	if ok, _ := probeServer(baseURL); ok {
		endpoint := strings.TrimRight(baseURL, "/") + "/mcp"
		client := &http.Client{Timeout: stdioRequestTimeout}
		dispatch := func(reqCtx context.Context, payload []byte, raw *jsonrpc.RawRequest) ([]byte, error) {
			req, err := newStdioMCPRequest(reqCtx, endpoint, payload, raw)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			defer func() {
				_ = resp.Body.Close()
			}()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			return validateStdioHTTPResponse(resp.StatusCode, body, endpoint)
		}
		return dispatch, func() {}, nil
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.redisAddr,
		Password: cfg.redisPassword,
		DB:       cfg.redisDB,
	})
	if err := pingRedis(ctx, rdb, initialPingTimeout); err != nil {
		homeDir, homeErr := os.UserHomeDir()
		if homeErr != nil {
			_ = rdb.Close()
			return nil, nil, fmt.Errorf("resolve home directory: %w", homeErr)
		}
		dataDir := filepath.Join(homeDir, ".chorus", "redis")
		if err := connectOrStartRedis(ctx, rdb, cfg.redisAddr, dataDir); err != nil {
			_ = rdb.Close()
			return nil, nil, fmt.Errorf("connect to Redis at %s: %w", cfg.redisAddr, err)
		}
	}
	svc, err := chorus.New(
		rdb,
		chorus.WithSwarmTTL(cfg.swarmTTL),
		chorus.WithStreamMaxLen(cfg.streamMaxLen),
		chorus.WithOnlineWindow(cfg.onlineWindow),
	)
	if err != nil {
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("initialize chorus service: %w", err)
	}
	handler := chorus.NewHTTPHandler(svc)
	dispatch := func(reqCtx context.Context, payload []byte, raw *jsonrpc.RawRequest) ([]byte, error) {
		req, err := newStdioMCPRequest(reqCtx, "http://127.0.0.1/mcp", payload, raw)
		if err != nil {
			return nil, err
		}
		rec := &memoryResponseWriter{header: make(http.Header)}
		handler.ServeHTTP(rec, req)
		return validateStdioHTTPResponse(
			rec.status,
			bytes.Clone(rec.body.Bytes()),
			"in-process MCP handler",
		)
	}
	cleanup := func() {
		_ = svc.Close()
		_ = rdb.Close()
	}
	return dispatch, cleanup, nil
}

// serveStdioLoop reads newline-delimited JSON-RPC payloads from in, handles
// valid notifications locally (including aborting in-flight requests on
// notifications/cancelled), dispatches requests concurrently so blocking read
// calls do not stall pings or discovery requests, and writes compacted
// single-line JSON responses to out.
func serveStdioLoop(
	ctx context.Context,
	in io.Reader,
	out io.Writer,
	dispatch stdioDispatcher,
) error {
	frames := make(chan stdioFrame)
	go readStdioFrames(ctx, in, frames)

	writer := &stdioWriter{out: out}
	cancels := &stdioCancels{active: make(map[string]stdioCancelEntry)}
	var (
		wg      sync.WaitGroup
		readErr error
	)

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case frame, ok := <-frames:
			if !ok {
				break loop
			}
			if frame.err != nil {
				readErr = frame.err
				break loop
			}
			var raw jsonrpc.RawRequest
			decoded := raw.UnmarshalJSON(frame.payload) == nil
			if decoded && isStdioNotification(&raw) {
				handleStdioNotification(&raw, cancels)
				continue
			}
			var rawPtr *jsonrpc.RawRequest
			if decoded {
				rawPtr = &raw
			}
			reqCtx := ctx
			var (
				idKey  string
				seq    uint64
				cancel context.CancelFunc
			)
			if rawPtr != nil && !rawPtr.Invalid && rawPtr.HasID && rawPtr.ID != nil {
				if idKey = canonicalStdioIDKey(rawPtr.ID); idKey != "" {
					// Register cancellation synchronously before launching the goroutine so
					// an immediately following notifications/cancelled frame never races it.
					reqCtx, cancel = context.WithCancel(ctx)
					seq = cancels.track(idKey, cancel)
				}
			}
			wg.Add(1)
			go dispatchStdioFrame(
				reqCtx,
				&wg,
				writer,
				cancels,
				idKey,
				seq,
				cancel,
				dispatch,
				frame.payload,
				rawPtr,
			)
		}
	}

	wg.Wait()
	if readErr != nil {
		return fmt.Errorf("read stdio input: %w", readErr)
	}
	return writer.firstErr()
}

// readStdioFrames streams non-empty trimmed lines from in onto frames until
// io.EOF, a read error, or ctx cancellation, then closes frames.
func readStdioFrames(ctx context.Context, in io.Reader, frames chan<- stdioFrame) {
	defer close(frames)
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			select {
			case frames <- stdioFrame{payload: bytes.Clone(trimmed)}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				select {
				case frames <- stdioFrame{err: err}:
				case <-ctx.Done():
				}
			}
			return
		}
	}
}

// isStdioNotification reports whether raw is a valid JSON-RPC 2.0 notification
// (omitting "id") which per JSON-RPC 2.0 section 4.1 must never receive a reply.
func isStdioNotification(raw *jsonrpc.RawRequest) bool {
	return !raw.Invalid &&
		raw.JSONRPC == "2.0" &&
		raw.HasMethod &&
		raw.Method != "" &&
		!raw.HasID
}

// handleStdioNotification processes MCP notifications on the stdio transport.
// notifications/cancelled aborts the matching in-flight request context; all
// other valid notifications are acknowledged silently with no stdout frame.
func handleStdioNotification(raw *jsonrpc.RawRequest, cancels *stdioCancels) {
	if raw.Method != "notifications/cancelled" || len(raw.Params) == 0 {
		return
	}
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(raw.Params, &params) != nil || len(params.RequestID) == 0 {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(params.RequestID))
	dec.UseNumber()
	var id any
	if dec.Decode(&id) != nil {
		return
	}
	if key := canonicalStdioIDKey(id); key != "" {
		cancels.cancel(key)
	}
}

// dispatchStdioFrame executes one JSON-RPC request frame and writes its
// compacted response line to writer unless the request was cancelled.
func dispatchStdioFrame(
	reqCtx context.Context,
	wg *sync.WaitGroup,
	writer *stdioWriter,
	cancels *stdioCancels,
	idKey string,
	seq uint64,
	cancel context.CancelFunc,
	dispatch stdioDispatcher,
	payload []byte,
	raw *jsonrpc.RawRequest,
) {
	defer wg.Done()
	if cancel != nil {
		defer func() {
			cancels.release(idKey, seq)
			cancel()
		}()
	}

	resp, err := dispatch(reqCtx, payload, raw)
	if err != nil {
		if reqCtx.Err() != nil {
			return
		}
		resp = stdioInternalErrorResponse(raw, err)
	}
	if reqCtx.Err() != nil {
		return
	}
	respLine := compactStdioJSON(resp)
	if len(respLine) == 0 {
		return
	}
	writer.writeLine(respLine)
}

// validateStdioHTTPResponse verifies that an HTTP response from either the
// relayed server or the in-process handler carries a valid JSON-RPC body.
// Valid notifications are handled before dispatch, so every dispatched frame
// expects a JSON-RPC response and empty or 202 replies fail fast.
func validateStdioHTTPResponse(status int, body []byte, target string) ([]byte, error) {
	if status == http.StatusAccepted {
		return nil, fmt.Errorf("unexpected HTTP %d response from %s", status, target)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("unexpected empty HTTP %d response from %s", status, target)
	}
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf(
			"unexpected HTTP %d non-JSON response from %s: %s",
			status,
			target,
			string(trimmed),
		)
	}
	return trimmed, nil
}

// newStdioMCPRequest constructs an HTTP POST request for payload. When params
// carry _meta["io.modelcontextprotocol/protocolVersion"], it populates
// MCP-Protocol-Version, Mcp-Method, and Mcp-Name headers for MCP 2026-07-28;
// otherwise it attaches mcpruntime.LegacyProtocolVersion ("2025-11-25") on
// post-initialize requests so Goa-AI's legacy JSON-RPC path handles stdio
// initialize, ping, tools/list, and tools/call natively.
func newStdioMCPRequest(
	ctx context.Context,
	targetURL string,
	payload []byte,
	raw *jsonrpc.RawRequest,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if raw != nil && !raw.Invalid {
		var params struct {
			Name json.RawMessage `json:"name"`
			Meta map[string]any  `json:"_meta"`
		}
		if len(raw.Params) > 0 && json.Unmarshal(raw.Params, &params) == nil {
			if proto, ok := params.Meta["io.modelcontextprotocol/protocolVersion"].(string); ok && proto != "" {
				req.Header.Set("MCP-Protocol-Version", proto)
				if raw.Method != "" {
					req.Header.Set("Mcp-Method", raw.Method)
				}
				if raw.Method == "tools/call" && len(params.Name) > 0 && !bytes.Equal(bytes.TrimSpace(params.Name), []byte("null")) {
					var name string
					if json.Unmarshal(params.Name, &name) == nil {
						req.Header.Set("Mcp-Name", encodeMCPHeaderValue(name))
					}
				}
				return req, nil
			}
		}
		if raw.Method == "initialize" || !raw.HasID {
			return req, nil
		}
	}
	req.Header.Set("MCP-Protocol-Version", mcpruntime.LegacyProtocolVersion)
	return req, nil
}

// encodeMCPHeaderValue mirrors MCP 2026-07-28 HTTP header encoding so values
// with leading/trailing whitespace, non-ASCII/control bytes, or the sentinel
// prefix are transported inside a standard-base64 envelope.
func encodeMCPHeaderValue(value string) string {
	unsafe := strings.HasPrefix(value, "=?base64?") || strings.TrimSpace(value) != value
	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e {
			unsafe = true
			break
		}
	}
	if unsafe {
		return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	}
	return value
}

// canonicalStdioIDKey encodes a decoded JSON-RPC request id into a canonical
// JSON map key (normalizing string escapes while keeping numbers distinct from
// strings), returning "" for nil.
func canonicalStdioIDKey(id any) string {
	if id == nil {
		return ""
	}
	encoded, err := json.Marshal(id)
	if err != nil || bytes.Equal(encoded, []byte("null")) {
		return ""
	}
	return string(encoded)
}

// compactStdioJSON removes embedded newlines from a JSON-RPC response so every
// stdio frame stays strictly one line.
func compactStdioJSON(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil
	}
	return compact.Bytes()
}

// stdioInternalErrorResponse builds a JSON-RPC -32603 internal error preserving
// the request id when present (or null otherwise) so callers never hang on
// transport failures or empty upstream responses.
func stdioInternalErrorResponse(raw *jsonrpc.RawRequest, err error) []byte {
	var id any
	if raw != nil && raw.HasID {
		id = raw.ID
	}
	out, marshalErr := json.Marshal(jsonrpc.MakeErrorResponse(id, jsonrpc.InternalError, err.Error(), nil))
	if marshalErr != nil {
		return nil
	}
	return out
}

// writeLine writes one newline-terminated frame to out while recording the
// first write error.
func (w *stdioWriter) writeLine(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	if _, err := w.out.Write(append(line, '\n')); err != nil {
		w.err = err
	}
}

// firstErr returns the first write error encountered by the writer, if any.
func (w *stdioWriter) firstErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// track registers cancel for an in-flight request identified by idKey and
// returns its monotonic registration sequence.
func (c *stdioCancels) track(idKey string, cancel context.CancelFunc) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.active[idKey] = stdioCancelEntry{seq: c.next, cancel: cancel}
	return c.next
}

// cancel aborts the in-flight request matching idKey when present.
func (c *stdioCancels) cancel(idKey string) {
	c.mu.Lock()
	entry, ok := c.active[idKey]
	if ok {
		delete(c.active, idKey)
	}
	c.mu.Unlock()
	if ok {
		entry.cancel()
	}
}

// release removes the active cancel entry for idKey when its sequence still
// matches seq.
func (c *stdioCancels) release(idKey string, seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.active[idKey]; ok && entry.seq == seq {
		delete(c.active, idKey)
	}
}
