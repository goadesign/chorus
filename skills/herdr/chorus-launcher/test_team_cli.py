"""Prove terminal choices retain the dialog executor's launch and replay contracts."""

import copy
import json
from pathlib import Path
import tempfile
import unittest

import launch
import team_cli
from test_launch import Herdr


class TeamCommandTests(unittest.TestCase):
    """Exercise real roster/config/pane ownership with only Herdr commands simulated."""

    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.home = Path(self.scratch.name)
        token = self.home / "token"
        token.write_text("private-test-token\n")
        self.roster = {"root": str(self.home), "swarm": "example", "roles": [{
            "role": "guide", "title": "Guide", "handle": "guide-example",
            "folder": str(self.home), "first_message": "Guide this project.",
            "connections": [{"name": "surla", "url": "http://127.0.0.1:8097/mcp",
                             "credential_file": str(token)}], "place": None,
        }]}
        roster_file = launch.roster_path(self.home, "sp1")
        roster_file.parent.mkdir(parents=True)
        roster_file.write_text(json.dumps(self.roster))
        self.owner = Herdr()
        self.choice = {"client": "claude", "program": "my-claude", "model": "custom-model",
                       "thinking": "high", "yolo": False}
        self.calls = []

    def execute(self, argv):
        """Return installed help without invoking a model or creating a process."""
        self.calls.append(argv)
        return "--model <model>\n--effort <level> (low, medium, high)\n--dangerously-skip-permissions"

    def start(self, choices):
        """Call the public command adapter against the real existing launch executor."""
        return team_cli.start({"space": "sp1", "choices": choices}, self.owner.app,
                              self.owner.core, self.home, self.execute)

    def test_same_executor_creates_connections_labels_and_reuses_running_roles(self):
        before = team_cli.inspect({"space": "sp1"}, self.owner.app, self.owner.core, self.home)
        self.assertFalse(before["roles"][0]["running"])
        self.assertNotIn("private-test-token", json.dumps(before))
        self.assertEqual(self.start({"guide": self.choice}), {"tab": "tb1", "created": 1, "reused": 0})
        argv = next(args["argv"] for name, args in self.owner.commands if name == "tab.new")
        self.assertEqual(argv[0], "my-claude")
        self.assertIn("--mcp-config", argv)
        config = Path(argv[argv.index("--mcp-config") + 1])
        self.assertIn("private-test-token", config.read_text())
        labels = self.owner.terminals[0]["labels"]
        self.assertEqual(labels["chorus.handle"], "guide-example")
        self.assertEqual(labels["chorus.role"], "guide")
        self.assertNotIn("private-test-token", json.dumps(self.owner.commands))
        self.assertEqual(self.calls[0], launch.split_argv(["my-claude", "--help"], str(self.home)))
        self.assertEqual(self.start({}), {"tab": "tb1", "created": 0, "reused": 1})
        self.assertEqual(self.start({"guide": self.choice})["created"], 0)
        changed = dict(self.choice, model="another-model")
        with self.assertRaisesRegex(RuntimeError, "different launch settings"):
            self.start({"guide": changed})
        self.assertEqual(sum(name == "tab.new" for name, _ in self.owner.commands), 1)

    def test_refusals_create_no_panes(self):
        cases = [({}, "needs an explicit choice"),
                 ({"unknown": self.choice}, "outside"),
                 ({"guide": dict(self.choice, extra=True)}, "requires"),
                 ({"guide": dict(self.choice, program="")}, "literal program"),
                 ({"guide": dict(self.choice, client="cursor")}, "connections"),
                 ({"guide": dict(self.choice, thinking="ultra")}, "unavailable")]
        for choices, message in cases:
            with self.subTest(choices=choices), self.assertRaisesRegex(ValueError, message):
                self.start(choices)
            self.assertEqual(self.owner.commands, [])

        # Single-variant Antigravity models without effort variants reject separate thinking flags.
        no_conn_roster = copy.deepcopy(self.roster)
        no_conn_roster["roles"][0]["connections"] = []
        launch.roster_path(self.home, "sp1").write_text(json.dumps(no_conn_roster))
        agy_exec = lambda argv: (
            "--model --effort (low|medium|high) --dangerously-skip-permissions"
            if argv[-1] == "--help"
            else "gemini-pro-orchestrator\tOrchestrator (Carbon)\n"
        )
        with self.assertRaisesRegex(ValueError, "unavailable"):
            team_cli.start(
                {"space": "sp1", "choices": {"guide": {"client": "antigravity", "program": "agy", "model": "gemini-pro-orchestrator", "thinking": "high", "yolo": False}}},
                self.owner.app,
                self.owner.core,
                self.home,
                agy_exec,
            )
        self.assertEqual(self.owner.commands, [])

    def test_missing_program_and_unadvertised_flags_refuse_before_creation(self):
        def unavailable(_argv):
            raise RuntimeError("command not found")
        with self.assertRaisesRegex(ValueError, "my-claude.*command not found"):
            team_cli.start({"space": "sp1", "choices": {"guide": self.choice}},
                           self.owner.app, self.owner.core, self.home, unavailable)
        self.execute = lambda _argv: "usage only"
        with self.assertRaisesRegex(ValueError, "does not advertise --model"):
            self.start({"guide": self.choice})
        self.assertEqual(self.owner.commands, [])

    def test_partial_launch_retry_reuses_completed_role_and_codex_has_own_identity(self):
        second = copy.deepcopy(self.roster["roles"][0])
        second.update(role="builder", title="Builder", handle="builder-example",
                      place={"beside": "guide-example", "direction": "right"})
        self.roster["roles"].append(second)
        launch.roster_path(self.home, "sp1").write_text(json.dumps(self.roster))
        choices = {"guide": dict(self.choice, thinking=None), "builder": {
            "client": "codex", "program": "codex", "model": None, "thinking": None, "yolo": False}}
        self.owner.fail_agent = "codex"
        with self.assertRaisesRegex(RuntimeError, "label failure"):
            self.start(choices)
        self.owner.fail_agent = None
        self.assertEqual(self.start(choices), {"tab": "tb1", "created": 1, "reused": 1})
        split = [args for name, args in self.owner.commands if name == "pane.split"][-1]
        self.assertIn("--no-daemon", split["argv"])
        self.assertEqual(sum(name == "tab.new" for name, _ in self.owner.commands), 1)


if __name__ == "__main__":
    unittest.main()
