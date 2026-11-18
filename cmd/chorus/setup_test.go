package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	genchorus "github.com/goadesign/chorus/gen/chorus"
	"github.com/goadesign/chorus/skills"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetupHerdrShortcut checks that installing the shortcut preserves existing
// key actions, file permissions, symlinks, and the file on repeated setup.
func TestSetupHerdrShortcut(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%t", symlink), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "keys.json")
			target := path
			if symlink {
				target = filepath.Join(t.TempDir(), "keyboard.json")
				require.NoError(t, os.Symlink(target, path))
			}
			before := "{\n  \"terminal\": {\"ctrl-r\": {\"command\":\"test\",\"args\":{\"number\":9007199254740993}}},\n  \"global\": {\n    \"cmd-t\": \"tab.new\"\n  }\n}\n"
			require.NoError(t, os.WriteFile(target, []byte(before), 0o640))
			var out bytes.Buffer
			require.NoError(t, setupHerdrShortcut(home, &out))
			assert.Contains(t, out.String(), "Added Herdr shortcut cmd-alt-shift-c")
			assert.Contains(t, out.String(), "user-managed")
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			want := strings.Replace(before, "\"tab.new\"\n", "\"tab.new\",\n    \"cmd-alt-shift-c\": \"mod.open:chorus-launcher\"\n", 1)
			assert.Equal(t, want, string(got))
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
			if symlink {
				link, err := os.Readlink(path)
				require.NoError(t, err)
				assert.Equal(t, target, link)
			}
			oldTime := time.Unix(1000000000, 0)
			require.NoError(t, os.Chtimes(target, oldTime, oldTime))
			out.Reset()
			require.NoError(t, setupHerdrShortcut(home, &out))
			assert.Contains(t, out.String(), "already opens Chorus in \"global\"")
			info, err = os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, oldTime, info.ModTime())
			got, err = os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		})
	}
}

// TestAddHerdrShortcut verifies conflict handling across every documented
// section and rejects malformed input without producing replacement bytes.
func TestAddHerdrShortcut(t *testing.T) {
	cases := []struct {
		name    string
		before  string
		want    string
		wantErr bool
	}{
		{name: "empty", before: `{}`, want: `{"global": {"cmd-alt-shift-c": "mod.open:chorus-launcher"}}`},
		{name: "empty global", before: `{"global": { }}`, want: `{"global": {"cmd-alt-shift-c": "mod.open:chorus-launcher" }}`},
		{name: "absent global", before: `{"terminal":{"ctrl-c":"terminal"}}`, want: `{"terminal":{"ctrl-c":"terminal"},"global": {"cmd-alt-shift-c": "mod.open:chorus-launcher"}}`},
		{name: "invalid JSON", before: `{"global":`, wantErr: true},
		{name: "array", before: `[]`, wantErr: true},
		{name: "null", before: `null`, wantErr: true},
		{name: "non-object global", before: `{"global":null}`, wantErr: true},
		{name: "non-object terminal", before: `{"terminal":[]}`, wantErr: true},
		{name: "duplicate global", before: `{"global":{},"global":{}}`, wantErr: true},
		{name: "trailing document", before: `{} {}`, wantErr: true},
		{name: "trailing junk", before: `{} oops`, wantErr: true},
		{name: "empty file", before: " ", wantErr: true},
		{name: "four-space indentation", before: "{\n    \"global\": {\n        \"cmd-t\": \"tab.new\"\n    }\n}", want: "{\n    \"global\": {\n        \"cmd-t\": \"tab.new\",\n        \"cmd-alt-shift-c\": \"mod.open:chorus-launcher\"\n    }\n}"},
		{name: "CRLF tabs", before: "{\r\n\t\"global\": {\r\n\t\t\"cmd-t\": \"tab.new\"\r\n\t}\r\n}", want: "{\r\n\t\"global\": {\r\n\t\t\"cmd-t\": \"tab.new\",\r\n\t\t\"cmd-alt-shift-c\": \"mod.open:chorus-launcher\"\r\n\t}\r\n}"},
	}
	for _, section := range []string{"global", "terminal", "layout:terminals", "mod:editor"} {
		for _, chord := range []string{"cmd-alt-shift-c", "SHIFT-CMD-ALT-C", "cmd-alt-C", "super-alt-shift-c", "win-alt-shift-c", "secondary-alt-shift-c", " cmd-alt-shift-c", "cmd-alt-shift-c cmd-k"} {
			before := fmt.Sprintf(`{%q:{%q:{"command":"other","args":{"id":42}}}}`, section, chord)
			cases = append(cases, struct {
				name, before, want string
				wantErr            bool
			}{name: section + " " + chord, before: before, want: before})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := addHerdrShortcut([]byte(tc.before))
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
			assert.True(t, json.Valid(got))
		})
	}
}

// TestHerdrShortcutOutcomes distinguishes an installed launcher binding from
// another action, including a specific context that overrides a global binding.
func TestHerdrShortcutOutcomes(t *testing.T) {
	cases := []struct{ name, before, notice string }{
		{"already string", `{"global":{"cmd-alt-shift-c":"mod.open:chorus-launcher"}}`, `already opens Chorus in "global"`},
		{"already object", `{"global":{"cmd-alt-shift-c":{"command":"mod.open","args":{"name":"chorus-launcher"}}}}`, `already opens Chorus in "global"`},
		{"taken", `{"terminal":{"cmd-alt-shift-c":"terminal.copy"}}`, `taken by command "terminal.copy" in "terminal"`},
		{"terminal text", `{"global":{"cmd-alt-shift-c":"send:private terminal text"}}`, `taken by command "send" in "global"`},
		{"sequence", `{"global":{"cmd-alt-shift-c cmd-k":"mod.open:chorus-launcher"}}`, `taken by command "mod.open" in "global"`},
		{"specific context", `{"global":{"cmd-alt-shift-c":"mod.open:chorus-launcher"},"terminal":{"cmd-alt-C":"terminal.copy"}}`, `taken by command "terminal.copy" in "terminal"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "keys.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.before), 0o600))
			var out bytes.Buffer
			require.NoError(t, setupHerdrShortcut(home, &out))
			assert.Contains(t, out.String(), tc.notice)
			assert.NotContains(t, out.String(), "private terminal text")
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.before, string(got))
		})
	}
}

// TestHerdrShortcutReadOnly preserves a file whose permissions prohibit edits.
func TestHerdrShortcutReadOnly(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "keys.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o444))
	var out bytes.Buffer
	require.NoError(t, setupHerdrShortcut(home, &out))
	assert.Contains(t, out.String(), "read-only; kept the file unchanged")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(got))
}

// TestSetupHerdrShortcutSkipsMissingAndRejectsInvalid keeps Herdr responsible
// for creating the complete default file and preserves a malformed existing file.
func TestSetupHerdrShortcutSkipsMissingAndRejectsInvalid(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "keys.json")
	var out bytes.Buffer
	require.NoError(t, setupHerdrShortcut(home, &out))
	assert.NoFileExists(t, path)
	assert.Contains(t, out.String(), "open Herdr and rerun chorus setup -global")
	before := []byte(`{"global":[]}`)
	require.NoError(t, os.WriteFile(path, before, 0o600))
	err := setupHerdrShortcut(home, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, got)
}

// TestSetupInstallsHerdrLauncher checks detection, repeated updates, and
// preservation of user preferences and unrelated files.
// No Herdr process or real user configuration is used by these tests.
func TestSetupInstallsHerdrLauncher(t *testing.T) {
	cases := []struct {
		// name describes the installation state supplied to global setup.
		name string
		// directory marks an existing default Herdr configuration directory.
		directory bool
		// custom selects an explicit HERDR_APP_HOME.
		custom bool
		// want says whether the launcher must be installed.
		want bool
	}{
		{name: "absent"},
		{name: "existing directory", directory: true, want: true},
		{name: "explicit home", custom: true, directory: true, want: true},
		{name: "missing explicit home", custom: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("HERDR_APP_HOME", "")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("COPILOT_HOME", "")
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			herdrHome := filepath.Join(home, ".config", "herdr-app")
			if tc.custom {
				require.NoError(t, os.MkdirAll(herdrHome, 0o755))
				herdrHome = filepath.Join(t.TempDir(), "custom-herdr")
				t.Setenv("HERDR_APP_HOME", herdrHome)
			}
			if tc.directory {
				require.NoError(t, os.MkdirAll(herdrHome, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(herdrHome, "keys.json"), []byte(`{"global":{"cmd-alt-shift-c":"tab.new"}}`), 0o600))
			}
			mod := filepath.Join(herdrHome, "mods", "chorus-launcher")
			unchangedTime := time.Unix(1000000000, 0)
			for attempt := range 2 {
				installedDir, err := setupHerdrLauncher(home, io.Discard)
				require.NoError(t, err)
				if !tc.want {
					assert.NoDirExists(t, mod)
					assert.Empty(t, installedDir)
					assert.NoDirExists(t, filepath.Join(home, ".config", "herdr-app", "mods"))
					return
				}
				assert.Equal(t, mod, installedDir)
				for _, name := range []string{"mod.json", "main.js", "launch.py", "team_cli.py"} {
					target := filepath.Join(mod, name)
					want, err := skills.HerdrLauncher.ReadFile("herdr/chorus-launcher/" + name)
					require.NoError(t, err)
					got, err := os.ReadFile(target)
					require.NoError(t, err)
					assert.Equal(t, want, got)
				}
				assert.NoFileExists(t, filepath.Join(mod, "test_launch.py"))
				assert.NoFileExists(t, filepath.Join(mod, "test_ui.mjs"))
				assert.NoFileExists(t, filepath.Join(mod, "README.md"))
				if attempt == 0 {
					// User settings and extra files survive the next installation.
					require.NoError(t, os.WriteFile(filepath.Join(mod, "notes.txt"), []byte("keep notes"), 0o600))
					require.NoError(t, os.WriteFile(filepath.Join(herdrHome, "shared.json"), []byte(`{"chorus-launcher.repo:/repo":{"swarm":"saved"}}`), 0o600))
					require.NoError(t, os.WriteFile(filepath.Join(mod, "main.js"), []byte("old installed version"), 0o644))
					require.NoError(t, os.Chtimes(filepath.Join(mod, "launch.py"), unchangedTime, unchangedTime))
				}
			}
			if tc.custom {
				assert.NoDirExists(t, filepath.Join(home, ".config", "herdr-app", "mods"))
			}
			if tc.directory {
				keys, err := os.ReadFile(filepath.Join(herdrHome, "keys.json"))
				require.NoError(t, err)
				assert.Equal(t, `{"global":{"cmd-alt-shift-c":"tab.new"}}`, string(keys))
			}
			info, err := os.Stat(filepath.Join(mod, "launch.py"))
			require.NoError(t, err)
			assert.Equal(t, unchangedTime, info.ModTime(), "unchanged files must not trigger a Herdr reload")
			shared, err := os.ReadFile(filepath.Join(herdrHome, "shared.json"))
			require.NoError(t, err)
			assert.Equal(t, `{"chorus-launcher.repo:/repo":{"swarm":"saved"}}`, string(shared))
			notes, err := os.ReadFile(filepath.Join(mod, "notes.txt"))
			require.NoError(t, err)
			assert.Equal(t, "keep notes", string(notes))
		})
	}
}

// TestSetupHerdrLauncherReportsInspectionError distinguishes a missing app
// home from an unreadable one instead of claiming an unsuccessful install worked.
func TestSetupHerdrLauncherReportsInspectionError(t *testing.T) {
	home := t.TempDir()
	blocked := filepath.Join(home, "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("blocked"), 0o600))
	herdrHome := filepath.Join(blocked, "herdr-app")
	t.Setenv("HERDR_APP_HOME", herdrHome)
	modDir, err := setupHerdrLauncher(home, io.Discard)
	require.Error(t, err)
	assert.Empty(t, modDir)
	assert.Contains(t, err.Error(), "inspect Herdr configuration "+herdrHome)
}

// TestHerdrLauncherBundle verifies that source runtime files are included in
// the installation; contributor tests and documentation stay in the repository.
func TestHerdrLauncherBundle(t *testing.T) {
	const source = "herdr/chorus-launcher"
	entries, err := os.ReadDir(filepath.Join("..", "..", "skills", source))
	require.NoError(t, err)
	home := t.TempDir()
	t.Setenv("HERDR_APP_HOME", home)
	modDir, err := setupHerdrLauncher(home, io.Discard)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "test_") || entry.Name() == "README.md" {
			continue
		}
		bundled, err := skills.HerdrLauncher.ReadFile(source + "/" + entry.Name())
		require.NoError(t, err, "source runtime file %s must be bundled", entry.Name())
		installed, err := os.ReadFile(filepath.Join(modDir, entry.Name()))
		require.NoError(t, err, "bundled runtime file %s must be installed", entry.Name())
		assert.Equal(t, bundled, installed)
	}
}

// TestRunSetupInstallsHerdrOnlyGlobally checks that project setup only changes
// project files, even when the project has the same name as the Herdr mod.
func TestRunSetupInstallsHerdrOnlyGlobally(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%t", global), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("HERDR_APP_HOME", "")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("COPILOT_HOME", "")
			t.Setenv("PATH", t.TempDir())
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
			require.NoError(t, saveServeArgs(home, serverConfig{addr: ":9550"}))
			project := filepath.Join(t.TempDir(), "chorus-launcher")
			require.NoError(t, os.MkdirAll(project, 0o755))
			t.Chdir(project)
			herdrHome := filepath.Join(home, ".config", "herdr-app")
			require.NoError(t, os.MkdirAll(herdrHome, 0o755))
			var args []string
			if global {
				args = []string{"-global"}
			}
			var out bytes.Buffer
			require.NoError(t, runSetup(args, &out))
			if global {
				assert.Contains(t, out.String(), "Installed Chorus launcher in "+filepath.Join(herdrHome, "mods", "chorus-launcher"))
				assert.Contains(t, out.String(), "herdr-app do mod.open --name chorus-launcher")
			} else {
				assert.Contains(t, out.String(), "Registered Chorus MCP (http://localhost:9550/mcp) in "+filepath.Join(project, ".mcp.json"))
				assert.NotContains(t, out.String(), "Installed Chorus launcher")
				assert.NoDirExists(t, filepath.Join(herdrHome, "mods"))
			}
		})
	}
}

// TestMergeCodexConfigFile verifies that registering Chorus in a Codex
// config.toml changes only mcp_servers.chorus.url: comments, other servers,
// quoted and dotted key spellings, and multi-line values are left as they
// were, and entries it cannot edit in place are refused without writing.
func TestMergeCodexConfigFile(t *testing.T) {
	const url = "http://localhost:9440/mcp"
	cases := []struct {
		name    string
		before  string
		want    string
		wantErr string
	}{
		{name: "missing file", want: "[mcp_servers.chorus]\nurl = \"" + url + "\"\n"},
		{
			name:   "commented headers keep other servers",
			before: "[mcp_servers.chorus] # chorus\nurl = \"http://old/mcp\"\n[mcp_servers.other] # comment\nurl = \"http://other/mcp\"\n",
			want:   "[mcp_servers.chorus] # chorus\nurl = \"" + url + "\"\n[mcp_servers.other] # comment\nurl = \"http://other/mcp\"\n",
		},
		{
			name:   "quoted and spaced header with comment after url",
			before: "model = \"o3\"\n[ mcp_servers . \"chorus\" ]\nurl = 'http://old/mcp' # keep\nenabled = true\n",
			want:   "model = \"o3\"\n[ mcp_servers . \"chorus\" ]\nurl = \"" + url + "\" # keep\nenabled = true\n",
		},
		{
			name:   "header without url",
			before: "[mcp_servers.chorus]\nenabled = true\n",
			want:   "[mcp_servers.chorus]\nurl = \"" + url + "\"\nenabled = true\n",
		},
		{
			name:   "dotted url under mcp_servers",
			before: "[mcp_servers]\nchorus.url = \"http://old/mcp\"\nother.url = \"http://other/mcp\"\n",
			want:   "[mcp_servers]\nchorus.url = \"" + url + "\"\nother.url = \"http://other/mcp\"\n",
		},
		{
			name:   "header-like lines inside strings and arrays are values",
			before: "notes = \"\"\"\n[mcp_servers.chorus]\nurl = \"x\"\n\"\"\"\nargs = [\n  [1, 2],\n]\n",
			want:   "notes = \"\"\"\n[mcp_servers.chorus]\nurl = \"x\"\n\"\"\"\nargs = [\n  [1, 2],\n]\n\n[mcp_servers.chorus]\nurl = \"" + url + "\"\n",
		},
		{
			name:   "nested tables under the entry are kept",
			before: "[mcp_servers.chorus]\nurl = \"http://old/mcp\"\n\n[mcp_servers.chorus.http_headers]\nX-Example = \"1\"\n",
			want:   "[mcp_servers.chorus]\nurl = \"" + url + "\"\n\n[mcp_servers.chorus.http_headers]\nX-Example = \"1\"\n",
		},
		{
			name:   "nested table before the entry header",
			before: "[mcp_servers.chorus.http_headers]\nX-Example = \"1\"\n",
			want:   "[mcp_servers.chorus.http_headers]\nX-Example = \"1\"\n\n[mcp_servers.chorus]\nurl = \"" + url + "\"\n",
		},
		{name: "duplicate url", before: "[mcp_servers.chorus]\nurl = \"a\"\nurl = \"b\"\n", wantErr: "not valid TOML"},
		{name: "duplicate table", before: "[mcp_servers.chorus]\nurl = \"a\"\n[mcp_servers.chorus]\nenabled = true\n", wantErr: "not valid TOML"},
		{name: "conflicting types", before: "mcp_servers = 1\n[mcp_servers.chorus]\nurl = \"a\"\n", wantErr: "not valid TOML"},
		{name: "inline table", before: "[mcp_servers]\nchorus = { url = \"http://old/mcp\" }\n", wantErr: "inline table"},
		{name: "inline mcp_servers", before: "mcp_servers = { other = { url = \"x\" } }\n", wantErr: "inline table"},
		{name: "dotted keys without url", before: "[mcp_servers]\nchorus.command = \"chorus\"\n", wantErr: "dotted keys"},
		{name: "array of tables", before: "[[mcp_servers.chorus]]\nurl = \"x\"\n", wantErr: "array of tables"},
		{name: "invalid TOML", before: "[mcp_servers.chorus\n", wantErr: "not valid TOML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.before != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.before), 0o600))
			}
			err := mergeCodexConfigFile(path, url)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Contains(t, err.Error(), "by hand")
				raw, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, tc.before, string(raw))
				return
			}
			require.NoError(t, err)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(raw))

			var doc struct {
				MCPServers map[string]struct {
					URL string `toml:"url"`
				} `toml:"mcp_servers"`
			}
			require.NoError(t, toml.Unmarshal(raw, &doc))
			assert.Equal(t, url, doc.MCPServers["chorus"].URL)

			require.NoError(t, mergeCodexConfigFile(path, url))
			again, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(raw), string(again), "second run must not change the file")
		})
	}
}

// TestMergeJSONMCPConfigFile verifies that registering Chorus in a JSON MCP
// configuration keeps unrelated keys and servers across both "mcpServers" and
// "servers" roots, starts missing or empty files from an empty object, and
// refuses, without writing, files whose root or servers map is not a JSON object.
func TestMergeJSONMCPConfigFile(t *testing.T) {
	const url = "http://localhost:9440/mcp"
	cases := []struct {
		name       string
		serversKey string
		entry      map[string]any
		before     string
		want       string
		wantErr    string
	}{
		{name: "missing file", want: "{\n  \"mcpServers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    }\n  }\n}\n"},
		{name: "empty file", before: " \n", want: "{\n  \"mcpServers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    }\n  }\n}\n"},
		{
			name:   "keeps other keys and servers",
			before: `{"theme":"dark","mcpServers":{"other":{"type":"http","url":"http://other/mcp"}}}`,
			want:   "{\n  \"mcpServers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    },\n    \"other\": {\n      \"type\": \"http\",\n      \"url\": \"http://other/mcp\"\n    }\n  },\n  \"theme\": \"dark\"\n}\n",
		},
		{
			name:       "vscode servers root",
			serversKey: "servers",
			before:     `{"inputs":[],"servers":{"other":{"type":"http","url":"http://other/mcp"}}}`,
			want:       "{\n  \"inputs\": [],\n  \"servers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    },\n    \"other\": {\n      \"type\": \"http\",\n      \"url\": \"http://other/mcp\"\n    }\n  }\n}\n",
		},
		{name: "null root", before: "null", wantErr: "must contain a JSON object"},
		{name: "array root", before: "[]", wantErr: "parse JSON"},
		{name: "null mcpServers", before: `{"mcpServers":null}`, wantErr: "mcpServers must be a JSON object"},
		{name: "array mcpServers", before: `{"mcpServers":[]}`, wantErr: "mcpServers must be a JSON object"},
		{name: "string mcpServers", before: `{"mcpServers":"x"}`, wantErr: "mcpServers must be a JSON object"},
		{name: "null vscode servers", serversKey: "servers", before: `{"servers":null}`, wantErr: "servers must be a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if tc.before != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.before), 0o600))
			}
			entry := tc.entry
			if entry == nil {
				entry = map[string]any{"type": "http", "url": url}
			}
			err := mergeJSONMCPConfigFile(jsonMCPTarget{
				path:       path,
				serversKey: tc.serversKey,
				entry:      entry,
			})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Contains(t, err.Error(), path)
				raw, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, tc.before, string(raw))
				return
			}
			require.NoError(t, err)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(raw))
		})
	}
}

// TestSetupRegistersClaudeCode verifies when global setup registers Claude
// Code: an installed claude executable alone (a Claude Code never started),
// an existing ~/.claude directory without .claude.json, and a custom
// CLAUDE_CONFIG_DIR all get the MCP entry and skill in the right place; a
// second run with a new URL updates the same entry; and with no executable
// and no Claude Code files nothing is written for it.
func TestSetupRegistersClaudeCode(t *testing.T) {
	cases := []struct {
		name       string
		executable bool
		claudeDir  bool
		customDir  bool
		want       bool
	}{
		{name: "executable only", executable: true, want: true},
		{name: "directory without config", claudeDir: true, want: true},
		{name: "custom config dir", customDir: true, want: true},
		{name: "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			if tc.executable {
				require.NoError(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
			}
			root := filepath.Join(home, ".claude")
			config := filepath.Join(home, ".claude.json")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("COPILOT_HOME", "")
			if tc.customDir {
				root = filepath.Join(t.TempDir(), "claude-config")
				require.NoError(t, os.MkdirAll(root, 0o755))
				config = filepath.Join(root, ".claude.json")
				t.Setenv("CLAUDE_CONFIG_DIR", root)
			}
			if tc.claudeDir {
				require.NoError(t, os.MkdirAll(root, 0o755))
			}
			skill := filepath.Join(root, "skills", "chorus", "SKILL.md")

			for _, url := range []string{"http://localhost:9440/mcp", "http://localhost:9550/mcp"} {
				updated, err := setupMCPConfigs("", home, url, true)
				require.NoError(t, err)
				if !tc.want {
					assert.NotContains(t, updated, config)
					assert.NoFileExists(t, config)
					assert.NoFileExists(t, skill)
					return
				}
				assert.Contains(t, updated, config)
				assert.Contains(t, updated, skill)
				raw, err := os.ReadFile(config)
				require.NoError(t, err)
				assert.JSONEq(t, `{"mcpServers":{"chorus":{"type":"http","url":"`+url+`"}}}`, string(raw))
				assert.FileExists(t, skill)
			}
			if tc.customDir {
				assert.NoFileExists(t, filepath.Join(home, ".claude.json"))
				assert.NoDirExists(t, filepath.Join(home, ".claude"))
			}
		})
	}
}

// TestSetupRegistersAdditionalAgents verifies client-specific MCP transport keys
// and skill locations across Gemini CLI (httpUrl), Windsurf (serverUrl), GitHub
// Copilot CLI (mcpServers), and Visual Studio Code (servers), both globally and
// in project-scoped directories.
func TestSetupRegistersAdditionalAgents(t *testing.T) {
	const url = "http://localhost:9440/mcp"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COPILOT_HOME", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)

	// Executable-only detection for Gemini CLI and GitHub Copilot CLI.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "gemini"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "copilot"), []byte("#!/bin/sh\nexit 0\n"), 0o755))

	// Pre-populate Windsurf and VS Code global directories with unrelated settings.
	windsurfDir := filepath.Join(home, ".codeium", "windsurf")
	require.NoError(t, os.MkdirAll(windsurfDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(windsurfDir, "mcp_config.json"),
		[]byte(`{"mcpServers":{"other":{"serverUrl":"https://other.example/mcp"}}}`),
		0o600,
	))

	vscodeDir := vscodeUserDir(home)
	if vscodeDir != "" {
		require.NoError(t, os.MkdirAll(vscodeDir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(vscodeDir, "mcp.json"),
			[]byte(`{"inputs":[],"servers":{"other":{"type":"http","url":"https://other.example/mcp"}}}`),
			0o600,
		))
	}

	updated, err := setupMCPConfigs("", home, url, true)
	require.NoError(t, err)

	// Gemini CLI must receive "httpUrl" (Streamable HTTP), never SSE "url".
	geminiCfg := filepath.Join(home, ".gemini", "settings.json")
	geminiSkill := filepath.Join(home, ".gemini", "skills", "chorus", "SKILL.md")
	assert.Contains(t, updated, geminiCfg)
	assert.Contains(t, updated, geminiSkill)
	rawGemini, err := os.ReadFile(geminiCfg)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"chorus":{"httpUrl":"`+url+`"}}}`, string(rawGemini))
	assert.FileExists(t, geminiSkill)

	// Windsurf must receive "serverUrl" while keeping existing servers.
	windsurfCfg := filepath.Join(windsurfDir, "mcp_config.json")
	windsurfSkill := filepath.Join(windsurfDir, "skills", "chorus", "SKILL.md")
	assert.Contains(t, updated, windsurfCfg)
	assert.Contains(t, updated, windsurfSkill)
	rawWindsurf, err := os.ReadFile(windsurfCfg)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"chorus":{"serverUrl":"`+url+`"},"other":{"serverUrl":"https://other.example/mcp"}}}`, string(rawWindsurf))
	assert.FileExists(t, windsurfSkill)

	// GitHub Copilot CLI receives standard mcpServers with required tools wildcard + skill.
	copilotCfg := filepath.Join(home, ".copilot", "mcp-config.json")
	copilotSkill := filepath.Join(home, ".copilot", "skills", "chorus", "SKILL.md")
	assert.Contains(t, updated, copilotCfg)
	assert.Contains(t, updated, copilotSkill)
	rawCopilot, err := os.ReadFile(copilotCfg)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"chorus":{"type":"http","url":"`+url+`","tools":["*"]}}}`, string(rawCopilot))
	assert.FileExists(t, copilotSkill)

	// Directory-only detection (no executables on PATH) and custom COPILOT_HOME.
	dirHome := t.TempDir()
	t.Setenv("HOME", dirHome)
	t.Setenv("PATH", t.TempDir())
	customCopilot := filepath.Join(t.TempDir(), "custom-copilot")
	t.Setenv("COPILOT_HOME", customCopilot)
	require.NoError(t, os.MkdirAll(customCopilot, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dirHome, ".gemini", "skills"), 0o755))
	dirUpdated, err := setupMCPConfigs("", dirHome, url, true)
	require.NoError(t, err)
	assert.Contains(t, dirUpdated, filepath.Join(dirHome, ".gemini", "settings.json"))
	assert.Contains(t, dirUpdated, filepath.Join(customCopilot, "mcp-config.json"))
	assert.NoDirExists(t, filepath.Join(dirHome, ".copilot"))
	t.Setenv("COPILOT_HOME", "")

	// Malformed existing Gemini CLI settings.json fails closed without overwriting.
	badHome := t.TempDir()
	t.Setenv("HOME", badHome)
	require.NoError(t, os.MkdirAll(filepath.Join(badHome, ".gemini"), 0o755))
	badSettings := filepath.Join(badHome, ".gemini", "settings.json")
	require.NoError(t, os.WriteFile(badSettings, []byte(`{"mcpServers":[]}`), 0o600))
	_, err = setupMCPConfigs("", badHome, url, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mcpServers must be a JSON object")

	// VS Code receives top-level "servers" while keeping "inputs" and peer servers.
	if vscodeDir != "" {
		vscodeCfg := filepath.Join(vscodeDir, "mcp.json")
		assert.Contains(t, updated, vscodeCfg)
		rawVSCode, readErr := os.ReadFile(vscodeCfg)
		require.NoError(t, readErr)
		assert.JSONEq(t, `{"inputs":[],"servers":{"chorus":{"type":"http","url":"`+url+`"},"other":{"type":"http","url":"https://other.example/mcp"}}}`, string(rawVSCode))
	}

	// Project-local setup updates .gemini/settings.json and .vscode/mcp.json when those directories exist.
	proj := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(proj, ".gemini"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(proj, ".vscode"), 0o755))
	projUpdated, err := setupMCPConfigs(proj, home, url, false)
	require.NoError(t, err)
	assert.Contains(t, projUpdated, filepath.Join(proj, ".gemini", "settings.json"))
	assert.Contains(t, projUpdated, filepath.Join(proj, ".vscode", "mcp.json"))
	rawProjGemini, err := os.ReadFile(filepath.Join(proj, ".gemini", "settings.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"chorus":{"httpUrl":"`+url+`"}}}`, string(rawProjGemini))
	rawProjVSCode, err := os.ReadFile(filepath.Join(proj, ".vscode", "mcp.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"servers":{"chorus":{"type":"http","url":"`+url+`"}}}`, string(rawProjVSCode))
}

// TestRepositoryDistributionManifests verifies that the published plugin,
// marketplace, MCP Registry, and npm package manifests parse cleanly, point to
// existing repository assets without duplicating SKILL.md, and carry versions
// matching the generated API version.
func TestRepositoryDistributionManifests(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	versionedFiles := []string{
		"plugin.json",
		filepath.Join(".claude-plugin", "plugin.json"),
		filepath.Join("npm", "chorus", "package.json"),
		"server.json",
	}
	for _, rel := range versionedFiles {
		raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
		require.NoError(t, err, "read %s", rel)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc), "parse %s", rel)
		assert.Equal(t, genchorus.APIVersion, doc["version"], "%s version must match APIVersion", rel)
	}

	// Verify Antigravity plugin.json logo resolves to an existing SVG.
	pluginRaw, err := os.ReadFile(filepath.Join(repoRoot, "plugin.json"))
	require.NoError(t, err)
	var pluginDoc struct {
		Logo string `json:"logo"`
	}
	require.NoError(t, json.Unmarshal(pluginRaw, &pluginDoc))
	assert.FileExists(t, filepath.Join(repoRoot, pluginDoc.Logo))

	// Verify Claude Code marketplace.json points at "./" where skills/chorus/SKILL.md and .mcp.json live.
	marketRaw, err := os.ReadFile(filepath.Join(repoRoot, ".claude-plugin", "marketplace.json"))
	require.NoError(t, err)
	var marketDoc struct {
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Source  string `json:"source"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(marketRaw, &marketDoc))
	require.Len(t, marketDoc.Plugins, 1)
	assert.Equal(t, "chorus", marketDoc.Plugins[0].Name)
	assert.Equal(t, genchorus.APIVersion, marketDoc.Plugins[0].Version)
	assert.Equal(t, "./", marketDoc.Plugins[0].Source)
	assert.FileExists(t, filepath.Join(repoRoot, ".mcp.json"))
	assert.FileExists(t, filepath.Join(repoRoot, "mcp_config.json"))
	assert.FileExists(t, filepath.Join(repoRoot, "skills", "chorus", "SKILL.md"))

	// Verify server.json schema invariants (description <= 100 chars, npm package version & mcpName match).
	serverRaw, err := os.ReadFile(filepath.Join(repoRoot, "server.json"))
	require.NoError(t, err)
	var serverDoc struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Packages    []struct {
			Identifier string `json:"identifier"`
			Version    string `json:"version"`
		} `json:"packages"`
	}
	require.NoError(t, json.Unmarshal(serverRaw, &serverDoc))
	assert.LessOrEqual(t, len(serverDoc.Description), 100, "MCP Registry schema requires description <= 100 chars")
	require.Len(t, serverDoc.Packages, 1)
	assert.Equal(t, "@goadesign/chorus", serverDoc.Packages[0].Identifier)
	assert.Equal(t, genchorus.APIVersion, serverDoc.Packages[0].Version)

	npmRaw, err := os.ReadFile(filepath.Join(repoRoot, "npm", "chorus", "package.json"))
	require.NoError(t, err)
	var npmDoc struct {
		MCPName string `json:"mcpName"`
	}
	require.NoError(t, json.Unmarshal(npmRaw, &npmDoc))
	assert.Equal(t, serverDoc.Name, npmDoc.MCPName)
}
