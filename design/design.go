// Package design declares the Goa and Goa-AI MCP 2026-07-28 contract for the
// Chorus multi-agent coordination service.
//
// Chorus exposes five tools (who, join, leave, send, read) at a single static
// endpoint (POST /mcp). Each tool takes the caller's agent handle ("agent") and
// an optional swarm namespace ("swarm", defaulting to "default"), and returns
// TextResult via ToolContent("content") so the generated MCP server omits
// outputSchema and structuredContent on the wire.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

var _ = API("chorus", func() {
	Title("Chorus Multi-Agent Coordination Server")
	Description("Token-optimal multi-agent coordination MCP server built on Goa-AI and Pulse.")
	Version("0.2.0")
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
			Attribute("swarm", String, "Swarm namespace (defaults to 'default').", func() {
				Default("default")
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("default")
			})
			Attribute("role", String, "Optional role description to advertise to peers in this swarm.", func() {
				MaxLength(80)
				Example("API Engineer")
			})
			Required("agent")
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
		Description("Create or subscribe to one or more channels and return their topic and recent messages.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("swarm", String, "Swarm namespace (defaults to 'default').", func() {
				Default("default")
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("default")
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
				MaxLength(80)
				Example("API Engineer")
			})
			Attribute("tail", Int, "Maximum recent messages to return per joined channel.", func() {
				Default(5)
				Minimum(0)
				Maximum(50)
				Example(5)
			})
			Required("agent", "channels")
		})
		Result(TextResult)
		Tool("join", "Subscribe to (or create) #channels, advertise your role, and return the channel topic and recent message tail. Call this first when joining a swarm.", func() {
			ToolContent("content")
			DestructiveHint(false)
			IdempotentHint(true)
		})
		JSONRPC(func() {})
	})

	Method("leave", func() {
		Description("Unsubscribe from one or more channels.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("swarm", String, "Swarm namespace (defaults to 'default').", func() {
				Default("default")
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("default")
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
		Tool("leave", "Unsubscribe from #channels.", func() {
			ToolContent("content")
			DestructiveHint(false)
			IdempotentHint(true)
		})
		JSONRPC(func() {})
	})

	Method("send", func() {
		Description("Send a message to @agent or #channel and atomically return any unread messages for the caller.")
		Payload(func() {
			Attribute("agent", String, "Your agent handle (without leading '@'), e.g. 'lead', 'api', or 'db'.", func() {
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
				Example("api")
			})
			Attribute("swarm", String, "Swarm namespace (defaults to 'default').", func() {
				Default("default")
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("default")
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
			Required("agent", "to", "text")
		})
		Result(TextResult)
		Tool("send", "Send a message to @agent or #channel and atomically return any unread messages under '--- unread ---'. Mention peers with @handle (or @all) to wake their watchers.", func() {
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
			Attribute("swarm", String, "Swarm namespace (defaults to 'default').", func() {
				Default("default")
				Pattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
				Example("default")
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
		Tool("read", "Read unread messages across subscribed #channels and @direct messages. Set wait_sec (e.g. 60) when blocked waiting on a peer; or run 'chorus watch -agent <handle> -mentions' in a background shell to listen at 0 token cost while doing local work.", func() {
			ToolContent("content")
			ReadOnlyHint(false)
		})
		JSONRPC(func() {})
	})
})
