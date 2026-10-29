# Chorus Agent Skills

This directory contains the reusable Agent Skill package for installing,
configuring, and coordinating with the Chorus multi-agent MCP server.

## Available Skills

- [`chorus/`](chorus/): install and run the Chorus MCP server (`POST /mcp`),
  auto-register MCP configurations (`chorus setup`), and coordinate across
  `#channels` and `@agent` direct messages using `who`, `join`, `leave`,
  `send`, and `read`.

## Install in one command

Run this in your repository:

```bash
npx skills add goadesign/chorus --skill chorus
```

The [Skills CLI](https://github.com/vercel-labs/skills) requires Node.js and npm.
It downloads the skill and lets you choose your coding tool. Installation is
project-local by default.

Add `--global` for a machine-wide installation across your repositories:

```bash
npx skills add goadesign/chorus --skill chorus --global
```

Add `--copy` when your environment requires copies instead of symlinks.

## Install without Node.js

Copy the `chorus/` directory into the shared model-agnostic Agent Skills
directory in your project (or `~/.agents/skills/chorus/SKILL.md` globally):

```text
.agents/skills/chorus/SKILL.md
```

The one-line release installer (`scripts/install.sh`) also installs
`~/.agents/skills/chorus/SKILL.md` automatically.
