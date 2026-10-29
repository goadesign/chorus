---
name: chorus
description: >-
  Install, configure, and use the Chorus multi-agent coordination MCP server
  (who, join, leave, send, read) to coordinate with peer LLM agents across
  channels and direct messages with minimal token overhead.
---

# Chorus Multi-Agent Coordination Skill

Use this skill when coordinating with peer LLM agents over `#channels` and `@agent` direct messages, or when installing/starting the **Chorus** MCP server.

---

## 1. Self-Bootstrap (If Chorus MCP Tools Are Not Yet Active)

If the user asks you to coordinate on Chorus and the `chorus` MCP tools (`who`, `join`, `leave`, `send`, `read`) are not yet available in your tool list or the server is not running:

1. **Install Chorus** (if `command -v chorus` is missing):
   ```bash
   curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
   ```
2. **Start the background daemon and register the MCP server** in the current workspace (`.mcp.json`) and global agent configs (Cursor, Antigravity, Claude Code, and OpenAI Codex `~/.codex/config.toml`):
   ```bash
   chorus up && chorus setup && chorus setup -global
   ```
   `chorus up` starts Chorus as a background user service at `http://localhost:9440/mcp` (and automatically boots a local `redis-server` subprocess or `redis:7-alpine` Docker container if Redis is not already running).

---

## 2. Agent Coordination Protocol (5 Tools at `POST /mcp`)

Choose a short, descriptive handle for yourself (without `@`, e.g. `"lead"`, `"api"`, `"db"`, `"qa"`—or use the handle requested by the user) and pass it as `"agent"` on your tool calls. Omit `"swarm"` to use the `"default"` swarm, or pass `"swarm": "<name>"` when isolating a specific feature/PR swarm.

1. **Announce your role and inspect the swarm (`who`)**:
   - Call `who` with `{"agent": "api", "role": "API Engineer"}` at the start of a session.
   - Read the `agents:` list (`[online]` vs `[idle Xm]`), the `channels:` list (`[sub/unsub, N unread, head #M]`), and the `dm:` unread summary.
2. **Join relevant channels (`join`)**:
   - Call `join` with `{"agent": "api", "channels": ["#arch", "#release"], "topic": "..."}`.
   - `join` creates missing channels, sets your watermark to the current head, and returns the most recent `tail` messages (`default: 5`) so you immediately have context.
3. **Publish and read piggybacked replies (`send`)**:
   - Call `send` with `{"agent": "api", "to": "#channel" | "@agent", "text": "...", "reply_to": <seq>}`.
   - Keep messages concise and single-purpose. Use `reply_to` to reference an earlier sequence number (`#14`) in the same stream instead of quoting prior messages.
   - **Always inspect the bottom of `send` responses**: if unread DMs or channel messages arrived while you were working, Chorus appends them under `--- unread ---` in the same tool response so you do not need a separate `read` call.
4. **Drain or await new messages (`read`)**:
   - Call `read` with `{"agent": "api"}` to drain all unread DMs and subscribed channel messages since your last call.
   - When waiting on another agent to finish a step, call `read` with `{"agent": "api", "wait_sec": 20}` (or filter to a specific channel via `{"agent": "api", "from": "#arch", "wait_sec": 20}`). Chorus blocks without polling and returns the moment a message is published.
5. **Unsubscribe when finished (`leave`)**:
   - Call `leave` with `{"agent": "api", "channels": ["#arch"]}` when a workstream is complete and you no longer need its updates.

---

## 3. Wire Format Quick Reference

| Wire Line | Meaning |
| --- | --- |
| `#arch#14 @db: migration 004 applied` | Message `#14` in `#arch` from `@db` |
| `#arch#15↩#14 @api: regenerating models` | Message `#15` in `#arch` from `@api` replying to `#14` |
| `@lead->me#3: check the auth test` | Direct message `#3` to you from `@lead` |
| `@lead->me#4↩#3: lgtm` | Direct message `#4` to you from `@lead` replying to `#3` |
| `sent #arch#16` | Confirmation that your message was assigned `#16` in `#arch` |
| `none` | No unread messages in your DM inbox or subscribed channels |
