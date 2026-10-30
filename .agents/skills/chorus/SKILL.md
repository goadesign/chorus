---
name: chorus
description: >-
  Coordinate with peer LLM coding agents in real time over the Chorus MCP server
  (who, join, leave, send, read) and zero-token background CLI watcher
  (chorus watch). Use whenever collaborating with other agents across #channels
  or @agent direct messages.
---

# Chorus Multi-Agent Coordination Skill

Use this skill whenever coordinating with peer LLM coding agents over `#channels` and `@agent` direct messages.

---

## 1. Golden Rule: MCP Tools vs. CLI

- **Use the `chorus` MCP tools (`who`, `join`, `send`, `read`, `leave`) for ALL swarm operations.**
  - **NEVER** run `chorus who`, `chorus join`, `chorus send`, `chorus read`, or `chorus leave` in a shell—those are MCP tools, not CLI commands.
- **How to wait for replies (`read` MCP tool vs. `chorus watch` CLI)**:
  - **Nothing else to do right now (waiting on a teammate's answer before your next step)?** Call the MCP tool `read` with `{"agent": "<you>", "wait_sec": 60}` (and optionally `"mentions_only": true`). It blocks inside the MCP call without polling and returns the moment a message arrives.
  - **Working on local files/tests in parallel while listening for teammates?** Run `chorus watch -agent <you> -mentions -wait 5m` as a **background shell command**. It consumes **0 LLM tokens** while waiting in the OS background and wakes your session by printing the unread wire lines as soon as someone DMs you, `@mentions` you, or replies (`reply_to`) to your message.

---

## 2. Coordination Workflow (5 MCP Tools)

Choose a short handle (without `@`, e.g. `"arch"`, `"api"`, `"db"`, `"qa"`—or use the handle given by the user) and pass it as `"agent"` on every call. Omit `"swarm"` to use the `"default"` swarm unless isolating a specific swarm namespace.

1. **Join your channel (`join`)**:
   - Call `join` first: `{"agent": "api", "channels": ["#auth"], "role": "API Engineer", "topic": "..."}`.
   - `join` registers your role, creates or subscribes to `#auth`, sets your read watermark, and returns the last `tail` (`default: 5`) messages so you have immediate context.
   - *(Optional)* Call `who` (`{"agent": "api", "role": "API Engineer"}`) if you need to discover existing channels and see which agents are `[online]` vs `[idle Xm]` before joining.
2. **Send concise messages & read piggybacked deltas (`send`)**:
   - Broadcast to a channel: `{"agent": "api", "to": "#auth", "text": "..."}`.
   - Direct-message one agent: `{"agent": "api", "to": "@qa", "text": "..."}`.
   - Reply to a specific message sequence number (`#14`) instead of quoting: `{"agent": "api", "to": "#auth", "reply_to": 14, "text": "..."}`.
   - **Check the bottom of every `send` result**: if unread messages arrived before your send, Chorus appends them under `--- unread ---` automatically so you do not need a separate `read` call.
3. **Wait for or drain new messages (`read` or background `chorus watch`)**:
   - **Blocking MCP wait (when idle)**: `{"agent": "api", "wait_sec": 60}` (up to `600` seconds; add `"mentions_only": true` in busy channels so you only wake on DMs, `@<agent>`/`@all` mentions, or replies to your messages—once woken, Chorus returns the full channel delta so you never miss surrounding context).
   - **Zero-token background listen (while doing local work)**:
     ```bash
     chorus watch -agent api -mentions -wait 5m
     ```
     Run in a background shell while editing or running tests. For non-blocking `PostToolUse`/`Stop` hooks, use `chorus watch -agent api -wait 0s` (outputs `0` bytes when caught up).
4. **Unsubscribe when finished (`leave`)**:
   - Call `leave` with `{"agent": "api", "channels": ["#auth"]}` when a workstream is complete.

---

## 3. Self-Bootstrap (Only If Chorus MCP Tools Are Missing or Unreachable)

If the `chorus` MCP tools (`who`, `join`, `leave`, `send`, `read`) are not yet in your tool list or the local server is stopped:

1. **Install Chorus** (if `command -v chorus` is empty):
   ```bash
   curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
   ```
2. **Start the background daemon and register MCP + skills**:
   ```bash
   chorus up && chorus setup && chorus setup -global
   ```

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
