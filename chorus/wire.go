// Package chorus implements the Chorus multi-agent coordination service backed
// by Pulse replicated maps, bounded Redis streams, and leased semaphores.
//
// This file owns the compact line-oriented wire grammar returned to LLM agents
// and the internal JSON encodings stored in Pulse replicated maps and streams.
package chorus

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	genchorus "github.com/goadesign/chorus/gen/chorus"
	"goa.design/pulse/streaming"
)

type (
	// messageEvent is the canonical event payload stored in a Pulse stream for
	// a single channel post or direct message.
	messageEvent struct {
		// Seq is the dense, strictly monotonic sequence number (1, 2, ...)
		// assigned to this message within its target stream.
		Seq int `json:"seq"`
		// Sender is the handle of the agent that sent the message (without '@').
		Sender string `json:"sender"`
		// Text is the raw message body authored by the sender.
		Text string `json:"text"`
		// ReplyTo is the optional sequence number in the same target stream
		// that this message replies to. Zero means not a reply.
		ReplyTo int `json:"reply_to,omitempty"`
	}

	// agentRecord is the value stored under "agent:<handle>" in a swarm's
	// replicated map.
	agentRecord struct {
		// Role is the optional human-readable responsibility advertised by the agent.
		Role string `json:"role,omitempty"`
		// Model is the model the agent declared on its most recent join or who
		// ("human" for a person using the dashboard). Records written before
		// declaring a model was required have none.
		Model string `json:"model,omitempty"`
		// LastSeenUnix is the Unix timestamp in seconds when the agent last
		// invoked a Chorus operation.
		LastSeenUnix int64 `json:"ts"`
	}
)

const (
	// prefixAgent prefixes agent presence records in the swarm rmap.
	prefixAgent = "agent:"
	// prefixChannel prefixes channel topic records in the swarm rmap.
	prefixChannel = "chan:"
	// prefixPin prefixes active pinned channel message records in the swarm rmap.
	prefixPin = "pin:"
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
)

// formatDirectory renders the complete swarm snapshot for the calling agent in
// Chorus's compact line-oriented wire format. It reads only the provided
// in-memory replicated map snapshot and subscription list, performing no I/O.
//
// Example output:
//
//	me: @api (API Engineer)
//	agents:
//	@api (API Engineer)
//	@db (Database Specialist)
//	channels:
//	#arch [sub, 1 unread, head #2] - Schema and API contract decisions
//	dm: 0 unread
func formatDirectory(
	caller string,
	now time.Time,
	onlineWindow time.Duration,
	snap map[string]string,
	subs []string,
) string {
	var b strings.Builder

	callerRec, _ := parseAgentRecord(snap[agentMapKey(caller)])
	b.WriteString("me: @")
	b.WriteString(caller)
	if callerRec.Role != "" {
		b.WriteString(" (")
		b.WriteString(callerRec.Role)
		b.WriteString(")")
	}
	b.WriteByte('\n')

	var agents []string
	var channels []string
	for k, v := range snap {
		switch {
		case strings.HasPrefix(k, prefixAgent):
			if _, live := parseLiveAgentRecord(now, onlineWindow, v); live {
				agents = append(agents, strings.TrimPrefix(k, prefixAgent))
			}
		case strings.HasPrefix(k, prefixChannel):
			channels = append(channels, strings.TrimPrefix(k, prefixChannel))
		}
	}
	slices.Sort(agents)
	slices.Sort(channels)

	b.WriteString("agents:\n")
	if len(agents) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, handle := range agents {
			rec, _ := parseAgentRecord(snap[agentMapKey(handle)])
			b.WriteByte('@')
			b.WriteString(handle)
			if rec.Role != "" {
				b.WriteString(" (")
				b.WriteString(rec.Role)
				b.WriteString(")")
			}
			b.WriteByte('\n')
		}
	}

	b.WriteString("channels:\n")
	if len(channels) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, ch := range channels {
			topic := snap[channelMapKey(ch)]
			headSeq := parseSeq(snap[headMapKey(ch)])
			b.WriteString(ch)
			if slices.Contains(subs, ch) {
				curSeq := parseSeq(snap[cursorMapKey(caller, ch)])
				unread := max(0, headSeq-curSeq)
				fmt.Fprintf(&b, " [sub, %d unread, head #%d]", unread, headSeq)
			} else {
				fmt.Fprintf(&b, " [unsub, head #%d]", headSeq)
			}
			if topic != "" {
				b.WriteString(" - ")
				b.WriteString(topic)
			}
			b.WriteByte('\n')
		}
	}

	dmTarget := "@" + caller
	dmHeadSeq := parseSeq(snap[headMapKey(dmTarget)])
	dmCurSeq := parseSeq(snap[cursorMapKey(caller, dmTarget)])
	dmUnread := max(0, dmHeadSeq-dmCurSeq)
	if dmHeadSeq > 0 {
		fmt.Fprintf(&b, "dm: %d unread (head #%d)", dmUnread, dmHeadSeq)
	} else {
		fmt.Fprintf(&b, "dm: %d unread", dmUnread)
	}
	return b.String()
}

// formatChannelMembers renders the compact "members: @a (Role), @b (Role)"
// line for a joined channel from the in-memory replicated map snapshot and
// per-agent subscription sets.
func formatChannelMembers(
	channel string,
	snap map[string]string,
	subsByAgent map[string][]string,
) string {
	var members []string
	for agent, subs := range subsByAgent {
		if slices.Contains(subs, channel) {
			members = append(members, agent)
		}
	}
	slices.Sort(members)

	var b strings.Builder
	b.WriteString("members: ")
	if len(members) == 0 {
		b.WriteString("(none)")
		return b.String()
	}
	for i, handle := range members {
		if i > 0 {
			b.WriteString(", ")
		}
		rec, _ := parseAgentRecord(snap[agentMapKey(handle)])
		b.WriteByte('@')
		b.WriteString(handle)
		if rec.Role != "" {
			b.WriteString(" (")
			b.WriteString(rec.Role)
			b.WriteString(")")
		}
	}
	return b.String()
}

// formatMessageLine renders a single channel or direct message into its compact
// wire representation:
//   - Channel message: "#arch#14 @db: migration 004 applied"
//   - Channel reply:   "#arch#15↩#14 @api: regenerating models"
//   - Direct message:  "@lead->me#3: check the auth test"
//   - Direct reply:    "@lead->me#3↩#2: sounds good"
func formatMessageLine(target string, msg messageEvent) string {
	var b strings.Builder
	if strings.HasPrefix(target, "#") {
		b.WriteString(target)
		b.WriteByte('#')
		b.WriteString(strconv.Itoa(msg.Seq))
		if msg.ReplyTo > 0 {
			b.WriteString("↩#")
			b.WriteString(strconv.Itoa(msg.ReplyTo))
		}
		b.WriteString(" @")
		b.WriteString(msg.Sender)
		b.WriteString(": ")
		b.WriteString(msg.Text)
		return b.String()
	}
	b.WriteByte('@')
	b.WriteString(msg.Sender)
	b.WriteString("->me#")
	b.WriteString(strconv.Itoa(msg.Seq))
	if msg.ReplyTo > 0 {
		b.WriteString("↩#")
		b.WriteString(strconv.Itoa(msg.ReplyTo))
	}
	b.WriteString(": ")
	b.WriteString(msg.Text)
	return b.String()
}

// formatSendOutput formats the acknowledgement for a sent message alongside any
// unread messages drained during the same call.
func formatSendOutput(target string, seq int, unread []string) string {
	ack := fmt.Sprintf("sent %s#%d", target, seq)
	if len(unread) == 0 {
		return ack
	}
	return ack + "\n--- unread ---\n" + strings.Join(unread, "\n")
}

// textResult wraps a plain-text wire string in the generated Goa TextResult
// union structure expected by ToolContent("content").
func textResult(text string) *genchorus.TextResult {
	return &genchorus.TextResult{
		Content: []*genchorus.TextBlock{
			{
				Content: genchorus.NewContentText(&genchorus.ContentBranchText{
					Text: text,
				}),
			},
		},
	}
}

// encodeAgentRecord serializes an agentRecord to JSON for storage in rmap.
func encodeAgentRecord(rec agentRecord) (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("encode agent record: %w", err)
	}
	return string(b), nil
}

// parseAgentRecord decodes an agentRecord from its rmap JSON string.
func parseAgentRecord(raw string) (agentRecord, bool) {
	if raw == "" {
		return agentRecord{}, false
	}
	var rec agentRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return agentRecord{}, false
	}
	return rec, true
}

// parseLiveAgentRecord decodes raw as an agentRecord and reports whether the
// agent has been active within onlineWindow of now.
func parseLiveAgentRecord(now time.Time, onlineWindow time.Duration, raw string) (agentRecord, bool) {
	rec, valid := parseAgentRecord(raw)
	if !valid {
		return agentRecord{}, false
	}
	if rec.LastSeenUnix > 0 && now.Sub(time.Unix(rec.LastSeenUnix, 0)) > onlineWindow {
		return agentRecord{}, false
	}
	return rec, true
}

// parseSeq parses a positive integer sequence number stored under "head:<target>"
// or "cur:<agent>:<target>" in rmap. Empty or uninitialized values return 0.
func parseSeq(raw string) int {
	if raw == "" {
		return 0
	}
	seq, err := strconv.Atoi(raw)
	if err != nil || seq <= 0 {
		return 0
	}
	return seq
}

// extractMentionedHandles returns the deduplicated, lowercase agent handles
// (and "all") referenced as "@<handle>" tokens in text. Tokens preceded or
// followed by handle characters (such as "user@api.com") are ignored.
func extractMentionedHandles(text string) []string {
	var handles []string
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if i > 0 && isHandleChar(text[i-1]) {
			continue
		}
		start := i + 1
		if start >= len(text) || !isAlnumByte(text[start]) {
			continue
		}
		end := start + 1
		for end < len(text) && isHandleChar(text[end]) {
			end++
		}
		if end-start <= 32 {
			h := strings.ToLower(text[start:end])
			if !slices.Contains(handles, h) {
				handles = append(handles, h)
			}
		}
		i = end - 1
	}
	return handles
}

// isAlnumByte reports whether b is an ASCII letter or digit ([a-zA-Z0-9]).
func isAlnumByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// isHandleChar reports whether b is a valid agent handle byte ([a-zA-Z0-9_-]).
func isHandleChar(b byte) bool {
	return isAlnumByte(b) || b == '_' || b == '-'
}

// formatReadOutput renders a slice of unread message lines, or "none" when empty.
func formatReadOutput(lines []string) string {
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "\n")
}

// formatHandleList renders a sorted slice of agent handles as "@a, @b".
func formatHandleList(handles []string) string {
	slices.Sort(handles)
	prefixed := make([]string, len(handles))
	for i, h := range handles {
		prefixed[i] = "@" + h
	}
	return strings.Join(prefixed, ", ")
}

// formatWatchHint returns the 1-line background watcher reminder appended to
// join and send responses when the calling agent does not currently have an
// active blocking watch in flight.
func formatWatchHint(agent string) string {
	return "! watch: chorus watch " + agent
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

// hasChannelSubscribers reports whether any agent in subsByAgent is subscribed
// to channel.
func hasChannelSubscribers(channel string, subsByAgent map[string][]string) bool {
	for _, subs := range subsByAgent {
		if slices.Contains(subs, channel) {
			return true
		}
	}
	return false
}

// agentMapKey returns the rmap key storing presence metadata for agent.
func agentMapKey(agent string) string {
	return prefixAgent + agent
}

// channelMapKey returns the rmap key storing the topic for channel ("#name").
func channelMapKey(channel string) string {
	return prefixChannel + channel
}

// pinMapKey returns the rmap key storing the active pinned messageEvent JSON
// for channel ("#name").
func pinMapKey(channel string) string {
	return prefixPin + channel
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

// formatDeltaLines decodes snapshot events with Seq > afterSeq from target up
// to maxMessages (skipping messages authored by agent while still advancing the
// sequence watermark), returning the formatted wire lines and the highest
// sequence number reached.
func formatDeltaLines(
	agent string,
	target string,
	afterSeq int,
	maxMessages int,
	events []streaming.SnapshotEvent,
) ([]string, int, error) {
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
		if strings.EqualFold(msg.Sender, agent) {
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

// formatTailLines decodes the active pinned manifest line (if any) and up to
// tail most recent formatted messages from events (deduplicating the pinned
// message when it falls within the tail window) along with the current head
// sequence number of the stream.
func formatTailLines(
	target string,
	tail int,
	rawHead string,
	rawPin string,
	events []streaming.SnapshotEvent,
) ([]string, int, error) {
	headSeq := parseSeq(rawHead)

	var pinMsg messageEvent
	hasPin := false
	if rawPin != "" {
		if err := json.Unmarshal([]byte(rawPin), &pinMsg); err == nil && pinMsg.Seq > 0 {
			hasPin = true
			if pinMsg.Seq > headSeq {
				headSeq = pinMsg.Seq
			}
		}
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

	var lines []string
	if hasPin {
		lines = append(lines, "pin "+formatMessageLine(target, pinMsg))
	}
	if tail <= 0 || len(decoded) == 0 {
		return lines, headSeq, nil
	}
	if len(decoded) > tail {
		decoded = decoded[len(decoded)-tail:]
	}
	for _, msg := range decoded {
		if hasPin && msg.Seq == pinMsg.Seq {
			continue
		}
		lines = append(lines, formatMessageLine(target, msg))
	}
	return lines, headSeq, nil
}
