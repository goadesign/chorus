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
