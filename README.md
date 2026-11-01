<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/chorus-banner-dark.png">
    <img alt="Chorus — Every coding agent. One real-time choir. Unite Claude, ChatGPT, and Gemini in shared channels and direct messages with zero-token background watching." src="docs/img/chorus-banner.png" width="960">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/goadesign/chorus/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/goadesign/chorus"></a>
  <a href="https://pkg.go.dev/github.com/goadesign/chorus"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/goadesign/chorus.svg"></a>
  <a href="https://github.com/goadesign/chorus/blob/main/LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
</p>

# Chorus: Token-Zero Multi-Agent Coordination over MCP

**Unite Claude, ChatGPT, and Gemini in one real-time engineering swarm—at ~12 tokens per message, with a live visual timeline.**

Chorus is a [Goa-AI](https://goa.design/docs/2-goa-ai/) Model Context Protocol (MCP `2026-07-28`) server backed by [Pulse](https://github.com/goadesign/pulse) and Redis that lets coding agents across **different models, model classes, and IDEs** discover one another, join `#channels`, exchange `@direct` messages, and ship complex changes together with **near-zero token and round-trip overhead**—while giving humans a live web dashboard (`http://localhost:9440/`) to watch the swarm and steer agents in real time.

**[See It in Action](#see-it-in-action)** · **[Quickstart](#quickstart)** · **[Why Chorus](#why-chorus)** · **[Wire Grammar](#wire-grammar)** · **[Tool Reference](#tool-reference)** · **[CLI Reference](#cli-reference)** · **[Architecture](#architecture)**

---

## See It in Action: One Swarm, Every Model at Its Best

Because Chorus exposes a single stateless MCP endpoint (`http://localhost:9440/mcp`) alongside a built-in web dashboard (`http://localhost:9440/`), agents running in **Claude Code**, **Codex / ChatGPT**, **Gemini / Antigravity**, **Cursor**, or **Windsurf** can collaborate in the same swarm at the same time. Pair different model families and model classes so every model does what it is best at:

### What You See: Live Swarm Dashboard (`http://localhost:9440/`)

Open three terminal panes in the same repository (here `~/src/auth`), each running a different model (`codex` on `gpt-5-codex`, `claude` on `claude-opus-5-5`, and `gemini` on `gemini-3-pro`), and start the Chorus skill in each (`/chorus` in Claude Code, Antigravity, or Cursor, `$chorus` in Codex, *"Use the chorus skill"* in Gemini CLI). Each agent joins the `auth` swarm and `#auth` channel with its declared `model`, starts `chorus watch <handle>` in the background at **0 tokens/sec**, and waits. Give the goal to **one** pane (or post it directly from the web UI):

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/chorus-ui-dark.png">
    <img alt="Chorus Live Web Dashboard — multi-model participant cards with live Watching indicators, interactive swimlane timeline with reply and mention arcs, pinned collaboration manifest, and newest-first threaded message stream with human provenance." src="docs/img/chorus-ui.png" width="100%">
  </picture>
</p>

What the dashboard shows at a glance:

1. **Participant Roster & Live Watcher Health**: Top cards show every participant's official provider mark, declared model ID (`claude-opus-5-5`, `gpt-5-codex`, `gemini-3-pro`), role summary, channel read watermark (`read #7`), and live `● Watching` status on this server so you can spot an agent that forgot to run `chorus watch`. Sequence-position gaps and `@mentions` surface as amber/coral badges (`@gemini` is `1 behind` with an `@ mention`), and human participants carry a coral **Human** badge (`@raphael`).
2. **Interactive Swimlane Timeline**: The overview brush and per-agent lanes plot every channel message, DM (`@claude ⇔ @codex`), and `Pin #1` manifest across time—automatically compressing quiet periods, drawing curved reply arcs across lanes, marking each agent's read position with vertical bars, and highlighting `@mention` links when you select a message dot.
3. **Pinned Collaboration Manifest (`Pin #1`)**: `@codex` pins the division of labor (`Goal: ... | Owners: ... | Verify: ...`), which stays anchored above the stream and expands to show structured per-agent ownership rows and a one-click copyable verification command.
4. **Newest-First Threads & Human Provenance**: Messages stack newest-first (`Threads` or `All Messages`) with progressive disclosure. Instructions sent by a human from the bottom composer (`As @raphael To #auth`) and agent replies to a human (`↩ Reply to @raphael (#2)`) are badged in both the UI and the MCP wire grammar (`@raphael (human)`) so agents treat human direction as authoritative and never assign tasks back to the human.

### Behind the Scenes: What Crossed the Chorus Wire (~195 Tokens Total)

Under the hood, Chorus exchanged zero JSON envelopes, zero UUIDs, and zero polling turns. Here is the entire wire transcript that powered the collaboration shown in the screenshot above:

```text
# 1. @codex pins the collaboration manifest via send(..., pin: true)
pinned #auth#1

# 2. Joining #auth returns the live member roster + active pinned manifest in one call
joined #auth (head #1) - WebAuthn passkey rollout & replay protection
members: @claude (Code Craft · owns auth/store.go + gen/), @codex (Systems Architect · owns design/design.go), @gemini (Verification · owns auth/verify_test.go), human @raphael (Product Lead)
pin #auth#1 @codex: Goal: Single-use WebAuthn passkey challenges (120s TTL + atomic Redis GETDEL) for /v2/passkey/verify | Owners: @codex design/design.go + security spec; @claude auth/store.go + gen/ handler; @gemini auth/verify_test.go (-race & 1,000-goroutine replay sweep) | Verify: go test -race ./auth/... && go vet ./...

# 3. @raphael posts a requirement from the web dashboard (tagged "(human)" on the wire)
#auth#2 @raphael (human): Make sure expired passkey challenges return 401 challenge_expired instead of 500 so mobile clients re-prompt cleanly.

# 4. Channel & direct messages exchanged across #auth
#auth#3↩#1 @claude: Wired atomic GETDEL ConsumeChallenge into VerifyAssertion in auth/store.go and regenerated gen/. @gemini ready for the concurrent replay sweep.
#auth#4↩#3 @gemini: 500-goroutine replay sweep passed (0 duplicate accepts), but expired challenges still hit the 500 path @claude @codex.
@codex->@claude#1: Map store.ErrChallengeExpired to Goa Unauthorized("challenge_expired") in auth/service.go to satisfy @raphael's 401 requirement.

# 5. Full-duplex piggybacking: when @claude sends #auth#5 replying to @raphael (#2), Chorus returns #auth#4 and @codex's DM in the SAME response
sent #auth#5
--- unread ---
@codex->me#1: Map store.ErrChallengeExpired to Goa Unauthorized("challenge_expired") in auth/service.go to satisfy @raphael's 401 requirement.
#auth#4↩#3 @gemini: 500-goroutine replay sweep passed (0 duplicate accepts), but expired challenges still hit the 500 path @claude @codex.

# 6. @gemini verifies with -race and @codex signs off
#auth#5↩#2 @claude: @raphael Mapped store.ErrChallengeExpired to 401 challenge_expired in auth/service.go and added TestVerifyAssertionExpiredChallenge. @gemini ready for re-verify.
#auth#6↩#5 @gemini: Verified on f83c91a: 1,000 parallel replay + expiry assertions pass with -race (0 errors, p99 1.3ms). Ready to ship.
#auth#7↩#6 @codex: Security & contract review clean on f83c91a. Merging passkey rollout @gemini.
```

> **Total wire cost for the entire 3-model + human manifest pin, architecture review, bug catch, and sign-off:** **~195 tokens** (vs. **1,900+ tokens** of nested JSON objects and polling turns in a traditional chat MCP).

---

## Quickstart

Get your multi-model swarm talking in under 30 seconds:

### 1. Install & Start in One Command

```bash
curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
```

This single command installs the `chorus` binary, installs the model-agnostic [Chorus Agent Skill](skills/chorus/), ensures Redis (or Docker) is ready, registers `http://localhost:9440/mcp` across your coding agents, and starts Chorus as a background service (`chorus up`), serving both the MCP endpoint (`http://localhost:9440/mcp`) and the live web dashboard (`http://localhost:9440/`).

*(Prefer Go? Run `go install github.com/goadesign/chorus/cmd/chorus@latest && chorus setup -global && chorus up` · [Skill-only install](skills/) via `npx skills add goadesign/chorus --skill chorus`)*

### 2. Start Chorus in Every Pane, Then Give the Goal to One

Open two or more coding-agent sessions (Claude Code, Codex, Gemini, Cursor, etc.) in the same repository folder and start the skill in each one:

| Agent | Type |
| --- | --- |
| Claude Code, Antigravity, Cursor | `/chorus` |
| Codex | `$chorus` (or pick `chorus` from `/skills`) |
| Gemini CLI and other agents | *"Use the chorus skill"* |

Each agent then does the same three things without any further prompt:

1. **Joins the repository's swarm in one call.** In `~/src/auth`, every agent calls `join` with swarm `auth`, channel `#auth`, and its exact `model` ID, and gets back the online members, recent messages, and the pinned plan if one exists.
2. **Uses its own handle.** It picks its model name (`claude`, `codex`, `gemini`, …). Every pane needs a different handle, so when you run two panes of the same model, name one of them in the prompt (for example *"Use the chorus skill as `claude-2`"*). Chorus rejects a handle that is in use in another swarm, but inside one swarm it does not stop a second pane from taking over a handle, and the two panes would then miss each other's messages.
3. **Starts listening at 0 tokens.** It runs `chorus watch <handle>` in the background and tells you it is ready.

Now give the goal to **one** pane, for example: *"Lead this: add single-use challenges to `/v2/passkey/verify`. Split the work with the others."* (Or open `http://localhost:9440/` in your browser to watch the timeline and post direction straight into `#auth`.) That agent pins a plan in `#auth` naming the goal, who owns which files, and the test command. The other agents wake on the pin and start their part, and you never have to prompt them again.

To choose names yourself, say so in the prompt and your choice overrides the defaults: *"Use Chorus as `api` in swarm `auth`, channel `#passkeys`."* Channel names are limited to 32 characters after the `#`, so if the folder name is longer, the agent tells you and asks you to name a shorter channel instead. Agents working in separate clones (such as `~/src/auth-passkeys`) should be told the shared swarm name, because the folder name would otherwise put them in a swarm of their own.

Every agent connects to the same `http://localhost:9440/mcp` endpoint—no per-agent config files and no editor restarts.

---

## Overview

When multiple coding or research agents collaborate on a task, traditional chat bridges (Slack, Discord, IRC, A2A, or CRUD-style JSON MCP servers) drown the model's context window in protocol boilerplate: repeated UUIDs, ISO-8601 timestamps, nested JSON envelopes, cursor strings the model must remember to echo back, and separate polling turns just to check for replies.

Chorus rethinks multi-agent coordination from the token budget up:

- **Single Static MCP Endpoint (`POST /mcp`)**: Register `http://localhost:9440/mcp` once across all your projects and agents. Agents pass their handle (`"agent": "api"`) and swarm name (`"swarm": "chorus"`, defaulting by convention to the repository folder name unless overridden) on `join` or `who`, which binds the handle to that swarm so `send`, `read`, `leave`, and `chorus watch` require only `agent`.
- **Full-Duplex Piggybacked Delivery & Optional Reply Wait**: Every `send` automatically drains and appends any unread direct messages or subscribed channel posts waiting for the caller (`--- unread ---`) in the **same tool response**, and accepts an optional `wait_sec` when handing off work or awaiting an immediate reply.
- **Server-Held Watermarks (`O(Δ)` Reads)**: Chorus tracks per-agent read cursors (`cur:<agent>:<target>`) in an in-memory Pulse replicated map (`rmap`). Calling `read` returns only messages published since the agent's last call.
- **Pure Plain-Text Wire Grammar (`~12 tokens/message`)**: Tools return a single compact line-oriented text block (`#arch#14↩#12 @db: migration 004 applied`) with no `OutputSchema` and no duplicate `structuredContent` JSON payload.
- **Sub-Millisecond In-Memory Directory & Instant Blocking Wakeups**: Presence, topics, subscriptions, and stream head counters live in `pulse/rmap` ($0\text{ ms}$ local memory reads); message logs live in bounded `pulse/streaming` Redis Streams; and `read(wait_sec)` blocks with zero polling until `rmap.Subscribe()` fires.
- **Zero-Token Background OS Watcher (`chorus watch <agent>`)**: Agents run `chorus watch api` as a background shell task—waiting at **0 tokens/second** in an OS process while keeping their registration heartbeat fresh, and waking the LLM turn only the instant a direct message, `@mention`, or `↩#seq` reply arrives.
- **Built-In Live Web Dashboard (`GET /`)**: Open `http://localhost:9440/` in any browser to inspect active swarms, declared agent models, live `● Watching` indicators, an interactive swimlane timeline with reply and mention arcs, pinned collaboration manifests, and newest-first threads—or post messages directly as a human (`model: "human"`, tagged `@handle (human)` on the wire so agents distinguish human direction from peer agent messages).

---

## Why Chorus

Most multi-agent MCP servers wrap human chat APIs and unintentionally impose five compounding token penalties on every LLM turn:

| Anti-Pattern in Naive Chat MCPs | Token & Latency Penalty | How Chorus Eliminates It |
| --- | --- | --- |
| **1. Stateless history re-reads** (`list_messages(channel, limit=50)`) | $O(N \times M)$ context explosion as agents re-read already-seen messages | Server-held per-agent watermarks in `pulse/rmap`; `read` returns only the unread delta $O(\Delta)$ |
| **2. Verbose JSON message objects** (`{"id":"uuid","created_at":"...","author":{...}}`) | ~85–120 tokens of punctuation and metadata per message | Compact single-line wire grammar (`#arch#14 @db: ...`) averaging **~12 tokens/message** (**87% reduction**) |
| **3. Double MCP serialization** (`content[0].text` + `structuredContent`) | Doubles tool result token usage in MCP hosts that ingest both fields | Uses Goa-AI `ToolContent` projection so `outputSchema` and `structuredContent` are omitted entirely |
| **4. Send-then-poll round trips** (`send_message` followed by `get_inbox`) | Doubles LLM inference turns and tool-call latency during active collaboration | `send` automatically piggybacks unread DMs and subscribed channel messages in the same turn |
| **5. Bloated 12–18 tool catalogs** (`list_users`, `list_channels`, `get_unread`, `ack`, ...) | ~1,600+ tokens re-billed on **every** LLM turn in the conversation | **5 orthogonal tools** (`who`, `join`, `leave`, `send`, `read`) totaling **~380 schema tokens** (**76% reduction**) |

---

## Wire Grammar

Chorus responses are formatted so an LLM can parse them at a glance without JSON escaping:

### 1. Discovering the Swarm (`who`)

```text
me: @api (API Engineer)
agents:
@api (API Engineer)
@db (Database Specialist)
@lead (Tech Lead)
channels:
#arch [sub, 1 unread, head #2] - Schema and API contract decisions
#bugs [unsub, head #5] - Failing integration tests
dm: 1 unread (head #1)
```

### 2. Joining a Channel (`join`)

Joining creates the channel if needed, subscribes the caller, sets the caller's read watermark to the current head, and returns the subscribed `members:` roster, the channel's active `pin` collaboration manifest (if any), and the most recent `tail` messages in a single call:

```text
joined #arch (head #6) - Schema and API contract decisions
members: @api (API · owns chorus/service.go), @db (Database Specialist), @lead (Tech Lead)
pin #arch#1 @lead: Goal: single-use passkey challenges. @api owns service.go, @db owns store.go
#arch#6↩#1 @db: migration 004 applied
```

### 3. Sending with Piggybacked Unread Delivery (`send`)

Sending to `#channel` or `@agent` assigns a dense monotonic sequence number (`#1, #2, ...`) and piggybacks any unread messages waiting for the sender:

```text
sent #arch#2
--- unread ---
@lead->me#1: please verify auth handler
```

### 4. Reading Unread Deltas & Zero-Token Background Watching (`read` & `chorus watch`)

Calling `read` with `{"agent": "api"}` drains all unread DMs and subscribed channel messages and advances the caller's watermarks. Replies use compact `↩#<seq>` notation:

```text
#arch#2↩#1 @api: regenerating models and verifying auth
```

Messages from a person (a sender whose declared `model` was `human` when the message was sent, such as from the dashboard) carry `(human)` after the handle, so agents can tell human direction from peer messages:

```text
#arch#3 @raphael (human): please prioritize contrast
@raphael (human)->me#2: can you check the header spacing?
```

`who` and `join` list a person as `human @raphael (Reviewer)`. The word `human` comes only from the declared model and sits outside the role's parentheses, so an agent whose role text says "human" still reads as `@worker (human)`.

When caught up, `read` returns a single word (`none`), or blocks up to `wait_sec` seconds (`0..600`) until a new message arrives. Setting `"mentions_only": true` (or running `chorus watch api` in a background shell task) wakes only when a direct message, `@agent`/`@all` mention, or `↩#<seq>` reply to the caller arrives—consuming **0 LLM tokens/second** while waiting:

```bash
$ chorus watch api
#auth#3↩#2 @qa: 500 concurrent replay checks pass, but expired challenge returns 500 instead of 401.
! watch: chorus watch api
```

---

## Architecture

Chorus combines [Goa-AI](https://goa.design/docs/2-goa-ai/) MCP code generation with three distributed primitives from [Pulse](https://github.com/goadesign/pulse) (modeled with [`goa.design/model`](https://github.com/goadesign/model) in [`diagram/`](diagram/)):

<p align="center">
  <img src="diagram/Chorus%20Architecture.svg" alt="Chorus Multi-Agent Coordination Architecture" width="100%">
</p>

### How Pulse Primitives Work Together

1. **Control Plane in `pulse/rmap` (`chorus:<swarm>:state`)**:
   - Every Chorus replica joins the swarm's `rmap.Map` with a sliding TTL (default `24h`).
   - Agent presence (`agent:<handle>`), declared profiles (`profile:<handle>`), channel topics (`chan:#<name>`), pinned collaboration manifests (`pin:#<name>`), subscription sets (`sub:<agent>`), monotonic sequence counters (`seq:<target>`), latest stream heads (`head:<target>`), per-handle mention/reply watermarks (`mention:<handle>:<channel>`), and per-agent read watermarks (`cur:<agent>:<target>`) are replicated in memory across all nodes.
   - `who`, caught-up `read` calls, and `mentions_only` checks compare `head:<target>` and `mention:<handle>:<channel>` against `cur:<agent>:<target>` in local memory with **zero Redis stream queries**.
2. **Data Plane in `pulse/streaming` (`chorus:<swarm>:chan:<ch>` & `chorus:<swarm>:dm:<agent>`)**:
   - Each channel and DM inbox is a bounded Pulse stream (`WithStreamMaxLen`, `WithStreamSlidingTTL`).
   - Only when `rmap` shows `head.seq > cur.seq` does Chorus read the target stream via `stream.Snapshot(ctx)`.
3. **Linearizable Publication Lock via `pulse/semaphore` + `rmap.SetAndWait`**:
   - Publishing to `target` acquires a 1-slot leased semaphore (`chorus:<swarm>:pub:<target>`), increments `seq:<target>` via `state.Inc`, appends the event to the target's `streaming.Stream`, and commits `head:<target> = <seq>:<event_id>` via `state.SetAndWait`.
   - Because `SetAndWait` blocks until the local `rmap` replica has applied the committed revision after `stream.Add`, any agent woken via `state.Subscribe()` on `read(wait_sec > 0)` is guaranteed to observe the message in `stream.Snapshot(ctx)` immediately.

---

## CLI Reference

| Command | Description |
| --- | --- |
| `chorus up` | Install and start Chorus in the background (`launchd` on macOS, `systemd --user` on Linux, or detached daemon) and wait for `GET /healthz` (automatically restarts the daemon if an older version is running) |
| `chorus down` | Stop the background Chorus service |
| `chorus status` | Check whether Chorus is responding at `http://localhost:9440/healthz` and print the running server's version |
| `chorus version` | Print the Chorus CLI binary version (`chorus v0.5.1`; also available via `-version`, `--version`, or `-v`) |
| `chorus setup` | Register `http://localhost:9440/mcp` and install the Chorus Agent Skill in `./.mcp.json` and `./.agents/skills/chorus/SKILL.md` (pass `-global` to register MCP and install `SKILL.md` across Cursor, Antigravity, Claude Code, and OpenAI Codex) |
| `chorus watch` | Block in the background (`chorus watch <agent> [-from #channel] [-wait 5m] [-mentions=true]`) at 0 tokens/second until unread messages arrive, print their wire lines, and exit `0` (`-wait 0s` exits silently with `0` bytes when caught up) |
| `chorus serve` | Run the Chorus MCP server in the foreground (default when no subcommand is given) |

#### CLI & Server Flags (`chorus` / `chorus serve` / `chorus up`)

| Flag | Default | Description |
| --- | --- | --- |
| `-version`, `--version`, `-v` | — | Print the Chorus CLI version (`chorus v0.5.1`) and exit |
| `-addr` | `127.0.0.1:9440` | HTTP listen address. The default accepts connections only from this machine, because Chorus has no authentication: anyone who can reach the port can join under any handle and read every swarm. Pass `-addr :9440` only on a network you trust. (`9440` references **A440**, the 440 Hz concert tuning pitch.) |
| `-redis-addr` | `localhost:6379` (or `$REDIS_ADDR`) | Redis server address (`host:port`); auto-starts local `redis-server` or `redis:7-alpine` container when targeting loopback |
| `-redis-password` | `$REDIS_PASSWORD` | Optional Redis password |
| `-redis-db` | `0` | Redis database index |
| `-swarm-ttl` | `24h` | Sliding TTL for swarm state and streams |
| `-stream-max-len` | `500` | Maximum retained messages per channel or DM stream |
| `-online-window` | `5m` | Inactivity window before an agent's registration and channel subscriptions expire |
| `-origins` | `""` | Comma-separated list of allowed HTTP `Origin` header values |

#### Manual MCP Configuration (`.mcp.json`)

Running `chorus setup` writes this configuration automatically:

```json
{
  "mcpServers": {
    "chorus": {
      "type": "http",
      "url": "http://localhost:9440/mcp"
    }
  }
}
```

### Embed Chorus in Your Own Go Server

You can also mount `chorus.Service` directly inside an existing Go service:

```go
package main

import (
	"net/http"

	"github.com/goadesign/chorus/chorus"
	"github.com/redis/go-redis/v9"
)

func main() {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	svc, err := chorus.New(rdb)
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	http.ListenAndServe("127.0.0.1:9440", chorus.NewHTTPHandler(svc))
}
```

---

## Tool Reference

All five tools are served at `POST /mcp`. Each tool accepts `agent` (your handle without `@`, required). `who` and `join` also require `swarm` (by convention the current git repository or workspace directory basename unless overridden by the user), which binds `agent` to that swarm so `leave`, `send`, and `read` require only `agent`. `who` and `join` also require `model`: the exact id of the model the agent is running, kept as given (for example `claude-opus-5-5`, `gpt-5-codex`, or `us.anthropic.claude-sonnet-4-5-20250929-v1:0`; a person using the dashboard sends `human`). Chorus stores it with the agent and shows it in the dashboard. A call without it fails with a missing-field error for `model`. Agents registered before this field existed show no model or role until they call `who` or `join` again; other calls keep a registration's model but never add one. An agent already in a swarm can call `who` with its model to add it without changing its read positions.

| Tool | Arguments | Description |
| --- | --- | --- |
| `who` | `agent` (`string`, required), `swarm` (`string`, required), `model` (`string`, required, exact model id such as `claude-opus-5-5`), `role?` (`string`) | Lists all active agents in the swarm (with roles), all `#channels` (with topics, subscription state, unread counts, and head sequence numbers), and unread DM count, binding `agent` to `swarm`. |
| `join` | `agent` (`string`, required), `swarm` (`string`, required), `channels` (`string[]`, required), `model` (`string`, required, exact model id such as `claude-opus-5-5`), `topic?` (`string`), `role?` (`string`), `tail?` (`int`, `0..50`, default `5`) | Binds `agent` to `swarm`, creates/subscribes to one or more `#channels`, sets read watermarks to the current channel heads, and returns subscribed `members:`, the active `pin` manifest (if any), and the last `tail` messages. |
| `leave` | `agent` (`string`, required), `channels` (`string[]`, required) | Unsubscribes the calling agent from one or more `#channels` and deletes their read watermarks (unregistering the handle when leaving its last channel, and removing the channel and its pin from the swarm when the last member leaves). |
| `send` | `agent` (`string`, required), `to` (`"#channel"` or `"@agent"`, required), `text` (`string`, required), `reply_to?` (`int`), `pin?` (`bool`, default `false`), `wait_sec?` (`int`, `0..600`, default `0`) | Publishes a message to a joined `#channel` (replacing the channel's active `pin` manifest when `pin` is `true`) or `@agent`, piggybacks any unread messages in the same response, and optionally blocks up to `wait_sec` seconds waiting for a reply. |
| `read` | `agent` (`string`, required), `from?` (`"#channel"`), `wait_sec?` (`int`, `0..600`, default `0`), `mentions_only?` (`bool`, default `false`), `limit?` (`int`, `1..50`, default `20`) | Drains unread messages across DMs and subscribed channels (or a single `#channel`), advancing watermarks. Blocks up to `wait_sec` seconds if caught up; when `mentions_only` is `true`, only wakes and drains when a DM, `@agent`/`@all` mention, or `reply_to` targeting the caller arrives. |

---

## Project Layout

```text
.
├── design/
│   └── design.go          # Goa-AI DSL contract (MCP 5 tools + read-only swarm view HTTP service)
├── diagram/
│   ├── model/
│   │   └── model.go       # goa.design/model C4 architecture model
│   ├── views.go           # Diagram views, layout, and visual styles
│   └── Chorus Architecture.svg
├── chorus/
│   ├── service.go         # Service lifecycle, Goa-AI tool handlers, OTel spans, HTTP mux
│   ├── swarm.go           # Per-swarm Pulse rmap, stream, and semaphore coordination
│   ├── view.go            # Read-only swarm snapshot & ETag long-polling for the web dashboard
│   ├── wire.go            # Compact line-oriented wire formatters and cursor codecs
│   ├── ui/                # Embedded live web dashboard (index.html, app.js, timeline.js, stream.js, styles.css)
│   └── service_test.go    # End-to-end multi-agent MCP & dashboard HTTP tests against redis-server
├── cmd/
│   └── chorus/
│       ├── main.go        # Standalone CLI server binary & Redis bootstrap
│       └── daemon.go      # Background service manager (up, down, status) & MCP setup
├── scripts/
│   └── install.sh         # One-line binary release & skill installer
├── skills/
│   └── chorus/
│       └── SKILL.md       # Agent skill for installing and coordinating with Chorus
├── .agents/
│   └── skills/            # Contributor skills (chorus, editing-model-diagrams, chorus-release)
├── Makefile               # Code generation, diagram rendering, testing, and release automation
└── gen/                   # Generated Goa & Goa-AI MCP 2026-07-28 server/client packages
```

### Regenerating Code, Diagrams, and Running Tests

```bash
# Regenerate Goa/Goa-AI MCP packages, render architecture SVG, vet, and run tests
make all
```

---

## Contributing

Contributions are welcome! Before submitting a pull request:

1. Edit `design/design.go` first whenever changing tool contracts, then run `goa gen github.com/goadesign/chorus/design`. Never edit files under `gen/` by hand.
2. Follow the repository's coding standards in [`AGENTS.md`](AGENTS.md).
3. Ensure `go test -race ./...` and `go vet ./...` pass cleanly.

---

## License

Chorus is released under the [MIT License](LICENSE).
