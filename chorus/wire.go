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
		// LastSeenUnix is the Unix timestamp in seconds when the agent last
		// invoked a Chorus operation.
		LastSeenUnix int64 `json:"ts"`
	}
)

// formatDirectory renders the complete swarm snapshot for the calling agent in
// Chorus's compact line-oriented wire format. It reads only the provided
// in-memory replicated map snapshot and subscription list, performing no I/O.
//
// Example output:
//
//	me: @api (API Engineer)
//	agents:
//	@api (API Engineer) [online]
//	@db (Database Specialist) [online]
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
	for k := range snap {
		switch {
		case strings.HasPrefix(k, prefixAgent):
			agents = append(agents, strings.TrimPrefix(k, prefixAgent))
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
			b.WriteString(" [")
			b.WriteString(formatLiveness(now, onlineWindow, rec.LastSeenUnix))
			b.WriteString("]\n")
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

// formatLiveness returns "online" when lastSeenUnix is within onlineWindow of
// now, or "idle <Xm>" / "idle <Xh>" when older.
func formatLiveness(now time.Time, onlineWindow time.Duration, lastSeenUnix int64) string {
	if lastSeenUnix <= 0 {
		return "online"
	}
	age := max(time.Duration(0), now.Sub(time.Unix(lastSeenUnix, 0)))
	if age <= onlineWindow {
		return "online"
	}
	mins := max(1, int(age.Minutes()))
	if mins < 60 {
		return fmt.Sprintf("idle %dm", mins)
	}
	return fmt.Sprintf("idle %dh", mins/60)
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
