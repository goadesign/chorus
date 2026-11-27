// This file claims live handles and prevents callers from crossing between
// agent, person, and local service identities.
package chorus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
	"goa.design/pulse/semaphore"
)

// serviceModel identifies a local service in profiles and saved messages.
const serviceModel = "service"

var (
	// errHandleInUse marks a registration that conflicts with a live participant.
	errHandleInUse = fmt.Errorf("%w: handle in use", errInvalidCallerState)
	// deleteBinding removes a handle only while it belongs to the swarm that
	// is removing its profile. Expiration and reassignment can overlap.
	deleteBinding = redis.NewScript(`if redis.call('HGET', KEYS[1], ARGV[1]) == ARGV[2] then
		return redis.call('HDEL', KEYS[1], ARGV[1])
	end
	return 0`)
)

// unbindParticipant clears a registration left behind in the named swarm.
func unbindParticipant(ctx context.Context, rdb *redis.Client, handle, swarm string) error {
	if err := deleteBinding.Run(ctx, rdb, []string{agentSwarmHashKey}, handle, swarm).Err(); err != nil {
		return fmt.Errorf("unbind %q from swarm %q: %w", handle, swarm, err)
	}
	return nil
}

// participantKind groups opaque MCP model identifiers into the agent kind.
func participantKind(model string) string {
	model = strings.TrimSpace(model)
	switch {
	case strings.EqualFold(model, humanModel):
		return humanModel
	case strings.EqualFold(model, serviceModel):
		return serviceModel
	default:
		return "agent"
	}
}

// isHTTPParticipant reports whether a name is reserved for local HTTP senders.
func isHTTPParticipant(model string) bool {
	return participantKind(model) != "agent"
}

// registerMCPAgent prevents the two HTTP participant kinds from being declared
// by MCP callers, whose model identifiers are otherwise opaque.
func (s *Service) registerMCPAgent(
	ctx context.Context, agent, swarm, model string, role *string,
) (*swarmHandle, error) {
	if isHTTPParticipant(model) {
		return nil, fmt.Errorf("%w: model %q is reserved for HTTP message senders", errInvalidCallerState, model)
	}
	return s.registerParticipant(ctx, agent, swarm, model, role)
}

// registerParticipant serializes claims for a handle across server replicas.
// An active handle retains its swarm and participant kind; a returning agent
// may still update its model and role. The stored model also owns provenance for
// messages later sent by the participant.
func (s *Service) registerParticipant(
	ctx context.Context, handle, swarm, model string, role *string,
) (_ *swarmHandle, err error) {
	guard, err := semaphore.New(
		s.rdb, "chorus:participant:"+handle, 1,
		semaphore.WithLeaseTTL(publishLeaseTTL),
		semaphore.WithHeartbeatInterval(publishHeartbeatInterval),
	)
	if err != nil {
		return nil, fmt.Errorf("create registration semaphore: %w", err)
	}
	lease, err := guard.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire registration lease: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishHeartbeatInterval)
		defer cancel()
		if releaseErr := lease.Release(releaseCtx); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release registration lease: %w", releaseErr))
		}
	}()

	sw, err := s.checkAgentSwarmAvailable(ctx, handle, swarm)
	if err != nil {
		return nil, err
	}
	if err := sw.state.Sync(ctx); err != nil {
		return nil, fmt.Errorf("sync registration for %q: %w", handle, err)
	}
	current, _ := sw.state.Get(modelMapKey(handle))
	live := sw.isLiveAgent(handle)
	if live && participantKind(current) != participantKind(model) {
		return nil, fmt.Errorf("%w: %q belongs to a different participant kind in swarm %q", errHandleInUse, handle, swarm)
	}
	now := s.clock()
	// A repeat post from the same local sender only refreshes presence.
	if live && isHTTPParticipant(model) && current == model && role == nil {
		return sw, sw.touchAgent(ctx, handle, now, false)
	}
	if err := sw.reapExpiredAgents(ctx, now); err != nil {
		return nil, err
	}
	var roleText string
	if role != nil {
		roleText = *role
	}
	// Bind before making the profile live so a failed bind cannot leave an
	// active registration that is invisible to another replica.
	if err := s.recordAgentSwarm(ctx, handle, swarm); err != nil {
		return nil, err
	}
	if err := sw.declareProfile(ctx, handle, roleText, model); err != nil {
		return nil, err
	}
	if err := sw.registerPresence(ctx, handle, now); err != nil {
		return nil, err
	}
	return sw, nil
}

// resolveMCPAgentSwarm checks the live profile of the handle given to send,
// read, or leave. People and services use the local HTTP interface.
func (s *Service) resolveMCPAgentSwarm(ctx context.Context, handle string) (*swarmHandle, error) {
	sw, err := s.resolveAgentSwarm(ctx, handle)
	if err != nil {
		return nil, err
	}
	model, _ := sw.state.Get(modelMapKey(handle))
	if isHTTPParticipant(model) {
		return nil, fmt.Errorf("%w: %q uses the local HTTP interface", errInvalidCallerState, handle)
	}
	return sw, nil
}
