// This file implements "chorus setup": it registers the Chorus MCP endpoint
// in project or global coding-agent configuration files and installs the
// embedded Chorus Agent Skill (SKILL.md) next to them. "chorus up" runs the same
// global setup before it starts the server.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goadesign/chorus/skills"
)

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
// MCP server entries.
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
		}
	}

	servers, ok := root["mcpServers"].(map[string]any)
	if !ok || servers == nil {
		servers = make(map[string]any)
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

// mergeCodexConfigFile idempotently inserts or updates the
// "[mcp_servers.chorus]" section in an OpenAI Codex TOML configuration file
// while preserving all existing tables, keys, and comments.
func mergeCodexConfigFile(path, mcpURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory for %s: %w", path, err)
	}

	perm := os.FileMode(0o600)
	var content string
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		content = string(raw)
	}

	urlLine := fmt.Sprintf("url = %q", mcpURL)
	if strings.TrimSpace(content) == "" {
		out := fmt.Sprintf("[mcp_servers.chorus]\n%s\n", urlLine)
		if err := os.WriteFile(path, []byte(out), perm); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}

	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var (
		out         []string
		inChorusSec bool
		foundSec    bool
		wroteURL    bool
	)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inChorusSec && !wroteURL {
				out = append(out, urlLine)
				wroteURL = true
			}
			inChorusSec = trimmed == "[mcp_servers.chorus]"
			if inChorusSec {
				foundSec = true
			}
			out = append(out, line)
			continue
		}
		if inChorusSec {
			if key, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(key) == "url" {
				if !wroteURL {
					out = append(out, urlLine)
					wroteURL = true
				}
				continue
			}
		}
		out = append(out, line)
	}
	if inChorusSec && !wroteURL {
		out = append(out, urlLine)
	}
	if !foundSec {
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "", "[mcp_servers.chorus]", urlLine, "")
	}

	rendered := strings.Join(out, "\n")
	if !strings.HasSuffix(rendered, "\n") {
		rendered += "\n"
	}
	if err := os.WriteFile(path, []byte(rendered), perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
