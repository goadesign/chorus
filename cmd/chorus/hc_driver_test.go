package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/goadesign/chorus/chorus/hcdriver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHCWake exercises the installed hc's real event stream
// and PTY input path. Its state, config, socket and terminal are disposable.
func TestHCWake(t *testing.T) {
	program, err := exec.LookPath("hc")
	if err != nil {
		t.Skip("hc is not installed")
	}
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HC_SOCKET", filepath.Join(root, "s"))
	t.Setenv("HC_SESSION", "test")
	t.Setenv("HC_TERMINAL_ID", "")
	t.Setenv("HC_SETTLE_MS", "0")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, program, "server", "start").CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		output, err := exec.CommandContext(cleanup, program, "server", "stop").CombinedOutput()
		assert.NoError(t, err, "%s", output)
	})
	const terminal = "chorus-guard-test"
	received := filepath.Join(root, "received")
	const script = `stty -echo; printf '\033]7501;state=idle\033\\'; IFS= read -r message; printf '%s' "$message" > "$1"; printf '\033]7501;state=working\033\\'`
	output, err = exec.CommandContext(ctx, program, "run", "--bg", "--name", terminal,
		"--label", "chorus.swarm=demo", "--label", "chorus.handle=worker", "--no-parent",
		"--", "sh", "-c", script, "sh", received).CombinedOutput()
	require.NoError(t, err, "%s", output)

	hc := hcConnect{binary: program}
	updates := make(chan hcdriver.Update)
	done := make(chan error, 1)
	go func() {
		done <- hc.Subscribe(ctx, func(update hcdriver.Update) {
			select {
			case updates <- update:
			case <-ctx.Done():
			}
		})
	}()
	defer func() {
		cancel()
		<-done
	}()
	address := waitHCTerminal(t, ctx, updates)
	assert.ErrorIs(t, hc.Send(ctx, address, "working", "must never appear"), hcdriver.ErrStatusChanged)
	const wake = "[Chorus] Read your messages."
	require.NoError(t, hc.Send(ctx, address, "idle", wake))
	data, err := os.ReadFile(received)
	require.NoError(t, err)
	assert.Equal(t, wake, string(data))
}

// TestHCSendDistinguishesLostReply checks that only an error before request
// submission is eligible for another try in the same turn.
func TestHCSendDistinguishesLostReply(t *testing.T) {
	root := t.TempDir()
	ctx := t.Context()
	missing := hcConnect{binary: filepath.Join(root, "missing")}
	assert.ErrorIs(t, missing.Send(ctx, "t1", "idle", "read"), hcdriver.ErrWakeNotSubmitted)
	program := filepath.Join(root, "accept-then-close")
	require.NoError(t, os.WriteFile(program, []byte("#!/bin/sh\nIFS= read -r request\n"), 0o755))
	err := (hcConnect{binary: program}).Send(ctx, "t1", "idle", "read")
	require.Error(t, err)
	assert.NotErrorIs(t, err, hcdriver.ErrWakeNotSubmitted)
}

// TestHCMissedEventsReplaceTerminalFacts checks the delta owning code without
// launching a process just to deliver JSON packets.
func TestHCMissedEventsReplaceTerminalFacts(t *testing.T) {
	wake := hcdriver.Terminal{Address: "t7", Status: &hcdriver.Status{State: "idle"}}
	terminals := map[string]hcdriver.Terminal{"t7": wake}
	var update hcdriver.Update
	emit := func(u hcdriver.Update) { update = u }
	loadHCSnapshot(hcSnapshot{Terminals: []hcdriver.Terminal{{Address: "t9"}}}, terminals, emit)
	assert.True(t, update.Snapshot)
	assert.NotContains(t, terminals, "t7")
	require.NoError(t, applyHCEvent(terminals, hcEvent{
		Terminal: "t9", Type: "status", Data: json.RawMessage(`{"status":{"state":"done"}}`),
	}, emit))
	require.Len(t, update.Terminals, 1)
	assert.Equal(t, "done", update.Terminals[0].Status.State)
	require.NoError(t, applyHCEvent(terminals, hcEvent{Terminal: "t9", Type: "removed"}, emit))
	assert.Empty(t, terminals)
}

// waitHCTerminal reads the snapshot or the live notification for the idle PTY.
func waitHCTerminal(t *testing.T, ctx context.Context, updates <-chan hcdriver.Update) string {
	t.Helper()
	for {
		select {
		case update := <-updates:
			for _, term := range update.Terminals {
				if term.Labels["chorus.handle"] == "worker" && term.Status != nil && term.Status.State == "idle" {
					return term.Address
				}
			}
		case <-ctx.Done():
			t.Fatal("no idle terminal in the hc event stream")
			return ""
		}
	}
}
