// Command chorus runs the Chorus multi-agent coordination MCP server backed by
// Pulse and Redis.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/goadesign/chorus/skills"
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
		// installSkill installs the embedded Chorus SKILL.md into global agent skill directories and exits.
		installSkill bool
	}
)

const (
	// defaultListenAddr is the default HTTP listen address.
	defaultListenAddr = ":8080"
	// defaultRedisAddr is the default Redis server address.
	defaultRedisAddr = "localhost:6379"
	// defaultSwarmTTL is the default sliding TTL for swarm state and streams.
	defaultSwarmTTL = 24 * time.Hour
	// defaultStreamMaxLen is the default maximum retained messages per stream.
	defaultStreamMaxLen = 500
	// defaultOnlineWindow is the default online activity window.
	defaultOnlineWindow = 2 * time.Minute
	// startupPingTimeout bounds the initial Redis connectivity check.
	startupPingTimeout = 5 * time.Second
	// shutdownTimeout bounds graceful HTTP and Pulse shutdown.
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("chorus server exited with error", "err", err)
		os.Exit(1)
	}
}

// run parses command-line flags, connects to Redis, and serves the Chorus MCP
// endpoint until interrupted.
func run() error {
	cfg := parseFlags()
	if cfg.installSkill {
		paths, err := skills.Install("")
		if err != nil {
			return err
		}
		for _, p := range paths {
			fmt.Printf("Installed Chorus skill to %s\n", p)
		}
		return nil
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.redisAddr,
		Password: cfg.redisPassword,
		DB:       cfg.redisDB,
	})
	defer func() {
		_ = rdb.Close()
	}()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), startupPingTimeout)
	err := rdb.Ping(pingCtx).Err()
	cancelPing()
	if err != nil {
		return fmt.Errorf("connect to redis at %s: %w", cfg.redisAddr, err)
	}

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
		slog.Info("chorus MCP server listening", "addr", cfg.addr, "endpoint", "/mcp/{swarm}/{agent}", "redis", cfg.redisAddr)
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

// parseFlags parses CLI flags and environment variable defaults into serverConfig.
func parseFlags() serverConfig {
	var cfg serverConfig
	defaultRedis := os.Getenv("REDIS_ADDR")
	if defaultRedis == "" {
		defaultRedis = defaultRedisAddr
	}
	flag.StringVar(&cfg.addr, "addr", defaultListenAddr, "HTTP listen address")
	flag.StringVar(&cfg.redisAddr, "redis-addr", defaultRedis, "Redis server address (host:port)")
	flag.StringVar(&cfg.redisPassword, "redis-password", os.Getenv("REDIS_PASSWORD"), "Redis password")
	flag.IntVar(&cfg.redisDB, "redis-db", 0, "Redis database index")
	flag.DurationVar(&cfg.swarmTTL, "swarm-ttl", defaultSwarmTTL, "Sliding TTL for swarm state and streams")
	flag.IntVar(&cfg.streamMaxLen, "stream-max-len", defaultStreamMaxLen, "Maximum retained messages per channel or DM stream")
	flag.DurationVar(&cfg.onlineWindow, "online-window", defaultOnlineWindow, "Activity window for marking agents [online]")
	flag.StringVar(&cfg.origins, "origins", "", "Comma-separated list of allowed HTTP Origin header values")
	flag.BoolVar(&cfg.installSkill, "install-skill", false, "Install the embedded Chorus agent skill into global skill directories and exit")
	flag.Parse()
	return cfg
}
