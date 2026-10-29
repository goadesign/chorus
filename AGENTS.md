# AGENTS.md

## Repository Overview

Chorus (`github.com/goadesign/chorus`) is a token-minimal multi-agent coordination MCP (`2026-07-28`) server built with [Goa-AI](https://goa.design/docs/2-goa-ai/) (`goa.design/goa-ai`) and backed by [Pulse](https://github.com/goadesign/pulse) (`goa.design/pulse`).

- `design/design.go`: Single source of truth for the Goa-AI MCP service contract (`who`, `join`, `leave`, `send`, `read`) and route-bound identity (`POST /mcp/{swarm}/{agent}`).
- `chorus/`: Core implementation (`service.go`, `swarm.go`, `wire.go`) and end-to-end MCP HTTP integration tests (`service_test.go`).
- `cmd/chorus/`: Standalone HTTP server binary.
- `scripts/install.sh`: One-line release binary installer.
- `skills/chorus/SKILL.md`: Agent skill teaching installation and multi-agent coordination workflows.
- `gen/`: Generated Goa and Goa-AI MCP server, adapter, codec, and client code. **Never edit files under `gen/` by hand**; run `goa gen github.com/goadesign/chorus/design`.

## Coding Rules

- Follow all global guidelines in `~/AGENTS.md`, including top-down declaration ordering (`types`, `consts`, `vars`, `public funcs`, `public methods`, `private funcs`, `private methods`) and plain-English header comments on all non-trivial declarations.
- Prefix all imports of generated packages under `gen/` with `gen` (e.g., `genchorus`, `genmcpchorus`, `genmcpchorussrv`, `genmcpchorusclient`).
- Keep model-visible tool schemas free of route-bound identity parameters (`swarm`, `agent`) and keep tool results projected via `ToolContent("content")` so `outputSchema` and `structuredContent` remain omitted.
- Verify all changes with `go test -race ./...` and `go vet ./...`.
