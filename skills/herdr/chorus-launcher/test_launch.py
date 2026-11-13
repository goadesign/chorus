"""Exercise project targeting, repeat launches, thinking options, and recovery without paid agents."""

import copy
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import launch


class Herdr:
    """Model only the native commands used by the launcher, retaining call evidence."""

    def __init__(self):
        self.listing = {"spaces": [{"id": "sp1", "tabs": []}]}
        self.terminals = []
        self.commands = []
        self.fail_agent = None
        self.fail_command = None

    @staticmethod
    def _agent_foreground(argv, folder, pid):
        inner = list(argv)
        cwd = folder or ""
        if len(inner) >= 6 and inner[1] == "-lic":
            cwd = inner[4]
            if 'while [[ "$1" != "--" ]]' in inner[2] and "--" in inner[5:]:
                sep = inner.index("--", 5)
                inner = inner[sep + 1 :]
            else:
                inner = inner[5:]
        if len(inner) > 1 and isinstance(inner[-1], str) and inner[-1].startswith("Use the installed chorus skill now."):
            inner = inner[:-1]
            if inner and inner[-1] in ("--", "--prompt-interactive", "--prompt"):
                inner = inner[:-1]
        prog = Path(inner[0]).name if inner else "sh"
        name = "2.1.323" if prog == "claude" else prog
        return {"pid": pid, "name": name, "argv": inner, "cwd": cwd}

    def exit_to_shell(self, address, argv=None, name="zsh"):
        terminal = next(t for t in self.terminals if t["address"] == address)
        shell_argv = list(argv) if argv is not None else ["/bin/zsh", "-li"]
        fg = terminal.get("foreground") or {"pid": 1000, "cwd": ""}
        terminal["foreground"] = {
            "pid": fg["pid"] + 500,
            "name": name,
            "argv": shell_argv,
            "cwd": fg["cwd"],
        }

    def app(self, argv):
        if argv == ["ls"]:
            return copy.deepcopy(self.listing)
        command, args = argv[1], json.loads(argv[2])
        self.commands.append((command, args))
        if self.fail_command == command:
            return {"error": f"{command} failed"}
        if command == "tab.select":
            return {"ok": True}
        if command == "terminal.kill":
            for space in self.listing["spaces"]:
                for tab in space["tabs"]:
                    tab["terminals"] = [p for p in tab["terminals"] if p["terminal"] != args["terminal"]]
            killed = next(t for t in self.terminals if t["address"] == args["terminal"])
            killed["state"] = "exited"
            killed["foreground"] = None
            return {"ok": True}
        if command == "pane.swap":
            tab = next(
                t
                for s in self.listing["spaces"]
                for t in s["tabs"]
                if any(p["pane"] == args["pane"] for p in t["terminals"])
                and any(p["pane"] == args["target"] for p in t["terminals"])
            )
            first = next(i for i, p in enumerate(tab["terminals"]) if p["pane"] == args["pane"])
            second = next(i for i, p in enumerate(tab["terminals"]) if p["pane"] == args["target"])
            tab["terminals"][first], tab["terminals"][second] = tab["terminals"][second], tab["terminals"][first]
            return {"ok": True}
        if command == "pane.close":
            tab, entry = next(
                (t, p)
                for s in self.listing["spaces"]
                for t in s["tabs"]
                for p in t["terminals"]
                if p["pane"] == args["pane"]
            )
            tab["terminals"] = [p for p in tab["terminals"] if p["pane"] != args["pane"]]
            closed = next(t for t in self.terminals if t["address"] == entry["terminal"])
            closed["state"] = "finished"
            closed["foreground"] = None
            return {"ok": True}
        if command == "tab.new":
            space = next(s for s in self.listing["spaces"] if s["id"] == args["space"])
            tab_id = "tb1" if space["id"] == "sp1" else f"tb-{space['id']}"
            tab = {"id": tab_id, "terminals": []}
            space["tabs"].append(tab)
        elif command == "pane.split":
            space, tab = next(
                (s, t)
                for s in self.listing["spaces"]
                for t in s["tabs"]
                if any(p["pane"] == args["pane"] for p in t["terminals"])
            )
        else:
            raise AssertionError(command)
        ordinal = len(self.terminals) + 1
        pane, terminal = f"p{ordinal}", f"t{ordinal}"
        tab["terminals"].append({"pane": pane, "terminal": terminal})
        self.terminals.append({
            "address": terminal,
            "state": "live",
            "foreground": self._agent_foreground(args["argv"], args.get("folder", ""), 1000 + ordinal),
            "labels": {"herdr.space": space["id"], "herdr.tab": tab["id"]},
        })
        return {"tab": tab["id"], "pane": pane, "terminal": terminal}

    def core(self, argv):
        if argv[:2] == ["ls", "--json"]:
            labels = dict(value.split("=", 1) for value in argv[3::2])
            return {"hidden_finished": 0, "terminals": [
                {"address": t["address"], "state": t["state"]}
                for t in self.terminals if all(t["labels"].get(k) == v for k, v in labels.items())
            ]}
        if argv[0] == "get":
            return copy.deepcopy(next(t for t in self.terminals if t["address"] == argv[1]))
        if self.fail_agent and f"chorus.agent={self.fail_agent}" in argv:
            raise RuntimeError("label failure")
        terminal = next(t for t in self.terminals if t["address"] == argv[1])
        terminal["labels"].update(item.split("=", 1) for item in argv[2:])
        return {}


def request():
    """Include a shell-sensitive folder to prove that paths remain argv data."""
    payload = {"space": "sp1", "root": "/Projects/a project's $(touch nope)", "swarm": "example", "agents": [
        {"key": "claude", "name": "Claude Code", "run": ["claude"]},
        {"key": "codex", "name": "Codex", "run": ["codex", "--no-daemon"]},
        {"key": "antigravity", "name": "Antigravity", "run": ["agy"]},
        {"key": "cursor", "name": "Cursor", "run": ["cursor-agent"]},
    ]}
    for agent in payload["agents"]:
        agent.update(id=f"row-{agent['key']}", model=None, thinking=None, yolo=False)
    return payload


class LaunchTests(unittest.TestCase):
    """Verify observable team creation rather than internal rendering details."""

    def test_creates_one_tab_four_panes_with_explicit_project_and_skill(self):
        owner, payload = Herdr(), request()
        result = launch.launch_team(payload, owner.app, owner.core)
        self.assertEqual(result, {"tab": "tb1", "created": 4, "reused": 0})
        creations = [(name, args) for name, args in owner.commands if name in ("tab.new", "pane.split")]
        self.assertEqual([name for name, _ in creations], ["tab.new", "pane.split", "pane.split", "pane.split"])
        self.assertEqual(creations[0][1]["space"], "sp1")
        self.assertEqual(creations[0][1]["folder"], payload["root"])
        self.assertEqual([args["direction"] for name, args in creations if name == "pane.split"], ["right", "down", "down"])
        handles = set()
        for name, args in creations:
            argv = args["argv"]
            if name == "pane.split":
                self.assertEqual(argv[4], payload["root"])
            self.assertIn("Use the installed chorus skill", argv[-1])
            self.assertIn("Do not claim", argv[-1])
            self.assertNotIn("--dangerously", argv)
            handle = argv[-1].split("using handle '")[1].split("'")[0]
            self.assertRegex(handle, r"^[a-f0-9]{32}$")
            handles.add(handle)
        self.assertEqual(len(handles), 4)
        self.assertIn("--prompt-interactive", creations[2][1]["argv"])
        self.assertEqual(len(owner.listing["spaces"][0]["tabs"]), 1)

    def test_reopening_reuses_existing_team_and_only_adds_missing_agent(self):
        owner, payload = Herdr(), request()
        launch.launch_team(payload, owner.app, owner.core)
        self.assertEqual(launch.launch_team(payload, owner.app, owner.core)["created"], 0)
        payload["agents"].append({"id": "row-custom", "key": "custom", "name": "Custom", "run": ["custom-agent", "--interactive"], "model": None, "thinking": None, "yolo": False})
        self.assertEqual(launch.launch_team(payload, owner.app, owner.core), {"tab": "tb1", "created": 1, "reused": 4})
        self.assertEqual(sum(name == "tab.new" for name, _ in owner.commands), 1)

    def test_different_swarm_does_not_repurpose_existing_conversations(self):
        owner, payload = Herdr(), request()
        launch.launch_team(payload, owner.app, owner.core)
        payload["swarm"] = "other"
        before = len(owner.commands)
        with self.assertRaisesRegex(RuntimeError, "different Chorus name"):
            launch.launch_team(payload, owner.app, owner.core)
        self.assertEqual(len(owner.commands), before)

    def test_failed_label_removes_only_new_pane_and_preserves_first_agent(self):
        owner = Herdr()
        owner.fail_agent = "codex"
        with self.assertRaisesRegex(RuntimeError, "label failure"):
            launch.launch_team(request(), owner.app, owner.core)
        self.assertEqual(owner.commands[-1], ("terminal.kill", {"terminal": "t2"}))
        self.assertEqual(owner.listing["spaces"][0]["tabs"][0]["terminals"], [{"pane": "p1", "terminal": "t1"}])

    def test_invalid_request_has_no_side_effects(self):
        for value in (None, {}, {**request(), "swarm": "invalid name"}, {**request(), "root": "relative"}):
            with self.subTest(value=value):
                owner = Herdr()
                with self.assertRaises(ValueError):
                    launch.launch_team(value, owner.app, owner.core)
                self.assertEqual(owner.commands, [])

    def test_two_models_and_thinking_levels_of_same_client_have_distinct_panes_and_strict_replay(self):
        owner, payload = Herdr(), request()
        first = {**payload["agents"][1], "model": "gpt-6.1-sol", "thinking": "high", "yolo": False}
        second = {**first, "id": "codex-second", "model": "gpt-6-luna", "thinking": "max", "yolo": True}
        payload["agents"] = [first, second]
        self.assertEqual(launch.launch_team(payload, owner.app, owner.core), {"tab": "tb1", "created": 2, "reused": 0})
        creations = [args["argv"] for name, args in owner.commands if name in ("tab.new", "pane.split")]
        self.assertIn("gpt-6.1-sol", creations[0])
        self.assertIn('model_reasoning_effort="high"', creations[0])
        self.assertNotIn("--dangerously-bypass-approvals-and-sandbox", creations[0])
        self.assertIn("gpt-6-luna", creations[1])
        self.assertIn('model_reasoning_effort="max"', creations[1])
        self.assertIn("--dangerously-bypass-approvals-and-sandbox", creations[1])
        self.assertIn("--no-daemon", creations[1])
        self.assertEqual(launch.launch_team(payload, owner.app, owner.core)["created"], 0)
        before = len(owner.commands)
        first["thinking"] = "low"
        with self.assertRaisesRegex(RuntimeError, "different launch settings"):
            launch.launch_team(payload, owner.app, owner.core)
        self.assertEqual(len(owner.commands), before)

    def test_client_yolo_thinking_and_model_arguments_precede_interactive_prompt(self):
        cases = [
            ("claude", "claude-opus-5-5", "high", ["claude", "--model", "claude-opus-5-5", "--effort", "high", "--dangerously-skip-permissions"]),
            ("codex", "gpt-6.1-sol", "xhigh", ["codex", "--model", "gpt-6.1-sol", "-c", 'model_reasoning_effort="xhigh"', "--dangerously-bypass-approvals-and-sandbox"]),
            ("antigravity", "gemini-4-argon", "high", ["antigravity", "--model", "gemini-4-argon", "--effort", "high", "--dangerously-skip-permissions", "--prompt-interactive"]),
            ("cursor", "claude-opus-5-5-max", "max", ["cursor", "--model", "claude-opus-5-5-max", "--yolo"]),
            ("gemini", "gemini-model", None, ["gemini", "--model", "gemini-model", "--yolo", "--prompt-interactive"]),
        ]
        for key, model, thinking, prefix in cases:
            with self.subTest(client=key):
                agent = {"key": key, "name": key, "run": [key], "model": model, "thinking": thinking, "yolo": True}
                argv = launch.initial_argv(agent, "/project", "team")
                self.assertEqual(argv[:-1], prefix)
                self.assertIn("Use the installed chorus skill", argv[-1])
                agent.update(model=None, thinking=None, yolo=False)
                argv = launch.initial_argv(agent, "/project", "team")
                self.assertNotIn("--model", argv)
                self.assertNotIn("--effort", argv)
                self.assertNotIn("-c", argv)
                self.assertNotIn("--trust", argv)
                self.assertNotIn("--approve-mcps", argv)
                self.assertNotIn("--sandbox", argv)

    def test_inspect_preserves_legacy_and_two_field_panes_without_inventing_thinking(self):
        owner, payload = Herdr(), request()
        launch.launch_team(payload, owner.app, owner.core)
        for terminal in owner.terminals[:2]:
            for label in ("chorus.row", "chorus.options"):
                del terminal["labels"][label]
        for terminal in owner.terminals[2:]:
            terminal["labels"]["chorus.options"] = json.dumps({"model": "saved-model", "yolo": True})
        result = launch.inspect_team({"space": "sp1", "root": payload["root"]}, owner.app, owner.core)
        self.assertEqual(len(result["rows"]), 4)
        self.assertTrue(all(row["model"] is None and row["thinking"] is None and row["yolo"] is None for row in result["rows"][:2]))
        self.assertTrue(all(row["model"] == "saved-model" and row["thinking"] is None and row["yolo"] is True for row in result["rows"][2:]))
        payload["agents"] = [{"id": row["id"]} for row in result["rows"]]
        self.assertEqual(launch.launch_team(payload, owner.app, owner.core)["created"], 0)

    def test_partial_launch_can_retry_same_rows_without_duplicate_agents(self):
        owner, payload = Herdr(), request()
        owner.fail_agent = "codex"
        with self.assertRaisesRegex(RuntimeError, "label failure"):
            launch.launch_team(payload, owner.app, owner.core)
        owner.fail_agent = None
        self.assertEqual(launch.launch_team(payload, owner.app, owner.core), {"tab": "tb1", "created": 3, "reused": 1})
        self.assertEqual(len(owner.listing["spaces"][0]["tabs"][0]["terminals"]), 4)

    def test_failed_lookup_after_first_tab_creation_leaves_a_reusable_labeled_pane(self):
        """A failed owner read after creation must not strand an unrecognized agent."""
        owner, payload = Herdr(), request()
        reads = 0

        def app(argv):
            nonlocal reads
            if argv == ["ls"]:
                reads += 1
                if reads == 2:
                    raise RuntimeError("listing timed out")
            return owner.app(argv)

        with self.assertRaisesRegex(RuntimeError, "listing timed out"):
            launch.launch_team(payload, app, owner.core)
        self.assertEqual(owner.terminals[0]["labels"]["chorus.row"], payload["agents"][0]["id"])
        self.assertEqual(launch.launch_team(payload, app, owner.core), {"tab": "tb1", "created": 3, "reused": 1})

    def test_invalid_options_duplicate_row_and_closed_reference_fail_before_creation(self):
        invalid_changes = (
            {"model": ""},
            {"model": "--flag"},
            {"yolo": "true"},
            {"key": "custom", "yolo": True},
            {"thinking": "--bad"},
            {"key": "gemini", "thinking": "high"},
            {"key": "cursor", "model": None, "thinking": "high"},
            {"key": "cursor", "model": "gpt-5.4-high", "thinking": "low"},
            {"key": "antigravity", "model": "gemini-4-argon-high", "thinking": "low"},
        )
        for change in invalid_changes:
            with self.subTest(change=change):
                owner, payload = Herdr(), request()
                payload["agents"][0].update(change)
                with self.assertRaises(ValueError):
                    launch.launch_team(payload, owner.app, owner.core)
                self.assertEqual(owner.commands, [])
        owner, payload = Herdr(), request()
        payload["agents"].append(payload["agents"][0].copy())
        with self.assertRaisesRegex(ValueError, "same team row"):
            launch.launch_team(payload, owner.app, owner.core)
        self.assertEqual(owner.commands, [])
        payload["agents"] = [payload["agents"][0], {"id": "closed-terminal"}]
        with self.assertRaisesRegex(RuntimeError, "was closed"):
            launch.launch_team(payload, owner.app, owner.core)
        self.assertEqual(owner.commands, [])

    def test_model_catalogs_and_thinking_levels_are_owned_by_clients(self):
        codex_json = json.dumps({"models": [
            {"slug": "gpt-6.1-sol", "display_name": "GPT-6.1-Sol", "visibility": "list",
             "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}, {"effort": "ultra"}]},
            {"slug": "gpt-6-luna", "display_name": "GPT-6-Luna", "visibility": "list",
             "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}]},
            {"slug": "hidden", "display_name": "Hidden", "visibility": "hide"},
        ]})
        codex_info = launch.discover({"key": "codex", "name": "Codex", "run": ["codex"]},
                                     lambda argv: "-c --model --dangerously-bypass-approvals-and-sandbox" if argv[-1] == "--help" else codex_json)
        self.assertTrue(codex_info["thinking_supported"])
        self.assertEqual(codex_info["efforts"], ["low", "high", "ultra"])
        self.assertEqual(codex_info["models"][0]["efforts"], ["low", "high", "ultra"])
        self.assertEqual(codex_info["models"][1]["efforts"], ["low", "high"])

        claude_help = (
            "  --model <model>                       Model for the current session\n"
            "  --effort <level>                      Effort level for the current session\n"
            "                                        (low, medium, high, xhigh, max)\n"
            "  --dangerously-skip-permissions        Bypass all permission checks\n"
        )
        claude_info = launch.discover({"key": "claude", "name": "Claude", "run": ["claude"]},
                                      lambda _argv: claude_help)
        self.assertTrue(claude_info["thinking_supported"])
        self.assertEqual(claude_info["efforts"], ["low", "medium", "high", "xhigh", "max"])
        no_list = launch.discover({"key": "claude", "name": "Claude", "run": ["claude"]},
                                  lambda _argv: "  --model <m>\n  --effort <level> Effort without choices\n  --input-format (text, stream-json)")
        self.assertFalse(no_list["thinking_supported"])
        self.assertEqual(no_list["efforts"], [])

        agy_help = (
            "  --dangerously-skip-permissions  Auto-approve\n"
            "  --effort                        Reasoning effort (low|medium|high|xhigh|max)\n"
            "  -i                              Short alias\n"
            "  --input-format                  Input format (text, stream-json)\n"
            "  --model                         Model for session\n"
        )
        agy_tsv = "gemini-4-argon-medium\tGemini 4 Argon (Medium)\ngemini-4-argon-high\tGemini 4 Argon (High)\ngpt-oss-120b-medium\tGPT-OSS 120B (Medium)\none\tOne\n"
        agy_info = launch.discover({"key": "antigravity", "name": "Antigravity", "run": ["agy"]},
                                   lambda argv: agy_help if argv[-1] == "--help" else agy_tsv)
        self.assertEqual(agy_info["models"], [
            {"id": "gemini-4-argon", "name": "Gemini 4 Argon", "efforts": ["medium", "high"]},
            {"id": "gpt-oss-120b-medium", "name": "GPT-OSS 120B (Medium)"},
            {"id": "one", "name": "One"},
        ])

        cursor_text = "\n".join([
            "Available models",
            "",
            "auto - Auto (current, default)",
            "claude-opus-5-5-low - Claude Opus 5.5 1M Low",
            "claude-opus-5-5-medium - Claude Opus 5.5 1M",
            "claude-opus-5-5-high - Claude Opus 5.5 1M High",
            "claude-opus-5-5-low-fast - Claude Opus 5.5 1M Low Fast",
            "claude-opus-5-5-high-fast - Claude Opus 5.5 1M High Fast",
            "claude-opus-5-thinking-low - Claude Opus 5 1M Low Thinking",
            "claude-opus-5-thinking-high - Claude Opus 5 1M Thinking",
            "claude-4.6-opus-high-thinking - Claude Opus 4.6 1M Thinking",
            "gpt-5.5-low - GPT-5.5 1M Low",
            "gpt-5.5-high - GPT-5.5 1M High",
            "gpt-5.5-extra-high - GPT-5.5 1M Extra High",
            "gpt-5.3-codex - Codex 5.3",
        ])
        cursor_info = launch.discover({"key": "cursor", "name": "Cursor", "run": ["cursor-agent"]},
                                      lambda argv: "--model --yolo" if argv[-1] == "--help" else cursor_text)
        self.assertTrue(cursor_info["thinking_supported"])
        by_name = {m["name"]: m for m in cursor_info["models"]}
        self.assertEqual(by_name["Auto"], {"id": "auto", "name": "Auto"})
        self.assertEqual(by_name["Claude Opus 5.5 1M"]["id"], "claude-opus-5-5-high")
        self.assertEqual(by_name["Claude Opus 5.5 1M"]["efforts"], ["low", "medium", "high"])
        self.assertEqual(by_name["Claude Opus 5.5 1M Fast"]["variants"], {"low": "claude-opus-5-5-low-fast", "high": "claude-opus-5-5-high-fast"})
        self.assertEqual(by_name["Claude Opus 5 1M Thinking"]["variants"]["high"], "claude-opus-5-thinking-high")
        self.assertEqual(by_name["GPT-5.5 1M"]["efforts"], ["low", "high", "extra-high"])
        self.assertEqual(by_name["GPT-5.5 1M"]["variants"]["extra-high"], "gpt-5.5-extra-high")
        self.assertNotIn("efforts", by_name["Claude Opus 4.6 1M Thinking"])
        self.assertNotIn("efforts", by_name["Codex 5.3"])

        failed = launch.discover({"key": "cursor", "name": "Cursor", "run": ["cursor-agent"]}, lambda argv: "--model" if argv[-1] == "--help" else "unreadable")
        self.assertIsNone(failed["yolo_flag"])
        self.assertIn("Could not load models", failed["note"])
        self.assertEqual(failed["models"], [])

    def test_successful_help_on_stderr_enables_installed_client_choices(self):
        """Use the actual helper process to cover clients whose help is on stderr."""
        script = "import sys; print('--model --effort (low|medium|high) --dangerously-skip-permissions', file=sys.stderr) if sys.argv[-1] == '--help' else print('one\\tOne')"
        payload = {"key": "antigravity", "name": "Antigravity", "run": [sys.executable, "-c", script]}
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, "-B", str(Path(launch.__file__)), directory, "models", json.dumps(payload)],
                                    stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
        options = json.loads(result.stdout)
        self.assertTrue(options["model_supported"])
        self.assertTrue(options["thinking_supported"])
        self.assertEqual(options["efforts"], ["low", "medium", "high"])
        self.assertEqual(options["yolo_flag"], "--dangerously-skip-permissions")
        self.assertEqual(options["models"], [{"id": "one", "name": "One"}])

    def test_shell_function_discovery_and_launch_use_the_project_directory(self):
        """Herdr supports configured shell functions as well as executable files."""
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            (folder / ".zshrc").write_text('chorus_test_client() { if [[ "${PWD:A}" != "${ZDOTDIR:A}" ]]; then return 3; fi; if [[ "$1" == "--help" ]]; then print -- "--model --dangerously-skip-permissions" >&2; else printf "one\\tOne\\n"; fi; }\n')
            payload = {"key": "antigravity", "name": "Test client", "run": ["chorus_test_client"]}
            env = {**os.environ, "ZDOTDIR": directory, "CI": "true"}
            result = subprocess.run([sys.executable, "-B", str(Path(launch.__file__)), directory, "models", json.dumps(payload)],
                                    cwd=directory, env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            options = json.loads(result.stdout)
            self.assertTrue(options["model_supported"])
            self.assertEqual(options["models"], [{"id": "one", "name": "One"}])
            result = subprocess.run(launch.split_argv(["chorus_test_client", "models"], directory), env=env,
                                    stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout, "one\tOne\n")

    def test_split_changes_directory_literally_before_starting_program(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "a project's $(touch nope)"
            root.mkdir()
            code = "import os,sys,json; print(json.dumps([os.getcwd(),sys.argv[1:]]))"
            argv = launch.split_argv(["python3", "-c", code, "a 'quoted' prompt; $(touch nope)"], str(root))
            result = subprocess.run(argv, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
            self.assertEqual(json.loads(result.stdout), [str(root.resolve()), ["a 'quoted' prompt; $(touch nope)"]])
            self.assertFalse((root / "nope").exists())

    def test_roster_three_roles_starts_handles_folders_messages_labels_and_placement(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "payments"
            builder_dir = root / "worktrees" / "builder"
            tester_dir = root / "worktrees" / "test-writer"
            for folder in (root, builder_dir, tester_dir):
                folder.mkdir(parents=True)
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "payments",
                "roles": [
                    {
                        "role": "guide",
                        "title": "Guide",
                        "handle": "guide",
                        "folder": str(root),
                        "first_message": "Shape the change with the user.",
                        "connections": [],
                        "place": None,
                    },
                    {
                        "role": "builder",
                        "title": "Builder",
                        "handle": "builder",
                        "folder": str(builder_dir),
                        "first_message": "Implement the assigned package.",
                        "connections": [],
                        "place": {"beside": "guide", "direction": "right"},
                    },
                    {
                        "role": "test-writer",
                        "title": "Test writer",
                        "handle": "test-writer",
                        "folder": str(tester_dir),
                        "first_message": "Write acceptance tests from the contract.",
                        "connections": [],
                        "place": {"beside": "builder", "direction": "down"},
                    },
                ],
            }))
            owner = Herdr()
            inspected = launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
            self.assertTrue(inspected["roster"])
            self.assertEqual(inspected["root"], str(root))
            self.assertEqual(inspected["swarm"], "payments")
            self.assertEqual([r["id"] for r in inspected["rows"]], ["role:guide", "role:builder", "role:test-writer"])
            payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "payments",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": "claude-opus-5-5", "thinking": "high", "yolo": False},
                    {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["codex"], "model": "gpt-6.1-sol", "thinking": "xhigh", "yolo": True},
                    {"id": "role:test-writer", "key": "antigravity", "name": "Antigravity", "run": ["agy"], "model": "gemini-4-argon", "thinking": "high", "yolo": False},
                ],
            }
            result = launch.launch_team(payload, owner.app, owner.core, app_home)
            self.assertEqual(result, {"tab": "tb1", "created": 3, "reused": 0})
            creations = [(name, args) for name, args in owner.commands if name in ("tab.new", "pane.split")]
            self.assertEqual([name for name, _ in creations], ["tab.new", "pane.split", "pane.split"])
            self.assertEqual(creations[0][1]["folder"], str(root))
            self.assertEqual(creations[1][1]["pane"], "p1")
            self.assertEqual(creations[1][1]["direction"], "right")
            self.assertEqual(creations[1][1]["argv"][4], str(builder_dir))
            self.assertEqual(creations[2][1]["pane"], "p2")
            self.assertEqual(creations[2][1]["direction"], "down")
            self.assertEqual(creations[2][1]["argv"][4], str(tester_dir))
            self.assertIn("using handle 'guide' and role 'Guide'", creations[0][1]["argv"][-1])
            self.assertIn("Shape the change with the user.", creations[0][1]["argv"][-1])
            self.assertIn("using handle 'builder' and role 'Builder'", creations[1][1]["argv"][-1])
            self.assertIn("Implement the assigned package.", creations[1][1]["argv"][-1])
            self.assertIn("using handle 'test-writer' and role 'Test writer'", creations[2][1]["argv"][-1])
            self.assertIn("Write acceptance tests from the contract.", creations[2][1]["argv"][-1])
            self.assertEqual(
                [(t["labels"]["chorus.handle"], t["labels"]["chorus.role"]) for t in owner.terminals],
                [("guide", "guide"), ("builder", "builder"), ("test-writer", "test-writer")],
            )
            reinspected = launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
            self.assertEqual([r["terminal"] for r in reinspected["rows"]], ["t1", "t2", "t3"])
            self.assertEqual([r["handle"] for r in reinspected["rows"]], ["guide", "builder", "test-writer"])
            self.assertEqual([r["role"] for r in reinspected["rows"]], ["guide", "builder", "test-writer"])

    def test_roster_adding_role_to_running_team_and_restarting_first_role_keep_one_tab(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "project"
            root.mkdir()
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roles = [
                {
                    "role": "guide",
                    "title": "Guide",
                    "handle": "guide",
                    "folder": str(root),
                    "first_message": "Lead the project.",
                    "connections": [],
                    "place": None,
                },
                {
                    "role": "builder",
                    "title": "Builder",
                    "handle": "builder",
                    "folder": str(root),
                    "first_message": "Build the service.",
                    "connections": [],
                    "place": {"beside": "guide", "direction": "right"},
                },
            ]
            roster_file.write_text(json.dumps({"root": str(root), "swarm": "proj", "roles": roles}))
            owner = Herdr()
            initial_payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                    {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["codex"], "model": None, "thinking": None, "yolo": False},
                ],
            }
            self.assertEqual(launch.launch_team(initial_payload, owner.app, owner.core, app_home), {"tab": "tb1", "created": 2, "reused": 0})
            roles.append({
                "role": "reviewer",
                "title": "Reviewer",
                "handle": "reviewer",
                "folder": str(root),
                "first_message": "Review the diff.",
                "connections": [],
                "place": {"beside": "builder", "direction": "down"},
            })
            roster_file.write_text(json.dumps({"root": str(root), "swarm": "proj", "roles": roles}))
            add_payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide"},
                    {"id": "role:builder"},
                    {"id": "role:reviewer", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                ],
            }
            self.assertEqual(launch.launch_team(add_payload, owner.app, owner.core, app_home), {"tab": "tb1", "created": 1, "reused": 2})
            self.assertEqual(sum(name == "tab.new" for name, _ in owner.commands), 1)
            self.assertEqual(owner.commands[-1][0], "pane.split")
            self.assertEqual(owner.commands[-1][1]["pane"], "p2")
            self.assertEqual(owner.commands[-1][1]["direction"], "down")

            # If the first role (place: null) exits while the team tab remains open, restarting it splits the first pane and swaps back into first position.
            owner.app(["do", "terminal.kill", json.dumps({"terminal": "t1"})])
            restart_first = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                    {"id": "role:builder"},
                    {"id": "role:reviewer"},
                ],
            }
            self.assertEqual(launch.launch_team(restart_first, owner.app, owner.core, app_home), {"tab": "tb1", "created": 1, "reused": 2})
            self.assertEqual(sum(name == "tab.new" for name, _ in owner.commands), 1)
            self.assertEqual(owner.commands[-2][0], "pane.split")
            self.assertEqual(owner.commands[-2][1]["pane"], "p2")
            self.assertEqual(owner.commands[-2][1]["direction"], "right")
            self.assertEqual(owner.commands[-1], ("pane.swap", {"pane": "p4", "target": "p2"}))
            self.assertEqual(
                owner.listing["spaces"][0]["tabs"][0]["terminals"],
                [{"pane": "p4", "terminal": "t4"}, {"pane": "p3", "terminal": "t3"}, {"pane": "p2", "terminal": "t2"}],
            )

    def test_roster_supports_empty_non_git_folder_two_projects_in_one_repo_and_other_focused_worktree(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            empty_non_git = Path(directory) / "empty-non-git"
            monorepo = Path(directory) / "monorepo"
            proj_a = monorepo / "services" / "orders"
            proj_b = monorepo / "services" / "billing"
            other_worktree = Path(directory) / "other-worktree"
            for folder in (empty_non_git, proj_a, proj_b, other_worktree):
                folder.mkdir(parents=True)
            rosters_dir = app_home / "data" / "chorus-launcher" / "rosters"
            rosters_dir.mkdir(parents=True)
            for space_id, root_dir, swarm_name, handle in (
                ("sp-empty", empty_non_git, "empty-proj", "guide-empty"),
                ("sp-alpha", proj_a, "orders", "guide-orders"),
                ("sp-beta", proj_b, "billing", "guide-billing"),
            ):
                (rosters_dir / f"{space_id}.json").write_text(json.dumps({
                    "root": str(root_dir),
                    "swarm": swarm_name,
                    "roles": [{
                        "role": "guide",
                        "title": "Guide",
                        "handle": handle,
                        "folder": str(root_dir),
                        "first_message": f"Start {swarm_name}.",
                        "connections": [],
                        "place": None,
                    }],
                }))
            owner = Herdr()
            owner.listing = {"spaces": [
                {"id": "sp-empty", "tabs": []},
                {"id": "sp-alpha", "tabs": []},
                {"id": "sp-beta", "tabs": []},
            ]}
            for space_id, root_dir, swarm_name, handle, expected_tab in (
                ("sp-empty", empty_non_git, "empty-proj", "guide-empty", "tb-sp-empty"),
                ("sp-alpha", proj_a, "orders", "guide-orders", "tb-sp-alpha"),
                ("sp-beta", proj_b, "billing", "guide-billing", "tb-sp-beta"),
            ):
                info = launch.inspect_team({"space": space_id, "root": ""}, owner.app, owner.core, app_home)
                self.assertEqual(info["root"], str(root_dir))
                self.assertEqual(info["swarm"], swarm_name)
                res = launch.launch_team({
                    "space": space_id,
                    "root": str(root_dir),
                    "swarm": swarm_name,
                    "agents": [{"id": f"role:{handle}", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False}],
                }, owner.app, owner.core, app_home)
                self.assertEqual(res, {"tab": expected_tab, "created": 1, "reused": 0})

            # Adding a second role in sp-alpha while another working copy is focused still uses sp-alpha's roster folders.
            (rosters_dir / "sp-alpha.json").write_text(json.dumps({
                "root": str(proj_a),
                "swarm": "orders",
                "roles": [
                    {
                        "role": "guide",
                        "title": "Guide",
                        "handle": "guide-orders",
                        "folder": str(proj_a),
                        "first_message": "Start orders.",
                        "connections": [],
                        "place": None,
                    },
                    {
                        "role": "builder",
                        "title": "Builder",
                        "handle": "builder-orders",
                        "folder": str(other_worktree),
                        "first_message": "Build in worktree.",
                        "connections": [],
                        "place": {"beside": "guide-orders", "direction": "right"},
                    },
                ],
            }))
            inspected_alpha = launch.inspect_team({"space": "sp-alpha", "root": ""}, owner.app, owner.core, app_home)
            self.assertEqual(inspected_alpha["root"], str(proj_a))
            self.assertEqual([r["terminal"] for r in inspected_alpha["rows"]], ["t2", None])
            res_alpha = launch.launch_team({
                "space": "sp-alpha",
                "root": str(proj_a),
                "swarm": "orders",
                "agents": [
                    {"id": "role:guide-orders"},
                    {"id": "role:builder-orders", "key": "codex", "name": "Codex", "run": ["codex"], "model": None, "thinking": None, "yolo": False},
                ],
            }, owner.app, owner.core, app_home)
            self.assertEqual(res_alpha, {"tab": "tb-sp-alpha", "created": 1, "reused": 1})
            self.assertEqual(owner.commands[-1][1]["argv"][4], str(other_worktree))

    def test_roster_claude_and_codex_connections_keep_secrets_out_of_argv_labels_and_logs_and_clean_up_mcp_file(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "project"
            root.mkdir()
            claude_secret = "secret-claude-token-998877"
            codex_secret = "secret-codex-token-112233"
            claude_token_file = Path(directory) / "guide.token"
            codex_token_file = Path(directory) / "builder.token"
            claude_token_file.write_text(claude_secret + "\n")
            codex_token_file.write_text(codex_secret + "\n")
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {
                        "role": "guide",
                        "title": "Guide",
                        "handle": "guide",
                        "folder": str(root),
                        "first_message": "Coordinate the change.",
                        "connections": [{
                            "name": "surla",
                            "url": "http://127.0.0.1:43127/mcp",
                            "credential_file": str(claude_token_file),
                        }],
                        "place": None,
                    },
                    {
                        "role": "builder",
                        "title": "Builder",
                        "handle": "builder",
                        "folder": str(root),
                        "first_message": "Build the change.",
                        "connections": [{
                            "name": "surla",
                            "url": "http://127.0.0.1:43127/mcp",
                            "credential_file": str(codex_token_file),
                        }],
                        "place": {"beside": "guide", "direction": "right"},
                    },
                ],
            }))
            owner = Herdr()
            payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": "claude-opus-5-5", "thinking": None, "yolo": False},
                    {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["python3", "-c", "import os,sys,json; print(json.dumps([os.getcwd(), os.environ.get('CHORUS_MCP_TOKEN_0'), sys.argv[1:]]))"], "model": None, "thinking": None, "yolo": False},
                ],
            }
            # If labeling fails on Claude's launch, the newly written MCP config file is unlinked immediately.
            owner.fail_agent = "claude"
            mcp_file = app_home / "data" / "chorus-launcher" / "mcp" / "sp1--guide.json"
            with self.assertRaisesRegex(RuntimeError, "label failure"):
                launch.launch_team(payload, owner.app, owner.core, app_home)
            self.assertFalse(mcp_file.exists())

            owner = Herdr()
            res = launch.launch_team(payload, owner.app, owner.core, app_home)
            self.assertEqual(res, {"tab": "tb1", "created": 2, "reused": 0})
            self.assertTrue(mcp_file.is_file())
            self.assertEqual(stat.S_IMODE(mcp_file.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(mcp_file.parent.stat().st_mode), 0o700)
            mcp_payload = json.loads(mcp_file.read_text())
            self.assertEqual(mcp_payload, {
                "mcpServers": {
                    "surla": {
                        "type": "http",
                        "url": "http://127.0.0.1:43127/mcp",
                        "headers": {"Authorization": f"Bearer {claude_secret}"},
                    }
                }
            })
            creations = [(name, args) for name, args in owner.commands if name in ("tab.new", "pane.split")]
            claude_argv = creations[0][1]["argv"]
            self.assertIn("--mcp-config", claude_argv)
            self.assertNotIn("--strict-mcp-config", claude_argv)
            self.assertEqual(claude_argv[claude_argv.index("--mcp-config") + 1], str(mcp_file))
            self.assertEqual(claude_argv[-2], "--")

            # Verify neither secret appears anywhere in recorded commands or terminal labels.
            serialized = json.dumps({"commands": owner.commands, "terminals": owner.terminals})
            self.assertNotIn(claude_secret, serialized)
            self.assertNotIn(codex_secret, serialized)

            # Execute Codex's wrapped argv directly to prove the child receives CHORUS_MCP_TOKEN_0 in its env without exposing the token in argv.
            codex_start_argv = creations[1][1]["argv"]
            proc = subprocess.run(codex_start_argv, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
            child_cwd, child_token, child_args = json.loads(proc.stdout)
            self.assertEqual(child_cwd, str(root.resolve()))
            self.assertEqual(child_token, codex_secret)
            self.assertEqual(
                child_args[:4],
                ["-c", 'mcp_servers.surla.url="http://127.0.0.1:43127/mcp"', "-c", 'mcp_servers.surla.bearer_token_env_var="CHORUS_MCP_TOKEN_0"'],
            )
            self.assertEqual(child_args[4], "--")

            # While guide's pane is live, inspect_team preserves sp1--guide.json; once guide's pane exits, inspect_team deletes it.
            launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
            self.assertTrue(mcp_file.exists())
            owner.app(["do", "terminal.kill", json.dumps({"terminal": "t1"})])
            launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
            self.assertFalse(mcp_file.exists())

            # Relaunching guide writes and overwrites sp1--guide.json with the updated token.
            claude_token_file.write_text("rotated-claude-secret-445566\n")
            relaunch_payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                    {"id": "role:builder"},
                ],
            }
            launch.launch_team(relaunch_payload, owner.app, owner.core, app_home)
            self.assertTrue(mcp_file.exists())
            self.assertEqual(
                json.loads(mcp_file.read_text())["mcpServers"]["surla"]["headers"]["Authorization"],
                "Bearer rotated-claude-secret-445566",
            )

    def test_roster_rejects_unsupported_connection_clients_before_starting_anything(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "project"
            root.mkdir()
            token_file = Path(directory) / "guide.token"
            token_file.write_text("secret-token\n")
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [{
                    "role": "guide",
                    "title": "Guide",
                    "handle": "guide",
                    "folder": str(root),
                    "first_message": "Guide the project.",
                    "connections": [{
                        "name": "surla",
                        "url": "http://127.0.0.1:43127/mcp",
                        "credential_file": str(token_file),
                    }],
                    "place": None,
                }],
            }))
            for key, name in (("cursor", "Cursor"), ("antigravity", "Antigravity"), ("gemini", "Gemini"), ("opencode", "OpenCode")):
                with self.subTest(client=key):
                    owner = Herdr()
                    payload = {
                        "space": "sp1",
                        "root": str(root),
                        "swarm": "proj",
                        "agents": [{"id": "role:guide", "key": key, "name": name, "run": [key], "model": None, "thinking": None, "yolo": False}],
                    }
                    with self.assertRaisesRegex(ValueError, "cannot yet be given a role's connections"):
                        launch.launch_team(payload, owner.app, owner.core, app_home)
                    self.assertEqual(owner.commands, [])

    def test_roster_first_message_with_leading_dashes_and_quotes_arrives_intact(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "project"
            root.mkdir()
            tricky_message = "- check status --help 'single' \"double\" $(touch nope)"
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {
                        "role": "guide",
                        "title": "Guide",
                        "handle": "guide",
                        "folder": str(root),
                        "first_message": tricky_message,
                        "connections": [],
                        "place": None,
                    },
                    {
                        "role": "builder",
                        "title": "Builder",
                        "handle": "builder",
                        "folder": str(root),
                        "first_message": tricky_message,
                        "connections": [],
                        "place": {"beside": "guide", "direction": "right"},
                    },
                ],
            }))
            owner = Herdr()
            payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["python3", "-c", "import sys,json; print(json.dumps(sys.argv[1:]))"], "model": None, "thinking": None, "yolo": False},
                    {"id": "role:builder", "key": "antigravity", "name": "Antigravity", "run": ["python3", "-c", "import sys,json; print(json.dumps(sys.argv[1:]))"], "model": None, "thinking": None, "yolo": False},
                ],
            }
            launch.launch_team(payload, owner.app, owner.core, app_home)
            creations = [(name, args) for name, args in owner.commands if name in ("tab.new", "pane.split")]
            for _, args in creations:
                proc = subprocess.run(args["argv"], stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
                received = json.loads(proc.stdout)
                self.assertTrue(received[-1].endswith(tricky_message))
            self.assertEqual(creations[0][1]["argv"][-2], "--")
            self.assertEqual(creations[1][1]["argv"][-2], "--prompt-interactive")
            self.assertFalse((root / "nope").exists())

    def test_roster_invalid_or_missing_files_start_nothing_without_leaking_secrets(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "project"
            root.mkdir()
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            bad_token_file = Path(directory) / "bad.token"
            bad_token_file.write_text("leaked-secret-line-1\nleaked-secret-line-2\n")
            base_role = {
                "role": "guide",
                "title": "Guide",
                "handle": "guide",
                "folder": str(root),
                "first_message": "Hello.",
                "connections": [],
                "place": None,
            }
            invalid_rosters = [
                "{not-json",
                json.dumps({"root": str(root / "missing"), "swarm": "proj", "roles": [base_role]}),
                json.dumps({"root": str(root), "swarm": "proj", "roles": [{**base_role, "folder": str(root / "missing-folder")}]}),
                json.dumps({"root": str(root), "swarm": "proj", "roles": [{**base_role, "connections": [{"name": "surla", "url": "http://127.0.0.1:8080/mcp", "credential_file": str(root / "missing.token")}]}]}),
                json.dumps({"root": str(root), "swarm": "proj", "roles": [{**base_role, "connections": [{"name": "surla", "url": "http://127.0.0.1:8080/mcp", "credential_file": str(bad_token_file)}]}]}),
                json.dumps({"root": str(root), "swarm": "proj", "roles": [{**base_role, "connections": [{"name": "chorus", "url": "http://127.0.0.1:8080/mcp", "credential_file": str(bad_token_file)}]}]}),
                json.dumps({"root": str(root), "swarm": "proj", "roles": [{**base_role, "place": {"beside": "guide", "direction": "right"}}]}),
            ]
            for raw_text in invalid_rosters:
                with self.subTest(roster=raw_text[:40]):
                    roster_file.write_text(raw_text)
                    owner = Herdr()
                    with self.assertRaises(ValueError) as ctx:
                        launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
                    self.assertNotIn("leaked-secret", str(ctx.exception))
                    with self.assertRaises(ValueError) as ctx2:
                        launch.launch_team({
                            "space": "sp1",
                            "root": str(root),
                            "swarm": "proj",
                            "agents": [{"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False}],
                        }, owner.app, owner.core, app_home)
                    self.assertNotIn("leaked-secret", str(ctx2.exception))
                    self.assertEqual(owner.commands, [])

    def test_roster_unlinks_claude_mcp_copy_when_tab_new_pane_split_or_target_pane_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home, root = Path(directory) / "herdr", Path(directory) / "project"
            root.mkdir()
            token_file = Path(directory) / "claude.token"
            token_file.write_bytes(b"secret-claude-token-cleanup\n")
            conn = [{"name": "surla", "url": "http://127.0.0.1:43127/mcp", "credential_file": str(token_file)}]
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {"role": "guide", "title": "Guide", "handle": "guide", "folder": str(root), "first_message": "Guide.", "connections": conn, "place": None},
                    {"role": "builder", "title": "Builder", "handle": "builder", "folder": str(root), "first_message": "Build.", "connections": conn, "place": {"beside": "guide", "direction": "right"}},
                    {"role": "reviewer", "title": "Reviewer", "handle": "reviewer", "folder": str(root), "first_message": "Review.", "connections": conn, "place": {"beside": "builder", "direction": "down"}},
                ],
            }))
            mcp_dir = app_home / "data" / "chorus-launcher" / "mcp"
            guide_mcp, builder_mcp, reviewer_mcp = mcp_dir / "sp1--guide.json", mcp_dir / "sp1--builder.json", mcp_dir / "sp1--reviewer.json"
            claude_row = lambda rid: {"id": f"role:{rid}", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False}

            # (1) Failed tab.new removes guide's MCP copy and leaves zero panes.
            owner_tab_fail = Herdr()
            owner_tab_fail.fail_command = "tab.new"
            with self.assertRaisesRegex(RuntimeError, "tab.new"):
                launch.launch_team({"space": "sp1", "root": str(root), "swarm": "proj", "agents": [claude_row("guide"), claude_row("builder"), claude_row("reviewer")]}, owner_tab_fail.app, owner_tab_fail.core, app_home)
            self.assertEqual(owner_tab_fail.terminals, [])
            self.assertFalse(guide_mcp.exists())

            # Open guide and builder first so subsequent split and target-pane failures run against a live team tab.
            owner = Herdr()
            res = launch.launch_team({"space": "sp1", "root": str(root), "swarm": "proj", "agents": [claude_row("guide"), claude_row("builder"), claude_row("reviewer")]}, owner.app, owner.core, app_home)
            self.assertEqual(res, {"tab": "tb1", "created": 3, "reused": 0})
            owner.app(["do", "terminal.kill", json.dumps({"terminal": "t3"})])
            reviewer_mcp.unlink()

            # (2) Failed pane.split when re-opening reviewer removes reviewer's MCP copy and leaves guide and builder untouched.
            owner.fail_command = "pane.split"
            with self.assertRaisesRegex(RuntimeError, "pane.split"):
                launch.launch_team({"space": "sp1", "root": str(root), "swarm": "proj", "agents": [{"id": "role:guide"}, {"id": "role:builder"}, claude_row("reviewer")]}, owner.app, owner.core, app_home)
            self.assertEqual([(t["address"], t["state"]) for t in owner.terminals if t["state"] == "live"], [("t1", "live"), ("t2", "live")])
            self.assertFalse(reviewer_mcp.exists())
            self.assertTrue(guide_mcp.exists())
            self.assertTrue(builder_mcp.exists())

            # (3) Missing target pane at place["beside"] (builder's pane is absent from the tab) removes reviewer's MCP copy and leaves existing terminals untouched.
            owner.fail_command = None
            owner.listing["spaces"][0]["tabs"][0]["terminals"] = [{"pane": "p1", "terminal": "t1"}]
            with self.assertRaisesRegex(RuntimeError, "because its pane is not open"):
                launch.launch_team({"space": "sp1", "root": str(root), "swarm": "proj", "agents": [{"id": "role:guide"}, {"id": "role:builder"}, claude_row("reviewer")]}, owner.app, owner.core, app_home)
            self.assertEqual([(t["address"], t["state"]) for t in owner.terminals if t["state"] == "live"], [("t1", "live"), ("t2", "live")])
            self.assertFalse(reviewer_mcp.exists())
            self.assertTrue(guide_mcp.exists())

    def test_roster_write_claude_mcp_config_unlinks_file_on_close_or_write_failure_after_bytes_written(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home, root = Path(directory) / "herdr", Path(directory) / "project"
            root.mkdir()
            token_file = Path(directory) / "claude.token"
            token_file.write_bytes(b"secret-claude-token-close-fail\n")
            conn = [{"name": "surla", "url": "http://127.0.0.1:43127/mcp", "credential_file": str(token_file)}]
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {"role": "guide", "title": "Guide", "handle": "guide", "folder": str(root), "first_message": "Guide.", "connections": conn, "place": None},
                    {"role": "builder", "title": "Builder", "handle": "builder", "folder": str(root), "first_message": "Build.", "connections": conn, "place": {"beside": "guide", "direction": "right"}},
                ],
            }))
            mcp_dir = app_home / "data" / "chorus-launcher" / "mcp"
            guide_mcp, builder_mcp = mcp_dir / "sp1--guide.json", mcp_dir / "sp1--builder.json"
            claude_row = lambda rid: {"id": f"role:{rid}", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False}
            real_close = os.close

            # (1) First role fails on os.close after writing bytes -> zero panes and no remaining file.
            def close_then_fail_always(fd):
                real_close(fd)
                raise OSError(5, "Input/output error on close")

            owner_first = Herdr()
            with mock.patch("launch.os.close", side_effect=close_then_fail_always):
                with self.assertRaisesRegex(OSError, "Input/output error on close"):
                    launch.launch_team({"space": "sp1", "root": str(root), "swarm": "proj", "agents": [claude_row("guide"), claude_row("builder")]}, owner_first.app, owner_first.core, app_home)
            self.assertEqual(owner_first.terminals, [])
            self.assertFalse(guide_mcp.exists())

            # (2) Second role fails on os.close after writing bytes -> first pane and its MCP file remain untouched; zero new panes and no remaining file for builder.
            def close_then_fail_on_builder(fd):
                real_close(fd)
                if builder_mcp.exists():
                    raise OSError(5, "Input/output error on close")

            owner = Herdr()
            with mock.patch("launch.os.close", side_effect=close_then_fail_on_builder):
                with self.assertRaisesRegex(OSError, "Input/output error on close"):
                    launch.launch_team({"space": "sp1", "root": str(root), "swarm": "proj", "agents": [claude_row("guide"), claude_row("builder")]}, owner.app, owner.core, app_home)
            self.assertEqual([(t["address"], t["state"]) for t in owner.terminals], [("t1", "live")])
            self.assertTrue(guide_mcp.exists())
            self.assertFalse(builder_mcp.exists())

    def test_roster_rejects_crlf_and_padded_credentials_and_delivers_identical_bytes_to_claude_and_codex(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home, root = Path(directory) / "herdr", Path(directory) / "project"
            root.mkdir()
            token_file = Path(directory) / "conn.token"
            conn = [{"name": "surla", "url": "http://127.0.0.1:43127/mcp", "credential_file": str(token_file)}]
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {"role": "guide", "title": "Guide", "handle": "guide", "folder": str(root), "first_message": "Coordinate.", "connections": conn, "place": None},
                    {"role": "builder", "title": "Builder", "handle": "builder", "folder": str(root), "first_message": "Build.", "connections": conn, "place": {"beside": "guide", "direction": "right"}},
                ],
            }))
            mcp_file = app_home / "data" / "chorus-launcher" / "mcp" / "sp1--guide.json"
            for bad_bytes in (b"secret-token\r\n", b"secret-token\r", b" secret-token\n", b"secret-token \n", b" secret-token ", b"secret\ttoken\n", b"secret token\n", b"secret-token\n\n", b"\n", b""):
                with self.subTest(raw=bad_bytes):
                    token_file.write_bytes(bad_bytes)
                    owner = Herdr()
                    with self.assertRaisesRegex(ValueError, "Empty or invalid credential file"):
                        launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
                    with self.assertRaisesRegex(ValueError, "Empty or invalid credential file"):
                        launch.launch_team({
                            "space": "sp1",
                            "root": str(root),
                            "swarm": "proj",
                            "agents": [
                                {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                                {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["codex"], "model": None, "thinking": None, "yolo": False},
                            ],
                        }, owner.app, owner.core, app_home)
                    self.assertEqual(owner.commands, [])
                    self.assertEqual(owner.terminals, [])
                    self.assertFalse(mcp_file.exists())

            expected_token = "exact-shared-secret-token-778899"
            for valid_bytes in (expected_token.encode("utf-8"), (expected_token + "\n").encode("utf-8")):
                with self.subTest(valid=valid_bytes):
                    token_file.write_bytes(valid_bytes)
                    owner = Herdr()
                    launch.launch_team({
                        "space": "sp1",
                        "root": str(root),
                        "swarm": "proj",
                        "agents": [
                            {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                            {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["python3", "-c", "import os,json; print(json.dumps(os.environ.get('CHORUS_MCP_TOKEN_0')))"], "model": None, "thinking": None, "yolo": False},
                        ],
                    }, owner.app, owner.core, app_home)
                    claude_header = json.loads(mcp_file.read_text())["mcpServers"]["surla"]["headers"]["Authorization"]
                    self.assertEqual(claude_header, f"Bearer {expected_token}")
                    codex_argv = [args["argv"] for name, args in owner.commands if name == "pane.split"][0]
                    proc = subprocess.run(codex_argv, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
                    self.assertEqual(claude_header.removeprefix("Bearer "), json.loads(proc.stdout))
                    self.assertEqual(json.loads(proc.stdout), expected_token)

    def test_roster_codex_wrapper_suppresses_shell_xtrace_verbose_and_traps_from_startup_files(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home, root, zdotdir = Path(directory) / "herdr", Path(directory) / "project", Path(directory) / "zdotdir"
            root.mkdir()
            zdotdir.mkdir()
            codex_secret = "secret-codex-xtrace-canary-556677"
            token_file = Path(directory) / "builder.token"
            token_file.write_bytes((codex_secret + "\n").encode("utf-8"))
            startup_body = (
                "setopt XTRACE VERBOSE\n"
                'TRAPDEBUG() { print -u2 "TRAPDEBUG:$ZSH_DEBUG_CMD:${CHORUS_MCP_TOKEN_0:-}"; }\n'
                'trap \'print -u2 "TRAP:$ZSH_DEBUG_CMD:${CHORUS_MCP_TOKEN_0:-}"\' DEBUG EXIT ZERR ERR\n'
                'chpwd() { print -u2 "CHPWD:${CHORUS_MCP_TOKEN_0:-}"; }\n'
            )
            (zdotdir / ".zshenv").write_text(startup_body)
            (zdotdir / ".zshrc").write_text(startup_body)
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {"role": "guide", "title": "Guide", "handle": "guide", "folder": str(root), "first_message": "Guide.", "connections": [], "place": None},
                    {"role": "builder", "title": "Builder", "handle": "builder", "folder": str(root), "first_message": "Build.", "connections": [{"name": "surla", "url": "http://127.0.0.1:43127/mcp", "credential_file": str(token_file)}], "place": {"beside": "guide", "direction": "right"}},
                ],
            }))
            owner = Herdr()
            child_code = (
                "import hashlib, json, os; "
                "val = os.environ.get('CHORUS_MCP_TOKEN_0', ''); "
                "print(json.dumps({'cwd': os.getcwd(), 'sha256': hashlib.sha256(val.encode()).hexdigest(), 'len': len(val)}))"
            )
            launch.launch_team({
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": None, "thinking": None, "yolo": False},
                    {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["python3", "-c", child_code], "model": None, "thinking": None, "yolo": False},
                ],
            }, owner.app, owner.core, app_home)
            codex_argv = [args["argv"] for name, args in owner.commands if name == "pane.split"][0]
            proc = subprocess.run(codex_argv, stdin=subprocess.DEVNULL, capture_output=True, text=True, env={**os.environ, "ZDOTDIR": str(zdotdir)}, check=True)
            self.assertNotIn(codex_secret, proc.stdout)
            self.assertNotIn(codex_secret, proc.stderr)
            child_report = json.loads(proc.stdout.strip().splitlines()[-1])
            self.assertEqual(child_report, {"cwd": str(root.resolve()), "sha256": hashlib.sha256(codex_secret.encode()).hexdigest(), "len": len(codex_secret)})

    def test_no_roster_path_records_byte_identical_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            (app_home / "data" / "chorus-launcher").mkdir(parents=True)
            # Without a roster file, an initial empty-root inspect probe reports roster: False without touching Herdr.
            probe_owner = Herdr()
            self.assertEqual(launch.inspect_team({"space": "sp1", "root": ""}, probe_owner.app, probe_owner.core, app_home), {"roster": False})
            self.assertEqual(probe_owner.commands, [])

            owner_default, payload_default = Herdr(), request()
            owner_with_home, payload_with_home = Herdr(), request()
            res_default = launch.launch_team(payload_default, owner_default.app, owner_default.core, None)
            res_with_home = launch.launch_team(payload_with_home, owner_with_home.app, owner_with_home.core, app_home)
            self.assertEqual(res_default, res_with_home)

            def scrub_uuid(obj):
                return launch.re.sub(r"using handle '[0-9a-f]{32}'", "using handle '<uuid>'", json.dumps(obj, sort_keys=True))

            self.assertEqual(scrub_uuid(owner_default.commands), scrub_uuid(owner_with_home.commands))
            self.assertEqual(owner_default.terminals, owner_with_home.terminals)

    def test_is_running_agent_shared_contract_distinguishes_idle_shells_wrappers_and_agents(self):
        for shell_argv in (
            ["/bin/zsh", "-l"],
            ["/bin/zsh", "-li"],
            ["zsh"],
            ["-zsh"],
            ["/bin/bash", "--login"],
            ["bash"],
            ["/bin/sh"],
            ["-sh"],
            ["fish", "-i"],
            ["dash"],
            ["ksh"],
            ["csh"],
            ["tcsh"],
            ["nu"],
            ["pwsh"],
            ["xonsh"],
            ["elvish"],
        ):
            with self.subTest(idle=shell_argv):
                self.assertFalse(launch.is_running_agent({
                    "state": "live",
                    "foreground": {"pid": 100, "name": Path(shell_argv[0].lstrip("-")).name, "argv": shell_argv, "cwd": "/proj"},
                }))
        with mock.patch.dict(os.environ, {"SHELL": "/opt/custom/myshell"}):
            self.assertFalse(launch.is_running_agent({
                "state": "live",
                "foreground": {"pid": 101, "name": "myshell", "argv": ["/opt/custom/myshell", "-l"], "cwd": "/proj"},
            }))
            self.assertTrue(launch.is_running_agent({
                "state": "live",
                "foreground": {"pid": 101, "name": "myshell", "argv": ["/opt/custom/myshell", "-lic", "claude"], "cwd": "/proj"},
            }))
        for non_running in (
            {"state": "finished", "foreground": {"pid": 100, "name": "claude", "argv": ["claude"], "cwd": "/proj"}},
            {"state": "live", "foreground": None},
            {"state": "live", "foreground": {"pid": 100, "name": "claude", "argv": [], "cwd": "/proj"}},
        ):
            self.assertFalse(launch.is_running_agent(non_running))
        for running_fg in (
            {"pid": 201, "name": "2.1.323", "argv": ["claude", "--dangerously-skip-permissions"], "cwd": "/proj"},
            {"pid": 202, "name": "codex", "argv": ["/opt/homebrew/bin/codex", "--no-daemon"], "cwd": "/proj"},
            {"pid": 203, "name": "agy", "argv": ["agy"], "cwd": "/proj"},
            {"pid": 204, "name": "zsh", "argv": ["/bin/zsh", "-lic", 'cd -- "$1" || exit; shift; "$@"', "/bin/zsh", "/proj", "claude"], "cwd": "/proj"},
            {"pid": 205, "name": "sh", "argv": ["/bin/sh", "-i", "-c", "codex"], "cwd": "/proj"},
        ):
            with self.subTest(running=running_fg["argv"]):
                self.assertTrue(launch.is_running_agent({"state": "live", "foreground": running_fg}))

    def test_roster_idle_shell_pane_is_not_running_removes_mcp_copy_and_restarts_in_place(self):
        with tempfile.TemporaryDirectory() as directory:
            app_home = Path(directory) / "herdr"
            root = Path(directory) / "project"
            root.mkdir()
            guide_token = Path(directory) / "guide.token"
            builder_token = Path(directory) / "builder.token"
            guide_token.write_text("guide-secret-token-111\n")
            builder_token.write_text("builder-secret-token-222\n")
            roster_file = app_home / "data" / "chorus-launcher" / "rosters" / "sp1.json"
            roster_file.parent.mkdir(parents=True)
            roster_file.write_text(json.dumps({
                "root": str(root),
                "swarm": "proj",
                "roles": [
                    {
                        "role": "guide",
                        "title": "Guide",
                        "handle": "guide",
                        "folder": str(root),
                        "first_message": "Lead the team.",
                        "connections": [{"name": "surla", "url": "http://127.0.0.1:43127/mcp", "credential_file": str(guide_token)}],
                        "place": None,
                    },
                    {
                        "role": "builder",
                        "title": "Builder",
                        "handle": "builder",
                        "folder": str(root),
                        "first_message": "Implement the service.",
                        "connections": [{"name": "surla", "url": "http://127.0.0.1:43127/mcp", "credential_file": str(builder_token)}],
                        "place": {"beside": "guide", "direction": "right"},
                    },
                    {
                        "role": "test-writer",
                        "title": "Test writer",
                        "handle": "test-writer",
                        "folder": str(root),
                        "first_message": "Write acceptance tests.",
                        "connections": [],
                        "place": {"beside": "builder", "direction": "down"},
                    },
                ],
            }))
            owner = Herdr()
            initial_payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": "claude-opus-5-5", "thinking": "high", "yolo": False},
                    {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["codex"], "model": "gpt-6.1-sol", "thinking": "xhigh", "yolo": True},
                    {"id": "role:test-writer", "key": "antigravity", "name": "Antigravity", "run": ["agy"], "model": "gemini-4-argon", "thinking": "high", "yolo": False},
                ],
            }
            self.assertEqual(launch.launch_team(initial_payload, owner.app, owner.core, app_home), {"tab": "tb1", "created": 3, "reused": 0})
            self.assertEqual(owner.terminals[0]["foreground"]["name"], "2.1.323")
            self.assertEqual(owner.terminals[0]["foreground"]["argv"][0], "claude")
            self.assertEqual(owner.terminals[1]["foreground"]["argv"][0], "codex")
            self.assertEqual(owner.terminals[2]["foreground"]["argv"][0], "agy")
            guide_mcp = app_home / "data" / "chorus-launcher" / "mcp" / "sp1--guide.json"
            self.assertTrue(guide_mcp.exists())

            # When guide (t1) and builder (t2) exit to idle shells while test-writer (t3) keeps running agy:
            # inspect_team treats guide and builder as not running (terminal: None) and removes guide's MCP credential copy.
            for shell_argv, shell_name in (
                (["/bin/zsh", "-li"], "zsh"),
                (["-zsh"], "zsh"),
                (["/bin/bash", "-l"], "bash"),
                (["/bin/sh"], "sh"),
            ):
                owner.exit_to_shell("t1", shell_argv, shell_name)
                owner.exit_to_shell("t2", ["-zsh"], "zsh")
                inspected = launch.inspect_team({"space": "sp1", "root": ""}, owner.app, owner.core, app_home)
                self.assertEqual([r["terminal"] for r in inspected["rows"]], [None, None, "t3"])
                self.assertEqual([r["key"] for r in inspected["rows"]], [None, None, "antigravity"])
            self.assertFalse(guide_mcp.exists())

            # Even if a stale MCP credential copy is present before launch_team runs, launch_team removes it when the role is not running.
            guide_mcp.write_text('{"stale": true}\n')
            guide_token.write_text("guide-rotated-secret-333\n")
            relaunch_payload = {
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "claude", "name": "Claude Code", "run": ["claude"], "model": "claude-opus-5-5", "thinking": "high", "yolo": False},
                    {"id": "role:builder", "key": "codex", "name": "Codex", "run": ["codex"], "model": "gpt-6.1-sol", "thinking": "xhigh", "yolo": True},
                    {"id": "role:test-writer"},
                ],
            }
            before_cmds = len(owner.commands)
            res = launch.launch_team(relaunch_payload, owner.app, owner.core, app_home)
            self.assertEqual(res, {"tab": "tb1", "created": 2, "reused": 1})
            new_cmds = owner.commands[before_cmds:]
            self.assertEqual(
                [name for name, _ in new_cmds],
                ["tab.select", "pane.split", "pane.swap", "pane.close", "pane.split", "pane.swap", "pane.close"],
            )
            self.assertEqual(new_cmds[1][1]["pane"], "p1")
            self.assertEqual(new_cmds[2], ("pane.swap", {"pane": "p4", "target": "p1"}))
            self.assertEqual(new_cmds[3], ("pane.close", {"pane": "p1"}))
            self.assertEqual(new_cmds[4][1]["pane"], "p2")
            self.assertEqual(new_cmds[5], ("pane.swap", {"pane": "p5", "target": "p2"}))
            self.assertEqual(new_cmds[6], ("pane.close", {"pane": "p2"}))
            self.assertEqual(
                owner.listing["spaces"][0]["tabs"][0]["terminals"],
                [{"pane": "p4", "terminal": "t4"}, {"pane": "p5", "terminal": "t5"}, {"pane": "p3", "terminal": "t3"}],
            )
            self.assertEqual(
                [(t["address"], t["state"]) for t in owner.terminals if t["state"] == "live"],
                [("t3", "live"), ("t4", "live"), ("t5", "live")],
            )
            self.assertTrue(guide_mcp.exists())
            self.assertEqual(
                json.loads(guide_mcp.read_text())["mcpServers"]["surla"]["headers"]["Authorization"],
                "Bearer guide-rotated-secret-333",
            )

            # If guide exits to a shell and restarts with Codex (no Claude MCP file needed), launch_team deletes sp1--guide.json and leaves it absent.
            owner.exit_to_shell("t4", ["/bin/zsh", "-l"], "zsh")
            self.assertTrue(guide_mcp.exists())
            res_codex = launch.launch_team({
                "space": "sp1",
                "root": str(root),
                "swarm": "proj",
                "agents": [
                    {"id": "role:guide", "key": "codex", "name": "Codex", "run": ["codex"], "model": "gpt-6.1-sol", "thinking": "high", "yolo": False},
                    {"id": "role:builder"},
                    {"id": "role:test-writer"},
                ],
            }, owner.app, owner.core, app_home)
            self.assertEqual(res_codex, {"tab": "tb1", "created": 1, "reused": 2})
            self.assertFalse(guide_mcp.exists())
            self.assertEqual(
                owner.listing["spaces"][0]["tabs"][0]["terminals"],
                [{"pane": "p6", "terminal": "t6"}, {"pane": "p5", "terminal": "t5"}, {"pane": "p3", "terminal": "t3"}],
            )


if __name__ == "__main__":
    unittest.main()
