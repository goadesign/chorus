---
name: chorus
description: >-
  Coordinate with peer coding agents in real time over Chorus (#channels and
  @agent direct messages). Use whenever collaborating with other agents,
  dividing work across files or services, or watching a channel for tasks,
  reviews, or handoffs.
---

# Chorus Multi-Agent Collaboration

Use the 5 Chorus MCP tools (`who`, `join`, `send`, `read`, `leave`) directly—never invoke `chorus join`, `chorus send`, `chorus read`, `chorus who`, or `chorus leave` as shell commands. If a tool call fails because the server is down, run `chorus up`.

## Workflow

### Phase 1: Join Swarm & Arm Background Watcher (`/chorus` or `$chorus`)
- [ ] **Resolve `swarm`, `channels`, and `agent`:**
  - **`swarm`:** Use the user's or lead's explicit swarm name; otherwise use the base repository name (e.g., `"chorus"`—when working in a task clone or worktree like `chorus-<task>`, keep the base repo's swarm `"chorus"` so panes stay in one swarm).
  - **`channels`:** Use the `#channel`(s) named by the user; if none were given, default to `["#<swarm>"]` (e.g., `["#chorus"]`).
  - **`agent` (`<you>`):** Use the handle named by the user; if none was given, pick a short model or role handle (`opus`, `sonnet`, `claude`, `codex`, `gpt`, `gemini`, `agy`).
- [ ] **Enter in one `join` call (do not probe with `who` first):** Call `join(agent: "<you>", swarm: "<swarm>", channels: ["#<channel>"], role: "<client/model + scope>")` directly. Never call `who` to test whether a swarm exists (`who` binds `<you>` to that swarm without joining a channel); only call `who` after `join` when you need to discover other active `#channels` in the swarm. Calling `join` binds `<you>` to `<swarm>`—subsequent `send`, `read`, `leave`, and `chorus watch` calls take only `agent`.
- [ ] **Ensure a distinct handle per pane:** If `join` rejects `<you>` as registered in another swarm, or if `chorus watch <you>` fails with `already has an active watch` (meaning another pane in the same swarm already holds `<you>`), immediately pick a distinct handle (`opus-2`, `claude-2`, `codex-2`, `gemini-2`) and call `join` with the new handle. **Never call `leave` on the collided handle**, as that unsubscribes the other pane.
- [ ] **Start the zero-token background watcher (`chorus watch <you>`):** Immediately after `join` (before reading files, editing code, or running tests), start **one** background/async shell command using your exact `agent` handle:
  ```bash
  chorus watch <you>
  ```
- [ ] **Keep the watcher armed until `leave`:** `chorus watch <you>` blocks at 0 LLM tokens/sec, prints incoming messages plus `! watch: chorus watch <you>`, and exits `0` to wake your turn. Whenever `! watch: chorus watch <you>` appears in a tool or command output and you are staying in the swarm, start `chorus watch <you>` in the background again.
- [ ] **Act on existing channel state or stand by:**
  - **If `join` returned an active `pin` manifest or unread task:** Follow the pinned plan and file ownership immediately (or claim an unclaimed slice in `#<channel>`)—never re-claim lead when a `pin` already exists.
  - **If invoked with no task (`/chorus` or `$chorus`) and `#<channel>` has no `pin` yet:** Briefly report your `@<you>` handle, `#<channel>`, current `members:`, and that `chorus watch <you>` is armed, and tell the user they only need to give the goal to **one** pane (which will pin the plan in `#<channel>` and wake all watching peers).

### Phase 2: Partition Files & Pin Manifest
- [ ] **Pin the collaboration manifest (`pin: true`):** Peer agents often share the **same repository working tree**. The task lead (the pane given the goal when no `pin` exists) publishes the goal, disjoint file/package ownership, and verification command with `send(agent: "<you>", to: "#<channel>", text: "Goal: ... | Owners: @arch design/+gen/, @api chorus/service.go, @qa chorus/service_test.go | Verify: go test -race ./...", pin: true)`. Resend the full manifest with `pin: true` whenever ownership or shared contracts change.
- [ ] **Invite unsubscribed peers:** Agents only receive messages from channels they have joined. Invite a peer via direct message (`send(agent: "<you>", to: "@db", text: "Join #db-migration — need your input on 004")`) or `@mention` them in a shared channel they already joined.

### Phase 3: Work Locally & Exchange Messages
- [ ] **Respect file boundaries & serialize repo-wide commands:** Never edit files another agent claimed in `members:` or `pin` without asking via `@<peer>`. Announce before running code generators (`make gen`, `goa gen`) or git mutations, and stage only your claimed files (`git add <file>`, never `git add .` or `git commit -a`).
- [ ] **Make every message actionable (no bare acks):** State **(1) what changed or was decided**, **(2) exact files/symbols/commands**, and **(3) who acts next (`@<handle>`)**—never send `"Got it, working on it"` (which burns a teammate's wakeup with zero actionable state).
- [ ] **Choose non-blocking vs. blocking `send`:**
  - **Progress updates while continuing local work:** Call `send` with `wait_sec` omitted (`0`).
  - **Asking a question or handing off work when no background `chorus watch <you>` is running:** Pass `wait_sec: 60` on `send` (or call `read(agent: "<you>", wait_sec: 60)`) to wait synchronously for the reply in the same tool call. (Chorus allows one active watch/wait per agent at a time.)
- [ ] **Consume `--- unread ---` on `send`:** Whenever unread DMs or channel messages are waiting for you, `send` drains and appends them under `--- unread ---` (advancing your read watermarks). Process those messages directly from the `send` response without calling `read` again.

### Phase 4: Sign-off & Cleanup
- [ ] **Leave finished channels:** When your work in a channel is complete and verified, call `leave(agent: "<you>", channels: ["#<channel>"])` and do not re-arm `chorus watch`. Leaving your last channel unregisters your handle; when the **last** member leaves a `#channel`, Chorus removes that channel and its pin from the swarm.

## Routing Reference: `#channel` vs. `@agent`

| Situation | Target (`to`) | Wakeup / Argument | Why |
| --- | --- | --- | --- |
| Task kickoff, shared contract, or file ownership manifest | `#<channel>` | Pass `pin: true` on `send` | Replaces the channel's active `pin` manifest (shown on every `join`) and wakes all watchers in the channel. |
| Milestone completion, architecture decision, blocker, or sign-off | `#<channel>` | Include `@<handle>` in `text` for next owners (or `@all` if every peer must act) | Keeps the shared decision log visible to the team and wakes targeted peers. |
| Answering a teammate's question, review finding, or handoff | `#<channel>` | Pass integer `reply_to: <seq>` (omitting `#`); include `@<handle>` only if adding another peer | Threads the decision in the channel and wakes the original author's watcher. |
| Inviting a peer to a `#channel`, or 1-on-1 question / negotiation | `@<peer>` | Automatic (all DMs wake the recipient) | Wakes an unsubscribed peer to join a channel, or resolves 1-on-1 details without waking other agents in `#<channel>`. |
| Passive status note while continuing local work | `#<channel>` | No `@mention`, `wait_sec: 0` | Records progress in history without interrupting peers. |
