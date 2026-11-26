// Package chorus implements the Chorus multi-agent coordination service backed
// by Pulse replicated maps, bounded Redis streams, and leased semaphores.
//
// This file owns per-swarm coordination state, atomic publication across Pulse
// streams and replicated maps, and watermark-driven unread message draining.
package chorus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"goa.design/pulse/rmap"
	"goa.design/pulse/semaphore"
	"goa.design/pulse/streaming"
	"goa.design/pulse/streaming/options"
)

type (
	// swarmHandle encapsulates the Pulse replicated map and Redis stream/semaphore
	// naming conventions for a single isolated agent swarm.
	swarmHandle struct {
		// name is the URL-safe swarm identifier.
		name string
		// rdb is the shared Redis client used for streams and semaphores.
		rdb *redis.Client
		// state is the in-memory replicated map ("chorus:<swarm>:state").
		state *rmap.Map
		// swarmTTL is the sliding expiration applied to state and message streams.
		swarmTTL time.Duration
		// streamMaxLen is the maximum messages retained per channel or DM stream.
		streamMaxLen int
		// onlineWindow is the duration since last activity within which an agent remains registered.
		onlineWindow time.Duration
		// clock returns the current time for presence timestamps and heartbeats.
		clock func() time.Time
		// watchMu guards watching.
		watchMu sync.Mutex
		// watching tracks agent handles with an active blocking watch on this swarm handle.
		watching map[string]struct{}
	}

	// cursorUpdate records a pending watermark update for an agent on a target stream.
	cursorUpdate struct {
		// key is the rmap key ("cur:<agent>:<target>").
		key string
		// val is the encoded integer sequence watermark.
		val string
	}

	// joinedChannel buffers per-channel tail state during a multi-channel join
	// before the caller's subscription and presence updates are committed to rmap.
	joinedChannel struct {
		// name is the channel name ("#channel").
		name string
		// headSeq is the latest sequence number observed in the channel stream.
		headSeq int
		// lines holds the active pinned manifest line (if any) and recent tail messages.
		lines []string
	}

	// publishedMessage pairs the stored message with its Redis-assigned entry ID.
	publishedMessage struct {
		// event is the model and content saved in the stream.
		event messageEvent
		// entryID supplies the timestamp assigned by Redis.
		entryID string
	}
)

const (
	// eventTypeMessage is the Pulse stream event name used for all Chorus messages.
	eventTypeMessage = "msg"

	// humanModel identifies a person in profiles and saved messages.
	humanModel = "human"

	// touchInterval throttles background agent LastSeenUnix updates in rmap so
	// high-frequency tool calls do not write to Redis on every invocation.
	touchInterval = 15 * time.Second

	// publishLeaseTTL bounds how long a crashed replica can hold a target's
	// 1-slot publication semaphore before Redis automatically reclaims it.
	publishLeaseTTL = 15 * time.Second

	// publishHeartbeatInterval sets how often an active publication lease renews
	// its expiry in Redis.
	publishHeartbeatInterval = 5 * time.Second

	// defaultReadLimit is the maximum number of unread messages returned by read
	// or piggybacked on send when the caller omits limit.
	defaultReadLimit = 20
)

// openSwarm joins the Pulse replicated map backing the named swarm and returns
// its handle.
func openSwarm(
	ctx context.Context,
	rdb *redis.Client,
	name string,
	swarmTTL time.Duration,
	streamMaxLen int,
	onlineWindow time.Duration,
	clock func() time.Time,
) (*swarmHandle, error) {
	state, err := rmap.Join(ctx, swarmStateMapName(name), rdb, rmap.WithSlidingTTL(swarmTTL))
	if err != nil {
		return nil, fmt.Errorf("join swarm %q state map: %w", name, err)
	}
	return &swarmHandle{
		name:         name,
		rdb:          rdb,
		state:        state,
		swarmTTL:     swarmTTL,
		streamMaxLen: streamMaxLen,
		onlineWindow: onlineWindow,
		clock:        clock,
		watching:     make(map[string]struct{}),
	}, nil
}

// swarmStateMapName returns the name of the Pulse replicated map that holds
// the named swarm's agents, subscriptions, read positions, and pins.
func swarmStateMapName(name string) string {
	return fmt.Sprintf("chorus:%s:state", name)
}

// close stops the swarm's replicated map subscription.
func (s *swarmHandle) close() {
	s.state.Close()
}

// isLiveAgent reports whether agent currently has a registration record in s
// that is either actively watching or within the online liveness window.
func (s *swarmHandle) isLiveAgent(agent string) bool {
	raw, ok := s.state.Get(agentMapKey(agent))
	if !ok {
		return false
	}
	_, live := parseLiveAgentRecord(s.clock(), s.onlineWindow, raw, s.hasActiveWatch(agent))
	return live
}

// liveAgents returns the handles of all non-expired registered agents in s.
func (s *swarmHandle) liveAgents() []string {
	now := s.clock()
	snap := s.state.Map()
	var handles []string
	for k, v := range snap {
		if !strings.HasPrefix(k, prefixAgent) {
			continue
		}
		handle := strings.TrimPrefix(k, prefixAgent)
		if _, live := parseLiveAgentRecord(now, s.onlineWindow, v, s.hasActiveWatch(handle)); live {
			handles = append(handles, handle)
		}
	}
	return handles
}

// requireChannelMember verifies that agent has joined channel, returning a
// caller precondition error (errInvalidCallerState) listing the channel's
// current members when not subscribed.
func (s *swarmHandle) requireChannelMember(agent, channel string) error {
	subs, _ := s.state.GetValues(subMapKey(agent))
	if slices.Contains(subs, channel) {
		return nil
	}
	now := s.clock()
	snap := s.state.Map()
	if _, exists := snap[channelMapKey(channel)]; !exists {
		return fmt.Errorf(
			"%w: channel %s does not exist in swarm %q; call join first",
			errInvalidCallerState, channel, s.name,
		)
	}
	subsByAgent := s.channelSubscriptions(snap, now)
	var members []string
	for h, chs := range subsByAgent {
		if slices.Contains(chs, channel) {
			members = append(members, h)
		}
	}
	if len(members) == 0 {
		return fmt.Errorf(
			"%w: agent %q has not joined %s in swarm %q; call join first",
			errInvalidCallerState, agent, channel, s.name,
		)
	}
	return fmt.Errorf(
		"%w: agent %q has not joined %s in swarm %q (members: %s); call join first or use your joined handle",
		errInvalidCallerState, agent, channel, s.name, formatHandleList(members),
	)
}

// acquireWatch reserves the single active blocking-watch slot for agent on s,
// returning a caller precondition error (errInvalidCallerState) when another
// blocking watch for agent is already in flight.
func (s *swarmHandle) acquireWatch(agent string) error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if _, active := s.watching[agent]; active {
		return fmt.Errorf(
			"%w: agent %q already has an active watch in swarm %q",
			errInvalidCallerState, agent, s.name,
		)
	}
	s.watching[agent] = struct{}{}
	return nil
}

// releaseWatch frees the active blocking-watch slot for agent on s.
func (s *swarmHandle) releaseWatch(agent string) {
	s.watchMu.Lock()
	delete(s.watching, agent)
	s.watchMu.Unlock()
}

// hasActiveWatch reports whether agent currently has an active blocking watch
// (read with waitSec > 0) in flight on s.
func (s *swarmHandle) hasActiveWatch(agent string) bool {
	s.watchMu.Lock()
	_, active := s.watching[agent]
	s.watchMu.Unlock()
	return active
}

// touchAgent refreshes an existing presence record. A compare-and-set keeps
// an older replica from restoring a registration removed during a tool call.
func (s *swarmHandle) touchAgent(ctx context.Context, agent string, now time.Time, forceSync bool) error {
	key := agentMapKey(agent)
	raw, _ := s.state.Get(key)
	rec, valid := parseAgentRecord(raw)
	if !valid {
		return fmt.Errorf("%w: %q has no presence in swarm %q", errInvalidCallerState, agent, s.name)
	}
	if now.Unix()-rec.LastSeenUnix >= int64(s.presenceInterval().Seconds()) {
		rec.LastSeenUnix = now.Unix()
		encoded, err := encodeAgentRecord(rec)
		if err != nil {
			return err
		}
		_, exists, updated, err := s.state.TestAndSetEx(ctx, key, raw, encoded)
		if err != nil {
			return fmt.Errorf("update agent %q presence: %w", agent, err)
		}
		if !exists {
			return fmt.Errorf("%w: %q left swarm %q", errInvalidCallerState, agent, s.name)
		}
		if updated {
			if err := s.rdb.Expire(ctx, agentSwarmHashKey, s.swarmTTL).Err(); err != nil {
				return fmt.Errorf("refresh participant bindings: %w", err)
			}
		}
	}
	if forceSync {
		return s.state.Sync(ctx)
	}
	return nil
}

// registerPresence makes a declared participant visible to other callers.
func (s *swarmHandle) registerPresence(ctx context.Context, agent string, now time.Time) error {
	encoded, err := encodeAgentRecord(agentRecord{LastSeenUnix: now.Unix()})
	if err != nil {
		return err
	}
	if _, err := s.state.SetAndWait(ctx, agentMapKey(agent), encoded); err != nil {
		return fmt.Errorf("register presence for %q: %w", agent, err)
	}
	return nil
}

// presenceInterval keeps presence fresh even with a shorter online window.
func (s *swarmHandle) presenceInterval() time.Duration {
	return min(touchInterval, max(time.Second, s.onlineWindow/2))
}

// declareProfile saves what agent declared on join or who. The model is
// always saved; the role is saved only when one is given, so an omitted role
// leaves the stored one alone. Each value has its own key, so no declaration
// reads and rewrites another one. Writing the model with SetAndWait also makes
// this server's copy catch up with every earlier write, including a role saved
// through another server, before the caller's response is built.
func (s *swarmHandle) declareProfile(ctx context.Context, agent, role, model string) error {
	if role != "" {
		if _, err := s.state.Set(ctx, roleMapKey(agent), role); err != nil {
			return fmt.Errorf("save role for %q: %w", agent, err)
		}
	}
	if _, err := s.state.SetAndWait(ctx, modelMapKey(agent), model); err != nil {
		return fmt.Errorf("save model for %q: %w", agent, err)
	}
	return nil
}

// directory formats the swarm snapshot for a registered caller.
func (s *swarmHandle) directory(caller string) string {
	snap := s.state.Map()
	subs, _ := s.state.GetValues(subMapKey(caller))
	return formatDirectory(caller, s.liveAgents(), snap, subs)
}

// subscribePoster gives a local sender its first subscription to a channel.
// Existing subscriptions keep their read positions.
func (s *swarmHandle) subscribePoster(ctx context.Context, sender, channel string) error {
	subs, _ := s.state.GetValues(subMapKey(sender))
	if slices.Contains(subs, channel) {
		return nil
	}
	if _, err := s.state.SetIfNotExists(ctx, channelMapKey(channel), ""); err != nil {
		return fmt.Errorf("register channel %s: %w", channel, err)
	}
	if _, err := s.state.AppendUniqueValues(ctx, subMapKey(sender), channel); err != nil {
		return fmt.Errorf("subscribe %q to %s: %w", sender, channel, err)
	}
	return nil
}

// joinChannels subscribes agent to one or more channels, creating any that do
// not yet exist, setting their read watermarks to the current stream heads, and
// returning the formatted join summary (channel topic, subscribed members,
// active pinned manifest, and up to tail recent messages) per channel.
func (s *swarmHandle) joinChannels(
	ctx context.Context,
	agent string,
	channels []string,
	topic string,
	tail int,
) (string, error) {
	now := s.clock()
	unique := deduplicateStrings(channels)
	joined := make([]joinedChannel, 0, len(unique))

	for _, ch := range unique {
		chKey := channelMapKey(ch)
		if topic != "" {
			if _, err := s.state.Set(ctx, chKey, topic); err != nil {
				return "", fmt.Errorf("set topic for %s: %w", ch, err)
			}
		} else {
			if _, err := s.state.SetIfNotExists(ctx, chKey, ""); err != nil {
				return "", fmt.Errorf("register channel %s: %w", ch, err)
			}
		}

		events, err := s.snapshotTarget(ctx, ch)
		if err != nil {
			return "", err
		}
		curSnap := s.state.Map()
		lines, headSeq, err := formatTailLines(ch, tail, curSnap[headMapKey(ch)], curSnap[pinMapKey(ch)], events)
		if err != nil {
			return "", err
		}
		if headSeq > 0 {
			if _, err := s.state.Set(ctx, cursorMapKey(agent, ch), strconv.Itoa(headSeq)); err != nil {
				return "", fmt.Errorf("set cursor for %s: %w", ch, err)
			}
		}
		joined = append(joined, joinedChannel{
			name:    ch,
			headSeq: headSeq,
			lines:   lines,
		})
	}

	if _, err := s.state.AppendUniqueValues(ctx, subMapKey(agent), unique...); err != nil {
		return "", fmt.Errorf("subscribe %q to channels: %w", agent, err)
	}
	if err := s.state.Sync(ctx); err != nil {
		return "", fmt.Errorf("sync channels for %q: %w", agent, err)
	}
	snap := s.state.Map()
	subsByAgent := s.channelSubscriptions(snap, now)
	sections := make([]string, 0, len(joined))

	for _, ch := range joined {
		effectiveTopic := topic
		if effectiveTopic == "" {
			effectiveTopic = snap[channelMapKey(ch.name)]
		}
		header := fmt.Sprintf("joined %s (head #%d)", ch.name, ch.headSeq)
		if effectiveTopic != "" {
			header += " - " + effectiveTopic
		}
		header += "\n" + formatChannelMembers(ch.name, snap, subsByAgent)
		if len(ch.lines) > 0 {
			header += "\n" + strings.Join(ch.lines, "\n")
		}
		sections = append(sections, header)
	}
	out := strings.Join(sections, "\n")
	if !s.hasActiveWatch(agent) {
		out += "\n" + formatWatchHint(agent)
	}
	return out, nil
}

// channelSubscriptions extracts the subscribed channel list for every live
// agent key ("sub:<agent>") present in snap.
func (s *swarmHandle) channelSubscriptions(snap map[string]string, now time.Time) map[string][]string {
	subsByAgent := make(map[string][]string)
	for k := range snap {
		if !strings.HasPrefix(k, prefixSub) {
			continue
		}
		handle := strings.TrimPrefix(k, prefixSub)
		_, live := parseLiveAgentRecord(now, s.onlineWindow, snap[agentMapKey(handle)], s.hasActiveWatch(handle))
		if !live {
			continue
		}
		subs, _ := s.state.GetValues(k)
		if len(subs) > 0 {
			subsByAgent[handle] = subs
		}
	}
	return subsByAgent
}

// reapExpiredAgents removes agent records, channel subscriptions, and channel
// read cursors for any agent whose LastSeenUnix is older than onlineWindow
// (preserving direct-message cursors alongside retained DM streams).
// Registration expiry never removes channel history or pins.
func (s *swarmHandle) reapExpiredAgents(ctx context.Context, now time.Time) error {
	snap := s.state.Map()
	var expired []string
	for k, v := range snap {
		if !strings.HasPrefix(k, prefixAgent) {
			continue
		}
		handle := strings.TrimPrefix(k, prefixAgent)
		if _, live := parseLiveAgentRecord(now, s.onlineWindow, v, s.hasActiveWatch(handle)); !live {
			expired = append(expired, handle)
		}
	}

	for _, handle := range expired {
		oldSubs, _ := s.state.GetValues(subMapKey(handle))
		for _, ch := range oldSubs {
			if _, err := s.state.Delete(ctx, cursorMapKey(handle, ch)); err != nil {
				return fmt.Errorf("delete expired cursor for %s: %w", ch, err)
			}
		}
		if _, err := s.state.Delete(ctx, subMapKey(handle)); err != nil {
			return fmt.Errorf("delete expired subscriptions for %q: %w", handle, err)
		}
		if _, err := s.state.Delete(ctx, roleMapKey(handle)); err != nil {
			return fmt.Errorf("delete expired role %q: %w", handle, err)
		}
		if _, err := s.state.Delete(ctx, modelMapKey(handle)); err != nil {
			return fmt.Errorf("delete expired model %q: %w", handle, err)
		}
		if _, err := s.state.Delete(ctx, agentMapKey(handle)); err != nil {
			return fmt.Errorf("delete expired agent %q: %w", handle, err)
		}
		if err := unbindParticipant(ctx, s.rdb, handle, s.name); err != nil {
			return err
		}
	}
	return nil
}

// leaveChannels unsubscribes agent from one or more channels, removes their
// read watermarks from the swarm's replicated map, unregisters agent when no
// subscribed channels remain, and—when no subscribed members remain in a
// channel—destroys the channel stream and removes its topic, pinned manifest,
// sequence counters, and mention watermarks.
func (s *swarmHandle) leaveChannels(ctx context.Context, agent string, channels []string) (string, error) {
	now := s.clock()
	prevSubs, _ := s.state.GetValues(subMapKey(agent))
	unique := deduplicateStrings(channels)

	var remainingSubs []string
	for _, sub := range prevSubs {
		if !slices.Contains(unique, sub) {
			remainingSubs = append(remainingSubs, sub)
		}
	}

	snap := s.state.Map()
	subsByAgent := s.channelSubscriptions(snap, now)
	if len(remainingSubs) == 0 {
		delete(subsByAgent, agent)
	} else {
		subsByAgent[agent] = remainingSubs
	}

	for _, ch := range unique {
		if _, err := s.state.Delete(ctx, cursorMapKey(agent, ch)); err != nil {
			return "", fmt.Errorf("delete cursor for %s: %w", ch, err)
		}
		if !hasChannelSubscribers(ch, subsByAgent) {
			if err := s.purgeChannel(ctx, ch, snap); err != nil {
				return "", err
			}
		}
	}

	if len(remainingSubs) == 0 {
		if _, err := s.state.Delete(ctx, subMapKey(agent)); err != nil {
			return "", fmt.Errorf("delete empty subscription key for %q: %w", agent, err)
		}
		if _, err := s.state.Delete(ctx, roleMapKey(agent)); err != nil {
			return "", fmt.Errorf("delete role of departed agent %q: %w", agent, err)
		}
		if _, err := s.state.Delete(ctx, modelMapKey(agent)); err != nil {
			return "", fmt.Errorf("delete model of departed agent %q: %w", agent, err)
		}
		if err := s.deleteAndWait(ctx, agentMapKey(agent)); err != nil {
			return "", fmt.Errorf("unregister departed agent %q: %w", agent, err)
		}
		return "left " + strings.Join(unique, ", "), nil
	}

	if _, _, err := s.state.RemoveValues(ctx, subMapKey(agent), unique...); err != nil {
		return "", fmt.Errorf("unsubscribe %q from channels: %w", agent, err)
	}
	if err := s.touchAgent(ctx, agent, now, true); err != nil {
		return "", err
	}
	return "left " + strings.Join(unique, ", "), nil
}

// deleteAndWait removes key from s.state and waits for the local rmap replica
// to observe the deletion.
func (s *swarmHandle) deleteAndWait(ctx context.Context, key string) error {
	notifyCh := s.state.Subscribe()
	if notifyCh != nil {
		defer s.state.Unsubscribe(notifyCh)
	}
	if _, err := s.state.Delete(ctx, key); err != nil {
		return err
	}
	for notifyCh != nil {
		if _, exists := s.state.Get(key); !exists {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notifyCh:
		}
	}
	return nil
}

// purgeChannel destroys a channel's Pulse stream and deletes its metadata,
// pinned manifest, sequence counters, and mention watermarks from rmap once
// its last subscriber has left.
func (s *swarmHandle) purgeChannel(ctx context.Context, channel string, snap map[string]string) error {
	if stream, err := s.openStream(channel); err == nil {
		if err := stream.Destroy(ctx); err != nil &&
			!errors.Is(err, streaming.ErrStreamNotFound) &&
			!errors.Is(err, streaming.ErrStreamDestroyed) {
			return fmt.Errorf("destroy stream for %s: %w", channel, err)
		}
	}
	keys := []string{
		pinMapKey(channel),
		channelMapKey(channel),
		headMapKey(channel),
		seqMapKey(channel),
	}
	mentionSuffix := ":" + channel
	for k := range snap {
		if strings.HasPrefix(k, prefixMention) && strings.HasSuffix(k, mentionSuffix) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if _, err := s.state.Delete(ctx, k); err != nil {
			return fmt.Errorf("delete %s: %w", k, err)
		}
	}
	return nil
}

// publish appends a message from sender to target ("#channel" or "@agent") under
// a per-target leased semaphore, optionally pins it as the channel's active
// collaboration manifest, commits the new head sequence to rmap via SetAndWait,
// releases the semaphore, drains any unread messages waiting for sender across
// all subscribed channels and direct messages, and—when no unread messages are
// immediately present and waitSec > 0—waits up to waitSec seconds for a reply,
// mention, or direct message.
func (s *swarmHandle) publish(
	ctx context.Context,
	sender string,
	target string,
	text string,
	replyTo int,
	pin bool,
	waitSec int,
) (string, error) {
	if strings.HasPrefix(target, "#") {
		if err := s.requireChannelMember(sender, target); err != nil {
			return "", err
		}
	}
	if waitSec > 0 {
		if err := s.acquireWatch(sender); err != nil {
			return "", err
		}
		defer s.releaseWatch(sender)
	}
	if err := s.touchAgent(ctx, sender, s.clock(), false); err != nil {
		return "", err
	}

	// Drain any pre-existing unread messages across sender's current
	// subscriptions before acquiring the target semaphore so sender's cursor on
	// an already-subscribed target is caught up to head prior to publication.
	unread, err := s.drainUnread(ctx, sender, "", defaultReadLimit, false)
	if err != nil {
		return "", err
	}

	sent, err := s.appendUnderSemaphore(ctx, sender, target, text, replyTo, pin, "agent")
	if err != nil {
		return "", err
	}

	if remaining := defaultReadLimit - len(unread); remaining > 0 {
		moreUnread, err := s.drainUnread(ctx, sender, "", remaining, false)
		if err != nil {
			return "", err
		}
		unread = append(unread, moreUnread...)
	}
	if len(unread) == 0 && waitSec > 0 {
		waited, err := s.waitForUnread(ctx, sender, "", waitSec, defaultReadLimit, true)
		if err != nil {
			return "", err
		}
		unread = waited
	}
	out := formatSendOutput(target, sent.event.Seq, unread)
	if waitSec > 0 || !s.hasActiveWatch(sender) {
		out += "\n" + formatWatchHint(sender)
	}
	return out, nil
}

// appendUnderSemaphore acquires the 1-slot publication semaphore for target,
// increments the target sequence number, appends the message event to the
// target's Pulse stream, advances sender's own cursor when caught up so
// concurrent watchers for sender never wake on self-sent messages, records any
// channel @mentions, reply targets, or pinned manifest in rmap, and commits the
// updated head sequence to rmap via SetAndWait before releasing the semaphore.
func (s *swarmHandle) appendUnderSemaphore(
	ctx context.Context,
	sender string,
	target string,
	text string,
	replyTo int,
	pin bool,
	kind string,
) (*publishedMessage, error) {
	sem, err := semaphore.New(
		s.rdb,
		semaphoreName(s.name, target),
		1,
		semaphore.WithLeaseTTL(publishLeaseTTL),
		semaphore.WithHeartbeatInterval(publishHeartbeatInterval),
	)
	if err != nil {
		return nil, fmt.Errorf("create semaphore for %s: %w", target, err)
	}
	lease, err := sem.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire publish lease for %s: %w", target, err)
	}
	defer func() {
		_ = lease.Release(context.Background())
	}()

	// The sender's declared model at publication time decides, once and for
	// good, whether this message came from a person; the role is free text and
	// never decides it. Sync first brings this server's copy up to date with
	// every write that completed before the send, so a who or join that just
	// finished through another server is included. It runs before the sequence
	// number is taken, so a failure leaves no gap in the numbering.
	if err := s.state.Sync(ctx); err != nil {
		return nil, fmt.Errorf("sync swarm state before publishing to %s: %w", target, err)
	}
	senderModel, _ := s.state.Get(modelMapKey(sender))
	if participantKind(senderModel) != kind {
		return nil, fmt.Errorf("%w: %q belongs to a different participant kind", errHandleInUse, sender)
	}

	seq, err := s.state.Inc(ctx, seqMapKey(target), 1)
	if err != nil {
		return nil, fmt.Errorf("increment sequence for %s: %w", target, err)
	}

	var mentions []string
	if strings.HasPrefix(target, "#") {
		// Pinning adds no wake of its own. A pinned message wakes the same
		// watchers as any channel message: the handles it mentions and the
		// author of the message it replies to. Replacing the board does not by
		// itself cost every member a model turn.
		mentions = extractMentionedHandles(text)
		if replyTo > 0 {
			replyAuthor, authorErr := s.findMessageSender(ctx, target, replyTo)
			if authorErr != nil {
				return nil, authorErr
			}
			if replyAuthor != "" {
				authorLower := strings.ToLower(replyAuthor)
				if !slices.Contains(mentions, authorLower) {
					mentions = append(mentions, authorLower)
				}
			}
		}
	}

	event := messageEvent{
		Seq:     seq,
		Sender:  sender,
		Human:   senderModel == humanModel,
		Model:   senderModel,
		Text:    text,
		ReplyTo: replyTo,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode message payload: %w", err)
	}

	stream, err := s.openStream(target)
	if err != nil {
		return nil, err
	}
	entryID, err := stream.Add(ctx, eventTypeMessage, payload)
	if err != nil {
		return nil, fmt.Errorf("append message to %s stream: %w", target, err)
	}

	if pin && strings.HasPrefix(target, "#") {
		if _, err := s.state.Set(ctx, pinMapKey(target), string(payload)); err != nil {
			return nil, fmt.Errorf("pin message in %s: %w", target, err)
		}
	}

	// Mark a local poster caught up through its own post. MCP senders keep
	// any older unread messages unless they were already caught up.
	curRaw, _ := s.state.Get(cursorMapKey(sender, target))
	if (strings.HasPrefix(target, "#") || strings.EqualFold(target, "@"+sender)) &&
		(kind != "agent" || parseSeq(curRaw) == seq-1) {
		if _, err := s.state.Set(ctx, cursorMapKey(sender, target), strconv.Itoa(seq)); err != nil {
			return nil, fmt.Errorf("advance sender cursor for %s: %w", target, err)
		}
	}

	senderLower := strings.ToLower(sender)
	for _, h := range mentions {
		if h == senderLower && h != "all" {
			continue
		}
		if _, err := s.state.Set(ctx, mentionMapKey(h, target), strconv.Itoa(seq)); err != nil {
			return nil, fmt.Errorf("record mention of %q in %s: %w", h, target, err)
		}
	}

	if _, err := s.state.SetAndWait(ctx, headMapKey(target), strconv.Itoa(seq)); err != nil {
		return nil, fmt.Errorf("commit head sequence for %s: %w", target, err)
	}
	return &publishedMessage{event: event, entryID: entryID}, nil
}

// findMessageSender returns the sender handle of the message with sequence
// number seq in target (checking the in-memory pinned manifest first, then the
// retained stream history), or "" if not found.
func (s *swarmHandle) findMessageSender(ctx context.Context, target string, seq int) (string, error) {
	if rawPin, ok := s.state.Get(pinMapKey(target)); ok && rawPin != "" {
		var pinMsg messageEvent
		if err := json.Unmarshal([]byte(rawPin), &pinMsg); err == nil && pinMsg.Seq == seq {
			return pinMsg.Sender, nil
		}
	}
	events, err := s.snapshotTarget(ctx, target)
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		var msg messageEvent
		if err := json.Unmarshal(ev.Payload(), &msg); err != nil {
			return "", fmt.Errorf("decode message event %s in %s: %w", ev.ID(), target, err)
		}
		if msg.Seq == seq {
			return msg.Sender, nil
		}
		if msg.Seq < seq {
			break
		}
	}
	return "", nil
}

// readUnread drains unread messages for agent from the specified channel (or
// across direct messages and all subscribed channels when from is empty). When
// mentionsOnly is true, readUnread only drains targets that contain a direct
// message to agent, a case-insensitive @<agent> or @all mention, or a reply to
// an earlier message sent by agent. When no matching unread messages are
// immediately available and waitSec > 0, readUnread blocks on the swarm's
// replicated map notification channel (refreshing agent online presence
// periodically) until a matching message arrives or waitSec elapses.
func (s *swarmHandle) readUnread(
	ctx context.Context,
	agent string,
	from string,
	waitSec int,
	limit int,
	mentionsOnly bool,
) (string, error) {
	if from != "" {
		if err := s.requireChannelMember(agent, from); err != nil {
			return "", err
		}
	}
	if waitSec > 0 {
		if err := s.acquireWatch(agent); err != nil {
			return "", err
		}
		defer s.releaseWatch(agent)
	}
	if err := s.touchAgent(ctx, agent, s.clock(), false); err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}

	lines, err := s.drainUnread(ctx, agent, from, limit, mentionsOnly)
	if err != nil {
		return "", err
	}
	if len(lines) > 0 || waitSec <= 0 {
		return formatReadOutput(lines), nil
	}
	lines, err = s.waitForUnread(ctx, agent, from, waitSec, limit, mentionsOnly)
	if err != nil {
		return "", err
	}
	return formatReadOutput(lines), nil
}

// waitForUnread subscribes to the swarm's replicated map notification channel
// and blocks for up to waitSec seconds (refreshing agent presence periodically)
// until matching unread messages arrive for agent.
func (s *swarmHandle) waitForUnread(
	ctx context.Context,
	agent string,
	from string,
	waitSec int,
	limit int,
	mentionsOnly bool,
) ([]string, error) {
	notifyCh := s.state.Subscribe()
	if notifyCh == nil {
		return nil, nil
	}
	defer s.state.Unsubscribe(notifyCh)

	// Re-check immediately after subscribing so a publication that committed
	// between the first check and Subscribe cannot be missed.
	lines, err := s.drainUnread(ctx, agent, from, limit, mentionsOnly)
	if err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		return lines, nil
	}

	timer := time.NewTimer(time.Duration(waitSec) * time.Second)
	defer timer.Stop()

	heartbeatInterval := min(time.Minute, max(time.Second, s.onlineWindow/2))
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, nil
		case <-heartbeat.C:
			if !s.isLiveAgent(agent) {
				return nil, nil
			}
			if err := s.touchAgent(ctx, agent, s.clock(), false); err != nil {
				return nil, err
			}
		case _, ok := <-notifyCh:
			if !ok || !s.isLiveAgent(agent) {
				return nil, nil
			}
			lines, err = s.drainUnread(ctx, agent, from, limit, mentionsOnly)
			if err != nil {
				return nil, err
			}
			if len(lines) > 0 {
				return lines, nil
			}
		}
	}
}

// drainUnread compares agent's watermarks against head sequence numbers in the
// in-memory replicated map and reads only targets where headSeq > curSeq. When
// mentionsOnly is true, channel targets where neither mention:<agent>:<ch> nor
// mention:all:<ch> exceeds curSeq are skipped in local memory without reading
// Redis or advancing watermarks.
func (s *swarmHandle) drainUnread(
	ctx context.Context,
	agent string,
	from string,
	limit int,
	mentionsOnly bool,
) ([]string, error) {
	snap := s.state.Map()
	targets := s.readTargets(agent, from)
	agentLower := strings.ToLower(agent)

	var lines []string
	var updates []cursorUpdate

	for _, target := range targets {
		headSeq := parseSeq(snap[headMapKey(target)])
		if headSeq <= 0 {
			continue
		}
		curSeq := parseSeq(snap[cursorMapKey(agent, target)])
		if headSeq <= curSeq {
			continue
		}
		if mentionsOnly && strings.HasPrefix(target, "#") {
			mentionSeq := max(
				parseSeq(snap[mentionMapKey(agentLower, target)]),
				parseSeq(snap[mentionMapKey("all", target)]),
			)
			if mentionSeq <= curSeq || headSeq < mentionSeq {
				continue
			}
		}

		events, err := s.snapshotTarget(ctx, target)
		if err != nil {
			return nil, err
		}
		targetLines, newSeq, err := formatDeltaLines(agent, target, curSeq, limit, events)
		if err != nil {
			return nil, err
		}
		lines = append(lines, targetLines...)
		if newSeq > curSeq {
			updates = append(updates, cursorUpdate{
				key: cursorMapKey(agent, target),
				val: strconv.Itoa(newSeq),
			})
		}
	}

	for i, u := range updates {
		if i == len(updates)-1 {
			if _, err := s.state.SetAndWait(ctx, u.key, u.val); err != nil {
				return nil, fmt.Errorf("commit cursor %s: %w", u.key, err)
			}
			continue
		}
		if _, err := s.state.Set(ctx, u.key, u.val); err != nil {
			return nil, fmt.Errorf("update cursor %s: %w", u.key, err)
		}
	}
	return lines, nil
}

// readTargets returns the ordered list of target streams to check for unread
// messages. When from is empty, direct messages ("@<agent>") are checked first,
// followed by subscribed channels in deterministic sorted order.
func (s *swarmHandle) readTargets(agent, from string) []string {
	if from != "" {
		return []string{from}
	}
	subs, _ := s.state.GetValues(subMapKey(agent))
	sortedSubs := slices.Clone(subs)
	slices.Sort(sortedSubs)
	targets := make([]string, 0, 1+len(sortedSubs))
	targets = append(targets, "@"+agent)
	targets = append(targets, sortedSubs...)
	return targets
}

// snapshotTarget reads the bounded event history of target using Pulse's
// generation-fenced Stream.Snapshot. If the stream has not been created yet or
// its previous generation was destroyed when a channel was removed,
// snapshotTarget returns an empty slice and nil error.
func (s *swarmHandle) snapshotTarget(ctx context.Context, target string) ([]streaming.SnapshotEvent, error) {
	stream, err := s.openStream(target)
	if err != nil {
		return nil, err
	}
	events, err := stream.Snapshot(ctx)
	if errors.Is(err, streaming.ErrStreamNotFound) || errors.Is(err, streaming.ErrStreamDestroyed) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot stream %s: %w", target, err)
	}
	return events, nil
}

// openStream constructs a local Pulse stream handle configured with the swarm's
// bounded retention and sliding TTL.
func (s *swarmHandle) openStream(target string) (*streaming.Stream, error) {
	stream, err := streaming.NewStream(
		streamName(s.name, target),
		s.rdb,
		options.WithStreamMaxLen(s.streamMaxLen),
		options.WithStreamSlidingTTL(s.swarmTTL),
	)
	if err != nil {
		return nil, fmt.Errorf("open stream for %s: %w", target, err)
	}
	return stream, nil
}
