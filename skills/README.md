# Chorus Agent Skills

This directory packages the model-agnostic [Chorus Agent Skill](chorus/SKILL.md) for coordinating multiple coding agents (Claude Code, Codex, Gemini / Antigravity, Cursor, Windsurf) over the Chorus MCP server (`POST /mcp`).

## Quick Install

Install the skill into your current repository in one command using the [Skills CLI](https://github.com/vercel-labs/skills) (requires Node.js):

```bash
npx skills add goadesign/chorus --skill chorus
```

Pass `--global` to install machine-wide across all repositories (`npx skills add goadesign/chorus --skill chorus --global`), or `--copy` when your environment requires file copies instead of symlinks.

## Install via `chorus setup` or Without Node.js

If you already have the `chorus` CLI installed (or used `scripts/install.sh`), register MCP and install the skill in one step:

```bash
chorus setup          # project-local (.mcp.json + .agents/skills/chorus/SKILL.md)
chorus setup -global  # global across Claude Code, Codex, Cursor, and Antigravity
```

To install manually without Node.js or the CLI, copy [`chorus/SKILL.md`](chorus/SKILL.md) to `.agents/skills/chorus/SKILL.md` in your repository (or `~/.agents/skills/chorus/SKILL.md` globally).

## Available Skills

| Skill | Path | Description |
| --- | --- | --- |
| `chorus` | [`chorus/SKILL.md`](chorus/SKILL.md) | 4-phase multi-agent coordination workflow (`who`, `join`, `send`, `read`, `leave`), swarm binding, zero-token background watching (`chorus watch <agent>`), pinned manifests, and `#channel` vs. `@agent` routing. |
