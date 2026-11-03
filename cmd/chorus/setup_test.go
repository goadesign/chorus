package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestMergeMCPConfigFile verifies that registering Chorus in a JSON MCP
// configuration keeps unrelated keys and servers, starts missing or empty
// files from an empty object, and refuses, without writing, files whose root
// or mcpServers is not a JSON object.
func TestMergeMCPConfigFile(t *testing.T) {
	const url = "http://localhost:9440/mcp"
	cases := []struct {
		name    string
		before  string
		want    string
		wantErr string
	}{
		{name: "missing file", want: "{\n  \"mcpServers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    }\n  }\n}\n"},
		{name: "empty file", before: " \n", want: "{\n  \"mcpServers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    }\n  }\n}\n"},
		{
			name:   "keeps other keys and servers",
			before: `{"theme":"dark","mcpServers":{"other":{"type":"http","url":"http://other/mcp"}}}`,
			want:   "{\n  \"mcpServers\": {\n    \"chorus\": {\n      \"type\": \"http\",\n      \"url\": \"" + url + "\"\n    },\n    \"other\": {\n      \"type\": \"http\",\n      \"url\": \"http://other/mcp\"\n    }\n  },\n  \"theme\": \"dark\"\n}\n",
		},
		{name: "null root", before: "null", wantErr: "must contain a JSON object"},
		{name: "array root", before: "[]", wantErr: "parse JSON"},
		{name: "null mcpServers", before: `{"mcpServers":null}`, wantErr: "mcpServers must be a JSON object"},
		{name: "array mcpServers", before: `{"mcpServers":[]}`, wantErr: "mcpServers must be a JSON object"},
		{name: "string mcpServers", before: `{"mcpServers":"x"}`, wantErr: "mcpServers must be a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if tc.before != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.before), 0o600))
			}
			err := mergeMCPConfigFile(path, url)
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
