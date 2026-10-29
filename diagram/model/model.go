// Package model defines the C4 architecture model for Chorus, including the
// LLM coding agents, the stateless Goa-AI MCP server container, and the backing
// Pulse/Redis distributed primitives.
package model

import (
	. "goa.design/model/dsl"
)

var _ = Design("Chorus Architecture", "Token-minimal multi-agent coordination MCP server built on Goa-AI and Pulse.", func() {
	Version("0.1.0")

	SoftwareSystem("LLM Coding Agents", "Peer coding agents (@lead, @api, @db) coordinating work via MCP 2026-07-28 tool calls at POST /mcp.", func() {
		External()
		Tag("Software System", "Agent")
		Uses("Chorus/Chorus MCP Server", "Calls who, join, leave, send, and read; receives piggybacked unread deltas", "MCP 2026-07-28 JSON-RPC / HTTP (:9440)", Synchronous)
	})

	SoftwareSystem("Chorus", "Multi-agent coordination MCP server backed by Pulse replicated maps, streams, and semaphores.", func() {
		Tag("Software System", "Chorus")

		Container(
			"Chorus MCP Server",
			"Stateless HTTP server (:9440) serving POST /mcp (who, join, leave, send, read) with an in-memory Pulse rmap replica.",
			"Go, Goa-AI & Pulse",
			func() {
				Tag("Container", "Server")
				URL("https://github.com/goadesign/chorus")
				Uses("Chorus/Swarm Control Plane", "Replicates state in memory (0ms directory reads & Subscribe wakeups)", "Redis Lua + Pub/Sub", Asynchronous)
				Uses("Chorus/Message Streams", "Appends messages and snapshots unread deltas when head > watermark", "Redis XADD / XRANGE", Synchronous)
				Uses("Chorus/Publication Lock", "Acquires 1-slot lease to linearize sequence numbers and head updates", "Pulse Semaphore", Synchronous)
			},
		)

		Container(
			"Swarm Control Plane",
			"Replicated hash map holding agent presence, channel topics, subscriptions, sequence counters, stream heads, and read watermarks.",
			"Redis & Pulse rmap",
			func() {
				Tag("Container", "Database", "ControlPlane")
			},
		)

		Container(
			"Message Streams",
			"Bounded append-only streams storing channel (#channel) and direct (@agent) messages with sliding TTL.",
			"Redis Streams & Pulse streaming",
			func() {
				Tag("Container", "Event Queue", "DataPlane")
			},
		)

		Container(
			"Publication Lock",
			"1-slot leased semaphore serializing sequence numbers.",
			"Redis & Pulse semaphore",
			func() {
				Tag("Container", "Lock")
			},
		)
	})
})
