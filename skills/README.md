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

## Start the Skill

Open a session in your repository and start the skill with no further instructions:

| Agent | Command |
| --- | --- |
| Claude Code, Antigravity, Cursor | `/chorus` |
| Codex | `$chorus` (or pick `chorus` from `/skills`; Codex has no bare `/chorus` command) |
| Gemini CLI and other agents | *"Use the chorus skill"* |

The agent joins the swarm named after the repository folder and its `#<swarm>` channel (in `~/src/auth`: swarm `auth`, channel `#auth`), uses its model name as its handle (give each pane of the same model its own name, such as `claude-2`, because Chorus does not stop two panes in one swarm from sharing a handle), starts `chorus watch <handle>` in the background, and reports that it is ready. Do this in every pane, then give the goal to one of them. That agent pins the plan as a task board with one owner per task, and the others pick up their part from it. By default each owner decides choices inside its own task, proposes changes that affect others with a deadline for objections, and waits for an explicit approval before a merge, release, or deploy. To change these rules for a swarm, say so in your prompt or in the pinned plan, for example *"every change needs @qa's approval"*. To use a different swarm, channel, or handle, name it in your prompt instead, for example *"Use Chorus as `api` in swarm `auth`, channel `#passkeys`."* Channel names are limited to 32 characters after the `#`, so if the folder name is longer, the agent tells you and asks you to name a shorter channel instead.

## Available Skills

| Skill | Path | Description |
| --- | --- | --- |
| `chorus` | [`chorus/SKILL.md`](chorus/SKILL.md) | 4-phase multi-agent coordination workflow (`who`, `join`, `send`, `read`, `leave`), swarm binding, zero-token background watching (`chorus watch <agent>`), a pinned task board with one owner per task, a default decision protocol (owners decide reversible choices, lazy consensus for shared ones, explicit approval for merges and deploys), and `#channel` vs. `@agent` routing. |
