# Chorus launcher for Herdr

Open the launcher from a project terminal with `herdr-app do mod.open --name chorus-launcher`.
On macOS, `chorus up` or `chorus setup -global` also adds **Command–Option–Shift–C**
when the existing Herdr keyboard file leaves that shortcut free in every section.
Existing bindings are preserved. If the keyboard file is missing, open Herdr
and rerun setup. Adding the shortcut makes that file user-managed: Herdr will
no longer replace it with new default shortcuts during upgrades.
Choose the agent app, model, thinking level, and YOLO setting for each new pane,
then choose **Open Chorus team**. The dialog dismisses automatically once all
requested panes have been created and labeled in the team tab; if a step fails,
the dialog stays open with completed panes marked read-only so you can retry the
rest.

The project is the Git checkout containing the focused terminal's live folder,
including a linked worktree. A space's display name does not choose the folder.

On first use in a repository, the launcher includes one row for each installed
agent app:
- **Claude Code** selects **Claude Opus 5.5** (`claude-opus-5-5`) with **App default** thinking.
- **Codex** selects **GPT-6.1 Sol** (`gpt-6.1-sol`) with **App default** thinking.
- **Cursor** selects **Claude Opus 5.5 1M** at **High** thinking (`claude-opus-5-5-high`).
- **Antigravity** selects **Gemini 4 Argon** (`gemini-4-argon`) at **High** thinking (`--effort high`).

Your chosen Chorus name and team rows (client, model, thinking level, and YOLO
mode) are remembered per project folder in Herdr's shared state. Reopening the
launcher in the same project checkout after closing its panes restores your
saved choices, while live terminals remain the source of truth whenever a team
tab is currently running.

**App default** deliberately uses your client's configuration. Choosing another
model, thinking level, or App default survives a model-options refresh. **Remove**
excludes a new row; **Add agent** adds another pane, including another copy of
the same client. **Add all installed** fills in installed apps that have no row
without discarding your choices.

## Models and thinking levels

The launcher checks options from the selected project directory using a login,
interactive zsh, so configured shell-function commands also work:
- **Claude Code:** offers `opus`, `sonnet`, and `haiku` aliases (plus the default
  `claude-opus-5-5` ID), and passes `--effort <level>` when `--effort` is
  advertised by `claude --help`. Claude checks model and effort support at startup.
- **Codex:** reads models and each model's `supported_reasoning_levels` from
  `codex debug models`, passing `-c model_reasoning_effort="<level>"` when a
  thinking level is chosen.
- **Antigravity:** groups variant rows from `agy models` by base model with their
  advertised levels and passes `--model <base> --effort <level>`. Entering a
  custom model ID that already ends with an effort suffix leaves thinking set by
  that model ID so `--effort` never conflicts.
- **Cursor:** groups multi-level model families from `cursor-agent --list-models`
  so the Thinking selector switches among the exact variant slugs Cursor lists
  (for example `claude-opus-5-5-low` through `claude-opus-5-5-max`), while
  single-variant models show **Set by model**.

**Enter model ID…** lets you specify an exact ID when the client supports `--model`.
No model catalog is saved. If discovery fails, the row explains how to retry; you
can still deliberately choose **App default**.

## YOLO mode

YOLO is off on every new row until enabled (or restored from your saved
repository settings). Off adds no permission flags and uses the client's existing
settings, which may already be permissive. Turning it on adds the following
client flag, only when the installed client's help advertises it:

| Client | Flag | Effect |
| --- | --- | --- |
| Claude Code | `--dangerously-skip-permissions` | Bypasses tool permission checks |
| Codex | `--dangerously-bypass-approvals-and-sandbox` | Disables approval prompts and sandboxing |
| Antigravity | `--dangerously-skip-permissions` | Auto-approves tool permission requests |
| Cursor | `--yolo` | Allows commands unless explicitly denied; keeps the sandbox setting |
| Gemini CLI | `--yolo` | Auto-approves tool actions; keeps the sandbox setting |

**Apply YOLO to all** affects only new rows whose client supports the flag.
**Turn YOLO off for all** restores those rows to client settings. Changing to a
client without verified YOLO support turns it off. Login and client startup
consent prompts can still require attention.

## Existing teams and retries

New panes open in a dedicated **Chorus · project** tab and invoke the installed
Chorus skill with unique handles. They join the displayed swarm and channel,
report their actual model, and wait for work. Give your goal to one pane.

Reopen the launcher to see the existing team's panes and recorded launch
settings. These rows are read-only: add another row to start a different model,
thinking level, or permission mode. Older panes show **Not recorded** for fields
launched before those labels existed rather than guessing. **Reload team**
refreshes the current pane list and keeps unopened rows.

Herdr owns all tab, pane, and process records via terminal labels
(`chorus.project`, `chorus.swarm`, `chorus.agent`, `chorus.row`, and
`chorus.options`). A kernel lock serializes launches and is released if the
helper exits.

## Updating Chorus

The launcher checks for a newer Chorus release without delaying team discovery.
When it finds one, or the installed binary needs to restart the older running
server, it offers **Update & restart**. Clicking approves `chorus update -yes`
from the project folder. The CLI installs the release, refreshes its bundled
skills, MCP configs, and launcher files, and restarts the server with its saved
settings. Existing agent conversations remain in their panes, and unopened
rows keep their selected options. If installation fails, the notice shows the
CLI's error and lets you retry; an unavailable release check does not prevent
opening a team. You can also run `chorus update` from a terminal to review the
versions and approve there.

## Requirements and checks

This launcher supports **local macOS or Linux projects** (not Windows) and uses
Herdr's `herdr-app` and `herdr-core` CLI links, Python 3 (standard library
only), and `/bin/zsh`. The `chorus` CLI must be installed and its server running
(`chorus up`) before opening a team. When Herdr's app directory
(`$HERDR_APP_HOME` or `~/.config/herdr-app`) already exists on the machine,
`chorus up` and `chorus setup -global` install the runtime mod files
(`mod.json`, `main.js`, and `launch.py`) into `<herdrHome>/mods/chorus-launcher`.

Run checks from the source directory containing the test files:

```sh
CI=true python3 -B -m unittest -v test_launch.py </dev/null
CI=true node --test test_ui.mjs </dev/null
CI=true herdr-app check "${HERDR_APP_HOME:-$HOME/.config/herdr-app}/mods/chorus-launcher" </dev/null
```

Open the launcher from a script:

```sh
CI=true herdr-app do mod.open --name chorus-launcher </dev/null
```
