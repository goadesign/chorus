// Package model defines the C4 architecture model for Chorus, including the
// LLM coding agents, the stateless Goa-AI MCP server container, and the backing
// Pulse/Redis distributed primitives.
package model

import (
	. "goa.design/model/dsl"
)

var _ = Design("Chorus Architecture", "Token-minimal multi-agent coordination MCP server built on Goa-AI and Pulse.", func() {
	Version("1.1.0")

	SoftwareSystem("LLM Coding Agents", "Claude, ChatGPT, and Gemini agents (arch, api, db, qa) coordinating via MCP 2026-07-28 at POST /mcp.", func() {
		External()
		Tag("Software System", "Agent")
		Uses("Chorus/Chorus MCP Server", "Calls who, join, leave, send, and read; receives piggybacked unread deltas", "MCP 2026-07-28 JSON-RPC / HTTP (:9440)", Synchronous)
	})

	SoftwareSystem("Browser and Local Tools", "People and local applications that read swarm activity and post messages.", func() {
		External()
		Uses("Chorus/Chorus MCP Server", "Reads swarm activity and posts messages", "Loopback HTTP", Synchronous)
	})

	SoftwareSystem("Herdr Core", "Runs local agent terminals and reports their current status.", func() {
		External()
	})

	SoftwareSystem("Chorus", "Multi-agent coordination MCP server backed by Pulse replicated maps, streams, and semaphores.", func() {
		Tag("Software System", "Chorus")

		Container(
			"Chorus MCP Server",
			"Serves MCP tools and the dashboard, keeps a local view of swarm state, and wakes terminal-backed agents.",
			"Go, Goa-AI & Pulse",
			func() {
				URL("https://github.com/goadesign/chorus")
				Uses("Herdr Core", "Observes terminal status and wakes idle agents", "Local hc connect", Asynchronous)
				Uses("Chorus/Swarm Control Plane", "Replicates state in memory (0ms directory reads & Subscribe wakeups)", "Redis Lua + Pub/Sub", Asynchronous)
				Uses("Chorus/Message Streams", "Appends messages and snapshots unread deltas when head > watermark", "Redis XADD / XRANGE", Synchronous)
				Uses("Chorus/Publication Lock", "Acquires 1-slot lease to linearize sequence numbers and head updates", "Pulse Semaphore", Synchronous)
			},
		)

		Container(
			"Swarm Control Plane",
			"Replicated hash map holding agent presence, channel topics, pinned manifests, subscriptions, sequence counters, stream heads, and read watermarks.",
			"Redis & Pulse rmap",
			func() {
				Tag("Database")
			},
		)

		Container(
			"Message Streams",
			"Bounded append-only streams storing channel (#channel) and direct (@agent) messages with sliding TTL.",
			"Redis Streams & Pulse streaming",
			func() {
				Tag("Event Queue")
			},
		)

		Container(
			"Publication Lock",
			"1-slot leased lock serializing sequence numbers.",
			"Redis & Pulse semaphore",
			func() {
				Tag("Lock")
			},
		)
	})
})
