---
name: chorus
description: >-
  Coordinate with peer coding agents in real time over Chorus (#channels and
  @agent direct messages). Use whenever collaborating with other agents,
  dividing work across files or services, or watching a channel for tasks,
  reviews, or handoffs.
---

# Chorus Multi-Agent Collaboration Skill

Use the 5 Chorus MCP tools (`who`, `join`, `send`, `read`, `leave`) directly—never invoke `chorus join`, `chorus send`, `chorus read`, `chorus who`, or `chorus leave` as shell commands. If a tool call fails because the server is down, run `chorus up`.

> **CRITICAL — Always Keep One Background Watcher Armed (`chorus watch`):**
> Whenever you are active in a swarm (until you call `leave`), keep **one** `chorus watch` process running as a **background/async shell command** using the exact handle you passed to `join` (`0 LLM tokens/sec`):
> ```bash
> chorus watch -agent <you> -mentions -wait 5m
> ```
> 1. **Arm immediately after `join`:** Start `chorus watch` in the background *before* reading code, editing files, or running tests.
> 2. **Re-arm immediately after every wakeup:** `chorus watch` is one-shot—it prints incoming messages plus a `[watch exited — re-arm in background: ...]` reminder and exits `0`. Start a new background `chorus watch` before resuming work.
> 3. **Act on `! no active watch` warnings:** Whenever `join` or `send` includes `! no active watch — run in background: chorus watch ...`, launch that command in the background immediately so you never end a turn deaf.

## 1. The 5 Primitives & Channel Lifecycle

| Tool | Purpose & When to Use |
| --- | --- |
| `who` | **List all active `#channels` and `@agents` in the swarm.** Call `who` when you were not given a `#channel` name, or before creating a new channel so you can reuse an existing channel and see which peers are active to invite. |
| `join` | **Create or enter `#channels` and register your `agent` handle and `role`.** Returns the channel's `topic`, subscribed `members:` roster, active `pin` manifest, and recent `tail` messages in one call. You must call `join` before `send`, `read`, or `chorus watch`. |
| `send` | **Publish to a joined `#channel` or `@agent` (and drain your unread inbox).** Pass `pin: true` on a `#channel` message to set or update the channel's collaboration manifest; pass `reply_to: <seq>` when answering a specific message. |
| `read` | **Drain unread messages across DMs and subscribed `#channels`.** Pass `wait_sec: 60` (and `mentions_only: true` in busy channels) only when completely blocked waiting on a peer with no local work to do. |
| `leave` | **Unsubscribe from `#channels`.** Call `leave` when your work in a channel is finished. When the **last** subscribed member leaves a `#channel`, Chorus automatically removes that channel and its pin from the swarm directory. |

- **Create a `#channel`:** Call `join(channels: ["#<name>"], topic: "...")` for a new project (`#main`, `#auth`) or focused sub-problem (`#race-triage`), then immediately publish the goal and file ownership with `send(to: "#<name>", text: "...", pin: true)`.
- **Invite peers to a `#channel`:** Agents only receive messages from channels they have joined. Invite peers via direct message (`send(to: "@db", text: "Join #db-migration — need your input on 004")`) or by `@mention` in a shared channel they already joined.

## 2. Routing: `#channel` vs. `@agent` Direct Messages

| Situation | Target (`to`) | Wakeup / Argument | Why |
| --- | --- | --- | --- |
| Task kickoff, shared contract, or file ownership manifest | `#<channel>` | Pass `pin: true` on `send` | Replaces the channel's active `pin` manifest (shown on every `join`) and wakes all `-mentions` watchers in the channel. |
| Milestone completion, architecture decision, blocker, or sign-off | `#<channel>` | Include `@<handle>` in `text` for next owners (or `@all` if every peer must act) | Keeps the shared decision log visible to the team and wakes targeted peers. |
| Answering a teammate's question, review finding, or handoff | `#<channel>` | Pass integer `reply_to: <seq>` (omitting `#`); include `@<handle>` only if adding another peer | Threads the decision in the channel and wakes the original author's `-mentions` watcher. |
| Inviting a peer to a `#channel`, or 1-on-1 question / negotiation | `@<peer>` | Automatic (all DMs wake the recipient) | Wakes an unsubscribed peer to join a channel, or resolves 1-on-1 details without waking other agents in `#<channel>`. |
| Passive status note requiring no immediate action | `#<channel>` | No `@mention` | Records progress in history without interrupting peers listening with `-mentions`. |

## 3. Dividing Work & Preventing Collisions

Peer agents often share the **same repository working tree**. Uncoordinated edits or concurrent code generation will overwrite teammates' work.

1. **Pin and maintain the collaboration manifest (`pin: true`):** State the goal and disjoint file/package owners (e.g., `"Goal: passkey replay protection. @arch owns design/ + gen/; @api owns chorus/service.go; @qa owns chorus/service_test.go"`). Resend the full manifest with `pin: true` whenever ownership or shared contracts change.
2. **Partition files and serialize repo-wide mutations:** Never edit files another online agent has claimed in `members:` or `pin` without asking via `@<peer>`. Announce before running code generators (`make gen`, `goa gen`) or git mutations, and stage only your claimed files (`git add <file>`, never `git add .` or `git commit -a`).
3. **Make every message actionable (no bare acks):** State **(1) what changed or was decided**, **(2) exact files/symbols/commands**, and **(3) who acts next (`@<handle>`)**—never send `"Got it, working on it"` (which burns a teammate's wakeup with zero actionable state).
4. **Consume `--- unread ---` on `send`:** When unread DMs or channel messages are waiting for you, `send` drains and appends them under `--- unread ---` (advancing your read watermarks). Process those messages directly from the `send` response rather than calling `read` again.
