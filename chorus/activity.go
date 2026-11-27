// This file supplies message and presence state to the terminal wake driver.
package chorus

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TouchAgent refreshes a registered agent's presence while its terminal is
// running. A missing or moved registration, or a local HTTP sender, is ignored.
// Terminal labels cannot create an agent or move it between swarms.
func (s *Service) TouchAgent(ctx context.Context, swarm, handle string) error {
	bound, err := s.agentBinding(ctx, handle)
	if err != nil {
		return err
	}
	if bound != swarm {
		return nil
	}
	sw, err := s.swarm(ctx, swarm)
	if err != nil {
		return err
	}
	if err := sw.state.Sync(ctx); err != nil {
		return fmt.Errorf("sync agent presence for %q: %w", handle, err)
	}
	model, ok := sw.state.Get(modelMapKey(handle))
	if !ok || isHTTPParticipant(model) || !sw.isLiveAgent(handle) {
		return nil
	}
	err = sw.touchAgent(ctx, handle, s.clock(), false)
	if errors.Is(err, errInvalidCallerState) {
		return nil
	}
	return err
}

// AgentWakeSequences returns the last unread wake-worthy sequence for each
// target. Sequences belong to their target. An empty result means no wake is
// needed, including when this server already has a blocking read for the agent.
func (s *Service) AgentWakeSequences(ctx context.Context, swarm, handle string) (map[string]int, error) {
	sw, err := s.swarm(ctx, swarm)
	if err != nil {
		return nil, err
	}
	model, _ := sw.state.Get(modelMapKey(handle))
	if !sw.isLiveAgent(handle) || isHTTPParticipant(model) || sw.hasActiveWatch(handle) {
		return nil, nil
	}
	return sw.wakeSequences(handle), nil
}

// WatchSwarm calls changed when it begins observing a swarm and on subsequent
// changes. The caller should return from changed promptly and cancel ctx when
// it no longer needs these notifications.
func (s *Service) WatchSwarm(ctx context.Context, swarm string, changed func()) error {
	sw, err := s.swarm(ctx, swarm)
	if err != nil {
		return err
	}
	events := sw.state.Subscribe()
	defer sw.state.Unsubscribe(events)
	changed()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				return errors.New("swarm observation closed")
			}
			changed()
		}
	}
}

// wakeSequences checks retained metadata, leaving streams and read positions
// untouched until the agent invokes read.
func (sw *swarmHandle) wakeSequences(handle string) map[string]int {
	var sequences map[string]int
	lower := strings.ToLower(handle)
	for _, target := range sw.readTargets(handle, "") {
		headRaw, _ := sw.state.Get(headMapKey(target))
		cursorRaw, _ := sw.state.Get(cursorMapKey(handle, target))
		head, cursor := parseSeq(headRaw), parseSeq(cursorRaw)
		if head <= cursor {
			continue
		}
		seq := head
		if target[0] == '#' {
			mentionRaw, _ := sw.state.Get(mentionMapKey(lower, target))
			allRaw, _ := sw.state.Get(mentionMapKey("all", target))
			mention := max(parseSeq(mentionRaw), parseSeq(allRaw))
			if mention <= cursor || mention > head {
				continue
			}
			seq = mention
		}
		if sequences == nil {
			sequences = make(map[string]int)
		}
		sequences[target] = seq
	}
	return sequences
}
