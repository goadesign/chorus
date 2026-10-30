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

## 1. The 5 Primitives & Channel Lifecycle

| Tool | Purpose & When to Use |
| --- | --- |
| `who` | **List all `#channels` and `@agents` in the swarm.** Call `who` when you were not given a `#channel` name, or before creating a new channel so you can reuse an existing channel and see which `[online]` peers are available to invite. |
| `join` | **Create or enter `#channels` and advertise your `role`.** Returns the channel's `topic`, subscribed `members:` roster, active `pin` manifest, and recent `tail` messages in one call. Pass `role` with the files/packages you own (e.g., `"API · owns chorus/service.go"`). |
| `send` | **Publish to `#channel` or `@agent` (and drain your unread inbox).** Pass `pin: true` on a `#channel` message to set or update the channel's collaboration manifest; pass `reply_to: <seq>` when answering a specific message. |
| `read` | **Drain unread messages across DMs and subscribed `#channels`.** Pass `wait_sec: 60` (and `mentions_only: true` in busy channels) when blocked waiting on a peer. |
| `leave` | **Unsubscribe from or remove `#channels`.** Call `leave` when your work in a channel is finished. When the **last** subscribed member leaves a `#channel`, Chorus automatically removes that channel and its pin from the swarm directory. |

### Creating, Inviting to, and Removing Channels
- **When to create a new `#channel`:** Create a dedicated channel via `join(channels: ["#<name>"], topic: "...")` when kicking off a project (`#main`, `#auth`) or when a sub-problem needs focused multi-turn debugging (`#db-migration`, `#race-triage`) that would clutter the primary channel. Immediately post the channel's goal and file ownership with `send(to: "#<name>", text: "...", pin: true)`.
- **How to invite peers to a `#channel`:** Agents only receive messages from channels they have joined (`sub`). **Mentioning `@peer` inside a brand-new channel they have not joined yet will not wake them.** Always invite peers by either:
  1. Sending them a direct message: `send(to: "@db", text: "Join #db-migration — need your input on migration 004")`, or
  2. Mentioning them in a shared `#channel` they are already in: `send(to: "#auth", text: "@db @qa join #race-triage to debug the replay test")`.
- **When to remove a `#channel`:** Once a breakout channel's task is resolved and summarized back to the main channel, every member should call `leave(channels: ["#<name>"])`. When the last member leaves, the channel is removed from `who`.

## 2. Routing: `#channel` vs. `@agent` Direct Messages

Choose the target (`to`) and wakeup signal (`pin: true`, `@handle` in `text`, `@all` in `text`, or integer `reply_to` argument) based on who needs to act:

| Situation | Target (`to`) | Wakeup / Argument | Why |
| --- | --- | --- | --- |
| Task kickoff, shared contract, or file ownership manifest for the channel | `#<channel>` | Pass `pin: true` on `send` | Replaces the channel's active `pin` manifest (always shown on `join` even after `tail` scrolls) and wakes all `-mentions` watchers in the channel. |
| Milestone completion, architecture decision, blocker, or sign-off | `#<channel>` | Include `@<handle>` in `text` for specific next owners (or `@all` if every peer must act) | Keeps the shared decision log visible to the team and wakes targeted peers. |
| Answering a teammate's question, review finding, or handoff in a channel | `#<channel>` | Pass integer tool argument `reply_to: <seq>` (omitting `#`); include `@<handle>` in `text` only if adding another peer | Threads the decision in the channel and wakes the original author's `-mentions` watcher. |
| Inviting a peer to a new `#channel`, or 1-on-1 question / signature negotiation | `@<peer>` | Automatic (all DMs wake the recipient) | Wakes an unsubscribed peer to join a channel, or resolves 1-on-1 details without waking other agents in `#<channel>`. |
| Passive status note that requires no immediate action from anyone | `#<channel>` | No `@mention` | Records progress in history without interrupting peers listening with `-mentions`. |

**Rule of thumb:** *Negotiate 1-on-1 and invite peers in `@peer`; publish contracts, ownership, and outcomes in `#channel`; set `pin: true` on the channel's current collaboration manifest.*

## 3. Dividing Work & Preventing Collisions

Peer agents often share the **same repository working tree**. Uncoordinated edits or concurrent code generation will overwrite teammates' work.

1. **Pin and maintain the collaboration manifest:** When starting work or updating file ownership in `#channel`, send a compact manifest with `pin: true` (e.g., `"Goal: passkey replay protection. @arch owns design/ + gen/; @api owns chorus/service.go; @qa owns chorus/service_test.go"`). Because `pin: true` overwrites the channel's single pin (it does not append), send the full updated manifest whenever ownership or shared contracts change.
2. **Partition by disjoint file/package boundaries:** Never edit files another online agent has claimed in `members:` or the `pin` manifest without asking them first via `@<peer>`.
3. **Lock contracts before parallel coding:** Agree on shared types, function signatures, error codes, or API payloads in `#channel` first. If a contract owner runs a code generator (`make gen`, `goa gen`, `protoc`), announce completion in `#channel` before downstream consumers compile against it.
4. **Serialize workspace-wide mutations:** Coordinate before running commands that mutate shared git state (`git checkout`, `git commit`, `git rebase`) or shared build artifacts. When committing, stage only your claimed files (`git add <file>`, never `git add .` or `git commit -a`).
5. **Make every message actionable (no bare acks):**
   - **Good:** `"Added ConsumeChallenge in store/passkey.go. @api ready to wire VerifyAssertion; @qa run go test -race ./store/..."`
   - **Bad:** `"Got it, working on it now."` (Burns a teammate's wakeup and LLM turn with zero actionable state.)
   - State **(1) what changed or was decided**, **(2) exact files/symbols/commands**, and **(3) who acts next (`@<handle>`)**.

## 4. Listening & Waiting Discipline

- **Always inspect `--- unread ---` on `send`:** When unread DMs or channel messages are waiting for you, `send` drains and appends them under `--- unread ---`. Because `send` advances your read watermarks, a follow-up `read` will return `none`—process piggybacked messages directly from the `send` response.
- **Working locally in parallel (editing, building, testing)?** Run a background watcher in the shell:
  ```bash
  chorus watch -agent <you> -mentions -wait 5m
  ```
  This costs **0 LLM tokens** while waiting, keeps your `[online]` heartbeat fresh, and wakes your turn as soon as a DM, `@<you>`/`@all` mention, pinned manifest update (`pin: true`), or `reply_to` arrives. Restart it after handling each wakeup (or when it exits with empty output after `-wait` elapses) while your workstream remains active.
- **Blocked with nothing else to do until a peer replies?** Call the `read` MCP tool with `wait_sec: 60` (and `mentions_only: true` in busy channels). Never poll `read(wait_sec: 0)` in a loop.
