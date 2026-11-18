"""Accept explicit roster choices without depending on Herdr's mod-only catalog."""

from pathlib import Path

import launch


def inspect(request: dict, app, core, app_home: Path, execute) -> dict:
    """Show roster state and, when chosen, each program's own current options."""
    if not isinstance(request, dict) or set(request) not in ({"space"}, {"space", "choices"}):
        raise ValueError("Team inspection requires a space ID and optional role choices.")
    prepared = prepare(request, app_home) if "choices" in request else None
    team = launch.inspect_team({"space": request["space"], "root": ""}, app, core, app_home)
    if not team.get("roster"):
        raise ValueError("This space has no team roster. Prepare the project's team first.")
    result = {
        "root": team["root"], "swarm": team["swarm"],
        "roles": [{
            "role": row["role"], "title": row["title"], "handle": row["handle"],
            "running": row["terminal"] is not None, "client": row["key"],
            "terminal": row["terminal"], "status": row["status"],
            "model": row["model"], "thinking": row["thinking"], "yolo": row["yolo"],
        } for row in team["rows"]],
    }
    if prepared is not None:
        # Programs are explicit choices, not inferred from client names or PATH.
        # One discovery serves every role choosing the same client and program.
        programs = {}
        for agent in prepared["agents"]:
            if set(agent) == {"id"}:
                continue
            key = (agent["key"], agent["name"])
            if key not in programs:
                details = launch.discover(agent, lambda argv: execute(launch.split_argv(argv, prepared["root"])))
                programs[key] = {"client": agent["key"], "program": agent["name"],
                                 **details, "choices": {}}
        for role, choice in request["choices"].items():
            options = programs[(choice["client"], choice["program"])]
            model = choice["model"]
            listed = model is None or any(
                model == entry["id"] or model in entry.get("variants", {}).values()
                for entry in options["models"]
            )
            options["choices"][role] = {"model": model, "note": "" if listed else
                f"not listed by {choice['program']}; the client checks it at startup"}
        result["client_options"] = list(programs.values())
    return result


def prepare(request: dict, app_home: Path) -> dict:
    """Derive trusted role identity and reject all malformed choices before execution."""
    if not isinstance(request, dict) or set(request) != {"space", "choices"}:
        raise ValueError("Team launch requires a space ID and role choices.")
    roster = launch.load_roster(app_home, request["space"])
    if roster is None:
        raise ValueError("This space has no team roster. Prepare the project's team first.")
    choices = request["choices"]
    if not isinstance(choices, dict):
        raise ValueError("Choices must be a JSON object keyed by roster role.")
    roles = {role["role"] for role in roster["roles"]}
    if choices.keys() - roles:
        raise ValueError("Choices name a role outside this space's current roster.")
    agents = []
    for role in roster["roles"]:
        agent = {"id": f"role:{role['handle']}"}
        if role["role"] in choices:
            choice = choices[role["role"]]
            if not isinstance(choice, dict) or set(choice) != {"client", "program", "model", "thinking", "yolo"}:
                raise ValueError(f"Role {role['role']} requires client, program, model, thinking and yolo.")
            program = choice["program"]
            if not isinstance(program, str) or not program or program.startswith("-") or any(ord(c) < 32 for c in program):
                raise ValueError(f"Role {role['role']} requires a literal program name or path, not shell code.")
            argv = [program, "--no-daemon"] if choice["client"] == "codex" else [program]
            agent.update(key=choice["client"], name=program, run=argv,
                         model=choice["model"], thinking=choice["thinking"], yolo=choice["yolo"])
        agents.append(agent)
    return launch.validate({"space": request["space"], "root": roster["root"],
                            "swarm": roster["swarm"], "agents": agents}, roster)


def check_options(agent: dict, root: str, execute) -> None:
    """Check the named program and requested flags through the same launch shell."""
    try:
        details = launch.discover(agent, lambda argv: execute(launch.split_argv(argv, root)))
    except Exception as error:
        raise ValueError(f"Program {agent['name']!r} could not run its option check: {error}") from None
    if agent["model"] is not None and not details["model_supported"]:
        raise ValueError(f"Program {agent['name']!r} does not advertise --model; choose model null.")
    if agent["yolo"] and not details["yolo_flag"]:
        raise ValueError(f"Program {agent['name']!r} does not advertise the requested permission flag.")
    thinking = agent["thinking"]
    if thinking is not None:
        entry = next((m for m in details["models"] if m["id"] == agent["model"]), None)
        levels = entry.get("efforts", [] if agent["key"] == "antigravity" else details["efforts"]) if entry else details["efforts"]
        if not details["thinking_supported"] or thinking not in levels or agent["key"] == "cursor":
            raise ValueError(f"Thinking level {thinking!r} is unavailable for {agent['name']!r} and this model.")


def start(request: dict, app, core, app_home: Path, execute) -> dict:
    """Check all new choices, then let the dialog's executor own creation and replay."""
    prepared = prepare(request, app_home)
    team = launch.inspect_team({"space": prepared["space"], "root": ""}, app, core, app_home)
    running = {row["id"] for row in team["rows"] if row["terminal"] is not None}
    for agent in prepared["agents"]:
        if agent["id"] not in running:
            if set(agent) == {"id"}:
                raise ValueError("Every role that is not running needs an explicit choice. Inspect the team and retry.")
            check_options(agent, prepared["root"], execute)
    return launch.launch_team(prepared, app, core, app_home)
