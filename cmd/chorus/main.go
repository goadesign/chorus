// Command chorus runs and manages the Chorus multi-agent coordination MCP
// server backed by Pulse and Redis.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/redis/go-redis/v9"
)

type (
	// serverConfig holds the CLI configuration for the Chorus HTTP server.
	serverConfig struct {
		// addr is the TCP listen address for the MCP HTTP server.
		addr string
		// redisAddr is the host:port of the backing Redis instance.
		redisAddr string
		// redisPassword is the optional password used to authenticate with Redis.
		redisPassword string
		// redisDB is the Redis database index.
		redisDB int
		// swarmTTL is the sliding TTL applied to swarm replicated maps and streams.
		swarmTTL time.Duration
		// streamMaxLen is the maximum number of messages retained per stream.
		streamMaxLen int
		// onlineWindow is the duration within which an active agent is marked online.
		onlineWindow time.Duration
		// origins is a comma-separated list of allowed Origin header values.
		origins string
	}
)

const (
	// defaultListenAddr is the default HTTP listen address (9440 references A440,
	// the 440 Hz concert pitch standard used to tune an ensemble).
	defaultListenAddr = ":9440"
	// defaultRedisAddr is the default Redis server address.
	defaultRedisAddr = "localhost:6379"
	// defaultSwarmTTL is the default sliding TTL for swarm state and streams.
	defaultSwarmTTL = 24 * time.Hour
	// defaultStreamMaxLen is the default maximum retained messages per stream.
	defaultStreamMaxLen = 500
	// defaultOnlineWindow is the default online activity window.
	defaultOnlineWindow = 2 * time.Minute
	// initialPingTimeout bounds the fast check for an already-running Redis instance.
	initialPingTimeout = 500 * time.Millisecond
	// autoStartTimeout bounds waiting for an auto-started local Redis or Docker container.
	autoStartTimeout = 15 * time.Second
	// shutdownTimeout bounds graceful HTTP and Pulse shutdown.
	shutdownTimeout = 10 * time.Second
	// dockerContainerName is the container name prefix used when auto-starting Redis via Docker.
	dockerContainerName = "chorus-redis"
	// dockerRedisImage is the Redis image used when auto-starting Redis via Docker.
	dockerRedisImage = "redis:7-alpine"
)

// main is the CLI entrypoint for the chorus command.
func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "chorus: %v\n", err)
		os.Exit(1)
	}
}

// dispatch routes CLI subcommands ("up", "down", "status", "setup", "serve")
// or defaults to running the foreground server when invoked with flags or no arguments.
func dispatch(args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "up":
			return runUp(args[1:])
		case "down":
			return runDown(args[1:])
		case "status":
			return runStatus(args[1:])
		case "setup":
			return runSetup(args[1:])
		case "serve":
			return runServe(args[1:])
		default:
			return fmt.Errorf("unknown subcommand %q (expected: up, down, status, setup, serve)", args[0])
		}
	}
	return runServe(args)
}

// runServe parses server flags, connects to (or auto-starts) Redis, and serves
// the Chorus MCP endpoint until interrupted.
func runServe(args []string) error {
	cfg, err := parseServerFlags(args)
	if err != nil {
		return err
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.redisAddr,
		Password: cfg.redisPassword,
		DB:       cfg.redisDB,
	})
	defer func() {
		_ = rdb.Close()
	}()

	stopRedis, err := connectOrStartRedis(context.Background(), rdb, cfg.redisAddr)
	if err != nil {
		return err
	}
	defer stopRedis()

	svc, err := chorus.New(
		rdb,
		chorus.WithSwarmTTL(cfg.swarmTTL),
		chorus.WithStreamMaxLen(cfg.streamMaxLen),
		chorus.WithOnlineWindow(cfg.onlineWindow),
	)
	if err != nil {
		return fmt.Errorf("create chorus service: %w", err)
	}
	defer func() {
		_ = svc.Close()
	}()

	var origins []string
	for _, o := range strings.Split(cfg.origins, ",") {
		trimmed := strings.TrimSpace(o)
		if trimmed != "" {
			origins = append(origins, trimmed)
		}
	}

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           chorus.NewHTTPHandler(svc, origins...),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("chorus MCP server listening", "addr", cfg.addr, "endpoint", "/mcp", "redis", cfg.redisAddr)
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-sigCtx.Done():
		slog.Info("shutting down chorus MCP server")
		shutCtx, cancelShut := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShut()
		if shutErr := httpServer.Shutdown(shutCtx); shutErr != nil {
			return fmt.Errorf("shutdown http server: %w", shutErr)
		}
		return <-errCh
	}
}

// parseServerFlags parses CLI flags and environment variable defaults into serverConfig.
func parseServerFlags(args []string) (serverConfig, error) {
	var cfg serverConfig
	defaultRedis := os.Getenv("REDIS_ADDR")
	if defaultRedis == "" {
		defaultRedis = defaultRedisAddr
	}
	fs := flag.NewFlagSet("chorus", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", defaultListenAddr, "HTTP listen address")
	fs.StringVar(&cfg.redisAddr, "redis-addr", defaultRedis, "Redis server address (host:port)")
	fs.StringVar(&cfg.redisPassword, "redis-password", os.Getenv("REDIS_PASSWORD"), "Redis password")
	fs.IntVar(&cfg.redisDB, "redis-db", 0, "Redis database index")
	fs.DurationVar(&cfg.swarmTTL, "swarm-ttl", defaultSwarmTTL, "Sliding TTL for swarm state and streams")
	fs.IntVar(&cfg.streamMaxLen, "stream-max-len", defaultStreamMaxLen, "Maximum retained messages per channel or DM stream")
	fs.DurationVar(&cfg.onlineWindow, "online-window", defaultOnlineWindow, "Activity window for marking agents [online]")
	fs.StringVar(&cfg.origins, "origins", "", "Comma-separated list of allowed HTTP Origin header values")
	if err := fs.Parse(args); err != nil {
		return serverConfig{}, err
	}
	return cfg, nil
}

// connectOrStartRedis verifies connectivity to rdb at redisAddr. When redisAddr
// targets a local loopback port and no server is listening, it automatically
// launches a managed redis-server subprocess or Docker container.
func connectOrStartRedis(ctx context.Context, rdb *redis.Client, redisAddr string) (func(), error) {
	noop := func() {}
	if err := pingRedis(ctx, rdb, initialPingTimeout); err == nil {
		return noop, nil
	}

	port, ok := loopbackPort(redisAddr)
	if !ok {
		return noop, fmt.Errorf("connect to redis at %s: server unreachable", redisAddr)
	}

	stopFn, startErr := startLocalRedis(port)
	if startErr != nil {
		return noop, fmt.Errorf(
			"redis is not running at %s and could not be auto-started (%w); install Redis with 'brew install redis', 'sudo apt-get install -y redis-server', or start Docker",
			redisAddr,
			startErr,
		)
	}

	deadline := time.Now().Add(autoStartTimeout)
	for time.Now().Before(deadline) {
		if err := pingRedis(ctx, rdb, initialPingTimeout); err == nil {
			return stopFn, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopFn()
	return noop, fmt.Errorf("timed out waiting for auto-started redis at %s", redisAddr)
}

// pingRedis pings rdb with a bounded timeout.
func pingRedis(ctx context.Context, rdb *redis.Client, timeout time.Duration) error {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return rdb.Ping(pingCtx).Err()
}

// loopbackPort extracts the TCP port if addr is a local loopback address
// (localhost, 127.0.0.1, or ::1).
func loopbackPort(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", false
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return port, true
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return port, true
}

// startLocalRedis starts a local Redis instance on port using either a local
// redis-server binary or a Docker container, returning a cleanup function.
func startLocalRedis(port string) (func(), error) {
	if redisBin, err := exec.LookPath("redis-server"); err == nil {
		return startRedisSubprocess(redisBin, port)
	}
	if dockerBin, err := exec.LookPath("docker"); err == nil {
		return startRedisDocker(dockerBin, port)
	}
	return nil, errors.New("neither redis-server nor docker found in PATH")
}

// startRedisSubprocess launches an ephemeral redis-server child process bound
// to 127.0.0.1:port.
func startRedisSubprocess(redisBin, port string) (func(), error) {
	tmpDir, err := os.MkdirTemp("", "chorus-redis-*")
	if err != nil {
		return nil, fmt.Errorf("create redis temp dir: %w", err)
	}
	cmd := exec.Command(
		redisBin,
		"--bind", "127.0.0.1",
		"--port", port,
		"--dir", tmpDir,
		"--save", "",
		"--appendonly", "no",
		"--loglevel", "warning",
	)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("start redis-server: %w", err)
	}
	slog.Info("auto-started local redis-server subprocess", "addr", "127.0.0.1:"+port, "pid", cmd.Process.Pid)
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_, _ = cmd.Process.Wait()
		}
		_ = os.RemoveAll(tmpDir)
	}, nil
}

// startRedisDocker launches an ephemeral redis:7-alpine container bound to
// 127.0.0.1:port via Docker.
func startRedisDocker(dockerBin, port string) (func(), error) {
	container := fmt.Sprintf("%s-%s", dockerContainerName, port)
	_ = exec.Command(dockerBin, "rm", "-f", container).Run()
	out, err := exec.Command(
		dockerBin,
		"run", "-d", "--rm",
		"--name", container,
		"-p", fmt.Sprintf("127.0.0.1:%s:6379", port),
		dockerRedisImage,
		"redis-server", "--save", "", "--appendonly", "no",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %w (%s)", dockerRedisImage, err, strings.TrimSpace(string(out)))
	}
	slog.Info("auto-started redis Docker container", "container", container, "addr", "127.0.0.1:"+port)
	return func() {
		_ = exec.Command(dockerBin, "stop", container).Run()
	}, nil
}
