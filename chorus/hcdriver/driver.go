// Package hcdriver preserves the presence of terminal-backed Chorus agents
// and wakes them for unread messages at safe turn boundaries.
package hcdriver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type (
	// HCClient observes terminal events and submits a message only if the
	// terminal still reports the requested status. Subscribe begins with a
	// snapshot and emits updates until the context or connection ends.
	HCClient interface {
		Subscribe(ctx context.Context, emit func(Update)) error
		Send(ctx context.Context, address, status, message string) error
	}

	// Chorus supplies registered presence and wake information without opening
	// message history or consuming an agent's unread messages.
	Chorus interface {
		TouchAgent(ctx context.Context, swarm, handle string) error
		AgentWakeSequences(ctx context.Context, swarm, handle string) (map[string]int, error)
		WatchSwarm(ctx context.Context, swarm string, changed func()) error
	}

	// Terminal holds the hc terminal facts that affect presence and wake-ups.
	Terminal struct {
		// Address is the stable name hc accepts for a send.
		Address string `json:"address"`
		// State says whether this terminal is live or finished.
		State string `json:"state"`
		// PID is absent while a kept terminal has no running program.
		PID *int `json:"pid"`
		// Labels connect the terminal to a named Chorus participant.
		Labels map[string]string `json:"labels"`
		// Foreground identifies the process currently using the terminal.
		Foreground *Foreground `json:"foreground"`
		// Status is absent until the program reports its state.
		Status *Status `json:"status"`
		// Turn increases when the program completes a working turn.
		Turn uint64 `json:"turn"`
	}

	// Foreground describes the current foreground program's arguments.
	Foreground struct {
		Argv []string `json:"argv"`
	}

	// Status is the terminal program's reported OSC 7501 state.
	Status struct {
		State string `json:"state"`
	}

	// Update replaces the terminal snapshot or changes one terminal in it.
	// A terminal with just its address removes that terminal from consideration.
	Update struct {
		// Snapshot replaces the terminal set after startup or missed events.
		Snapshot bool
		// Terminals supplies the complete facts for the affected addresses.
		Terminals []Terminal
	}

	// driver runs one subscription at a time and remembers completed wake-ups
	// across reconnects for terminals still present in the new snapshot.
	driver struct {
		hc  HCClient
		bus Chorus
		// shell is the login executable used in the idle foreground check.
		shell string
		// wakes survives reconnection so lost replies cannot cause duplicates.
		wakes map[string]delivery
	}

	// delivery records the last submit attempt that may have typed.
	delivery struct {
		// owner disambiguates reused terminal addresses across snapshots.
		owner terminalOwner
		// turn is the reported turn when the request was sent.
		turn uint64
		// sequences are the unread messages that prompted the request.
		sequences map[string]int
		// unsupported records a definitely refused done guard on older hc.
		unsupported bool
	}

	// terminalOwner identifies the process labeled for a Chorus participant.
	terminalOwner struct {
		pid           int
		swarm, handle string
	}

	// wakeAttempt owns one request, including the previous delivery state
	// restored if hc definitely refused the request.
	wakeAttempt struct {
		// terminal is the process and status observed before the request.
		terminal Terminal
		// prior can be restored only after a confirmed refusal.
		prior *delivery
		// current records an attempt that might reach the terminal.
		current delivery
		// cancel stops the hc command when the terminal or session ends.
		cancel context.CancelFunc
		// retired means the terminal disappeared or changed its participant.
		retired bool
	}

	// wakeResult hands a completed request back to the session loop.
	wakeResult struct {
		attempt *wakeAttempt
		err     error
	}
)

const (
	// presenceInterval is the default period between presence refreshes.
	presenceInterval = time.Minute
	// retryInterval backs off after a lost subscription.
	retryInterval = time.Second
	// activityTimeout bounds a single Chorus state operation.
	activityTimeout = 12 * time.Second
	// sendTimeout covers hc's default 60s draft wait, 3s quiet period, and
	// 5s wait for the program to report its next state after submission.
	sendTimeout = 75 * time.Second
)

var (
	// ErrStatusChanged means the terminal could not be safely woken. No input
	// went into the terminal, so the driver can observe the next status event.
	ErrStatusChanged = errors.New("terminal status changed before wake")
	// ErrGuardUnsupported means the installed hc cannot guard this status.
	ErrGuardUnsupported = errors.New("terminal status guard is unsupported")
	// ErrWakeNotSubmitted means the adapter could not forward the command.
	ErrWakeNotSubmitted = errors.New("terminal wake was not submitted")

	// Chorus and terminal labels use the same spelling for each identifier.
	swarmName  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	handleName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

	// idleShells matches the launcher skill's idle-shell predicate.
	idleShells = []string{"sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh", "nu", "pwsh", "xonsh", "elvish"}
)

// Run reconnects to hc after a lost connection and continues observing until
// the owning Chorus server cancels ctx. The caller waits for Run to return
// before closing bus.
func Run(ctx context.Context, hc HCClient, bus Chorus, shell string, onlineWindow time.Duration) {
	d := &driver{hc: hc, bus: bus, shell: shellName(shell), wakes: make(map[string]delivery)}
	ticker := time.NewTicker(min(presenceInterval, max(time.Second, onlineWindow/2)))
	defer ticker.Stop()
	for ctx.Err() == nil {
		err := d.runSession(ctx, ticker.C)
		recordFailure(ctx, "subscribe", err)
		if !waitRetry(ctx) {
			return
		}
	}
}

// runSession owns the current hc connection, its tracked terminals, and one
// notification subscription for each swarm represented by those terminals.
func (d *driver) runSession(ctx context.Context, presence <-chan time.Time) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	inFlight := make(map[string]*wakeAttempt)
	results := make(chan wakeResult)
	defer func() {
		cancel()
		// Collect the outcomes before reconnecting. A confirmed refusal must
		// restore the previous state even when the subscription has ended.
		for range len(inFlight) {
			d.finishWake(<-results)
		}
		workers.Wait()
	}()
	changes := make(chan Update)
	ended := make(chan error, 1)
	workers.Go(func() { d.subscribe(ctx, changes, ended) })

	terminals := make(map[string]Terminal)
	swarms := make(map[string]context.CancelFunc)
	changed := make(chan struct{}, 1)
	retry := time.NewTimer(retryInterval)
	retry.Stop()
	defer retry.Stop()
	var retrySignal <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ended:
			return err
		case update := <-changes:
			d.applyUpdate(ctx, terminals, update)
			d.watchSwarms(ctx, &workers, terminals, swarms, changed)
			for address, attempt := range inFlight {
				if current, ok := terminals[address]; !ok || terminalIdentity(current) != attempt.current.owner {
					attempt.retired = true
					attempt.cancel()
				}
			}
		case result := <-results:
			attempt := result.attempt
			delete(inFlight, attempt.terminal.Address)
			d.finishWake(result)
			if !attempt.retired && errors.Is(result.err, ErrWakeNotSubmitted) {
				if retrySignal == nil {
					retry.Reset(retryInterval)
					retrySignal = retry.C
				}
				continue
			}
			if !attempt.retired && errors.Is(result.err, ErrStatusChanged) {
				current := terminals[attempt.terminal.Address]
				if current.Turn == attempt.terminal.Turn && current.Status != nil &&
					current.Status.State == attempt.terminal.Status.State {
					continue
				}
			}
		case <-retrySignal:
			retrySignal = nil
		case <-presence:
			for _, term := range terminals {
				d.touch(ctx, term)
			}
		case <-changed:
		}
		for address, term := range terminals {
			if _, pending := inFlight[address]; pending {
				continue
			}
			attempt := d.planWake(ctx, term)
			if attempt == nil {
				continue
			}
			sendCtx, stop := context.WithTimeout(ctx, sendTimeout)
			attempt.cancel = stop
			inFlight[address] = attempt
			// Once the worker starts, a lost reply can leave input queued or
			// submitted. Mark the attempt until hc proves otherwise.
			d.wakes[address] = attempt.current
			workers.Go(func() { d.sendWake(sendCtx, attempt, results) })
		}
	}
}

// subscribe stops delivering updates if its session has already ended.
func (d *driver) subscribe(ctx context.Context, dst chan<- Update, ended chan<- error) {
	ended <- d.hc.Subscribe(ctx, func(update Update) {
		select {
		case dst <- update:
		case <-ctx.Done():
		}
	})
}

// applyUpdate retains only local labeled terminal sessions with a running
// program. New registrations get a presence check immediately.
func (d *driver) applyUpdate(ctx context.Context, terminals map[string]Terminal, update Update) {
	if update.Snapshot {
		clear(terminals)
	}
	for _, term := range update.Terminals {
		if !d.tracked(term) {
			delete(terminals, term.Address)
			delete(d.wakes, term.Address)
			continue
		}
		old, existed := terminals[term.Address]
		terminals[term.Address] = term
		owner := terminalIdentity(term)
		if last, seen := d.wakes[term.Address]; seen && last.owner != owner {
			delete(d.wakes, term.Address)
		}
		if !existed || terminalIdentity(old) != owner {
			d.touch(ctx, term)
		}
	}
	if update.Snapshot {
		for address := range d.wakes {
			if _, ok := terminals[address]; !ok {
				delete(d.wakes, address)
			}
		}
	}
}

// watchSwarms subscribes once per represented swarm and cancels subscriptions
// when their last terminal disappears.
func (d *driver) watchSwarms(
	ctx context.Context, workers *sync.WaitGroup,
	terminals map[string]Terminal, swarms map[string]context.CancelFunc,
	changed chan<- struct{},
) {
	wanted := make(map[string]bool)
	for _, term := range terminals {
		swarm := term.Labels["chorus.swarm"]
		wanted[swarm] = true
		if _, ok := swarms[swarm]; !ok {
			watchCtx, cancel := context.WithCancel(ctx)
			swarms[swarm] = cancel
			workers.Go(func() { d.watchSwarm(watchCtx, swarm, changed) })
		}
	}
	for swarm, cancel := range swarms {
		if !wanted[swarm] {
			cancel()
			delete(swarms, swarm)
		}
	}
}

// watchSwarm coalesces notifications while the driver reads current state.
func (d *driver) watchSwarm(ctx context.Context, swarm string, changed chan<- struct{}) {
	for ctx.Err() == nil {
		err := d.bus.WatchSwarm(ctx, swarm, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
		recordFailure(ctx, "watch_swarm", err)
		if !waitRetry(ctx) {
			return
		}
	}
}

// touch refreshes the agent profile if it has already joined this swarm.
func (d *driver) touch(ctx context.Context, term Terminal) {
	ctx, cancel := context.WithTimeout(ctx, activityTimeout)
	defer cancel()
	err := d.bus.TouchAgent(ctx, term.Labels["chorus.swarm"], term.Labels["chorus.handle"])
	recordFailure(ctx, "touch_agent", err)
}

// planWake tracks drained targets even while the terminal is working. It
// returns an attempt only for a guarded status and new unread work.
func (d *driver) planWake(ctx context.Context, term Terminal) *wakeAttempt {
	previous, seen := d.wakes[term.Address]
	canWake := term.Status != nil && (term.Status.State == "idle" || term.Status.State == "done")
	if !seen && !canWake {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, activityTimeout)
	defer cancel()
	swarm, handle := term.Labels["chorus.swarm"], term.Labels["chorus.handle"]
	sequences, err := d.bus.AgentWakeSequences(ctx, swarm, handle)
	if err != nil {
		recordFailure(ctx, "wake_sequences", err)
		return nil
	}
	for target := range previous.sequences {
		if _, unread := sequences[target]; !unread {
			delete(previous.sequences, target)
		}
	}
	if len(sequences) == 0 || !canWake {
		return nil
	}
	if seen && (previous.turn == term.Turn || !hasNewWake(previous.sequences, sequences)) && !previous.unsupported {
		return nil
	}
	if seen && previous.unsupported && previous.turn == term.Turn && term.Status.State == "done" {
		return nil
	}
	attempt := &wakeAttempt{
		terminal: term,
		current:  delivery{owner: terminalIdentity(term), turn: term.Turn, sequences: sequences},
	}
	if seen {
		attempt.prior = &previous
	}
	return attempt
}

// sendWake can wait for a draft to clear without stopping sibling wakes or
// presence refreshes. The session owns cancellation and consumes the result.
func (d *driver) sendWake(ctx context.Context, attempt *wakeAttempt, results chan<- wakeResult) {
	defer attempt.cancel()
	term := attempt.terminal
	swarm, handle := term.Labels["chorus.swarm"], term.Labels["chorus.handle"]
	message := fmt.Sprintf("[Chorus] Unread message(s) waiting in swarm %s for @%s. Call the Chorus read tool with agent %q and respond in Chorus.", swarm, handle, handle)
	ctx, span := otel.Tracer("goa.design/chorus/hcdriver").Start(ctx, "chorus.hc.send_wake")
	span.SetAttributes(attribute.String("chorus.swarm", swarm), attribute.String("chorus.agent", handle))
	err := d.hc.Send(ctx, term.Address, term.Status.State, message)
	if err != nil && !errors.Is(err, ErrStatusChanged) && !errors.Is(err, ErrGuardUnsupported) &&
		!errors.Is(err, context.Canceled) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
	results <- wakeResult{attempt: attempt, err: err}
}

// finishWake keeps a submitted or indeterminate delivery. Confirmed refusal
// restores the previous delivery so a later status event can retry safely.
func (d *driver) finishWake(result wakeResult) {
	attempt, err := result.attempt, result.err
	if attempt.retired {
		return
	}
	address := attempt.terminal.Address
	switch {
	case errors.Is(err, ErrGuardUnsupported):
		d.wakes[address] = delivery{owner: attempt.current.owner, turn: attempt.current.turn, unsupported: true}
	case errors.Is(err, ErrStatusChanged) || errors.Is(err, ErrWakeNotSubmitted):
		if attempt.prior == nil {
			delete(d.wakes, address)
			return
		}
		d.wakes[address] = *attempt.prior
	}
}

// terminalIdentity derives the owner from a tracked terminal with a live PID.
func terminalIdentity(term Terminal) terminalOwner {
	return terminalOwner{pid: *term.PID, swarm: term.Labels["chorus.swarm"], handle: term.Labels["chorus.handle"]}
}

// hasNewWake compares sequences only within the stream that assigned them.
func hasNewWake(previous, current map[string]int) bool {
	for target, seq := range current {
		if previous[target] != seq {
			return true
		}
	}
	return false
}

// tracked accepts only local terminals whose labels name Chorus participants.
func (d *driver) tracked(term Terminal) bool {
	return !strings.Contains(term.Address, "/") &&
		term.PID != nil &&
		swarmName.MatchString(term.Labels["chorus.swarm"]) &&
		handleName.MatchString(term.Labels["chorus.handle"]) &&
		roleRunning(term, d.shell)
}

// roleRunning treats a live terminal with unreported foreground as running.
// A reported interactive login shell is idle unless it is executing a command.
func roleRunning(term Terminal, shell string) bool {
	if term.State != "live" {
		return false
	}
	if term.Foreground == nil || len(term.Foreground.Argv) == 0 || term.Foreground.Argv[0] == "" {
		return true
	}
	argv := term.Foreground.Argv
	base := shellName(argv[0])
	if !slices.Contains(idleShells, base) && base != shell {
		return true
	}
	for _, arg := range argv[1:min(3, len(argv))] {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "c") {
			return true
		}
	}
	return false
}

// shellName accepts both executable paths and login names such as -zsh.
func shellName(value string) string {
	if value == "" {
		return ""
	}
	return strings.TrimLeft(filepath.Base(strings.TrimLeft(value, "-")), "-")
}

// recordFailure captures a failed operation on the driver without ending it.
func recordFailure(ctx context.Context, operation string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	_, span := otel.Tracer("goa.design/chorus/hcdriver").Start(ctx, "chorus.hc."+operation)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.End()
}

// waitRetry avoids spinning when hc or Redis is temporarily unavailable.
func waitRetry(ctx context.Context) bool {
	timer := time.NewTimer(retryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
