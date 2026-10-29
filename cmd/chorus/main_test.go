package main

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestLoopbackPort verifies detection of loopback host:port addresses.
func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		addr string
		port string
		ok   bool
	}{
		{addr: "localhost:6379", port: "6379", ok: true},
		{addr: "127.0.0.1:6380", port: "6380", ok: true},
		{addr: "[::1]:6381", port: "6381", ok: true},
		{addr: ":6379", port: "6379", ok: true},
		{addr: "redis.internal:6379", port: "", ok: false},
		{addr: "10.0.0.5:6379", port: "", ok: false},
		{addr: "invalid", port: "", ok: false},
	}
	for _, tc := range cases {
		port, ok := loopbackPort(tc.addr)
		if ok != tc.ok || port != tc.port {
			t.Fatalf("loopbackPort(%q) = (%q, %v), want (%q, %v)", tc.addr, port, ok, tc.port, tc.ok)
		}
	}
}

// TestConnectOrStartRedis verifies that connectOrStartRedis automatically boots
// a local Redis instance when targeting an unused loopback port and cleans it
// up when the returned stop function is called.
func TestConnectOrStartRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer func() {
		_ = rdb.Close()
	}()

	stopFn, err := connectOrStartRedis(context.Background(), rdb, addr)
	if err != nil {
		t.Fatalf("connectOrStartRedis(%q) failed: %v", addr, err)
	}
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		stopFn()
		t.Fatalf("expected auto-started Redis at %s to respond to PING: %v", addr, err)
	}

	stopFn()
	if err := pingRedis(context.Background(), rdb, initialPingTimeout); err == nil {
		t.Fatalf("expected auto-started Redis at %s to stop after stopFn()", addr)
	}
}
