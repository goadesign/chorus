// This file implements "chorus setup": it registers the Chorus MCP endpoint
// in project or global coding-agent configuration files and installs the
// embedded Chorus Agent Skill (SKILL.md) next to them. "chorus up" runs the same
// global setup before it starts the server.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/goadesign/chorus/skills"
)

// codexURLEdit says where mergeCodexConfigFile writes mcp_servers.chorus.url
// in a Codex config.toml document. At most one field is set; neither set means
// the document has no Chorus entry and a new table is appended.
type codexURLEdit struct {
	// value is the byte range of the existing url string, quotes included.
	value unstable.Range
	// afterHeader is the byte offset just past the [mcp_servers.chorus]
	// header line, used when that table has no url yet.
	afterHeader int
}

// runSetup registers the Chorus MCP server URL in project and/or global MCP
// configuration files and installs the Chorus Agent Skill (SKILL.md).
func runSetup(args []string) error {
	var (
		addr   string
		global bool
	)
	fs := flag.NewFlagSet("chorus setup", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	fs.BoolVar(&global, "global", false, "Register Chorus in global coding-agent MCP configuration files")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mcpURL := localBaseURL(addr) + "/mcp"
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}

	updated, err := setupMCPConfigs(cwd, homeDir, mcpURL, global)
	if err != nil {
		return err
	}
	for _, p := range updated {
		if strings.HasSuffix(p, ".md") {
			fmt.Printf("Installed Chorus skill in %s\n", p)
		} else {
			fmt.Printf("Registered Chorus MCP (%s) in %s\n", mcpURL, p)
		}
	}
	return nil
}

// setupMCPConfigs merges the Chorus MCP server entry into the project's
// .mcp.json (and .codex/config.toml if .codex exists) and installs
// .agents/skills/chorus/SKILL.md when global is false, or updates detected
// global agent MCP config files and skill directories under homeDir (Universal
// .agents, Cursor, Antigravity, Claude Code, and OpenAI Codex) when global is
// true.
func setupMCPConfigs(cwd, homeDir, mcpURL string, global bool) ([]string, error) {
	var (
		jsonTargets  []string
		tomlTargets  []string
		skillTargets []string
	)
	if !global {
		jsonTargets = append(jsonTargets, filepath.Join(cwd, ".mcp.json"))
		if info, err := os.Stat(filepath.Join(cwd, ".codex")); err == nil && info.IsDir() {
			tomlTargets = append(tomlTargets, filepath.Join(cwd, ".codex", "config.toml"))
		}
		skillTargets = append(skillTargets, filepath.Join(cwd, ".agents", "skills", "chorus", "SKILL.md"))
	} else {
		skillTargets = append(skillTargets, filepath.Join(homeDir, ".agents", "skills", "chorus", "SKILL.md"))
		candidates := []struct {
			parentDir string
			config    string
			skill     string
		}{
			{
				parentDir: filepath.Join(homeDir, ".cursor"),
				config:    filepath.Join(homeDir, ".cursor", "mcp.json"),
				skill:     filepath.Join(homeDir, ".cursor", "skills", "chorus", "SKILL.md"),
			},
			{
				parentDir: filepath.Join(homeDir, ".gemini", "config"),
				config:    filepath.Join(homeDir, ".gemini", "config", "mcp_config.json"),
				skill:     filepath.Join(homeDir, ".gemini", "config", "skills", "chorus", "SKILL.md"),
			},
		}
		for _, c := range candidates {
			if info, err := os.Stat(c.parentDir); err == nil && info.IsDir() {
				jsonTargets = append(jsonTargets, c.config)
				skillTargets = append(skillTargets, c.skill)
			}
		}
		claudeConfig := filepath.Join(homeDir, ".claude.json")
		if _, err := os.Stat(claudeConfig); err == nil {
			jsonTargets = append(jsonTargets, claudeConfig)
		}
		claudeDir := filepath.Join(homeDir, ".claude")
		if info, err := os.Stat(claudeDir); err == nil && info.IsDir() {
			skillTargets = append(skillTargets, filepath.Join(claudeDir, "skills", "chorus", "SKILL.md"))
		}
		codexDir := filepath.Join(homeDir, ".codex")
		if info, err := os.Stat(codexDir); err == nil && info.IsDir() {
			tomlTargets = append(tomlTargets, filepath.Join(codexDir, "config.toml"))
			skillTargets = append(skillTargets, filepath.Join(codexDir, "skills", "chorus", "SKILL.md"))
		} else if realHome, homeErr := os.UserHomeDir(); homeErr == nil && realHome == homeDir {
			if _, lookErr := exec.LookPath("codex"); lookErr == nil {
				tomlTargets = append(tomlTargets, filepath.Join(codexDir, "config.toml"))
				skillTargets = append(skillTargets, filepath.Join(codexDir, "skills", "chorus", "SKILL.md"))
			}
		}
		if len(jsonTargets) == 0 && len(tomlTargets) == 0 {
			jsonTargets = append(jsonTargets, filepath.Join(homeDir, ".chorus", "mcp.json"))
		}
	}

	updated := make([]string, 0, len(jsonTargets)+len(tomlTargets)+len(skillTargets))
	for _, target := range jsonTargets {
		if err := mergeMCPConfigFile(target, mcpURL); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	for _, target := range tomlTargets {
		if err := mergeCodexConfigFile(target, mcpURL); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	for _, target := range skillTargets {
		if err := writeSkillFile(target); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	return updated, nil
}

// writeSkillFile writes the embedded Chorus SKILL.md to path, creating parent
// directories as needed.
func writeSkillFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create skill directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, skills.ChorusSkillMD, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// mergeMCPConfigFile idempotently inserts or updates "mcpServers.chorus" in a
// JSON configuration file while preserving all existing top-level keys and peer
// MCP server entries. A missing or empty file starts from an empty object. It
// returns an error, writing nothing, when the file is not a JSON object (for
// example "null" or an array) or its mcpServers is present but not an object,
// so a malformed setting is reported instead of silently replaced.
func mergeMCPConfigFile(path, mcpURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory for %s: %w", path, err)
	}

	root := make(map[string]any)
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if unmarshalErr := json.Unmarshal(raw, &root); unmarshalErr != nil {
				return fmt.Errorf("parse JSON in %s: %w", path, unmarshalErr)
			}
			if root == nil {
				return fmt.Errorf("%s must contain a JSON object, not null", path)
			}
		}
	}

	servers := make(map[string]any)
	if existing, present := root["mcpServers"]; present {
		var ok bool
		if servers, ok = existing.(map[string]any); !ok || servers == nil {
			return fmt.Errorf("%s: mcpServers must be a JSON object", path)
		}
	}
	if filepath.Base(path) == "mcp_config.json" {
		servers["chorus"] = map[string]any{
			"url": mcpURL,
		}
	} else {
		servers["chorus"] = map[string]any{
			"type": "http",
			"url":  mcpURL,
		}
	}
	root["mcpServers"] = servers

	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON for %s: %w", path, err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// mergeCodexConfigFile sets mcp_servers.chorus.url to mcpURL in an OpenAI
// Codex TOML configuration file and leaves every other byte unchanged, so
// comments, formatting, and other servers survive. It parses the file with a
// full TOML parser to find where the setting lives:
//   - an existing url (under [mcp_servers.chorus] in any key spelling, or as a
//     dotted key such as chorus.url under [mcp_servers]) has only its string
//     value replaced;
//   - an existing [mcp_servers.chorus] table without a url gets a url line
//     right after its header;
//   - otherwise a new [mcp_servers.chorus] table is appended.
//
// It returns an error, writing nothing, when the file is not valid TOML or
// defines the Chorus entry in a form it cannot edit in place (an inline table,
// an array of tables, dotted keys without a url, or a non-string url); the
// message names the setting to add by hand. Tables nested under the entry,
// such as [mcp_servers.chorus.http_headers], are kept as they are.
func mergeCodexConfigFile(path, mcpURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory for %s: %w", path, err)
	}

	perm := os.FileMode(0o600)
	var content []byte
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
		if content, err = os.ReadFile(path); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}

	urlValue := strconv.Quote(mcpURL)
	edit, err := locateCodexChorusURL(content)
	if err != nil {
		return fmt.Errorf("%s: %w; set mcp_servers.chorus.url = %s by hand", path, err, urlValue)
	}

	var out []byte
	switch {
	case edit.value.Length > 0:
		at := int(edit.value.Offset)
		out = append(out, content[:at]...)
		out = append(out, urlValue...)
		out = append(out, content[at+int(edit.value.Length):]...)
	case edit.afterHeader > 0:
		out = append(out, content[:edit.afterHeader]...)
		if out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, "url = "+urlValue+"\n"...)
		out = append(out, content[edit.afterHeader:]...)
	default:
		out = append(out, bytes.TrimRight(content, " \t\r\n")...)
		if len(out) > 0 {
			out = append(out, "\n\n"...)
		}
		out = append(out, "[mcp_servers.chorus]\nurl = "+urlValue+"\n"...)
	}
	if err := os.WriteFile(path, out, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// locateCodexChorusURL parses a Codex config.toml document and reports where
// mergeCodexConfigFile must write mcp_servers.chorus.url. An empty result
// means the document does not define the Chorus entry yet. It returns an
// error for invalid TOML and for Chorus entries it cannot edit in place.
func locateCodexChorusURL(doc []byte) (codexURLEdit, error) {
	// The position-aware parser checks syntax only, so decode the whole
	// document first: Codex rejects duplicate keys, redefined tables, and
	// type conflicts, and editing such a file would report a registration
	// Codex never loads. The decoded value is discarded.
	var discard map[string]any
	if err := toml.Unmarshal(doc, &discard); err != nil {
		return codexURLEdit{}, fmt.Errorf("not valid TOML: %w", err)
	}

	var (
		edit    codexURLEdit
		dotted  bool
		table   []string
		parser  unstable.Parser
		chorus  = []string{"mcp_servers", "chorus"}
		fullURL = []string{"mcp_servers", "chorus", "url"}
	)
	parser.Reset(doc)
	for parser.NextExpression() {
		expr := parser.Expression()
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			var last *unstable.Node
			table, last = tomlKeyPath(nil, expr.Key())
			if !slices.Equal(table, chorus) {
				continue // other tables, including ones nested under the entry
			}
			if expr.Kind == unstable.ArrayTable {
				return edit, errors.New("mcp_servers.chorus is defined as an array of tables")
			}
			end := int(last.Raw.Offset + last.Raw.Length)
			if nl := bytes.IndexByte(doc[end:], '\n'); nl >= 0 {
				edit.afterHeader = end + nl + 1
			} else {
				edit.afterHeader = len(doc)
			}
		case unstable.KeyValue:
			key, _ := tomlKeyPath(slices.Clone(table), expr.Key())
			switch {
			case slices.Equal(key, fullURL):
				value := expr.Value()
				if value.Kind != unstable.String {
					return edit, errors.New("mcp_servers.chorus.url is not a string")
				}
				edit.value = value.Raw
			case slices.Equal(key, chorus) || slices.Equal(key, chorus[:1]):
				return edit, errors.New("mcp_servers or mcp_servers.chorus is an inline table")
			case len(key) > 2 && slices.Equal(key[:2], chorus) && len(table) < 2:
				dotted = true
			}
		}
	}
	if err := parser.Error(); err != nil {
		return edit, fmt.Errorf("not valid TOML: %w", err)
	}
	if dotted && edit.value.Length == 0 {
		return edit, errors.New("mcp_servers.chorus is defined with dotted keys and no url")
	}
	return edit, nil
}

// tomlKeyPath appends the decoded segments of a TOML key (bare, quoted, or
// dotted, so [a."b"] and [ a . b ] both give a, b) to prefix and returns the
// path with the last segment's node, whose Raw range ends the key in the input.
func tomlKeyPath(prefix []string, it unstable.Iterator) ([]string, *unstable.Node) {
	var last *unstable.Node
	for it.Next() {
		last = it.Node()
		prefix = append(prefix, string(last.Data))
	}
	return prefix, last
}

// setupForUp runs the global setup for "chorus up": it registers mcpURL in the
// detected coding agents' global MCP configuration and installs the Chorus
// skill next to them, then writes a one-line summary to out. Hand-edited
// installed skill files are replaced by the embedded version. The error names
// how to skip this step when a configuration file cannot be updated.
func setupForUp(homeDir, mcpURL string, out io.Writer) error {
	updated, err := setupMCPConfigs("", homeDir, mcpURL, true)
	if err != nil {
		return fmt.Errorf("register Chorus with your coding agents: %w (fix the file, or set CHORUS_NO_SETUP=1 to skip this step)", err)
	}
	fmt.Fprintf(out, "Registered Chorus with your coding agents (%d files; 'chorus setup -global' lists them)\n", len(updated))
	return nil
}
