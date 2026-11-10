"""Inspect and open a project's chosen coding agents through Herdr commands.

Herdr owns all process and tab state. Terminal labels identify panes created by
this launcher; an OS lock serializes creation without retaining a team index.
"""

from __future__ import annotations

import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
from typing import Callable
import uuid


# Each command receives argv directly; project paths and prompts never become code.
Command = Callable[[list[str]], object]


# These are client flags, not model names. Unknown clients keep their own settings.
CLIENTS = {
    "claude": {"yolo": "--dangerously-skip-permissions", "thinking": "--effort", "prompt": []},
    "codex": {"yolo": "--dangerously-bypass-approvals-and-sandbox", "thinking": "-c", "prompt": []},
    "antigravity": {"yolo": "--dangerously-skip-permissions", "thinking": "--effort", "prompt": ["--prompt-interactive"]},
    "cursor": {"yolo": "--yolo", "thinking": None, "prompt": []},
    "gemini": {"yolo": "--yolo", "thinking": None, "prompt": ["--prompt-interactive"]},
    "opencode": {"yolo": None, "thinking": None, "prompt": ["--prompt"]},
}

# Antigravity and Cursor encode effort suffixes directly into variant model IDs.
AGY_SUFFIX = re.compile(r"-(low|medium|high|xhigh|max)$")
CURSOR_EFFORTS = ("none", "minimal", "low", "medium", "high", "xhigh", "extra-high", "max")
CURSOR_VARIANT = re.compile(r"^(?P<base>[A-Za-z0-9._:/+=-]+?)-(?P<effort>" + "|".join(CURSOR_EFFORTS) + r")(?P<fast>-fast)?$")
CURSOR_SUFFIX_LABEL = {
    "none": "None",
    "minimal": "Minimal",
    "low": "Low",
    "medium": "Medium",
    "high": "High",
    "xhigh": "Extra High",
    "extra-high": "Extra High",
    "max": "Max",
}


def agent_command(agent: dict) -> None:
    """Require a named client and literal argv before invoking a local program."""
    if not all(isinstance(agent.get(k), str) and agent[k] for k in ("key", "name")):
        raise ValueError("Herdr returned an unnamed agent.")
    argv = agent.get("run")
    if not isinstance(argv, list) or not argv or not all(isinstance(v, str) and v for v in argv):
        raise ValueError(f"{agent['name']} has no valid launch command in Herdr.")


def help_efforts(help_text: str) -> list[str]:
    """Extract advertised --effort levels from CLI help text when present."""
    match = re.search(
        r"(?<![\w-])--effort(?![\w-])[^\n()]*(?:\n[ \t]+(?![-\s])[^\n()]*)*\((?:choices:\s*)?([a-z0-9_,\s|-]+)\)",
        help_text,
    )
    if not match:
        return []
    return [
        token.strip()
        for token in re.split(r"[,|\s]+", match.group(1))
        if re.fullmatch(r"[a-z0-9_-]+", token.strip())
    ]


def cursor_family_name(name: str, effort: str, fast: bool) -> str:
    """Remove only the variant's effort word while keeping speed and context labels."""
    label = re.escape(CURSOR_SUFFIX_LABEL[effort])
    cleaned = re.sub(rf"\s+{label}(?=(?:\s+(?:No\s+)?Thinking)?(?:\s+\(NO ZDR\))?(?:\s+Fast)?$)", "", name)
    return re.sub(r"\s{2,}", " ", cleaned).strip() or name


def model_choices(key: str, output: str, efforts: list[str] | None = None) -> list[dict]:
    """Read each client's own model-list format and per-model thinking levels."""
    if key == "codex":
        models = []
        for m in json.loads(output)["models"]:
            if m.get("visibility") != "list":
                continue
            item = {"id": m["slug"], "name": m["display_name"]}
            levels = [lvl["effort"] for lvl in m.get("supported_reasoning_levels", [])
                      if isinstance(lvl, dict) and isinstance(lvl.get("effort"), str) and lvl["effort"]]
            if levels:
                item["efforts"] = levels
            models.append(item)
        return models
    separator = "\t" if key == "antigravity" else " - "
    raw = []
    for line in output.splitlines():
        model, found, name = line.strip().partition(separator)
        if found and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/\[\],=+-]*", model) and name:
            if key == "cursor":
                name = re.sub(r"\s+\((?:current|default|current,\s*default)\)$", "", name.replace("\u200b", "")).strip()
            raw.append((model, name))
    if not raw:
        raise ValueError("The client returned no readable models.")
    if key == "antigravity" and efforts:
        grouped = {}
        for model, name in raw:
            matched = next((e for e in efforts if model.endswith(f"-{e}") and re.search(rf"\s+\({re.escape(e)}\)$", name, re.I)), None)
            if matched:
                base = model[:-len(matched) - 1]
                base_name = re.sub(rf"\s+\({re.escape(matched)}\)$", "", name, flags=re.I)
                entry = grouped.setdefault(base, {"id": base, "name": base_name, "levels": set()})
                entry["levels"].add(matched)
        emitted = set()
        models = []
        for model, name in raw:
            matched = next((e for e in efforts if model.endswith(f"-{e}") and re.search(rf"\s+\({re.escape(e)}\)$", name, re.I)), None)
            base = model[:-len(matched) - 1] if matched else None
            if base and len(grouped[base]["levels"]) >= 2:
                if base in emitted:
                    continue
                emitted.add(base)
                models.append({
                    "id": base,
                    "name": grouped[base]["name"],
                    "efforts": [e for e in efforts if e in grouped[base]["levels"]],
                })
            else:
                models.append({"id": model, "name": name})
        return models
    if key == "cursor":
        families = {}
        for model, name in raw:
            match = CURSOR_VARIANT.match(model)
            if match:
                family_key = (match.group("base"), match.group("fast") or "")
                families.setdefault(family_key, {})[match.group("effort")] = (model, name)
        emitted = set()
        models = []
        for model, name in raw:
            match = CURSOR_VARIANT.match(model)
            family_key = (match.group("base"), match.group("fast") or "") if match else None
            variants = families.get(family_key) if family_key else None
            if variants and len(variants) >= 2:
                if family_key in emitted:
                    continue
                emitted.add(family_key)
                ordered = [e for e in CURSOR_EFFORTS if e in variants]
                default_effort = "high" if "high" in variants else ("medium" if "medium" in variants else ordered[0])
                default_slug, default_name = variants[default_effort]
                models.append({
                    "id": default_slug,
                    "name": cursor_family_name(default_name, default_effort, bool(family_key[1])),
                    "efforts": ordered,
                    "variants": {e: variants[e][0] for e in ordered},
                })
            else:
                models.append({"id": model, "name": name})
        return models
    return [{"id": model, "name": name} for model, name in raw]


def discover(agent: dict, cli: Callable[[list[str]], str]) -> dict:
    """Return flags verified by installed help and models supplied by the client."""
    agent_command(agent)
    key = agent["key"]
    result = {
        "models": [],
        "model_supported": False,
        "thinking_supported": False,
        "efforts": [],
        "yolo_flag": None,
        "note": "",
        "error": False,
    }
    help_text = cli([*agent["run"], "--help"])
    result["model_supported"] = bool(re.search(r"(?<![\w-])--model(?![\w-])", help_text))
    thinking_mode = CLIENTS.get(key, {}).get("thinking")
    if thinking_mode == "--effort":
        result["efforts"] = help_efforts(help_text)
        result["thinking_supported"] = bool(result["efforts"])
    elif thinking_mode == "-c":
        result["thinking_supported"] = bool(re.search(r"(?<![\w-])(?:-c|--config)(?![\w-])", help_text))
    flag = CLIENTS.get(key, {}).get("yolo")
    if flag and re.search(r"(?<![\w-])" + re.escape(flag) + r"(?![\w-])", help_text):
        result["yolo_flag"] = flag
    if not result["model_supported"]:
        result["note"] = "This client uses its configured model; no supported --model flag was found."
    elif key == "claude":
        # Public aliases and the default Opus 5.5 ID resolve in Claude's configured provider.
        result["models"] = [
            {"id": "claude-opus-5-5", "name": "Claude Opus 5.5"},
            *({"id": name, "name": name.capitalize()} for name in ("opus", "sonnet", "haiku")),
        ]
        result["note"] = "Claude aliases use your configured provider. Claude checks model access at startup."
    elif key in ("codex", "antigravity", "cursor"):
        args = {"codex": ["debug", "models"], "antigravity": ["models"], "cursor": ["--list-models"]}[key]
        try:
            result["models"] = model_choices(key, cli([*agent["run"], *args]), result["efforts"])
            if key == "codex" and result["thinking_supported"]:
                seen = []
                for model in result["models"]:
                    for effort in model.get("efforts", []):
                        if effort not in seen:
                            seen.append(effort)
                result["efforts"] = seen
            elif key == "cursor":
                result["thinking_supported"] = any(bool(model.get("efforts")) for model in result["models"])
        except (ValueError, KeyError, TypeError, RuntimeError, subprocess.TimeoutExpired) as error:
            result["error"] = True
            result["note"] = f"Could not load models: {error}. Retry, use the client default, or enter an exact model ID."
    else:
        result["note"] = "Use the client default or enter an exact model ID supported by this client."
    return result


def validate(request: object) -> dict:
    """Reject malformed launch requests before creating any terminal."""
    if not isinstance(request, dict) or set(request) != {"space", "root", "swarm", "agents"}:
        raise ValueError("Invalid Chorus launch request. Reopen the launcher.")
    for key in ("space", "root", "swarm"):
        if not isinstance(request[key], str) or not request[key]:
            raise ValueError(f"Missing {key}. Reopen the launcher from a project terminal.")
    if not Path(request["root"]).is_absolute() or "/" in request["space"]:
        raise ValueError("Select a local project terminal.")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,31}", request["swarm"]):
        raise ValueError("The Chorus name must be 1–32 letters, numbers, underscores or hyphens.")
    agents = request["agents"]
    if not isinstance(agents, list) or not agents:
        raise ValueError("Add at least one agent before opening the team.")
    seen = set()
    for agent in agents:
        if not isinstance(agent, dict) or not isinstance(agent.get("id"), str) or not agent["id"]:
            raise ValueError("Each team row needs its own ID. Reopen the launcher.")
        if agent["id"] in seen:
            raise ValueError("The same team row was sent twice. Reopen the launcher.")
        seen.add(agent["id"])
        if set(agent) == {"id"}:
            continue  # Existing panes are references; their settings belong to Herdr.
        if set(agent) != {"id", "key", "name", "run", "model", "thinking", "yolo"}:
            raise ValueError("Invalid team row. Reopen the launcher.")
        agent_command(agent)
        if type(agent["yolo"]) is not bool:
            raise ValueError("Choose whether to use YOLO mode for each new agent.")
        model = agent["model"]
        if model is not None and (not isinstance(model, str) or not model.strip() or model != model.strip() or model.startswith("-") or any(ord(c) < 32 for c in model)):
            raise ValueError("Enter a nonempty model ID without surrounding whitespace or control characters.")
        thinking = agent["thinking"]
        if thinking is not None and (not isinstance(thinking, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,31}", thinking)):
            raise ValueError("Choose a valid thinking level for each new agent.")
        client = CLIENTS.get(agent["key"], {})
        if thinking is not None:
            if not client.get("thinking"):
                raise ValueError(f"Thinking level is not supported for {agent['name']}.")
            if agent["key"] == "antigravity" and model and AGY_SUFFIX.search(model):
                raise ValueError("Antigravity cannot combine a suffixed model ID with a separate thinking level.")
        if agent["yolo"] and not client.get("yolo"):
            raise ValueError(f"YOLO mode is not supported for {agent['name']}.")
    return request


def initial_argv(agent: dict, root: str, swarm: str) -> list[str]:
    """Give each CLI an interactive first prompt and a unique Chorus handle."""
    # The UUID is a valid 32-character Chorus handle; role names remain readable.
    handle = uuid.uuid4().hex
    prompt = (
        "Use the installed chorus skill now. "
        f"Your project is {root!r}. Join swarm {swarm!r}, channel {'#' + swarm!r}, "
        f"using handle {handle!r} and role {'Herdr ' + agent['name'] + ' teammate'!r}. "
        "Report your actual runtime model when joining. "
        "Startup only: join and keep listening according to the skill. "
        "Preserve the current pinned plan and report available. Do not claim or "
        "start tasks, edit files, run project checks, spawn helpers, or change the "
        "pin until the user or lead assigns you work. If the skill or Chorus MCP "
        "tools are unavailable, report the missing setup in this pane; do not "
        "install anything, change permissions, or substitute shell commands for MCP."
    )
    # These clients require a flag to keep the initial prompt interactive.
    client = CLIENTS.get(agent["key"], {})
    model = ["--model", agent["model"]] if agent["model"] is not None else []
    thinking = []
    if agent.get("thinking") is not None:
        if client.get("thinking") == "--effort":
            thinking = ["--effort", agent["thinking"]]
        elif client.get("thinking") == "-c":
            thinking = ["-c", f'model_reasoning_effort="{agent["thinking"]}"']
    permissions = [client["yolo"]] if agent["yolo"] else []
    return [*agent["run"], *model, *thinking, *permissions, *client.get("prompt", []), prompt]


def split_argv(argv: list[str], root: str) -> list[str]:
    """Start each split in the chosen checkout even if another agent changes cwd."""
    return ["/bin/zsh", "-lic", 'cd -- "$1" || exit; shift; "$@"', "/bin/zsh", root, *argv]


def app_command(app: Command, command: str, **args: object) -> dict:
    """Read the created resource IDs from Herdr's command reply."""
    result = app(["do", command, json.dumps(args)])
    if not isinstance(result, dict) or result.get("error"):
        raise RuntimeError(f"Herdr could not {command}: {result}")
    return result


def saved_team(root: str, space: str, app: Command, core: Command) -> tuple:
    """Load the project's team from live Herdr terminals, including older panes."""
    listing = app(["ls"])
    target = next((s for s in listing["spaces"] if s["id"] == space), None)
    if target is None:
        raise RuntimeError("The selected space was closed. Reopen the launcher from your project.")
    live = core(["ls", "--json", "--label", f"chorus.project={root}", "--label", f"herdr.space={space}"])
    # ls filters on labels but omits them from its summaries; get owns full records.
    records = [core(["get", item["address"], "--json"]) for item in live["terminals"]]
    tabs = {tab["id"]: tab for tab in target["tabs"]}
    owned = [t for t in records if t.get("state") == "live"
             and t.get("labels", {}).get("chorus.project") == root
             and t["labels"].get("herdr.space") == space
             and t["labels"].get("herdr.tab") in tabs]
    teams = {t["labels"]["herdr.tab"] for t in owned}
    if len(teams) > 1:
        raise RuntimeError("This project has more than one labeled Chorus tab. Close the unwanted team tab, then try again.")
    tab_id = next(iter(teams), None)
    return tabs, tab_id, owned


def team_rows(owned: list[dict]) -> list[dict]:
    """Show saved launch choices, marking older settings as unrecorded."""
    rows = []
    for terminal in owned:
        labels = terminal["labels"]
        options = json.loads(labels["chorus.options"]) if "chorus.options" in labels else None
        if options is not None and (
            not isinstance(options, dict)
            or set(options) not in ({"model", "yolo"}, {"model", "thinking", "yolo"})
            or type(options["yolo"]) is not bool
            or (options["model"] is not None and not isinstance(options["model"], str))
            or (options.get("thinking") is not None and not isinstance(options.get("thinking"), str))
        ):
            raise ValueError("A Chorus pane has invalid saved launch settings. Check its terminal labels before continuing.")
        rows.append({
            "id": labels.get("chorus.row", terminal["address"]),
            "key": labels["chorus.agent"],
            "terminal": terminal["address"],
            "model": options["model"] if options else None,
            "thinking": options.get("thinking") if options else None,
            "yolo": options["yolo"] if options else None,
        })
    if len({row["id"] for row in rows}) != len(rows):
        raise RuntimeError("Two Chorus panes have the same row label. Check their terminal labels before continuing.")
    return rows


def inspect_team(request: dict, app: Command, core: Command) -> dict:
    """Return existing panes and a unique prefix for new dialog rows."""
    if set(request) != {"space", "root"} or not all(isinstance(request[k], str) and request[k] for k in request) or not Path(request["root"]).is_absolute():
        raise ValueError("Reopen the launcher from a local project terminal.")
    _, tab, owned = saved_team(request["root"], request["space"], app, core)
    swarms = {t["labels"]["chorus.swarm"] for t in owned}
    if len(swarms) > 1:
        raise RuntimeError("The Chorus tab contains different swarm names. Check its terminal labels before continuing.")
    return {"tab": tab, "swarm": next(iter(swarms), None), "rows": team_rows(owned), "session": uuid.uuid4().hex}


def launch_team(request: dict, app: Command, core: Command) -> dict:
    """Reuse exact team rows and add new rows without changing running sessions."""
    request = validate(request)
    space, root, swarm, agents = (request[k] for k in ("space", "root", "swarm", "agents"))
    tabs, tab_id, owned = saved_team(root, space, app, core)
    if any(t["labels"].get("chorus.swarm") != swarm for t in owned):
        raise RuntimeError("This project's team already uses a different Chorus name. Use its existing name or close that team tab first.")
    present = {row["id"]: row for row in team_rows(owned)}
    # Validate every reference before any creation, including a replay after failure.
    for agent in agents:
        old = present.get(agent["id"])
        if old is None and set(agent) == {"id"}:
            raise RuntimeError("An existing Chorus pane was closed. Reload the team before opening more panes.")
        if old and set(agent) != {"id"} and any(old[k] != agent[k] for k in ("key", "model", "thinking", "yolo")):
            raise RuntimeError("This team row already has a running conversation with different launch settings. Reload the team and add a new row.")
    reused = sum(agent["id"] in present for agent in agents)
    created = 0
    if tab_id:
        app_command(app, "tab.select", id=tab_id)
    # Split the shallowest existing leaf first, alternating horizontal/vertical.
    leaves = [(p["pane"], 0) for p in tabs[tab_id]["terminals"]] if tab_id else []
    for agent in agents:
        if agent["id"] in present:
            continue
        argv = initial_argv(agent, root, swarm)
        if tab_id is None:
            reply = app_command(app, "tab.new", space=space, name=f"Chorus · {Path(root).name}", folder=root, argv=argv)
            tab_id, terminal = reply["tab"], reply["terminal"]
            pane = None
            depth = 0
        else:
            if not leaves:
                raise RuntimeError("The Chorus tab has no terminal pane to split. Close it and open the team again.")
            target_pane, depth = leaves.pop(0)
            reply = app_command(app, "pane.split", pane=target_pane, direction="right" if depth % 2 == 0 else "down", argv=split_argv(argv, root))
            terminal, pane = reply["terminal"], reply["pane"]
            depth += 1
            leaves.append((target_pane, depth))
        # Label only the process just created; never send input to an existing agent.
        try:
            options = json.dumps({"model": agent["model"], "thinking": agent["thinking"], "yolo": agent["yolo"]})
            core(["label", terminal, f"chorus.project={root}", f"chorus.swarm={swarm}", f"chorus.agent={agent['key']}", f"chorus.row={agent['id']}", f"chorus.options={options}"])
        except Exception:
            app_command(app, "terminal.kill", terminal=terminal)
            raise
        if pane is None:
            # A failed lookup now leaves a labeled, reusable conversation.
            current = app(["ls"])
            tab = next(t for s in current["spaces"] for t in s["tabs"] if t["id"] == tab_id)
            pane = next(t["pane"] for t in tab["terminals"] if t["terminal"] == terminal)
        leaves.append((pane, depth))
        leaves.sort(key=lambda leaf: leaf[1])
        created += 1
    return {"tab": tab_id, "created": created, "reused": reused}


def main() -> int:
    """Run the accepted launch independently of the dialog, with a kernel lock."""
    try:
        app_home = Path(sys.argv[1]).resolve(strict=True)
        action, request = sys.argv[2], json.loads(sys.argv[3])
        os.environ.update(CI="true", HERDR_APP_HOME=str(app_home))
        bins = {name: shutil.which(name) for name in ("herdr-app", "herdr-core")}
        if not all(bins.values()):
            raise RuntimeError("Herdr's CLI links are missing from PATH. Reopen Herdr after installing its CLI links.")
        directory = app_home / "data" / "chorus-launcher"
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        deadline = time.monotonic() + 110

        def execute(argv: list[str]) -> str:
            """Bound each owner API call by the remaining operation deadline."""
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("Opening the team timed out. Reopen the launcher to add remaining agents.")
            result = subprocess.run(argv, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=min(20, remaining), check=False)
            if result.returncode:
                raise RuntimeError((result.stderr or result.stdout).strip() or f"{argv[0]} exited {result.returncode}")
            # Go clients commonly print successful help to stderr; model lists do not.
            return result.stdout + result.stderr if argv[-1] == "--help" else result.stdout

        def call(binary: str, args: list[str]) -> object:
            """Decode Herdr's JSON reply at its command boundary."""
            output = execute([bins[binary], *args])
            return json.loads(output) if output.strip() else {}

        app, core = lambda args: call("herdr-app", args), lambda args: call("herdr-core", args)
        if action == "inspect":
            print(json.dumps(inspect_team(request, app, core)))
            return 0
        if action == "models":
            print(json.dumps(discover(request, lambda argv: execute(split_argv(argv, os.getcwd())))))
            return 0
        if action != "launch":
            raise ValueError("Unknown launcher action. Reopen Start Chorus.")

        # flock is released by the OS even if this process or Herdr exits.
        with (directory / "launch.lock").open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError("A Chorus team is already opening. Wait for it to finish, then reopen the launcher.") from None
            result = launch_team(request, app, core)
        print(json.dumps(result))
        return 0
    except Exception as error:
        print(f"{error}\nExisting panes remain available; reopen Start Chorus to continue.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
