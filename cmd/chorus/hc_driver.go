// This file adapts hc connect's plain JSON-RPC protocol to the Chorus driver.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/goadesign/chorus/chorus/hcdriver"
)

type (
	// hcConnect opens a plain-mode connection to the local terminal owner.
	hcConnect struct {
		// binary is hc or herdr-core on the server's PATH.
		binary string
	}

	// hcRequest is the single request sent on an hc connect process's stdin.
	hcRequest struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}

	// hcPacket decodes responses and event notifications from hc connect.
	hcPacket struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Params json.RawMessage `json:"params"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Kind string `json:"kind"`
			} `json:"data"`
		} `json:"error"`
	}

	// hcSnapshot replaces all terminal facts at startup or after missed events.
	hcSnapshot struct {
		// Seq excludes notifications already represented in Terminals.
		Seq uint64 `json:"seq"`
		// Terminals lists the current records from the core.
		Terminals []hcdriver.Terminal `json:"terminals"`
	}

	// hcEvent identifies a delta for one terminal.
	hcEvent struct {
		// Seq identifies the notification within this connection's state.
		Seq uint64 `json:"seq"`
		// Type selects the shape of Data.
		Type string `json:"type"`
		// Terminal is the address whose facts changed.
		Terminal string `json:"terminal"`
		// Data remains raw until the specific terminal event is applied.
		Data json.RawMessage `json:"data"`
	}
)

// Subscribe opens the plain event stream. hc's label filter compares exact
// values, so this connection receives all local terminal label changes and
// lets the driver pick terminals carrying both Chorus labels.
func (hc hcConnect) Subscribe(ctx context.Context, emit func(hcdriver.Update)) error {
	_, err := hc.exchange(ctx, "events.subscribe", struct{}{}, func(reply hcPacket, decoder *json.Decoder) error {
		var snap hcSnapshot
		if err := json.Unmarshal(reply.Result, &snap); err != nil {
			return fmt.Errorf("decode hc terminal snapshot: %w", err)
		}
		terminals := make(map[string]hcdriver.Terminal, len(snap.Terminals))
		loadHCSnapshot(snap, terminals, emit)
		seq := snap.Seq
		for {
			var packet hcPacket
			if err := decoder.Decode(&packet); err != nil {
				return fmt.Errorf("read hc terminal events: %w", err)
			}
			switch packet.Method {
			case "missed":
				var replacement hcSnapshot
				if err := json.Unmarshal(packet.Params, &replacement); err != nil {
					return fmt.Errorf("decode replacement hc snapshot: %w", err)
				}
				seq = replacement.Seq
				loadHCSnapshot(replacement, terminals, emit)
			case "event":
				var event hcEvent
				if err := json.Unmarshal(packet.Params, &event); err != nil {
					return fmt.Errorf("decode hc terminal event: %w", err)
				}
				if event.Seq > seq {
					if err := applyHCEvent(terminals, event, emit); err != nil {
						return fmt.Errorf("apply hc terminal event %q: %w", event.Type, err)
					}
					seq = event.Seq
				}
			}
		}
	})
	return err
}

// Send keeps the status guard on every request, including against older hc
// versions that cannot accept a done guard. It never forces terminal input.
func (hc hcConnect) Send(ctx context.Context, address, status, message string) error {
	params := struct {
		Terminal string `json:"terminal"`
		Text     string `json:"text"`
		If       string `json:"if"`
		Force    bool   `json:"force"`
	}{Terminal: address, Text: message, If: status}
	submitted, err := hc.exchange(ctx, "terminal.send", params, func(reply hcPacket, _ *json.Decoder) error {
		if reply.Error == nil {
			return nil
		}
		if status == "done" && reply.Error.Code == -32602 {
			return hcdriver.ErrGuardUnsupported
		}
		switch reply.Error.Data.Kind {
		case "status_mismatch", "blocked", "not_found", "finished":
			return hcdriver.ErrStatusChanged
		default:
			return fmt.Errorf("hc terminal.send: %s", reply.Error.Message)
		}
	})
	if err != nil && !submitted {
		return fmt.Errorf("%w: %w", hcdriver.ErrWakeNotSubmitted, err)
	}
	return err
}

// startHCDriver runs while the Chorus HTTP listener is serving, if hc is
// installed. The returned function waits for shutdown after canceling ctx.
func startHCDriver(ctx context.Context, svc *chorus.Service, onlineWindow time.Duration) func() {
	program, err := exec.LookPath("hc")
	if err != nil {
		program, err = exec.LookPath("herdr-core")
		if err != nil {
			return func() {}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		hcdriver.Run(ctx, hcConnect{binary: program}, svc, os.Getenv("SHELL"), onlineWindow)
	}()
	return func() {
		cancel()
		<-done
	}
}

// loadHCSnapshot discards facts lost on the old stream or missed because the
// consumer fell behind.
func loadHCSnapshot(
	snap hcSnapshot, terminals map[string]hcdriver.Terminal, emit func(hcdriver.Update),
) {
	clear(terminals)
	for _, terminal := range snap.Terminals {
		terminals[terminal.Address] = terminal
	}
	emit(hcdriver.Update{Snapshot: true, Terminals: snap.Terminals})
}

// applyHCEvent updates the fields the driver uses. Other terminal events are
// for display and do not change presence or the safe wake condition.
func applyHCEvent(
	terminals map[string]hcdriver.Terminal, event hcEvent, emit func(hcdriver.Update),
) error {
	term := terminals[event.Terminal]
	switch event.Type {
	case "created", "restarted":
		term = hcdriver.Terminal{}
		if err := json.Unmarshal(event.Data, &term); err != nil {
			return err
		}
	case "status":
		var data struct{ Status *hcdriver.Status }
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		term.Status = data.Status
	case "turn":
		var data struct{ Turn uint64 }
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		term.Turn = data.Turn
	case "foreground":
		var data *hcdriver.Foreground
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		term.Foreground = data
	case "labels":
		var data struct {
			Labels map[string]string
			Record *hcdriver.Terminal
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		if data.Record != nil {
			term = *data.Record
		}
		term.Labels = data.Labels
	case "lost", "finished", "removed":
		// A kept but lost terminal can receive labels before a restart. A
		// terminal with only its address cannot be touched or woken.
		term = hcdriver.Terminal{Address: event.Terminal}
	default:
		return nil
	}
	if term.Address == "" {
		return nil
	}
	if event.Type == "removed" {
		delete(terminals, term.Address)
	} else {
		terminals[term.Address] = term
	}
	emit(hcdriver.Update{Terminals: []hcdriver.Terminal{term}})
	return nil
}

// exchange keeps stdin open for the lifetime of the request or stream and
// always reaps the helper process. In plain mode a subscription owns its
// connection, so sends use a separate connection.
func (hc hcConnect) exchange(
	ctx context.Context, method string, params any, receive func(hcPacket, *json.Decoder) error,
) (submitted bool, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd := exec.CommandContext(ctx, hc.binary, "connect")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, fmt.Errorf("open hc input: %w", err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("open hc output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start hc connect: %w", err)
	}
	// Stop and reap the helper even when it continues waiting after a reply.
	defer cmd.Wait()
	defer cancel(nil)
	var handshake *time.Timer
	if method == "events.subscribe" {
		handshake = time.AfterFunc(15*time.Second, func() { cancel(context.DeadlineExceeded) })
		defer handshake.Stop()
	}
	if err := json.NewEncoder(stdin).Encode(hcRequest{"2.0", 1, method, params}); err != nil {
		return false, fmt.Errorf("write hc %s: %w", method, err)
	}
	decoder := json.NewDecoder(stdout)
	var reply hcPacket
	if err := decoder.Decode(&reply); err != nil {
		if ctx.Err() != nil {
			return true, context.Cause(ctx)
		}
		return true, fmt.Errorf("read hc %s reply: %w", method, err)
	}
	if handshake != nil {
		handshake.Stop()
	}
	if reply.ID != 1 {
		return true, fmt.Errorf("unexpected hc %s reply ID %d", method, reply.ID)
	}
	if reply.Error != nil && method == "events.subscribe" {
		return true, fmt.Errorf("hc events.subscribe: %s", reply.Error.Message)
	}
	if reply.Error == nil && len(reply.Result) == 0 {
		return true, errors.New("hc response has no result")
	}
	return true, receive(reply, decoder)
}
