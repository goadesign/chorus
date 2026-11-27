package hcdriver

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuardedWake checks that retries never drop the status guard and that
// one read prompt consumes a turn's opportunity to wake on the same mail.
func TestGuardedWake(t *testing.T) {
	terminal := trackedTerminal()
	terminal.Status = &Status{State: "done"}
	bus := &testBus{sequences: map[string]int{"@guide": 1}}
	hc := &testHC{sendErr: ErrGuardUnsupported}
	d := &driver{bus: bus, hc: hc, wakes: make(map[string]delivery)}
	ctx := context.Background()
	attemptWake(ctx, d, terminal)
	attemptWake(ctx, d, terminal)
	require.Equal(t, []string{"done"}, hc.guards)

	hc.sendErr = ErrStatusChanged
	terminal.Status = &Status{State: "idle"}
	attemptWake(ctx, d, terminal)
	hc.sendErr = nil
	attemptWake(ctx, d, terminal)
	require.Equal(t, []string{"done", "idle", "idle"}, hc.guards)
	assert.Contains(t, hc.text, "swarm review for @guide")
	assert.Contains(t, hc.text, "agent \"guide\"")
	assert.Equal(t, terminal.Address, hc.address)

	terminal.Turn++
	attemptWake(ctx, d, terminal)
	assert.Len(t, hc.guards, 3, "stale unread mail must not start an automatic loop")
	bus.sequences = map[string]int{"#review": 1}
	terminal.Status = &Status{State: "working"}
	attemptWake(ctx, d, terminal)
	assert.Len(t, hc.guards, 3)
	terminal.Status = &Status{State: "idle"}
	attemptWake(ctx, d, terminal)
	attemptWake(ctx, d, terminal)
	assert.Equal(t, []string{"done", "idle", "idle", "idle"}, hc.guards)

	terminal.Turn++
	terminal.Status = &Status{State: "working"}
	bus.sequences = nil
	attemptWake(ctx, d, terminal)
	terminal.Status = &Status{State: "idle"}
	bus.sequences = map[string]int{"#review": 1}
	hc.sendErr = ErrWakeNotSubmitted
	attemptWake(ctx, d, terminal)
	hc.sendErr = errors.New("reply lost after submission")
	attemptWake(ctx, d, terminal)
	hc.sendErr = nil
	attemptWake(ctx, d, terminal)
	assert.Equal(t, []string{"done", "idle", "idle", "idle", "idle", "idle"}, hc.guards)
}

// attemptWake applies one driver decision and completes it synchronously.
func attemptWake(ctx context.Context, d *driver, term Terminal) {
	attempt := d.planWake(ctx, term)
	if attempt == nil {
		return
	}
	sendCtx, cancel := context.WithCancel(ctx)
	attempt.cancel = cancel
	d.wakes[term.Address] = attempt.current
	results := make(chan wakeResult, 1)
	d.sendWake(sendCtx, attempt, results)
	d.finishWake(<-results)
}

// TestSessionRefreshesPresentTerminal runs the injected subscription and
// drives the timer directly, so no real time needs to pass for a heartbeat.
func TestSessionRefreshesPresentTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	updates := make(chan Update, 1)
	bus := &testBus{touches: make(chan string, 3)}
	hc := &testHC{updates: updates}
	d := &driver{bus: bus, hc: hc, wakes: make(map[string]delivery)}
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- d.runSession(ctx, ticks) }()
	terminal := trackedTerminal()
	updates <- Update{Snapshot: true, Terminals: []Terminal{terminal}}
	select {
	case target := <-bus.touches:
		assert.Equal(t, "review/guide", target)
	case <-ctx.Done():
		t.Fatal("terminal was never registered with the driver")
	}
	select {
	case ticks <- time.Now():
	case <-ctx.Done():
		t.Fatal("presence timer was never consumed")
	}
	select {
	case target := <-bus.touches:
		assert.Equal(t, "review/guide", target)
	case <-ctx.Done():
		t.Fatal("presence timer did not refresh the terminal")
	}
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

// TestDraftWaitDoesNotBlockOtherTerminals holds one send open like a draft in
// hc. Two terminals can submit and both keep refreshing their presence.
func TestDraftWaitDoesNotBlockOtherTerminals(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	updates := make(chan Update, 1)
	started := make(chan string, 2)
	bus := &testBus{sequences: map[string]int{"#review": 1}, touches: make(chan string, 4)}
	hc := &testHC{updates: updates, send: func(ctx context.Context, address, _ string) error {
		select {
		case started <- address:
		case <-ctx.Done():
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	d := &driver{bus: bus, hc: hc, wakes: make(map[string]delivery)}
	ticks, done := make(chan time.Time), make(chan error, 1)
	go func() { done <- d.runSession(ctx, ticks) }()
	first, second := trackedTerminal(), trackedTerminal()
	first.Status = &Status{State: "idle"}
	second.Address, second.Status = "t2", &Status{State: "done"}
	second.Labels["chorus.handle"] = "builder"
	updates <- Update{Snapshot: true, Terminals: []Terminal{first, second}}
	startedTerminals := make([]string, 0, 2)
	for range 2 {
		select {
		case address := <-started:
			startedTerminals = append(startedTerminals, address)
		case <-ctx.Done():
			t.Fatal("one terminal's send blocked its sibling")
		}
	}
	assert.ElementsMatch(t, []string{"t1", "t2"}, startedTerminals)
	select {
	case ticks <- time.Now():
	case <-ctx.Done():
		t.Fatal("the send blocked the presence timer")
	}
	touches := make([]string, 0, 4)
	for range 4 {
		select {
		case target := <-bus.touches:
			touches = append(touches, target)
		case <-ctx.Done():
			t.Fatal("the send delayed a presence refresh")
		}
	}
	assert.ElementsMatch(t, []string{"review/guide", "review/builder", "review/guide", "review/builder"}, touches)
	assert.Empty(t, started, "only one send can be in flight per terminal")
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

// TestWakeReplansAfterStatusChanges queues an idle report before returning a
// refusal from a done send. The next send must use idle without a timer tick.
func TestWakeReplansAfterStatusChanges(t *testing.T) {
	for _, failure := range []error{ErrStatusChanged, ErrGuardUnsupported} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			updates, observed := make(chan Update, 2), make(chan struct{}, 2)
			guards, returnDone := make(chan string, 2), make(chan struct{})
			bus := &testBus{sequences: map[string]int{"#review": 1}, touches: make(chan string, 2)}
			hc := &testHC{updates: updates, observed: observed, send: func(ctx context.Context, _, guard string) error {
				select {
				case guards <- guard:
				case <-ctx.Done():
					return ctx.Err()
				}
				if guard == "done" {
					select {
					case <-returnDone:
						return failure
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}}
			d := &driver{bus: bus, hc: hc, wakes: make(map[string]delivery)}
			done := make(chan error, 1)
			go func() { done <- d.runSession(ctx, nil) }()
			term := trackedTerminal()
			term.Status = &Status{State: "done"}
			updates <- Update{Snapshot: true, Terminals: []Terminal{term}}
			receiveObserved(t, ctx, observed)
			require.Equal(t, "done", receiveGuard(t, ctx, guards))
			term.Status = &Status{State: "idle"}
			updates <- Update{Terminals: []Terminal{term}}
			receiveObserved(t, ctx, observed)
			close(returnDone)
			assert.Equal(t, "idle", receiveGuard(t, ctx, guards))
			cancel()
			assert.ErrorIs(t, <-done, context.Canceled)
		})
	}
}

// TestWakeStateAcrossReconnect checks both the lost session's pending outcome
// and the first replacement snapshot's terminal identity.
func TestWakeStateAcrossReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	updates, started := make(chan Update, 1), make(chan string, 1)
	bus := &testBus{sequences: map[string]int{"#review": 2}, touches: make(chan string, 2)}
	hc := &testHC{updates: updates, send: func(ctx context.Context, _, guard string) error {
		select {
		case started <- guard:
		case <-ctx.Done():
		}
		<-ctx.Done()
		return ErrWakeNotSubmitted
	}}
	term := trackedTerminal()
	term.Status = &Status{State: "idle"}
	previous := delivery{owner: terminalIdentity(term), turn: 1, sequences: map[string]int{"#review": 1}}
	d := &driver{bus: bus, hc: hc, wakes: map[string]delivery{term.Address: previous}}
	done := make(chan error, 1)
	go func() { done <- d.runSession(ctx, nil) }()
	updates <- Update{Snapshot: true, Terminals: []Terminal{term}}
	require.Equal(t, "idle", receiveGuard(t, ctx, started))
	close(updates)
	select {
	case err := <-done:
		assert.ErrorIs(t, err, io.EOF)
	case <-ctx.Done():
		t.Fatal("session did not finish the unsubmitted wake")
	}
	assert.Equal(t, previous, d.wakes[term.Address])
	newPID := *term.PID + 1
	term.PID = &newPID
	d.applyUpdate(ctx, map[string]Terminal{}, Update{Snapshot: true, Terminals: []Terminal{term}})
	assert.NotContains(t, d.wakes, term.Address)
}

// receiveObserved waits for a test update to enter the session loop.
func receiveObserved(t *testing.T, ctx context.Context, observed <-chan struct{}) {
	t.Helper()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("terminal update did not reach the driver")
	}
}

// receiveGuard reads the exact status that the driver passes to hc.
func receiveGuard(t *testing.T, ctx context.Context, guards <-chan string) string {
	t.Helper()
	select {
	case guard := <-guards:
		return guard
	case <-ctx.Done():
		t.Fatal("the driver did not send the wake")
		return ""
	}
}

// TestRoleRunning protects the shared login-shell predicate.
func TestRoleRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{"unreported", nil, true},
		{"login shell", []string{"-/bin/zsh"}, false},
		{"environment shell", []string{"/opt/bin/myshell", "-l"}, false},
		{"shell command", []string{"/bin/zsh", "-lic", "exec codex"}, true},
		{"second shell flag", []string{"bash", "-l", "-c", "exec codex"}, true},
		{"long flag", []string{"bash", "--norc"}, false},
		{"agent", []string{"codex"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := Terminal{State: "live", Foreground: &Foreground{Argv: tc.argv}}
			assert.Equal(t, tc.want, roleRunning(term, "myshell"))
			term.State = "finished"
			assert.False(t, roleRunning(term, "myshell"))
		})
	}
}

// trackedTerminal is a local, labeled agent with a running process.
func trackedTerminal() Terminal {
	pid := 100
	return Terminal{
		Address: "t1", PID: &pid, State: "live", Turn: 2,
		Labels: map[string]string{"chorus.swarm": "review", "chorus.handle": "guide"},
	}
}

// testHC supplies terminal facts and records guarded sends.
type testHC struct {
	updates  chan Update
	observed chan struct{}
	sendErr  error
	guards   []string
	text     string
	address  string
	send     func(context.Context, string, string) error
}

func (hc *testHC) Subscribe(ctx context.Context, emit func(Update)) error {
	for {
		select {
		case update, ok := <-hc.updates:
			if !ok {
				return io.EOF
			}
			emit(update)
			if hc.observed != nil {
				select {
				case hc.observed <- struct{}{}:
				case <-ctx.Done():
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (hc *testHC) Send(ctx context.Context, address, guard, text string) error {
	if hc.send != nil {
		return hc.send(ctx, address, guard)
	}
	hc.guards = append(hc.guards, guard)
	hc.address, hc.text = address, text
	return hc.sendErr
}

// testBus exposes one recipient's wake sequence and observes presence updates.
type testBus struct {
	sequences map[string]int
	touches   chan string
}

func (b *testBus) TouchAgent(ctx context.Context, swarm, handle string) error {
	select {
	case b.touches <- swarm + "/" + handle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *testBus) AgentWakeSequences(context.Context, string, string) (map[string]int, error) {
	return b.sequences, nil
}

func (b *testBus) WatchSwarm(ctx context.Context, _ string, changed func()) error {
	changed()
	<-ctx.Done()
	return ctx.Err()
}
