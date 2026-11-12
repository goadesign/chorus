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

# Only clients with verified per-process additive MCP flags may take role connections.
CONNECTION_CLIENTS = {"claude", "codex"}
NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,31}")

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


def roster_path(app_home: Path, space: str) -> Path:
    """Return the caller-supplied roster file path for one Herdr space."""
    if (
        not isinstance(space, str)
        or not space
        or "/" in space
        or "\\" in space
        or "\0" in space
        or space in (".", "..")
    ):
        raise ValueError("Select a local project terminal.")
    return app_home / "data" / "chorus-launcher" / "rosters" / f"{space}.json"


def mcp_config_path(app_home: Path, space: str, handle: str) -> Path:
    """Return the mode-0600 Claude Code MCP config path for one space and handle."""
    roster_path(app_home, space)
    if not isinstance(handle, str) or not NAME_PATTERN.fullmatch(handle):
        raise ValueError("Invalid Chorus handle in team roster.")
    return app_home / "data" / "chorus-launcher" / "mcp" / f"{space}--{handle}.json"


def read_credential(path_str: object, name: str) -> str:
    """Read a non-empty bearer token from disk without including secrets in errors."""
    if not isinstance(path_str, str) or not path_str or not Path(path_str).is_absolute():
        raise ValueError(f"Connection {name!r} requires an absolute credential_file path.")
    path = Path(path_str)
    try:
        is_file = path.is_file()
    except OSError:
        is_file = False
    if not is_file:
        raise ValueError(f"Missing credential file for connection {name!r}: {path_str}")
    try:
        token = path.read_text(encoding="utf-8").strip()
    except (OSError, UnicodeDecodeError):
        raise ValueError(f"Unreadable credential file for connection {name!r}: {path_str}") from None
    if not token or any(ord(c) < 32 for c in token):
        raise ValueError(f"Empty or invalid credential file for connection {name!r}: {path_str}")
    return token


def load_roster(app_home: Path | None, space: str) -> dict | None:
    """Load and validate a caller-supplied space roster when one exists."""
    if app_home is None:
        return None
    path = roster_path(app_home, space)
    if not path.exists():
        return None
    if not path.is_file():
        raise ValueError("The team roster is not a regular file.")
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError(f"Could not read team roster: {error}") from None
    if not isinstance(raw, dict) or set(raw) != {"root", "swarm", "roles"}:
        raise ValueError("Invalid team roster format.")
    root = raw["root"]
    if not isinstance(root, str) or not root or not Path(root).is_absolute() or not Path(root).is_dir():
        raise ValueError("The team roster root must be an existing absolute directory.")
    swarm = raw["swarm"]
    if not isinstance(swarm, str) or not NAME_PATTERN.fullmatch(swarm):
        raise ValueError("The team roster swarm name must be 1–32 letters, numbers, underscores or hyphens.")
    roles = raw["roles"]
    if not isinstance(roles, list) or not roles:
        raise ValueError("The team roster must include at least one role.")
    seen_roles: set[str] = set()
    seen_handles: set[str] = set()
    for idx, entry in enumerate(roles):
        if not isinstance(entry, dict) or set(entry) != {
            "role",
            "title",
            "handle",
            "folder",
            "first_message",
            "connections",
            "place",
        }:
            raise ValueError("Invalid role entry in team roster.")
        role_id, handle = entry["role"], entry["handle"]
        if not isinstance(role_id, str) or not NAME_PATTERN.fullmatch(role_id):
            raise ValueError("Each roster role must be 1–32 letters, numbers, underscores or hyphens.")
        if not isinstance(handle, str) or not NAME_PATTERN.fullmatch(handle):
            raise ValueError("Each roster handle must be 1–32 letters, numbers, underscores or hyphens.")
        if role_id in seen_roles:
            raise ValueError(f"Duplicate role {role_id!r} in team roster.")
        if handle in seen_handles:
            raise ValueError(f"Duplicate handle {handle!r} in team roster.")
        for field in ("title", "first_message"):
            if not isinstance(entry[field], str) or not entry[field].strip():
                raise ValueError(f"Roster role {role_id!r} is missing {field}.")
        folder = entry["folder"]
        if not isinstance(folder, str) or not folder or not Path(folder).is_absolute() or not Path(folder).is_dir():
            raise ValueError(f"Roster role {role_id!r} folder must be an existing absolute directory.")
        connections = entry["connections"]
        if not isinstance(connections, list):
            raise ValueError(f"Roster role {role_id!r} connections must be a list.")
        seen_conns: set[str] = set()
        for conn in connections:
            if not isinstance(conn, dict) or set(conn) != {"name", "url", "credential_file"}:
                raise ValueError(f"Invalid connection in roster role {role_id!r}.")
            conn_name, url = conn["name"], conn["url"]
            if not isinstance(conn_name, str) or not NAME_PATTERN.fullmatch(conn_name) or conn_name == "chorus":
                raise ValueError(f"Invalid connection name {conn_name!r} in roster role {role_id!r}.")
            if conn_name in seen_conns:
                raise ValueError(f"Duplicate connection {conn_name!r} in roster role {role_id!r}.")
            seen_conns.add(conn_name)
            if (
                not isinstance(url, str)
                or not (url.startswith("http://") or url.startswith("https://"))
                or any(c.isspace() or ord(c) < 32 for c in url)
            ):
                raise ValueError(f"Invalid URL for connection {conn_name!r} in roster role {role_id!r}.")
            read_credential(conn["credential_file"], conn_name)
        place = entry["place"]
        if idx == 0:
            if place is not None:
                raise ValueError("The first roster role must have place set to null.")
        else:
            if (
                not isinstance(place, dict)
                or set(place) != {"beside", "direction"}
                or place["beside"] not in seen_handles
                or place["direction"] not in ("right", "down")
            ):
                raise ValueError(f"Roster role {role_id!r} must be placed right or down beside an earlier role handle.")
        seen_roles.add(role_id)
        seen_handles.add(handle)
    return raw


def cleanup_stale_mcp_files(app_home: Path | None, space: str, live_handles: set[str]) -> None:
    """Remove per-agent Claude Code MCP files for handles in this space with no live pane."""
    if app_home is None:
        return
    mcp_dir = app_home / "data" / "chorus-launcher" / "mcp"
    if not mcp_dir.is_dir():
        return
    prefix = f"{space}--"
    for entry in mcp_dir.iterdir():
        if entry.name.startswith(prefix) and entry.name.endswith(".json"):
            handle = entry.name[len(prefix) : -5]
            if handle not in live_handles:
                entry.unlink(missing_ok=True)


def write_claude_mcp_config(app_home: Path, space: str, handle: str, connections: list[dict]) -> Path:
    """Write a mode-0600 Claude Code MCP config file for one role launch."""
    path = mcp_config_path(app_home, space, handle)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    servers = {}
    for conn in connections:
        token = read_credential(conn["credential_file"], conn["name"])
        servers[conn["name"]] = {
            "type": "http",
            "url": conn["url"],
            "headers": {"Authorization": f"Bearer {token}"},
        }
    payload = (json.dumps({"mcpServers": servers}, indent=2) + "\n").encode("utf-8")
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.fchmod(fd, 0o600)
        os.write(fd, payload)
    finally:
        os.close(fd)
    return path


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


def validate(request: object, roster: dict | None = None) -> dict:
    """Reject malformed launch requests before creating any terminal."""
    if not isinstance(request, dict) or set(request) != {"space", "root", "swarm", "agents"}:
        raise ValueError("Invalid Chorus launch request. Reopen the launcher.")
    for key in ("space", "root", "swarm"):
        if not isinstance(request[key], str) or not request[key]:
            raise ValueError(f"Missing {key}. Reopen the launcher from a project terminal.")
    if not Path(request["root"]).is_absolute() or "/" in request["space"]:
        raise ValueError("Select a local project terminal.")
    if not NAME_PATTERN.fullmatch(request["swarm"]):
        raise ValueError("The Chorus name must be 1–32 letters, numbers, underscores or hyphens.")
    if roster is not None and (request["root"] != roster["root"] or request["swarm"] != roster["swarm"]):
        raise ValueError("The launch request does not match the space roster. Reload the team and try again.")
    agents = request["agents"]
    if not isinstance(agents, list) or not agents:
        raise ValueError("Add at least one agent before opening the team.")
    if roster is not None:
        expected_ids = [f"role:{entry['handle']}" for entry in roster["roles"]]
        if [agent.get("id") if isinstance(agent, dict) else None for agent in agents] != expected_ids:
            raise ValueError("The launch request does not match the current team roster. Reload the team and try again.")
    seen = set()
    for idx, agent in enumerate(agents):
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
        if roster is not None and roster["roles"][idx]["connections"] and agent["key"] not in CONNECTION_CLIENTS:
            raise ValueError(f"{agent['name']} cannot yet be given a role's connections.")
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


def roster_argv(
    agent: dict,
    role: dict,
    root: str,
    swarm: str,
    mcp_config: Path | None = None,
) -> tuple[list[str], list[tuple[str, str]]]:
    """Build argv and per-process credential file bindings for one roster role."""
    handle = role["handle"]
    title = role["title"]
    prompt = (
        "Use the installed chorus skill now. "
        f"Your project is {root!r}. Join swarm {swarm!r}, channel {'#' + swarm!r}, "
        f"using handle {handle!r} and role {title!r}. "
        "Report your actual runtime model when joining. "
        f"Keep `CI=true chorus watch {handle} -wait 24h </dev/null` running in the background while you work. "
        f"{role['first_message']}"
    )
    client = CLIENTS.get(agent["key"], {})
    model = ["--model", agent["model"]] if agent["model"] is not None else []
    thinking = []
    if agent.get("thinking") is not None:
        if client.get("thinking") == "--effort":
            thinking = ["--effort", agent["thinking"]]
        elif client.get("thinking") == "-c":
            thinking = ["-c", f'model_reasoning_effort="{agent["thinking"]}"']
    permissions = [client["yolo"]] if agent["yolo"] else []
    connection_flags: list[str] = []
    env_files: list[tuple[str, str]] = []
    if role["connections"]:
        if agent["key"] == "claude":
            if mcp_config is None:
                raise ValueError("Claude Code requires a prepared MCP configuration file for role connections.")
            connection_flags = ["--mcp-config", str(mcp_config)]
        elif agent["key"] == "codex":
            for idx, conn in enumerate(role["connections"]):
                env_var = f"CHORUS_MCP_TOKEN_{idx}"
                connection_flags.extend([
                    "-c",
                    f'mcp_servers.{conn["name"]}.url={json.dumps(conn["url"])}',
                    "-c",
                    f'mcp_servers.{conn["name"]}.bearer_token_env_var="{env_var}"',
                ])
                env_files.append((env_var, conn["credential_file"]))
        else:
            raise ValueError(f"{agent['name']} cannot yet be given a role's connections.")
    prompt_flags = client.get("prompt", [])
    prompt_args = [*prompt_flags, prompt] if prompt_flags else ["--", prompt]
    return [*agent["run"], *model, *thinking, *permissions, *connection_flags, *prompt_args], env_files


def split_argv(argv: list[str], root: str) -> list[str]:
    """Start each split in the chosen checkout even if another agent changes cwd."""
    return ["/bin/zsh", "-lic", 'cd -- "$1" || exit; shift; "$@"', "/bin/zsh", root, *argv]


def wrap_roster_argv(
    argv: list[str],
    folder: str,
    env_files: list[tuple[str, str]],
    always_wrap: bool,
) -> list[str]:
    """Start a roster role in its folder and load any Codex bearer tokens from files."""
    if env_files:
        flat = [item for pair in env_files for item in pair]
        script = (
            'cd -- "$1" || exit; shift; '
            'while [[ "$1" != "--" ]]; do '
            'typeset -gx "$1=$(<"$2")" || exit; shift 2; '
            'done; shift; exec "$@"'
        )
        return ["/bin/zsh", "-lic", script, "/bin/zsh", folder, *flat, "--", *argv]
    if always_wrap:
        return split_argv(argv, folder)
    return argv


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
        row = {
            "id": labels.get("chorus.row", terminal["address"]),
            "key": labels["chorus.agent"],
            "terminal": terminal["address"],
            "model": options["model"] if options else None,
            "thinking": options.get("thinking") if options else None,
            "yolo": options["yolo"] if options else None,
        }
        if "chorus.handle" in labels:
            row["handle"] = labels["chorus.handle"]
        if "chorus.role" in labels:
            row["role"] = labels["chorus.role"]
        rows.append(row)
    if len({row["id"] for row in rows}) != len(rows):
        raise RuntimeError("Two Chorus panes have the same row label. Check their terminal labels before continuing.")
    return rows


def roster_rows(roster: dict, owned: list[dict]) -> list[dict]:
    """Build one dialog row per roster role in roster order, matching live panes by handle."""
    parsed_rows = team_rows(owned)
    by_handle: dict[str, dict] = {}
    for terminal, parsed in zip(owned, parsed_rows):
        handle = terminal["labels"].get("chorus.handle")
        if handle:
            if handle in by_handle:
                raise RuntimeError("Two Chorus panes have the same handle label. Check their terminal labels before continuing.")
            by_handle[handle] = parsed
    rows = []
    for entry in roster["roles"]:
        handle = entry["handle"]
        live = by_handle.get(handle)
        rows.append({
            "id": f"role:{handle}",
            "role": entry["role"],
            "title": entry["title"],
            "handle": handle,
            "has_connections": bool(entry["connections"]),
            "key": live["key"] if live else None,
            "terminal": live["terminal"] if live else None,
            "model": live["model"] if live else None,
            "thinking": live["thinking"] if live else None,
            "yolo": live["yolo"] if live else None,
        })
    return rows


def inspect_team(
    request: dict,
    app: Command,
    core: Command,
    app_home: Path | None = None,
) -> dict:
    """Return existing panes and a unique prefix for new dialog rows."""
    if (
        not isinstance(request, dict)
        or set(request) != {"space", "root"}
        or not isinstance(request.get("space"), str)
        or not request["space"]
        or not isinstance(request.get("root"), str)
    ):
        raise ValueError("Reopen the launcher from a local project terminal.")
    roster = load_roster(app_home, request["space"])
    if roster is not None:
        root = roster["root"]
        _, tab, owned = saved_team(root, request["space"], app, core)
        swarms = {t["labels"]["chorus.swarm"] for t in owned}
        if len(swarms) > 1 or (swarms and next(iter(swarms)) != roster["swarm"]):
            raise RuntimeError("The Chorus tab contains different swarm names. Check its terminal labels before continuing.")
        live_handles = {
            t["labels"]["chorus.handle"]
            for t in owned
            if isinstance(t["labels"].get("chorus.handle"), str) and t["labels"]["chorus.handle"]
        }
        cleanup_stale_mcp_files(app_home, request["space"], live_handles)
        return {
            "roster": True,
            "root": root,
            "tab": tab,
            "swarm": roster["swarm"],
            "rows": roster_rows(roster, owned),
            "session": uuid.uuid4().hex,
        }
    if app_home is not None and request["root"] == "":
        return {"roster": False}
    if not request["root"] or not Path(request["root"]).is_absolute():
        raise ValueError("Reopen the launcher from a local project terminal.")
    _, tab, owned = saved_team(request["root"], request["space"], app, core)
    swarms = {t["labels"]["chorus.swarm"] for t in owned}
    if len(swarms) > 1:
        raise RuntimeError("The Chorus tab contains different swarm names. Check its terminal labels before continuing.")
    return {"tab": tab, "swarm": next(iter(swarms), None), "rows": team_rows(owned), "session": uuid.uuid4().hex}


def launch_team(
    request: dict,
    app: Command,
    core: Command,
    app_home: Path | None = None,
) -> dict:
    """Reuse exact team rows and add new rows without changing running sessions."""
    roster = (
        load_roster(app_home, request["space"])
        if isinstance(request, dict) and isinstance(request.get("space"), str) and request["space"]
        else None
    )
    request = validate(request, roster)
    space, root, swarm, agents = (request[k] for k in ("space", "root", "swarm", "agents"))
    tabs, tab_id, owned = saved_team(root, space, app, core)
    if any(t["labels"].get("chorus.swarm") != swarm for t in owned):
        raise RuntimeError("This project's team already uses a different Chorus name. Use its existing name or close that team tab first.")
    if roster is not None:
        current_rows = roster_rows(roster, owned)
        present = {row["id"]: row for row in current_rows if row["terminal"] is not None}
    else:
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
    if roster is not None:
        if app_home is None:
            raise ValueError("Missing Herdr application directory for roster launch.")
        pane_by_terminal = (
            {item["terminal"]: item["pane"] for item in tabs[tab_id]["terminals"]}
            if tab_id
            else {}
        )
        pane_by_handle: dict[str, str] = {}
        for terminal in owned:
            handle = terminal["labels"].get("chorus.handle")
            pane_id = pane_by_terminal.get(terminal["address"])
            if handle and pane_id:
                pane_by_handle[handle] = pane_id
        cleanup_stale_mcp_files(app_home, space, set(pane_by_handle))
        for agent, role_entry in zip(agents, roster["roles"]):
            if agent["id"] in present:
                continue
            handle = role_entry["handle"]
            role_id = role_entry["role"]
            folder = role_entry["folder"]
            wrote_mcp: Path | None = None
            if role_entry["connections"] and agent["key"] == "claude":
                wrote_mcp = write_claude_mcp_config(app_home, space, handle, role_entry["connections"])
            raw_argv, env_files = roster_argv(agent, role_entry, root, swarm, wrote_mcp)
            if tab_id is None:
                start_argv = wrap_roster_argv(raw_argv, folder, env_files, always_wrap=False)
                reply = app_command(
                    app,
                    "tab.new",
                    space=space,
                    name=f"Chorus · {Path(root).name}",
                    folder=folder,
                    argv=start_argv,
                )
                tab_id, terminal = reply["tab"], reply["terminal"]
                pane = None
            else:
                place = role_entry["place"]
                if place is None:
                    existing_terminals = tabs.get(tab_id, {}).get("terminals", [])
                    target_pane = (
                        existing_terminals[0]["pane"]
                        if existing_terminals
                        else next(iter(pane_by_handle.values()), None)
                    )
                    if not target_pane:
                        raise RuntimeError("The Chorus tab has no terminal pane to split. Close it and open the team again.")
                    direction = "right"
                else:
                    target_pane = pane_by_handle.get(place["beside"])
                    if not target_pane:
                        raise RuntimeError(f"Cannot place role {role_id!r} beside {place['beside']!r} because its pane is not open.")
                    direction = place["direction"]
                start_argv = wrap_roster_argv(raw_argv, folder, env_files, always_wrap=True)
                reply = app_command(
                    app,
                    "pane.split",
                    pane=target_pane,
                    direction=direction,
                    argv=start_argv,
                )
                terminal, pane = reply["terminal"], reply["pane"]
            try:
                options = json.dumps({"model": agent["model"], "thinking": agent["thinking"], "yolo": agent["yolo"]})
                core([
                    "label",
                    terminal,
                    f"chorus.project={root}",
                    f"chorus.swarm={swarm}",
                    f"chorus.agent={agent['key']}",
                    f"chorus.row={agent['id']}",
                    f"chorus.handle={handle}",
                    f"chorus.role={role_id}",
                    f"chorus.options={options}",
                ])
            except Exception:
                if wrote_mcp is not None:
                    wrote_mcp.unlink(missing_ok=True)
                app_command(app, "terminal.kill", terminal=terminal)
                raise
            if pane is None:
                current = app(["ls"])
                tab = next(t for s in current["spaces"] for t in s["tabs"] if t["id"] == tab_id)
                pane = next(t["pane"] for t in tab["terminals"] if t["terminal"] == terminal)
            pane_by_handle[handle] = pane
            created += 1
        return {"tab": tab_id, "created": created, "reused": reused}
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
            print(json.dumps(inspect_team(request, app, core, app_home)))
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
            result = launch_team(request, app, core, app_home)
        print(json.dumps(result))
        return 0
    except Exception as error:
        print(f"{error}\nExisting panes remain available; reopen Start Chorus to continue.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
