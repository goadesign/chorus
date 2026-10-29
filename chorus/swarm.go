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
		// state is the in-memory replicated map ("chorus:<swarm>:state")
		// synchronized across replicas via Redis Hashes and Pub/Sub.
		state *rmap.Map
		// swarmTTL is the sliding expiration applied to the swarm's replicated
		// map and message streams.
		swarmTTL time.Duration
		// streamMaxLen is the maximum number of messages retained per channel
		// or direct-message stream.
		streamMaxLen int
		// onlineWindow is the duration since an agent's last activity within
		// which the agent is reported as "[online]".
		onlineWindow time.Duration
		// clock returns the current time for presence timestamps and heartbeats.
		clock func() time.Time
	}

	// cursorUpdate records a pending watermark update for an agent on a target stream.
	cursorUpdate struct {
		// key is the rmap key ("cur:<agent>:<target>").
		key string
		// val is the encoded integer sequence watermark.
		val string
	}
)

const (
	// prefixAgent prefixes agent presence records in the swarm rmap.
	prefixAgent = "agent:"
	// prefixChannel prefixes channel topic records in the swarm rmap.
	prefixChannel = "chan:"
	// prefixSub prefixes per-agent channel subscription lists in the swarm rmap.
	prefixSub = "sub:"
	// prefixSeq prefixes monotonic per-target sequence counters in the swarm rmap.
	prefixSeq = "seq:"
	// prefixHead prefixes latest published sequence numbers in the swarm rmap.
	prefixHead = "head:"
	// prefixMention prefixes latest channel mention/reply sequence numbers in the swarm rmap.
	prefixMention = "mention:"
	// prefixCursor prefixes per-agent read watermarks in the swarm rmap.
	prefixCursor = "cur:"

	// eventTypeMessage is the Pulse stream event name used for all Chorus messages.
	eventTypeMessage = "msg"

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
	mapName := fmt.Sprintf("chorus:%s:state", name)
	state, err := rmap.Join(ctx, mapName, rdb, rmap.WithSlidingTTL(swarmTTL))
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
	}, nil
}

// agentMapKey returns the rmap key storing presence metadata for agent.
func agentMapKey(agent string) string {
	return prefixAgent + agent
}

// channelMapKey returns the rmap key storing the topic for channel ("#name").
func channelMapKey(channel string) string {
	return prefixChannel + channel
}

// subMapKey returns the rmap key storing the JSON array of subscribed channels
// for agent.
func subMapKey(agent string) string {
	return prefixSub + agent
}

// seqMapKey returns the rmap key storing the monotonic sequence counter for
// target ("#channel" or "@agent").
func seqMapKey(target string) string {
	return prefixSeq + target
}

// headMapKey returns the rmap key storing the latest sequence number published
// to target ("#channel" or "@agent").
func headMapKey(target string) string {
	return prefixHead + target
}

// mentionMapKey returns the rmap key storing the latest sequence number in
// channel where handleLower (lowercase agent handle or "all") was @mentioned
// or replied to.
func mentionMapKey(handleLower, channel string) string {
	return prefixMention + handleLower + ":" + channel
}

// cursorMapKey returns the rmap key storing agent's read watermark on target
// ("#channel" or "@agent").
func cursorMapKey(agent, target string) string {
	return prefixCursor + agent + ":" + target
}

// streamName returns the Pulse stream name for target ("#channel" or "@agent")
// within swarm.
func streamName(swarm, target string) string {
	if strings.HasPrefix(target, "#") {
		return fmt.Sprintf("chorus:%s:chan:%s", swarm, strings.TrimPrefix(target, "#"))
	}
	return fmt.Sprintf("chorus:%s:dm:%s", swarm, strings.TrimPrefix(target, "@"))
}

// semaphoreName returns the Pulse semaphore name serializing publications to
// target ("#channel" or "@agent") within swarm.
func semaphoreName(swarm, target string) string {
	return fmt.Sprintf("chorus:%s:pub:%s", swarm, target)
}

// formatReadOutput renders a slice of unread message lines, or "none" when empty.
func formatReadOutput(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "\n")
}

// deduplicateStrings returns a new slice preserving the order of first appearance
// with duplicate strings removed.
func deduplicateStrings(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// close stops the swarm's replicated map subscription.
func (s *swarmHandle) close() {
	s.state.Close()
}

// touchAgent ensures the calling agent is registered in the swarm's replicated
// map and refreshes its role and LastSeenUnix timestamp when needed. When forceSync
// is true or the agent record is newly created or changes role, touchAgent waits
// for the local rmap replica to observe the committed revision.
func (s *swarmHandle) touchAgent(ctx context.Context, agent, role string, now time.Time, forceSync bool) error {
	key := agentMapKey(agent)
	raw, ok := s.state.Get(key)
	rec, valid := parseAgentRecord(raw)

	roleChanged := role != "" && role != rec.Role
	if role != "" {
		rec.Role = role
	}
	stale := now.Unix()-rec.LastSeenUnix >= int64(touchInterval.Seconds())
	if ok && valid && !roleChanged && !stale && !forceSync {
		return nil
	}

	rec.LastSeenUnix = now.Unix()
	encoded, err := encodeAgentRecord(rec)
	if err != nil {
		return err
	}
	if !ok || !valid || roleChanged || forceSync {
		if _, err := s.state.SetAndWait(ctx, key, encoded); err != nil {
			return fmt.Errorf("sync agent %q presence: %w", agent, err)
		}
		return nil
	}
	if _, err := s.state.Set(ctx, key, encoded); err != nil {
		return fmt.Errorf("update agent %q presence: %w", agent, err)
	}
	return nil
}

// directory returns the formatted swarm snapshot for caller after refreshing
// caller's presence and optional role.
func (s *swarmHandle) directory(ctx context.Context, caller, role string) (string, error) {
	now := s.clock()
	if err := s.touchAgent(ctx, caller, role, now, false); err != nil {
		return "", err
	}
	snap := s.state.Map()
	subs, _ := s.state.GetValues(subMapKey(caller))
	return formatDirectory(caller, now, s.onlineWindow, snap, subs), nil
}

// joinChannels subscribes agent to one or more channels, creating any that do
// not yet exist, setting their read watermarks to the current stream heads, and
// returning the formatted join summary along with up to tail recent messages per
// channel.
func (s *swarmHandle) joinChannels(
	ctx context.Context,
	agent string,
	channels []string,
	topic string,
	role string,
	tail int,
) (string, error) {
	unique := deduplicateStrings(channels)
	var sections []string

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

		lines, headSeq, err := s.tailTarget(ctx, ch, tail)
		if err != nil {
			return "", err
		}
		if headSeq > 0 {
			if _, err := s.state.Set(ctx, cursorMapKey(agent, ch), strconv.Itoa(headSeq)); err != nil {
				return "", fmt.Errorf("set cursor for %s: %w", ch, err)
			}
		}

		effectiveTopic := topic
		if effectiveTopic == "" {
			effectiveTopic, _ = s.state.Get(chKey)
		}
		header := fmt.Sprintf("joined %s (head #%d)", ch, headSeq)
		if effectiveTopic != "" {
			header += " - " + effectiveTopic
		}
		if len(lines) > 0 {
			header += "\n" + strings.Join(lines, "\n")
		}
		sections = append(sections, header)
	}

	if _, err := s.state.AppendUniqueValues(ctx, subMapKey(agent), unique...); err != nil {
		return "", fmt.Errorf("subscribe %q to channels: %w", agent, err)
	}
	if err := s.touchAgent(ctx, agent, role, s.clock(), true); err != nil {
		return "", err
	}
	return strings.Join(sections, "\n"), nil
}

// leaveChannels unsubscribes agent from one or more channels and removes their
// read watermarks from the swarm's replicated map.
func (s *swarmHandle) leaveChannels(ctx context.Context, agent string, channels []string) (string, error) {
	unique := deduplicateStrings(channels)
	for _, ch := range unique {
		if _, err := s.state.Delete(ctx, cursorMapKey(agent, ch)); err != nil {
			return "", fmt.Errorf("delete cursor for %s: %w", ch, err)
		}
	}
	if _, _, err := s.state.RemoveValues(ctx, subMapKey(agent), unique...); err != nil {
		return "", fmt.Errorf("unsubscribe %q from channels: %w", agent, err)
	}
	if err := s.touchAgent(ctx, agent, "", s.clock(), true); err != nil {
		return "", err
	}
	return "left " + strings.Join(unique, ", "), nil
}

// publish appends a message from sender to target ("#channel" or "@agent") under
// a per-target leased semaphore, commits the new head sequence to rmap via
// SetAndWait, releases the semaphore, and drains any unread messages waiting for
// sender across all subscribed channels and direct messages.
func (s *swarmHandle) publish(
	ctx context.Context,
	sender string,
	target string,
	text string,
	replyTo int,
) (string, error) {
	if err := s.touchAgent(ctx, sender, "", s.clock(), false); err != nil {
		return "", err
	}

	autoSubscribed := false
	if strings.HasPrefix(target, "#") {
		if _, ok := s.state.Get(channelMapKey(target)); !ok {
			if _, err := s.state.SetIfNotExists(ctx, channelMapKey(target), ""); err != nil {
				return "", fmt.Errorf("register channel %s: %w", target, err)
			}
		}
		subs, _ := s.state.GetValues(subMapKey(sender))
		if !slices.Contains(subs, target) {
			if _, err := s.state.AppendUniqueValues(ctx, subMapKey(sender), target); err != nil {
				return "", fmt.Errorf("auto-subscribe %q to %s: %w", sender, target, err)
			}
			autoSubscribed = true
		}
	}

	seq, err := s.appendUnderSemaphore(ctx, sender, target, text, replyTo, autoSubscribed)
	if err != nil {
		return "", err
	}

	unread, err := s.drainUnread(ctx, sender, "", defaultReadLimit, false, target, seq)
	if err != nil {
		return "", err
	}
	return formatSendOutput(target, seq, unread), nil
}

// appendUnderSemaphore acquires the 1-slot publication semaphore for target,
// increments the target sequence number, appends the message event to the
// target's Pulse stream, records any channel @mentions or reply targets in
// rmap, and commits the updated head sequence to rmap via SetAndWait before
// releasing the semaphore.
func (s *swarmHandle) appendUnderSemaphore(
	ctx context.Context,
	sender string,
	target string,
	text string,
	replyTo int,
	autoSubscribed bool,
) (int, error) {
	sem, err := semaphore.New(
		s.rdb,
		semaphoreName(s.name, target),
		1,
		semaphore.WithLeaseTTL(publishLeaseTTL),
		semaphore.WithHeartbeatInterval(publishHeartbeatInterval),
	)
	if err != nil {
		return 0, fmt.Errorf("create semaphore for %s: %w", target, err)
	}
	lease, err := sem.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire publish lease for %s: %w", target, err)
	}
	defer func() {
		_ = lease.Release(context.Background())
	}()

	seq, err := s.state.Inc(ctx, seqMapKey(target), 1)
	if err != nil {
		return 0, fmt.Errorf("increment sequence for %s: %w", target, err)
	}

	// When a sender auto-subscribes by posting to a channel without a prior join,
	// initialize its cursor to seq-1 so drainUnread advances to seq without
	// dumping pre-subscription channel history.
	if autoSubscribed && seq > 1 {
		if _, err := s.state.Set(ctx, cursorMapKey(sender, target), strconv.Itoa(seq-1)); err != nil {
			return 0, fmt.Errorf("initialize auto-subscribe cursor for %s: %w", target, err)
		}
	}

	var mentions []string
	if strings.HasPrefix(target, "#") {
		mentions = extractMentionedHandles(text)
		if replyTo > 0 {
			replyAuthor, authorErr := s.findMessageSender(ctx, target, replyTo)
			if authorErr != nil {
				return 0, authorErr
			}
			if replyAuthor != "" {
				authorLower := strings.ToLower(replyAuthor)
				if !slices.Contains(mentions, authorLower) {
					mentions = append(mentions, authorLower)
				}
			}
		}
	}

	payload, err := json.Marshal(messageEvent{
		Seq:     seq,
		Sender:  sender,
		Text:    text,
		ReplyTo: replyTo,
	})
	if err != nil {
		return 0, fmt.Errorf("encode message payload: %w", err)
	}

	stream, err := s.openStream(target)
	if err != nil {
		return 0, err
	}
	if _, err := stream.Add(ctx, eventTypeMessage, payload); err != nil {
		return 0, fmt.Errorf("append message to %s stream: %w", target, err)
	}

	senderLower := strings.ToLower(sender)
	for _, h := range mentions {
		if h == senderLower {
			continue
		}
		if _, err := s.state.Set(ctx, mentionMapKey(h, target), strconv.Itoa(seq)); err != nil {
			return 0, fmt.Errorf("record mention of %q in %s: %w", h, target, err)
		}
	}

	if _, err := s.state.SetAndWait(ctx, headMapKey(target), strconv.Itoa(seq)); err != nil {
		return 0, fmt.Errorf("commit head sequence for %s: %w", target, err)
	}
	return seq, nil
}

// findMessageSender returns the sender handle of the message with sequence
// number seq in target, or "" if not found in the retained stream history.
func (s *swarmHandle) findMessageSender(ctx context.Context, target string, seq int) (string, error) {
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
	if err := s.touchAgent(ctx, agent, "", s.clock(), false); err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}

	lines, err := s.drainUnread(ctx, agent, from, limit, mentionsOnly, "", 0)
	if err != nil {
		return "", err
	}
	if len(lines) > 0 || waitSec <= 0 {
		return formatReadOutput(lines), nil
	}

	notifyCh := s.state.Subscribe()
	if notifyCh == nil {
		return "none", nil
	}
	defer s.state.Unsubscribe(notifyCh)

	// Re-check immediately after subscribing so a publication that committed
	// between the first check and Subscribe cannot be missed.
	lines, err = s.drainUnread(ctx, agent, from, limit, mentionsOnly, "", 0)
	if err != nil {
		return "", err
	}
	if len(lines) > 0 {
		return formatReadOutput(lines), nil
	}

	timer := time.NewTimer(time.Duration(waitSec) * time.Second)
	defer timer.Stop()

	heartbeatInterval := max(time.Second, s.onlineWindow/2)
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "none", nil
		case <-heartbeat.C:
			if err := s.touchAgent(ctx, agent, "", s.clock(), false); err != nil {
				return "", err
			}
		case _, ok := <-notifyCh:
			if !ok {
				return "none", nil
			}
			lines, err = s.drainUnread(ctx, agent, from, limit, mentionsOnly, "", 0)
			if err != nil {
				return "", err
			}
			if len(lines) > 0 {
				return formatReadOutput(lines), nil
			}
		}
	}
}

// drainUnread compares agent's watermarks against head sequence numbers in the
// in-memory replicated map and reads only targets where headSeq > curSeq. When
// mentionsOnly is true, channel targets where neither mention:<agent>:<ch> nor
// mention:all:<ch> exceeds curSeq are skipped in local memory without reading
// Redis or advancing watermarks. When called from publish, excludeTarget and
// excludeSeq identify the message just sent by agent so it is not echoed back
// as unread while still advancing agent's watermark on excludeTarget.
func (s *swarmHandle) drainUnread(
	ctx context.Context,
	agent string,
	from string,
	limit int,
	mentionsOnly bool,
	excludeTarget string,
	excludeSeq int,
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

		// Fast path when draining right after publishing to target: if no other
		// message intervened between curSeq and excludeSeq, advance the cursor
		// without reading the stream from Redis.
		if target == excludeTarget && headSeq == excludeSeq && curSeq == excludeSeq-1 {
			updates = append(updates, cursorUpdate{
				key: cursorMapKey(agent, target),
				val: strconv.Itoa(excludeSeq),
			})
			continue
		}

		targetLines, newSeq, err := s.readTargetDelta(ctx, target, curSeq, limit, excludeTarget, excludeSeq)
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

// readTargetDelta reads messages with Seq > afterSeq from target's stream up to
// maxMessages, returning the formatted wire lines and the highest sequence number
// reached.
func (s *swarmHandle) readTargetDelta(
	ctx context.Context,
	target string,
	afterSeq int,
	maxMessages int,
	excludeTarget string,
	excludeSeq int,
) ([]string, int, error) {
	events, err := s.snapshotTarget(ctx, target)
	if err != nil {
		return nil, 0, err
	}

	var lines []string
	var lastSeq int
	for _, ev := range events {
		var msg messageEvent
		if err := json.Unmarshal(ev.Payload(), &msg); err != nil {
			return nil, 0, fmt.Errorf("decode message event %s in %s: %w", ev.ID(), target, err)
		}
		if msg.Seq <= afterSeq {
			continue
		}
		if target == excludeTarget && msg.Seq == excludeSeq {
			lastSeq = msg.Seq
			continue
		}
		if len(lines) >= maxMessages {
			break
		}
		lines = append(lines, formatMessageLine(target, msg))
		lastSeq = msg.Seq
	}
	return lines, lastSeq, nil
}

// tailTarget returns up to tail most recent formatted messages from target along
// with the current head sequence number of the stream.
func (s *swarmHandle) tailTarget(ctx context.Context, target string, tail int) ([]string, int, error) {
	headSeq := parseSeq(s.state.Map()[headMapKey(target)])
	events, err := s.snapshotTarget(ctx, target)
	if err != nil {
		return nil, 0, err
	}
	if len(events) == 0 {
		return nil, headSeq, nil
	}

	decoded := make([]messageEvent, 0, len(events))
	for _, ev := range events {
		var msg messageEvent
		if err := json.Unmarshal(ev.Payload(), &msg); err != nil {
			return nil, 0, fmt.Errorf("decode message event %s in %s: %w", ev.ID(), target, err)
		}
		decoded = append(decoded, msg)
		if msg.Seq >= headSeq {
			headSeq = msg.Seq
		}
	}

	if tail <= 0 {
		return nil, headSeq, nil
	}
	if len(decoded) > tail {
		decoded = decoded[len(decoded)-tail:]
	}
	lines := make([]string, 0, len(decoded))
	for _, msg := range decoded {
		lines = append(lines, formatMessageLine(target, msg))
	}
	return lines, headSeq, nil
}

// snapshotTarget reads the bounded event history of target using Pulse's
// generation-fenced Stream.Snapshot. If the stream has not been created yet,
// snapshotTarget returns an empty slice and nil error.
func (s *swarmHandle) snapshotTarget(ctx context.Context, target string) ([]streaming.SnapshotEvent, error) {
	stream, err := s.openStream(target)
	if err != nil {
		return nil, err
	}
	events, err := stream.Snapshot(ctx)
	if errors.Is(err, streaming.ErrStreamNotFound) {
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
