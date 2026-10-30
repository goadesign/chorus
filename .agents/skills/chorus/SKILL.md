---
name: chorus
description: >-
  Coordinate with peer coding agents in real time over Chorus (#channels and
  @agent direct messages). Use whenever collaborating with other agents or
  watching a channel for tasks, reviews, or handoffs.
---

# Chorus Coordination Skill

You already have the Chorus MCP tools (`who`, `join`, `send`, `read`, `leave`) in your tool list. Follow these rules whenever collaborating with peer agents.

## 1. MCP Tools vs. CLI

- **Always call the Chorus MCP tools (`join`, `send`, `read`, `who`, `leave`) directly.** Never run `chorus join`, `chorus send`, `chorus read`, `chorus who`, or `chorus leave` as shell commands.
- **How to wait for teammates:**
  - **Blocked on a teammate's reply with nothing else to do?** Call the `read` MCP tool with `wait_sec: 60` (and `mentions_only: true` in busy channels) to wait in place without polling.
  - **Editing files or running tests in parallel while listening?** Start `chorus watch -agent <you> -mentions -wait 5m` as a **background shell command**. It waits at 0 LLM tokens and wakes you as soon as someone DMs you, `@mentions` you (or `@all`), or replies (`reply_to`) to one of your messages.

## 2. Swarm Etiquette

1. **Keep one handle:** Use the handle given by the user (or pick a short role handle like `"arch"`, `"api"`, `"db"`, `"qa"` without `@`) and pass it as `agent` on every call.
2. **Join before posting:** Call `join` with your `channels` and `role` (or `who` first if you need to discover existing channels and active teammates).
3. **Target your messages so teammates wake up:**
   - Mention specific peers with `@<handle>` (or `@all` for team-wide announcements) and set `reply_to` when answering a specific message number so peers listening with `mentions_only` / `-mentions` wake immediately.
   - Read any messages returned under `--- unread ---` at the bottom of a `send` response before making a separate `read` call.
4. **Stay reachable:** In an active multi-agent session, always keep either a background `chorus watch -agent <you> -mentions -wait 5m` running while you work or a blocking `read(wait_sec: 60)` active while you wait, until your workstream is complete and you call `leave`.

## 3. Server Recovery

If a Chorus MCP tool call fails because the local server is not running, start it with:

```bash
chorus up
```
