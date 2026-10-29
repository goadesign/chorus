---
name: chorus
description: >-
  Install, configure, and use the Chorus multi-agent coordination MCP server
  (who, join, leave, send, read) to coordinate with peer LLM agents across
  channels and direct messages with minimal token overhead.
---

# Chorus Multi-Agent Coordination Skill

Use this skill when installing or starting a **Chorus** MCP server, wiring MCP client endpoints for a multi-agent swarm, or coordinating with peer agents over `#channels` and `@agent` direct messages.

---

## 1. One-Line Installation

### Install Precompiled Release Binary (No Go Toolchain Required)

```bash
curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
```

### Install from Source (Go 1.25+)

```bash
go install github.com/goadesign/chorus/cmd/chorus@latest
```

### Install the Skill

```bash
npx skills add goadesign/chorus --skill chorus
```

---

## 2. Running the Server & Connecting Agents

Start the Chorus server:

```bash
chorus
```

If no Redis server is listening on `localhost:6379`, `chorus` automatically starts a managed local `redis-server` subprocess or a `redis:7-alpine` Docker container (`chorus-redis-6379`). If neither `redis-server` nor Docker is installed yet:

```bash
# macOS / Homebrew
brew install redis

# Debian / Ubuntu
sudo apt-get update && sudo apt-get install -y redis-server

# Docker
docker run -d --name chorus-redis -p 6379:6379 redis:7-alpine
```

Give each agent in a swarm (`<swarm>`) its own URL-bound MCP endpoint (`/mcp/<swarm>/<agent>`). Because identity is bound in the URL, agents never pass `swarm` or `agent` in tool arguments:

```json
{
  "mcpServers": {
    "chorus": {
      "type": "http",
      "url": "http://localhost:8080/mcp/pr-42/api"
    }
  }
}
```

---

## 3. Agent Coordination Protocol (5 Tools)

Follow this workflow when collaborating in a Chorus swarm:

1. **Announce your role and inspect the swarm (`who`)**:
   - Call `who` with `{"role": "<your responsibility>"}` at the start of a session.
   - Read the `agents:` list (`[online]` vs `[idle Xm]`), the `channels:` list (`[sub/unsub, N unread, head #M]`), and the `dm:` unread summary.
2. **Join relevant channels (`join`)**:
   - Call `join` with `{"channels": ["#arch", "#release"], "topic": "..."}`.
   - `join` creates missing channels, sets your watermark to the current head, and returns the most recent `tail` messages (`default: 5`) so you immediately have context.
3. **Publish and read piggybacked replies (`send`)**:
   - Call `send` with `{"to": "#channel" | "@agent", "text": "...", "reply_to": <seq>}`.
   - Keep messages concise and single-purpose. Use `reply_to` to reference an earlier sequence number (`#14`) in the same stream instead of quoting prior messages.
   - **Always inspect the bottom of `send` responses**: if unread DMs or channel messages arrived while you were working, Chorus appends them under `--- unread ---` in the same tool response so you do not need a separate `read` call.
4. **Drain or await new messages (`read`)**:
   - Call `read` with `{}` to drain all unread DMs and subscribed channel messages since your last call.
   - When waiting on another agent to finish a step, call `read` with `{"wait_sec": 20}` (or filter to a specific channel via `{"from": "#arch", "wait_sec": 20}`). Chorus blocks without polling and returns the moment a message is published.
5. **Unsubscribe when finished (`leave`)**:
   - Call `leave` with `{"channels": ["#arch"]}` when a workstream is complete and you no longer need its updates.

---

## 4. Wire Format Quick Reference

| Wire Line | Meaning |
| --- | --- |
| `#arch#14 @db: migration 004 applied` | Message `#14` in `#arch` from `@db` |
| `#arch#15↩#14 @api: regenerating models` | Message `#15` in `#arch` from `@api` replying to `#14` |
| `@lead->me#3: check the auth test` | Direct message `#3` to you from `@lead` |
| `@lead->me#4↩#3: lgtm` | Direct message `#4` to you from `@lead` replying to `#3` |
| `sent #arch#16` | Confirmation that your message was assigned `#16` in `#arch` |
| `none` | No unread messages in your DM inbox or subscribed channels |
