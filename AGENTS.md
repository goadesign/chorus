# AGENTS.md

## Repository Overview

Chorus (`github.com/goadesign/chorus`) is a token-minimal multi-agent coordination MCP (`2026-07-28`) server built with [Goa-AI](https://goa.design/docs/2-goa-ai/) (`goa.design/goa-ai`) and backed by [Pulse](https://github.com/goadesign/pulse) (`goa.design/pulse`).

- `design/design.go`: Single source of truth for the Goa-AI MCP service contract (`who`, `join`, `leave`, `send`, `read`) at `POST /mcp`.
- `diagram/`: [`goa.design/model`](https://github.com/goadesign/model) C4 architecture model (`model/model.go`), view/style DSL (`views.go`), and rendered SVG (`Chorus Architecture.svg`). **Never edit `diagram/*.svg` or `diagram/design.json` by hand**; run `make diagram`.
- `chorus/`: Core implementation (`service.go`, `swarm.go`, `wire.go`) and end-to-end MCP HTTP integration tests (`service_test.go`).
- `cmd/chorus/`: Standalone HTTP server binary, background service manager (`up`, `down`, `status`), MCP config registrar (`setup`), and zero-token background listener (`watch`) (`main.go`, `daemon.go`, `main_test.go`).
- `skills/`: Model-agnostic Chorus Agent Skill package (`README.md`, `chorus/SKILL.md`).
- `.agents/skills/`: Contributor and maintainer skills (`chorus`, `editing-model-diagrams`, `chorus-release`).
- `scripts/install.sh`: One-line release binary and skill installer.
- `Makefile`: Code generation (`make gen`), diagram rendering (`make diagram`), dependency updates (`make depend`), tests (`make test`), and release automation (`make release`).
- `gen/`: Generated Goa and Goa-AI MCP server, adapter, codec, and client code. **Never edit files under `gen/` by hand**; run `make gen` (`goa gen github.com/goadesign/chorus/design`).

---

## 1. Architecture, Personas, and Plain-English Contract

- **ARCHITECT (default for backend and systems work):** Reason top-down from system edges, trust boundaries, and true resource limits before inspecting or optimizing local functions—never bottom-up from whichever file is open or an adjacent local hook. Design from first principles for strong contracts, clear ownership, the fewest public concepts/contracts, less code (the best code is deleted or never written because it carries zero maintenance cost—never at the expense of conceptual correctness, elegance, exactness, robustness, performance, security, or clear contracts), and invalid states that cannot be represented. Never treat existing internal call shapes, transport hops, size caps, or unit tests of old mechanisms as given constraints when redesigning or optimizing a flow. Read relevant code before editing, plan concisely, verify with focused evidence, and revise assumptions when evidence disagrees.
- **REVIEWS:** Always begin by asking: *"Is this the simplest, most elegant approach? At a high level, what is the best from-scratch approach given all existing flows on `origin/main`?"* Assume the answer is **"No"** until you have traced every caller across `origin/main` and disproven simpler designs—starting with net deletion. Verify that every check, serialization format, size budget, and network hop lives at the owning architectural boundary rather than inside internal loops, that any fix for a state, memory, CPU, or coordination cost deletes the offending retained state, copy, or scan outright whenever current end-to-end flows allow rather than bounding, evicting, or flagging it, that truthful upstream state or telemetry is never filtered or distorted at an intermediate hop to steer a misspecified downstream consumer, and that the result is governed by strong contracts with no fallbacks or heuristics. Whenever a review pass identifies high-priority or design-touching findings, apply the fixes and automatically run a follow-up review pass via a fresh read-only `research` subagent (`Model: "pro"`) without waiting to be asked, repeating until a pass returns no high-priority or design-touching findings.
- **Plain-English contract (all replies, design gates, code comments, docs, PRs, and release notes):**
  - Always state the concrete input/event, what the code or system does (downstream calls and state changes), and what the caller or user receives/observes (and what errors mean) before naming any category label or abstraction.
  - Use clear, common words; define unavoidable domain terms on first use; and never rely on session-coined shorthand or metaphors (`canonical path`, `hydrate`, `project`, `bridge`, `fence`, `provider boundary`, `receipt`, `aggregate`, `selection`, `CAS`, `sidecar`, `shell`).
  - Include a concrete example whenever a label could hide accepted input, a decision, or changed output. Rewrite unless a newcomer unfamiliar with the subsystem can explain the input, action, and result without opening another file.

---

## 2. Design Decisions, Necessity, Boundaries, and Pre-Edit Gate

- **Necessity and challenging additions:**
  - Make simplicity and less code the first design priority while preserving required behavior and correctness. Start from the concrete user outcome and separate required obligations from assumptions, preferences, and historical artifacts: when the owning boundary changes, replace tests of obsolete internal mechanics with tests of the true boundary invariant.
  - Treat every added or retained state, stored copy, intermediary hop, filter/wrapper, serialization step, size cap, cache, overflow stash, coordination step, retry path, or setting as a reason to stop and recheck the premise. Before adding or splitting an endpoint, API field, public concept, durable record, caller step, tool variant, dependency, or numeric limit:
    1. Separate the observed failure from the proposed fix: when a downstream consumer produces the wrong verdict $F(x)$ on an upstream payload or span $x$, first check whether the emitted fact $x$ is false or the consumer predicate $F$ (or resource lifecycle) is misspecified, and inspect every field/attribute already carried on $x$. Never censor, filter, or distort truthful upstream state or telemetry at an intermediate hop to steer a downstream consumer whose predicate is wrong.
    2. Compare reusing the existing mechanism, correcting/extending its owning layer, and adding a separate mechanism: answer *"If we omit this, which required case fails, and why cannot the existing mechanism or corrected owning layer handle it?"* Actively try to disprove the addition by inspecting the existing path and payload schema for evidence that makes it unnecessary.
    3. Map the addition or limit to the exact decision or physical resource it protects, name the narrowest boundary and lifetime where its fact is invariant, and test a valid counterexample at the next wider lifetime.
    4. Treat the **5 symptom-management patterns**—(a) eviction/LRU/TTL/numeric caps on retained in-memory state, (b) in-process caches or overflow stashes, (c) copy/clone helpers or duplicate decoded representations alongside raw bytes, (d) intermediate filters/suppressors on truthful spans, errors, or payloads, and (e) repeated internal checks, wrappers, or mode flags inside an already-validated flow—as an automatic **"No"** (a failed design gate). First enumerate every caller across `origin/main` and test **Hypothesis 0 (deleting that retained state, copy helper, hop, or scan outright)** instead of managing the symptom.
- **Pre-edit gate (`MECHANICAL` vs. `DESIGN-TOUCHING`):** Fix mechanical changes (formatting, ordering, tests pinning established behavior, factual doc drift) directly. Before the first edit for any design-touching change (schema, contract, error semantics, component ownership, lifecycle predicates, or adding any hook/wrapper/lookup), read the owning contract (`design/design.go`, `README.md`) and all callers on `origin/main`, and state the gate in writing:
  1. **Causal scope and end-to-end caller inventory:** Trace the full end-to-end flow from ingress edge to storage/output on `origin/main`, and list every caller that reads or depends on the affected state, type, or hop.
  2. **Exact before/after contract and strength:** State what becomes required, derived, rejected, removed, or unchanged, and whether the contract becomes stronger or weaker (optional fields, dual modes, presence-dependent behavior, and wider enums are presumptive weakenings; two honest methods beat one method with modes).
  3. **Owning layer and from-scratch falsification (Hypothesis 0 first):** Answer *"What is the best from-scratch design across the full stack given all existing `origin/main` flows?"* First test **Hypothesis 0 (net deletion)**: state concretely which `origin/main` caller breaks if the offending retained state, copy helper, intermediate hop, or repeated check is deleted outright.
  4. **Effects and proof:** Explain user-visible behavior, compatibility, operational risk, implementation scope, and verification proof.
- **Top-down boundary placement and surface shrinking:**
  - **Single-edge validation:** Put required fields, enums, formats, patterns, lengths, and typed decoding in `design/design.go` so Goa and Goa-AI generate boundary validation; reserve handwritten validation in `chorus/` for cross-field rules, storage loads, or documented generator gaps. Trust constructed invariants internally and fail fast (no defensive guards, fallbacks, or silent recovery for contract-guaranteed values). Normalize strings (`strings.TrimSpace`) only at external boundaries where whitespace is insignificant.
  - **Public surface and zero-producer elimination:** Choose the smallest public contract (never export a type merely to simplify one call site or pass parsed data that a consumer can decode privately). Whenever removing or replacing a producer (tool, event emitter, enum variant, or API method), trace every downstream consumer, verify by repo-wide search whether any live producer still emits that branch, and delete any zero-producer vertical slice plus superseded helpers in the same change. A helper must enforce an invariant, own an external contract, remove meaningful duplication, or add domain language (never wrap a lookup, field access, literal, or pass-through value).

---

## 3. Agentic Systems and MCP Contract Design

- **Mechanical vs. semantic decisions:**
  - **Mechanical decisions** (values derivable from typed contracts/results or execution state—such as cursor watermarks, sequence numbers,subscription tracking, and the default `"default"` swarm namespace) belong to code via schema defaults, codecs, or runtime state. Never ask the LLM to pass or repeat opaque cursors in tool arguments.
  - **Semantic decisions** (the agent's chosen `agent` handle, optional `swarm` namespace override, which `#channel` or `@agent` to message, message text, role summary, or whether to block via `wait_sec`) stay model-driven with concise, well-constrained schemas.
- **Token-minimal wire representation:**
  - Keep tool results projected via `ToolContent("content")` in `design/design.go` so `outputSchema` and `structuredContent` remain omitted from MCP `tools/list` and `tools/call` responses.
  - Treat tool and argument `Description` strings in `design/design.go` as model-facing prompts: state the concrete effect, input format, and result format concisely without exposing internal orchestration or storage implementation details.

---

## 4. Go Coding Rules, Observability, Testing, and Published Changes

- **Go style and organization:**
  - Format with `go fmt ./...` and group standard-library imports separately from external imports.
  - Prefix every generated-package import alias under `gen/` with `gen` (e.g., `genchorus`, `genmcpchorus`, `genmcpchorussrv`, `genmcpchorusclient`—never bare service or generic transport aliases).
  - Use `lower_snake_case.go` filenames (`<=1000` lines), short lowercase package names, `any` over `interface{}`, `%w` error wrapping with `errors.Is`/`errors.As` (never silently discard errors where failure matters), 1-line function signatures `<=100` columns where practical, combined adjacent same-type parameters, `len(x) == 0` without redundant `nil` checks, multi-line blocks, trailing-comma struct literals, grouped `type (...)` blocks with field comments inside, and built-in `min`/`max`/`clear`.
  - **Declaration order & composition roots:** Order declarations within every file top-down as:
    1. `types` (`type (...)`)
    2. `consts` (`const (...)`)
    3. `vars` (`var (...)`)
    4. Exported functions (`New`, `NewHTTPHandler`, `With...`)
    5. Exported methods
    6. Unexported functions
    7. Unexported methods
    Place main entry-point logic first and helpers last; prefer named helpers over large anonymous functions, especially for concurrency. Command entrypoints (`cmd/chorus/main.go`) are composition roots that parse flags/env, construct downstream clients (`*redis.Client`), and pass instantiated dependencies into runtime packages (`chorus.New`); runtime packages never read flags/env or construct external clients from raw addresses.
- **Comments:**
  - Add a plain-English header comment to every declaration—packages, files, functions, methods (both exported and unexported), types/interfaces, structs, struct fields, and `const`/`var` declarations—unless genuinely trivial.
  - Exported declarations and fields require stdlib-quality GoDoc stating what the construct is for, who calls or uses it, important invariants, ownership/lifecycle expectations, and error semantics.
  - Unexported declarations and struct fields must state their contract, role, or invariant rather than restating the identifier name.
  - Comment non-trivial multi-step flows (such as linearizable stream publication and watermark advancement) at their entry point and phase transitions, and reread every changed file end-to-end before finishing to verify no non-trivial declaration or struct field lacks a header comment.
- **Observability:**
  - Use OpenTelemetry traces (`go.opentelemetry.io/otel`) for application behavior: create a span per tool operation, preserve parent/child links, record exact bounded attributes, and record actual operation failures with both `span.RecordError(err)` and `codes.Error` status (never marking expected negative states such as invalid tool input or empty reads as span errors).
- **Generated code and tests:**
  - Never edit files under `gen/` by hand; edit `design/design.go` and run `goa gen github.com/goadesign/chorus/design`.
  - Write fast, deterministic tests covering both protocol validation and multi-agent concurrent workflows, and verify all changes with:
    ```bash
    go test -race ./...
    go vet ./...
    ```
- **Pull requests, commits, and release notes:**
  - Write standalone plain-English commit messages, PR descriptions, and release notes for a developer unfamiliar with the authoring conversation: lead with the affected capability, the concrete problem and why it matters, and the resulting behavior before implementation details.
  - Never add AI co-author trailers, generated-by notices, or assistant mentions to commits, pull requests, issues, reviews, comments, or release notes, and never prefix branch names with model/assistant names (`codex/`, `gpt/`, `claude/`).
