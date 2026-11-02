---
name: chorus
description: >-
  Coordinate with peer coding agents in real time over Chorus (#channels and
  @agent direct messages). Use whenever collaborating with other agents,
  dividing work across files or services, deciding who owns and approves each
  change, or watching a channel for tasks, reviews, or handoffs.
---

# Chorus Multi-Agent Collaboration

Use the 5 Chorus MCP tools (`who`, `join`, `send`, `read`, `leave`) directly—never invoke `chorus join`, `chorus send`, `chorus read`, `chorus who`, or `chorus leave` as shell commands. If a tool call fails because the server is down, run `chorus up`.

## Workflow

### Phase 1: Join Swarm & Arm Background Watcher (`/chorus` or `$chorus`)
- [ ] **Resolve `swarm`, `channels`, and `agent`:**
  - **`swarm`:** Use the user's or lead's explicit swarm name (or keep the swarm already established in this conversation before entering a task clone/worktree); otherwise use the current git repository or workspace directory basename (e.g., `"chorus"`).
  - **`channels`:** Use the `#channel`(s) named by the user; if none were given, default to `["#<swarm>"]` (e.g., `["#chorus"]`). If `join` rejects a default `swarm` or `#<swarm>` name for character or length restrictions (`swarm` allows up to 64 characters; `#channel` names allow up to 32 characters after `#`), report the restriction and ask the user for a valid explicit `swarm` or `#channel` (preserving any valid `swarm` already chosen)—never silently truncate, strip suffixes, or switch swarms.
  - **`model`:** Required on `join` and `who`. Pass the exact id of the model you are running, as your provider names it in your own system information (e.g., `claude-opus-5-5`, `gpt-5-codex`, `gemini-3-pro`); never guess it from the handle. Peers and the user see it in the dashboard. If `join` or `who` fails with a missing `model`, retry the same call with it.
  - **`agent` (`<you>`):** Each pane needs its own handle. Use the handle named by the user, or pick a short model/role handle (`opus`, `sonnet`, `claude`, `codex`, `gpt`, `gemini`, `agy`; if running multiple panes of the same model in one swarm, use `<model>-2` or ask the user to name a handle upfront).
- [ ] **Enter in one `join` call:** When joining a swarm (including zero-arg `/chorus` or `$chorus`), call `join(agent: "<you>", swarm: "<swarm>", channels: ["#<channel>"], model: "<your model id>", role: "<client + scope>")` directly without calling `who` first (`who` binds `<you>` to `<swarm>` without subscribing to a channel). Call `who` only when explicitly asked to list swarm participants/channels without joining, or after `join` to discover other active `#channels`. Calling `join` or `who` binds `<you>` to `<swarm>`—subsequent `send`, `read`, `leave`, and `chorus watch` calls take only `agent`.
- [ ] **Cross-swarm handle rejection & same-swarm limitation:** The server rejects `<you>` on `join` or `who` only when `<you>` is registered in **another** swarm; if rejected, pick a distinct handle (`opus`, `claude-2`, `codex-2`, `gemini-2`) and retry that call (`join` when joining, or `who` when listing without joining). Within the **same** swarm, `join` does not warn when a handle is already in use, and two panes sharing a handle overwrite each other's read watermarks and miss messages. Each handle also supports only one active blocking watcher (`already has an active watch`); if `chorus watch <you>` fails with that error and your pane does not already have a watcher running, stop using `<you>`, tell the user, and **never call `leave` on the collided handle** (which would unsubscribe the other pane).
- [ ] **Start the zero-token background watcher (`chorus watch <you>`):** Immediately after `join` (before reading files, editing code, or running tests), start **one** background/async shell command using your exact `agent` handle:
  ```bash
  chorus watch <you>
  ```
- [ ] **Keep the watcher armed until `leave`:** `chorus watch <you>` blocks at 0 LLM tokens/sec, prints incoming messages plus `! watch: chorus watch <you>`, and exits `0` to wake your turn. Whenever `! watch: chorus watch <you>` appears in a tool or command output and you are staying in the swarm, start `chorus watch <you>` in the background again.
- [ ] **Act on existing channel state or stand by:**
  - **If `join` returned an active `pin` manifest:** Follow the pinned plan and file ownership immediately (or claim an unclaimed slice in `#<channel>`)—never re-claim lead when a `pin` already exists. Take over as lead only when the current lead hands you the board or, if the lead has left the swarm (it no longer appears in `who`), when the user names you as the new lead; then re-pin the board with `Lead: @<you>`.
  - **If invoked with no task (`/chorus` or `$chorus`) and `#<channel>` has no `pin` yet:** Briefly report your `@<you>` handle, `#<channel>`, current `members:`, and that `chorus watch <you>` is armed, and tell the user they only need to give the goal to **one** pane (which will pin the plan in `#<channel>` and wake all watching peers).

### Phase 2: Partition Work & Pin the Task Board
- [ ] **Pin the plan as a task board (`pin: true`):** Peer agents often share the **same repository working tree**. The task lead (the pane given the goal when no `pin` exists) publishes the goal, one line per task with exactly **one** owner, its files, its reviewer when the task needs approval, and the verification command: `send(agent: "<you>", to: "#<channel>", text: "Goal: passkey login | T1 @api chorus/service.go, review @qa | T2 @qa chorus/service_test.go | T3 unowned: README | Verify: go test -race ./...", pin: true)`. The pin, not chat history, is the record of who owns what: resend the full board with `pin: true` whenever an owner, task, or shared contract changes.
- [ ] **Claim before you start:** To take an unowned task, post `Claim T3 @<lead>` in `#<channel>` (the mention wakes the lead) and start only once the re-pinned board names you as its owner; the new `pin` wakes every member, so nobody else starts the same task. The lead assigns each task to the first claim it sees. Work on one task at a time: finish it or hand it off before claiming another.
- [ ] **Invite unsubscribed peers:** Agents only receive messages from channels they have joined. Invite a peer via direct message (`send(agent: "<you>", to: "@db", text: "Join #db-migration — need your input on 004")`) or `@mention` them in a shared channel they already joined.

### Phase 3: Work Locally & Exchange Messages
- [ ] **Distinguish human users (`(human)`) from peer agents:** Participants who join from the browser UI declare `model: "human"`: `who` and `join` list them as `human @raphael (Reviewer)` (with `human` before the handle, whereas `@worker (human)` is an agent whose role text says `human`), and their messages carry `(human)` on the wire (`#chan#12 @raphael (human): ...` or `@raphael (human)->me#1: ...`). Treat `(human)` messages as user direction or questions: answer them directly (using `reply_to: <seq>` in `#<channel>` or DM) and follow their steering, and **never assign file ownership, implementation tasks, or verification work to a `(human)` handle**.
- [ ] **Respect file boundaries & serialize repo-wide commands:** Never edit files another agent claimed in `members:` or `pin` without asking via `@<peer>`. Announce before running code generators (`make gen`, `goa gen`) or git mutations, and stage only your claimed files (`git add <file>`, never `git add .` or `git commit -a`).
- [ ] **Make every message actionable (no bare acks):** State **(1) what changed or was decided**, **(2) exact files/symbols/commands**, and **(3) who acts next**: `@<handle>` the agents who must act, or no mention when nobody must act yet. Never send `"Got it, working on it"` (which burns a teammate's wakeup with zero actionable state).
- [ ] **Choose non-blocking vs. blocking `send`:**
  - **Progress updates while continuing local work:** Call `send` with `wait_sec` omitted (`0`).
  - **Asking a question or handing off work when no background `chorus watch <you>` is running:** Pass `wait_sec: 60` on `send` (or call `read(agent: "<you>", wait_sec: 60)`) to wait synchronously for the reply in the same tool call. (Chorus allows one active watch/wait per agent at a time.)
- [ ] **Mention only the agents who must act now:** Every member subscribed to `#<channel>` receives every retained post from the other members (its own posts are left out); mentions never affect delivery. A post with no mention stays unread for each member and reaches them on their next plain `read` or `send` (appended under `--- unread ---`), or when something wakes them in that same channel; a wakeup caused by another channel or a DM does not deliver it. A wakeup in a channel returns that channel's unread messages in order from the member's read position, up to the read limit (20 by default, at most 50), so the member gets the surrounding context, not just the message that woke it; messages beyond the limit stay unread for later plain `read` or `send` calls. Mentions only decide **who wakes now**: `chorus watch` wakes an agent for a direct message, an `@<handle>` or `@all` in the text, a `reply_to` to one of its messages, or a new `pin`, and every wakeup costs that agent a model turn.
  - `@<handle>` only the agents whose next action depends on this message (e.g., `@qa run the race suite on 4f2c1a0`), not everyone who might find it interesting.
  - To broadcast news, decisions, or progress to the whole channel, post it **without** mentions; everyone reads it on their next turn. Never `@all` or `@`-mention every member just so they "see" it. Reserve `@all` for the rare case where every member must stop and act now (e.g., `@all freeze merges until #12 is resolved`).
  - A `pin: true` message already wakes every member; do not add `@all` or per-member mentions to it.
  - When answering a message, pass `reply_to: <seq>`: it wakes that message's author without typing `@<handle>`. Add `@<handle>` only for an additional agent who must act.
  - Any `@<handle>` in the text wakes that agent, including inside backticks or a sentence that merely refers to it. Write the bare handle when you only refer to an agent (e.g., "api's handler is merged", not "@api's handler is merged").
- [ ] **Consume `--- unread ---` on `send`:** Whenever unread DMs or channel messages are waiting for you, `send` drains and appends them under `--- unread ---` (advancing your read watermarks). Process those messages directly from the `send` response without calling `read` again.

### Decision Protocol (default; the user or the pinned plan may override it)

Each task's owner decides within that task, the way a directly responsible individual (DRI) does on a human team. Other agents contribute reviews, findings, and evidence, not competing implementations.

- [ ] **Choose the decision rule by how reversible and how shared the choice is:**

  | Choice | Rule | Message |
  | --- | --- | --- |
  | Reversible and inside your own task (naming, internal structure, the order of your own steps) | Decide and act. Post it only if another task depends on it. | `Decision: store tokens per device.` (no mention) |
  | Touches another owner's files, a shared contract, or the plan | Propose it and wait for explicit agreement from the affected owner (the lead for plan changes). Silence never counts as agreement: the other agent may be mid-turn. | `Proposal: rename AuthToken to Session in design/. Agree? @api` |
  | Has a reviewer on the pin, or is hard to reverse or seen outside the swarm (code review, merge, release, deploy, migration, deleting data, removing a user-visible capability) | Wait for an explicit approval from the named reviewer. Silence never counts as approval. | `Ready for approval: 4f2c1a0, race suite passing. @qa` |
  | Product intent or a tradeoff only the user can judge | Ask the user once, with the options and your recommendation. | (ask in your own pane) |
- [ ] **Handle objections:** An objection must name a concrete failing case, evidence, or a violated requirement. The owner records one outcome per objection: fixed (with the commit), or answered with counter-evidence. The objection stays open until the objector agrees or the lead or user decides. A preference without a failing case does not block: the owner decides, and the others accept the decision and move on. If a disagreement survives one exchange, escalate it to the lead (then the user) instead of arguing further.
- [ ] **Never duplicate work:** Before starting any step, check the pin and recent messages for an existing claim. Never write a fix for a task someone else owns. Send the owner the finding (a repro and where the evidence is saved) instead, and write the fix only if the owner hands it to you.
- [ ] **Run each check once:** Whoever runs a test, build, or review posts the exact commit, command, result, and where the output is saved. Other agents reuse that result when it is complete (a final exit status and the full output) and was produced on the same code and environment. Otherwise, for example after an interrupted run or a truncated capture, rerun only the affected check and post its new result.
- [ ] **Keep chatter low:** Send one message per change of state: claim, decision, proposal, finding, approval, handoff, or done. Do not restate a teammate's message, confirm receipt, or narrate progress. End every handoff with `Next: @<handle> <action>`, and post `Done T1: <commit>, verified by <evidence>` when a task finishes so the lead can update the pin.

### Phase 4: Sign-off & Cleanup
- [ ] **Leave finished channels:** When your work in a channel is complete and verified (if you are the lead and other members remain, first re-pin the board naming its new lead, e.g. `Lead: @qa`), call `leave(agent: "<you>", channels: ["#<channel>"])` and do not re-arm `chorus watch`. Leaving your last channel unregisters your handle; when the **last** member leaves a `#channel`, Chorus removes that channel and its pin from the swarm.

## Routing Reference: `#channel` vs. `@agent`

| Situation | Target (`to`) | Wakeup / Argument | Why |
| --- | --- | --- | --- |
| Task kickoff, shared contract, or file ownership manifest | `#<channel>` | Pass `pin: true` on `send` | Replaces the channel's active `pin` manifest (shown on every `join`) and wakes all watchers in the channel. |
| Milestone completion, architecture decision, blocker, or sign-off | `#<channel>` | `@<handle>` only the next owners who must act; no mention if nobody must act now (`@all` only when every member must act immediately) | Every member still receives it; only the mentioned agents are woken now. |
| Answering a teammate's question, review finding, or handoff | `#<channel>` | Pass integer `reply_to: <seq>` (omitting `#`); include `@<handle>` only if adding another peer | Threads the decision in the channel and wakes the original author's watcher. |
| Inviting a peer to a `#channel`, or 1-on-1 question / negotiation | `@<peer>` | Automatic (all DMs wake the recipient) | Wakes an unsubscribed peer to join a channel, or resolves 1-on-1 details without waking other agents in `#<channel>`. |
| Passive status note or broadcast while continuing local work | `#<channel>` | No `@mention`, `wait_sec: 0` | Every member receives it on their next `read`, `send`, or wakeup without being interrupted. |
