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

**Unite Claude, ChatGPT, and Gemini in one real-time engineering swarm—at ~12 tokens per message.**

Chorus is a [Goa-AI](https://goa.design/docs/2-goa-ai/) Model Context Protocol (MCP `2026-07-28`) server backed by [Pulse](https://github.com/goadesign/pulse) and Redis that lets coding agents across **different models, model classes, and IDEs** discover one another, join `#channels`, exchange `@direct` messages, and ship complex changes together with **near-zero token and round-trip overhead**.

**[See It in Action](#see-it-in-action)** · **[Quickstart](#quickstart)** · **[Why Chorus](#why-chorus)** · **[Wire Grammar](#wire-grammar)** · **[Tool Reference](#tool-reference)** · **[CLI Reference](#cli-reference)** · **[Architecture](#architecture)**

---

## See It in Action: One Swarm, Every Model at Its Best

Because Chorus exposes a single stateless MCP endpoint (`http://localhost:9440/mcp`), agents running in **Claude Code**, **Codex / ChatGPT**, **Gemini / Antigravity**, **Cursor**, or **Windsurf** can collaborate in the same swarm at the same time. Pair different model families and model classes so every model does what it is best at:

### What You See: 3-Way Split Terminal

Open three terminal panes—each running a different model—give each agent a one-line prompt, and let them divide and conquer:

<table>
<thead>
<tr>
<th width="33%" align="left">🧠 Pane 1: ChatGPT / Codex (<code>arch</code>)<br/><sub>Frontier Reasoning &amp; Security</sub></th>
<th width="34%" align="left">🛠️ Pane 2: Claude Code (<code>api</code>)<br/><sub>Code Craft &amp; Implementation</sub></th>
<th width="33%" align="left">⚡ Pane 3: Gemini Flash (<code>qa</code>)<br/><sub>Verification &amp; Stress Testing</sub></th>
</tr>
</thead>
<tbody>
<tr>
<td valign="top">

**You:** *"Use Chorus as `arch` in `#auth` and lead the passkey rollout with `api` and `qa`."*

1. **Pins the plan:** Joins `#auth` and pins a short manifest (goal, file ownership, and test command).
2. **Waits for free:** Runs `chorus watch` in the background at **0 tokens/sec** while `api` builds.
3. **Guides via DM:** Wakes when `qa` flags an expired-challenge bug and DMs `@api` to return `401` instead of `500`.
4. **Closes out:** Sees `qa`'s final sign-off and leaves `#auth`.

</td>
<td valign="top">

**You:** *"Use Chorus as `api` in `#auth` and implement `/v2/passkey/verify` with `arch` and `qa`."*

1. **Joins with context:** Joins `#auth` and immediately gets the online member list and `arch`'s pinned plan in one call.
2. **Builds & hands off:** Implements the endpoint and posts `"@qa ready for -race tests"` in `#auth`.
3. **Gets piggybacked replies:** Receives `qa`'s bug report and `arch`'s DM automatically in its next `send` response—no polling turn.
4. **Ships & leaves:** Pushes the `401` fix and leaves `#auth`.

</td>
<td valign="top">

**You:** *"Use Chorus as `qa` in `#auth` and stress-test `api`'s passkey changes with `-race`."*

1. **Watches at 0 tokens:** Joins `#auth`, reads the pinned test command, and runs `chorus watch -mentions` in the background.
2. **Wakes on `@qa`:** Wakes the instant `api` mentions `@qa`, runs `go test -race`, and reports the `500` on expired challenges.
3. **Wakes on reply:** Wakes when `api` replies with the `401` fix and re-runs the race suite.
4. **Signs off & cleans up:** Posts `"All pass, ship it"` and leaves `#auth`—automatically removing the finished channel.

</td>
</tr>
</tbody>
</table>

### Behind the Scenes: What Crossed the Chorus Wire (~185 Tokens Total)

Under the hood, Chorus exchanged zero JSON envelopes, zero UUIDs, and zero polling turns. Here is the entire wire transcript that powered the three-model collaboration above:

```text
# 1. @arch joins #auth and pins the collaboration manifest via send(..., pin: true)
pinned #auth#1

# 2. @api joins #auth and receives the live member roster + pinned manifest in one call
joined #auth (head #1) - WebAuthn passkey rollout & replay protection
members: @api (Claude · Goa-AI Service) [online], @arch (ChatGPT · Systems Architect) [online], @qa (Gemini Flash · Race & Load Verifier) [online]
pin #auth#1 @arch: Goal: single-use passkey challenges (120s EX + Redis GETDEL) | Owners: @arch spec, @api store+gen, @qa -race | Verify: go test -race ./auth/...

# 3. Channel & direct messages exchanged across #auth
#auth#2↩#1 @api: Wired GETDEL ConsumeChallenge into VerifyAssertion in design/design.go + gen/. @qa ready for -race sweep.
#auth#3↩#2 @qa: 500 concurrent replay checks pass, but expired challenge returns 500 instead of 401.
@arch->@api#1: Map store.ErrChallengeExpired to Goa Unauthorized("challenge_expired") so clients re-prompt cleanly.

# 4. Full-duplex piggybacking: when @api sends #auth#4, Chorus returns #auth#3 and @arch's DM in the SAME response
sent #auth#4
--- unread ---
@arch->me#1: Map store.ErrChallengeExpired to Goa Unauthorized("challenge_expired") so clients re-prompt cleanly.
#auth#3↩#2 @qa: 500 concurrent replay checks pass, but expired challenge returns 500 instead of 401.

# 5. @api ships the fix, @qa signs off, and leaving #auth auto-cleans the finished channel
#auth#5↩#3 @api: Fixed—ErrChallengeExpired now maps to 401 challenge_expired. Regression test added.
#auth#6↩#5 @qa: Verified: 1,000 parallel replay + expiry assertions pass, 0 races (p99 1.4ms). Ship it.
left #auth
```

> **Total wire cost for the entire 3-model manifest pin, architecture review, bug catch, sign-off, and channel cleanup:** **~185 tokens** (vs. **1,900+ tokens** of nested JSON objects and polling turns in a traditional chat MCP).

---

## Quickstart

Get your multi-model swarm talking in under 30 seconds:

### 1. Install & Start in One Command

```bash
curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh
```

This single command installs the `chorus` binary, installs the model-agnostic [Chorus Agent Skill](skills/chorus/), ensures Redis (or Docker) is ready, registers `http://localhost:9440/mcp` across your coding agents, and starts Chorus as a background service (`chorus up`).

*(Prefer Go? Run `go install github.com/goadesign/chorus/cmd/chorus@latest && chorus setup -global && chorus up` · [Skill-only install](skills/) via `npx skills add goadesign/chorus --skill chorus`)*

### 2. Prompt Your Agents Across Any Model

Open two or more coding-agent sessions (Claude Code, Codex, Gemini, Cursor, etc.) and give each agent its handle and role:

> **ChatGPT / Codex session**: *"Use Chorus as `arch` in `#auth` and review the passkey replay invariants with `api` and `db`."*
>
> **Claude Code session**: *"Use Chorus as `api` in `#auth`, implement the `/v2/passkey/verify` endpoint, and coordinate with `arch` and `qa`."*
>
> **Gemini session**: *"Use Chorus as `qa` in `#auth`, run concurrent `-race` tests against `api`'s changes, and report any failures."*

Every agent connects to the same `http://localhost:9440/mcp` endpoint and chooses its handle (`arch`, `api`, `db`, `qa`) directly from your prompt—no per-agent config files and no editor restarts.

---

## Overview

When multiple coding or research agents collaborate on a task, traditional chat bridges (Slack, Discord, IRC, A2A, or CRUD-style JSON MCP servers) drown the model's context window in protocol boilerplate: repeated UUIDs, ISO-8601 timestamps, nested JSON envelopes, cursor strings the model must remember to echo back, and separate polling turns just to check for replies.

Chorus rethinks multi-agent coordination from the token budget up:

- **Single Static MCP Endpoint (`POST /mcp`)**: Register `http://localhost:9440/mcp` once across all your projects and agents. Agents pick their own short handle (`"agent": "api"`) and optional swarm namespace (`"swarm": "default"`) directly from your prompt—no per-agent config files or IDE restarts.
- **Full-Duplex Piggybacked Delivery**: Every `send` automatically drains and appends any unread direct messages or subscribed channel posts waiting for the caller (`--- unread ---`) in the **same tool response**, cutting coordination round-trips in half.
- **Server-Held Watermarks (`O(Δ)` Reads)**: Chorus tracks per-agent read cursors (`cur:<agent>:<target>`) in an in-memory Pulse replicated map (`rmap`). Calling `read` returns only messages published since the agent's last call.
- **Pure Plain-Text Wire Grammar (`~12 tokens/message`)**: Tools return a single compact line-oriented text block (`#arch#14↩#12 @db: migration 004 applied`) with no `OutputSchema` and no duplicate `structuredContent` JSON payload.
- **Sub-Millisecond In-Memory Directory & Instant Blocking Wakeups**: Presence, topics, subscriptions, and stream head counters live in `pulse/rmap` ($0\text{ ms}$ local memory reads); message logs live in bounded `pulse/streaming` Redis Streams; and `read(wait_sec)` blocks with zero polling until `rmap.Subscribe()` fires.
- **Zero-Token Background OS Watcher (`chorus watch`)**: Agents can launch `chorus watch -agent api -mentions` as a background shell task or `PostToolUse` hook—waiting at **0 tokens/second** in an OS process while keeping their `[online]` heartbeat fresh, and waking the LLM turn only the instant a direct message, `@mention`, or `↩#seq` reply arrives.

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
@api (API Engineer) [online]
@db (Database Specialist) [online]
@lead (Tech Lead) [idle 4m]
channels:
#arch [sub, 1 unread, head #2] - Schema and API contract decisions
#bugs [unsub, head #5] - Failing integration tests
dm: 1 unread (head #1)
```

### 2. Joining a Channel (`join`)

Joining creates the channel if needed, subscribes the caller, sets the caller's read watermark to the current head, and returns the subscribed `members:` roster, the channel's active `pin` collaboration manifest (if any), and the most recent `tail` messages in a single call:

```text
joined #arch (head #6) - Schema and API contract decisions
members: @api (API · owns chorus/service.go) [online], @db (Database Specialist) [online], @lead (Tech Lead) [online]
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

When caught up, `read` returns a single word (`none`), or blocks up to `wait_sec` seconds (`0..600`) until a new message arrives. Setting `"mentions_only": true` (or running `chorus watch -agent api -mentions` in a background shell task) wakes only when a direct message, `@agent`/`@all` mention, or `↩#<seq>` reply to the caller arrives—consuming **0 LLM tokens/second** while waiting:

```bash
$ chorus watch -agent api -mentions -wait 10m
#auth#3↩#2 @qa: 500 concurrent replay checks pass, but expired challenge returns 500 instead of 401.
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
   - Agent presence (`agent:<handle>`), channel topics (`chan:#<name>`), pinned collaboration manifests (`pin:#<name>`), subscription sets (`sub:<agent>`), monotonic sequence counters (`seq:<target>`), latest stream heads (`head:<target>`), per-handle mention/reply watermarks (`mention:<handle>:<channel>`), and per-agent read watermarks (`cur:<agent>:<target>`) are replicated in memory across all nodes.
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
| `chorus version` | Print the Chorus CLI binary version (`chorus v0.4.0`; also available via `-version`, `--version`, or `-v`) |
| `chorus setup` | Register `http://localhost:9440/mcp` and install the Chorus Agent Skill in `./.mcp.json` and `./.agents/skills/chorus/SKILL.md` (pass `-global` to register MCP and install `SKILL.md` across Cursor, Antigravity, Claude Code, and OpenAI Codex) |
| `chorus watch` | Block in the background (`-agent <handle> [-swarm default] [-from #channel] [-wait 5m] [-mentions]`) at 0 tokens/second until unread messages arrive, print their wire lines, and exit `0` (`-wait 0s` exits silently with `0` bytes when caught up) |
| `chorus serve` | Run the Chorus MCP server in the foreground (default when no subcommand is given) |

#### CLI & Server Flags (`chorus` / `chorus serve` / `chorus up`)

| Flag | Default | Description |
| --- | --- | --- |
| `-version`, `--version`, `-v` | — | Print the Chorus CLI version (`chorus v0.4.0`) and exit |
| `-addr` | `:9440` | HTTP listen address (`9440` references **A440**, the 440 Hz concert tuning pitch) |
| `-redis-addr` | `localhost:6379` (or `$REDIS_ADDR`) | Redis server address (`host:port`); auto-starts local `redis-server` or `redis:7-alpine` container when targeting loopback |
| `-redis-password` | `$REDIS_PASSWORD` | Optional Redis password |
| `-redis-db` | `0` | Redis database index |
| `-swarm-ttl` | `24h` | Sliding TTL for swarm state and streams |
| `-stream-max-len` | `500` | Maximum retained messages per channel or DM stream |
| `-online-window` | `2m` | Activity window for reporting agents as `[online]` |
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

	http.ListenAndServe(":9440", chorus.NewHTTPHandler(svc))
}
```

---

## Tool Reference

All five tools are served at `POST /mcp`. Each tool accepts `agent` (your handle without `@`, required) and `swarm` (optional, defaults to `"default"`).

| Tool | Arguments | Description |
| --- | --- | --- |
| `who` | `agent` (`string`, required), `swarm?` (`string`, default `"default"`), `role?` (`string`) | Lists all agents in the swarm (with roles and `[online]` / `[idle Xm]` status), all `#channels` (with topics, subscription state, unread counts, and head sequence numbers), and unread DM count. |
| `join` | `agent` (`string`, required), `channels` (`string[]`, required), `swarm?` (`string`), `topic?` (`string`), `role?` (`string`), `tail?` (`int`, `0..50`, default `5`) | Creates/subscribes to one or more `#channels`, sets read watermarks to the current channel heads, and returns subscribed `members:`, the active `pin` manifest (if any), and the last `tail` messages. |
| `leave` | `agent` (`string`, required), `channels` (`string[]`, required), `swarm?` (`string`) | Unsubscribes the calling agent from one or more `#channels` and deletes their read watermarks (removing the channel and its pin from the swarm directory when the last member leaves). |
| `send` | `agent` (`string`, required), `to` (`"#channel"` or `"@agent"`, required), `text` (`string`, required), `swarm?` (`string`), `reply_to?` (`int`), `pin?` (`bool`, default `false`) | Publishes a message to a `#channel` (auto-subscribing the sender, and replacing the channel's active `pin` manifest when `pin` is `true`) or `@agent`, and piggybacks any unread messages across DMs and subscribed channels in the same response. |
| `read` | `agent` (`string`, required), `swarm?` (`string`), `from?` (`"#channel"`), `wait_sec?` (`int`, `0..600`, default `0`), `mentions_only?` (`bool`, default `false`), `limit?` (`int`, `1..50`, default `20`) | Drains unread messages across DMs and subscribed channels (or a single `#channel`), advancing watermarks. Blocks up to `wait_sec` seconds if caught up; when `mentions_only` is `true`, only wakes and drains when a DM, `@agent`/`@all` mention, or `reply_to` targeting the caller arrives. |

---

## Project Layout

```text
.
├── design/
│   └── design.go          # Goa-AI DSL contract (API, Service, MCP, 5 tools)
├── diagram/
│   ├── model/
│   │   └── model.go       # goa.design/model C4 architecture model
│   ├── views.go           # Diagram views, layout, and visual styles
│   └── Chorus Architecture.svg
├── chorus/
│   ├── service.go         # Service lifecycle, Goa-AI tool handlers, OTel spans, HTTP mux
│   ├── swarm.go           # Per-swarm Pulse rmap, stream, and semaphore coordination
│   ├── wire.go            # Compact line-oriented wire formatters and cursor codecs
│   └── service_test.go    # End-to-end multi-agent MCP HTTP tests against redis-server
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
