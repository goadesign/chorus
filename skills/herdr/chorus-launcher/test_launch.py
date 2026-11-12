"""Exercise project targeting, repeat launches, thinking options, and recovery without paid agents."""

import copy
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

import launch


class Herdr:
    """Model only the native commands used by the launcher, retaining call evidence."""

    def __init__(self):
        self.listing = {"spaces": [{"id": "sp1", "tabs": []}]}
        self.terminals = []
        self.commands = []
        self.fail_agent = None

    def app(self, argv):
        if argv == ["ls"]:
            return copy.deepcopy(self.listing)
        command, args = argv[1], json.loads(argv[2])
        self.commands.append((command, args))
        if command == "tab.select":
            return {"ok": True}
        if command == "terminal.kill":
            for space in self.listing["spaces"]:
                for tab in space["tabs"]:
                    tab["terminals"] = [p for p in tab["terminals"] if p["terminal"] != args["terminal"]]
            next(t for t in self.terminals if t["address"] == args["terminal"])["state"] = "exited"
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
        self.terminals.append({"address": terminal, "state": "live", "labels": {"herdr.space": space["id"], "herdr.tab": tab["id"]}})
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

            # If the first role (place: null) exits while the team tab remains open, restarting it splits in the existing tab.
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
            self.assertEqual(owner.commands[-1][0], "pane.split")
            self.assertEqual(owner.commands[-1][1]["pane"], "p2")
            self.assertEqual(owner.commands[-1][1]["direction"], "right")

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


if __name__ == "__main__":
    unittest.main()
