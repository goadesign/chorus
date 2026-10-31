// Package design declares the Goa and Goa-AI MCP 2026-07-28 contract for the
// Chorus multi-agent coordination service.
//
// Chorus exposes five tools (who, join, leave, send, read) at a single static
// endpoint (POST /mcp). Agents specify their handle ("agent") and swarm name
// ("swarm") when calling join or who, which binds the handle to that swarm so
// send, read, and leave require only "agent". All tools return TextResult via
// ToolContent("content") so the generated MCP server omits outputSchema and
// structuredContent on the wire.
//
// A separate read-only HTTP service ("view") serves the browser dashboard on
// the same listener: GET /swarms lists swarms that have registered agents, and
// GET /swarms/{swarm}/view returns one swarm's agents, channels, direct-message
// inboxes, and retained messages as JSON. It never registers a handle, binds a
// handle to a swarm, or moves a read position, and it is not an MCP tool, so
// agents never see it in tools/list.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

var _ = API("chorus", func() {
	Title("Chorus Multi-Agent Coordination Server")
	Description("Token-optimal multi-agent coordination MCP server built on Goa-AI and Pulse.")
	Version("0.5.1")
})

// TextBlock is the single text content item required by ToolContent("content").
// Declaring only the text branch inside the content union satisfies Goa-AI's
// MCP content validator while generating a minimal text-only converter.
var TextBlock = Type("TextBlock", func() {
	Description("Single MCP text content item.")
	OneOf("content", "Text content union mapped to MCP content.", func() {
		Attribute("text", func() {
			Description("Plain-text wire response.")
			Attribute("text", String, "Compact line-oriented Chorus wire text.", func() {
				Example("sent #arch#14")
			})
			Required("text")
		})
	})
	Required("content")
})

// TextResult wraps TextBlock as the sole result field of every Chorus tool so
// mcpcontract.ToolResult projects structured output to expr.Empty.
var TextResult = Type("TextResult", func() {
	Description("Text-only MCP tool result.")
	Attribute("content", ArrayOfRequired(TextBlock), "MCP text content blocks.", func() {
		MinLength(1)
	})
	Required("content")
})

var _ = Service("chorus", func() {
	Description("Chorus coordinates swarms of LLM agents over Pulse replicated maps and streams.")

	MCP("chorus", "1.0.0")

	JSONRPC(func() {
		POST("/mcp")
	})

	Method("who", func() {
		Description("List active participants, channels, subscriptions, and unread counts in the swarm.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("swarm", String, "Swarm name: use the current git repository or workspace directory basename (e.g. 'chorus') unless the user specifies a swarm name.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("chorus")
			})
			Attribute("role", String, "Optional role description to advertise to peers in this swarm.", func() {
				MaxLength(120)
				Example("API Engineer · owns chorus/service.go")
			})
			Required("agent", "swarm")
		})
		Result(TextResult)
		Tool("who", "List active participants (@agent), channels (#channel), and unread counts in the swarm (call join first when you already know which #channel to enter).", func() {
			ToolContent("content")
			ReadOnlyHint(true)
			IdempotentHint(true)
		})
		JSONRPC(func() {})
	})

	Method("join", func() {
		Description("Create or subscribe to one or more channels and return their topic, members, pinned manifest, and recent messages.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("swarm", String, "Swarm name: use the current git repository or workspace directory basename (e.g. 'chorus') unless the user specifies a swarm name.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("chorus")
			})
			Attribute("channels", ArrayOf(String), "Channels to create or subscribe to (each prefixed with '#').", func() {
				MinLength(1)
				MaxLength(20)
				Elem(func() {
					Pattern(`^#[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
					Example("#arch")
				})
				Example([]string{"#arch"})
			})
			Attribute("topic", String, "Optional channel topic to set on the joined channel(s).", func() {
				MaxLength(160)
				Example("Schema and API contract decisions")
			})
			Attribute("role", String, "Optional role description to advertise for the calling agent.", func() {
				MaxLength(120)
				Example("API Engineer · owns chorus/service.go")
			})
			Attribute("tail", Int, "Maximum recent messages to return per joined channel.", func() {
				Default(5)
				Minimum(0)
				Maximum(50)
				Example(5)
			})
			Required("agent", "swarm", "channels")
		})
		Result(TextResult)
		Tool("join", "Subscribe to (or create) #channels in the specified swarm, advertise your role, and return the channel topic, subscribed members, pinned manifest, and recent message tail. Call this first when joining a swarm.", func() {
			ToolContent("content")
			DestructiveHint(false)
			IdempotentHint(true)
		})
		JSONRPC(func() {})
	})

	Method("leave", func() {
		Description("Unsubscribe from one or more channels (removing a channel from the swarm directory when its last member leaves).")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("channels", ArrayOf(String), "Channels to unsubscribe from (each prefixed with '#').", func() {
				MinLength(1)
				MaxLength(20)
				Elem(func() {
					Pattern(`^#[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
					Example("#arch")
				})
				Example([]string{"#arch"})
			})
			Required("agent", "channels")
		})
		Result(TextResult)
		Tool("leave", "Unsubscribe from #channels. When the last subscribed member leaves a #channel, the channel is removed from the swarm directory.", func() {
			ToolContent("content")
			DestructiveHint(false)
			IdempotentHint(true)
		})
		JSONRPC(func() {})
	})

	Method("send", func() {
		Description("Send a message to @agent or #channel, atomically return any unread messages for the caller, and optionally wait for a reply.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("to", String, "Target '@agent' for direct messages or '#channel' for channel messages.", func() {
				Pattern(`^(@|#)[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("#arch")
			})
			Attribute("text", String, "Message text to send.", func() {
				MinLength(1)
				MaxLength(4000)
				Example("migration 004 applied cleanly")
			})
			Attribute("reply_to", Int, "Optional sequence number in the target conversation being replied to.", func() {
				Minimum(1)
				Example(14)
			})
			Attribute("pin", Boolean, "When true on a #channel message, pins this message as the channel's active collaboration manifest (replacing any previous pin) so joining agents always see it.", func() {
				Default(false)
				Example(true)
			})
			Attribute("wait_sec", Int, "Optional seconds to block waiting for a reply, @mention, @all, or direct message after sending (0 returns immediately).", func() {
				Default(0)
				Minimum(0)
				Maximum(600)
				Example(60)
			})
			Required("agent", "to", "text")
		})
		Result(TextResult)
		Tool("send", "Send a message to @agent or #channel and atomically return any unread messages under '--- unread ---'. Leave wait_sec=0 when continuing local work; set wait_sec (e.g. 60) when asking a question or handing off work and waiting for a reply. Set pin=true on a #channel message to pin or update the channel's collaboration manifest.", func() {
			ToolContent("content")
			DestructiveHint(false)
		})
		JSONRPC(func() {})
	})

	Method("read", func() {
		Description("Drain unread messages across subscribed channels and direct messages, optionally waiting for new arrivals.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("from", String, "Optional channel filter ('#channel'); omit to read across all subscribed channels and direct messages.", func() {
				Pattern(`^#[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("#arch")
			})
			Attribute("wait_sec", Int, "Seconds to block waiting if no unread messages are immediately available.", func() {
				Default(0)
				Minimum(0)
				Maximum(600)
				Example(20)
			})
			Attribute("mentions_only", Boolean, "When true, only wake and drain when a direct message, @agent/@all mention, or reply to your message is unread.", func() {
				Default(false)
				Example(true)
			})
			Attribute("limit", Int, "Maximum unread messages to return per target.", func() {
				Default(20)
				Minimum(1)
				Maximum(50)
				Example(20)
			})
			Required("agent")
		})
		Result(TextResult)
		Tool("read", "Read unread messages across subscribed #channels and @direct messages. Set wait_sec (e.g. 60) when blocked waiting on a peer; or run 'chorus watch <agent>' in a background shell to listen at 0 token cost while doing local work.", func() {
			ToolContent("content")
			ReadOnlyHint(false)
		})
		JSONRPC(func() {})
	})
})

// MessageType is one retained channel post or direct message as stored in its
// Pulse stream.
var MessageType = Type("Message", func() {
	Description("A retained channel post or direct message.")
	Attribute("seq", Int, "Sequence number within its channel or inbox (1, 2, ...).", func() {
		Minimum(1)
		Example(14)
	})
	Attribute("sender", String, "Handle of the agent that sent the message (without '@').", func() {
		Example("db")
	})
	Attribute("text", String, "Message body exactly as sent.", func() {
		Example("migration 004 applied")
	})
	Attribute("reply_to", Int, "Sequence number the sender passed as reply_to. Absent when the message is not a reply. In a direct-message inbox the number is not guaranteed to refer to this inbox.", func() {
		Minimum(1)
		Example(12)
	})
	Attribute("sent_at_ms", Int64, "Unix time in milliseconds when Redis stored the message, taken from its stream entry ID.", func() {
		Example(1793467612968)
	})
	Required("seq", "sender", "text", "sent_at_ms")
})

// Pin is a channel's active pinned message. Chorus stores the pinned message
// without its stream entry ID, so a pin has no send time.
var Pin = Type("Pin", func() {
	Description("The channel's active pinned message.")
	Attribute("seq", Int, "Sequence number of the pinned message in its channel.", func() {
		Minimum(1)
		Example(1)
	})
	Attribute("sender", String, "Handle of the agent that pinned the message.", func() {
		Example("lead")
	})
	Attribute("text", String, "Pinned message body exactly as sent.", func() {
		Example("Goal: single-use passkey challenges | Verify: go test -race ./...")
	})
	Required("seq", "sender", "text")
})

// Position is one channel member's read position.
var Position = Type("Position", func() {
	Description("A channel member's read position.")
	Attribute("handle", String, "Member handle (without '@').", func() {
		Example("api")
	})
	Attribute("read_seq", Int, "Sequence number up to which the member has read, sent, or joined.", func() {
		Minimum(0)
		Example(12)
	})
	Attribute("behind", Int, "Channel head_seq minus read_seq: messages since the member last read, sent, or joined.", func() {
		Minimum(0)
		Example(2)
	})
	Attribute("mentioned", Boolean, "True when the member was @-mentioned, @all-mentioned, or replied to after read_seq.")
	Required("handle", "read_seq", "behind", "mentioned")
})

// AgentType is one registered agent in a swarm.
var AgentType = Type("Agent", func() {
	Description("A registered agent.")
	Attribute("handle", String, "Agent handle (without '@').", func() {
		Example("api")
	})
	Attribute("role", String, "Role the agent advertised on join or who. Absent when none was given.", func() {
		Example("API Engineer")
	})
	Attribute("last_seen_unix", Int64, "Unix time in seconds of the agent's last Chorus call or watcher heartbeat.", func() {
		Example(1793467612)
	})
	Attribute("channels", ArrayOf(String), "Channels the agent is subscribed to, sorted.", func() {
		Example([]string{"#arch"})
	})
	Attribute("watching_on_this_server", Boolean, "True when this server process is holding a blocking read or chorus watch for the agent. Another server process may hold one this process cannot see.")
	Required("handle", "last_seen_unix", "channels", "watching_on_this_server")
})

// Channel is one channel with its pin, members' read positions, and retained
// messages.
var Channel = Type("Channel", func() {
	Description("A channel and its retained messages.")
	Attribute("name", String, "Channel name including '#'.", func() {
		Example("#arch")
	})
	Attribute("topic", String, "Channel topic. Absent when none was set.", func() {
		Example("Schema and API contract decisions")
	})
	Attribute("pin", Pin, "Active pinned message. Absent when the channel has no pin.")
	Attribute("head_seq", Int, "Sequence number of the latest message published to the channel.", func() {
		Minimum(0)
		Example(14)
	})
	Attribute("positions", ArrayOf(Position), "Read position of every subscribed member, sorted by handle.")
	Attribute("messages", ArrayOf(MessageType), "Every retained message, oldest first. Older messages may have been trimmed.")
	Required("name", "head_seq", "positions", "messages")
})

// Inbox is one agent's direct-message inbox: the messages other agents sent
// to that agent.
var Inbox = Type("Inbox", func() {
	Description("Direct messages sent to one recipient.")
	Attribute("recipient", String, "Handle the messages were sent to (without '@').", func() {
		Example("api")
	})
	Attribute("head_seq", Int, "Sequence number of the latest message in the inbox.", func() {
		Minimum(0)
		Example(3)
	})
	Attribute("read_seq", Int, "Sequence number up to which the recipient has read.", func() {
		Minimum(0)
		Example(2)
	})
	Attribute("behind", Int, "head_seq minus read_seq: messages since the recipient last read its inbox.", func() {
		Minimum(0)
		Example(1)
	})
	Attribute("messages", ArrayOf(MessageType), "Every retained message, oldest first. Older messages may have been trimmed.")
	Required("recipient", "head_seq", "read_seq", "behind", "messages")
})

// SwarmView is the complete read-only snapshot of one swarm returned to the
// browser dashboard.
var SwarmView = Type("SwarmView", func() {
	Description("Read-only snapshot of one swarm.")
	Attribute("swarm", String, "Swarm name.", func() {
		Example("chorus")
	})
	Attribute("agents", ArrayOf(AgentType), "Registered agents, sorted by handle.")
	Attribute("channels", ArrayOf(Channel), "Channels, sorted by name.")
	Attribute("inboxes", ArrayOf(Inbox), "Direct-message inboxes that hold at least one message, sorted by recipient.")
	Attribute("etag", String, "Hash of the response body. Send it back as If-None-Match to wait for the next change.")
	Required("swarm", "agents", "channels", "inboxes", "etag")
})

var _ = Service("view", func() {
	Description("Serves the read-only browser dashboard data. Only requests addressed to a loopback host are accepted; it never changes swarm state.")

	Error("not_modified", ErrorResult, "The swarm did not change before the wait ended.")
	Error("forbidden", ErrorResult, "The request's Host or Origin header is not allowed.")

	HTTP(func() {
		Response("not_modified", StatusNotModified, func() {
			Body(Empty)
		})
		Response("forbidden", StatusForbidden)
	})

	Method("swarms", func() {
		Description("List the swarms that currently have registered agents, sorted by name.")
		Result(func() {
			Attribute("swarms", ArrayOf(String), "Swarm names, sorted.", func() {
				Example([]string{"chorus"})
			})
			Required("swarms")
		})
		HTTP(func() {
			GET("/swarms")
			Response(StatusOK)
		})
	})

	Method("view", func() {
		Description("Return one swarm's agents, channels, inboxes, and retained messages. With if_none_match and wait, block until the snapshot differs or the wait ends.")
		Payload(func() {
			Attribute("swarm", String, "Swarm name.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("chorus")
			})
			Attribute("if_none_match", String, "ETag of the snapshot the caller already has.", func() {
				Example(`"3f2a9c"`)
			})
			Attribute("wait", Int, "Seconds to wait for a change when if_none_match matches the current snapshot.", func() {
				Default(0)
				Minimum(0)
				Maximum(60)
				Example(25)
			})
			Required("swarm")
		})
		Result(SwarmView)
		HTTP(func() {
			GET("/swarms/{swarm}/view")
			Header("if_none_match:If-None-Match")
			Param("wait")
			Response(StatusOK, func() {
				Header("etag:ETag")
			})
		})
	})
})
