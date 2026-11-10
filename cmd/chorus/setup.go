// This file implements "chorus setup": it registers the Chorus MCP endpoint
// in project or global coding-agent configuration files and installs the
// embedded Chorus Agent Skill (SKILL.md) next to them. Global setup also installs
// the bundled launcher when Herdr is detected. "chorus up" runs the same global
// setup before it starts the server.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// herdrShortcutBinding describes an existing assignment of the launcher's first
// keypress. Command arguments stay private to the keyboard file.
type herdrShortcutBinding struct {
	// section is the keyboard context that already uses the keypress.
	section string
	// command names the action without any arguments or terminal text.
	command string
	// opensLauncher is true only for a single keypress opening this launcher.
	opensLauncher bool
}

const (
	// herdrLauncherShortcut opens the launcher without taking a common terminal key.
	herdrLauncherShortcut = "cmd-alt-shift-c"
	// herdrLauncherAction is Herdr's documented command shorthand for this mod.
	herdrLauncherAction = "mod.open:chorus-launcher"
)

// runSetup registers the Chorus MCP server URL in project and/or global MCP
// configuration files and installs the Chorus Agent Skill (SKILL.md). Global
// setup also installs the launcher when Herdr is detected.
func runSetup(args []string, out io.Writer) error {
	var (
		addr   string
		global bool
	)
	fs := flag.NewFlagSet("chorus setup", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	fs.BoolVar(&global, "global", false, "Register coding agents globally and install the Chorus launcher for Herdr")
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
			fmt.Fprintf(out, "Installed Chorus skill in %s\n", p)
		} else {
			fmt.Fprintf(out, "Registered Chorus MCP (%s) in %s\n", mcpURL, p)
		}
	}
	if global {
		modDir, err := setupHerdrLauncher(homeDir, out)
		if err != nil {
			return fmt.Errorf("install Chorus launcher in Herdr: %w", err)
		}
		writeHerdrLauncherSummary(out, modDir)
	}
	return nil
}

// setupMCPConfigs merges the Chorus MCP server entry into the project's
// .mcp.json (and .codex/config.toml if .codex exists) and installs
// .agents/skills/chorus/SKILL.md when global is false, or updates detected
// global agent MCP config files and skill directories under homeDir (Universal
// .agents, Cursor, Antigravity, Claude Code, and OpenAI Codex) when global is
// true. Herdr installation is a separate operation called by runSetup and
// setupForUp, so these paths always describe MCP and skill files.
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
		// Claude Code keeps user MCP servers in .claude.json and skills in
		// skills/ under ~/.claude, or both under CLAUDE_CONFIG_DIR when set.
		// It counts as installed when either location exists or, for the
		// real home, the claude executable is on PATH, so a Claude Code that
		// has never been started is registered too.
		claudeDir := filepath.Join(homeDir, ".claude")
		claudeConfig := filepath.Join(homeDir, ".claude.json")
		if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
			claudeDir = dir
			claudeConfig = filepath.Join(dir, ".claude.json")
		}
		if claudeInstalled(homeDir, claudeDir, claudeConfig) {
			jsonTargets = append(jsonTargets, claudeConfig)
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
		if err := writeIntegrationFile(target, skills.ChorusSkillMD); err != nil {
			return nil, err
		}
		updated = append(updated, target)
	}
	return updated, nil
}

// setupHerdrLauncher installs the bundled mod into Herdr's existing home
// directory. Herdr creates that directory on its first launch. Only bundled
// runtime files are replaced; saved preferences and existing bindings survive.
// The launcher shortcut is added only when its global key is unassigned and
// Herdr has already created its keyboard file. Shortcut outcomes are written to out.
// It returns the installed mod directory, or an empty string when Herdr has
// no existing home directory, and reports read or write failures with a path.
func setupHerdrLauncher(homeDir string, out io.Writer) (string, error) {
	herdrHome := cmp.Or(os.Getenv("HERDR_APP_HOME"), filepath.Join(homeDir, ".config", "herdr-app"))
	info, err := os.Stat(herdrHome)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect Herdr configuration %s: %w", herdrHome, err)
	}
	if !info.IsDir() {
		return "", nil
	}

	const source = "herdr/chorus-launcher"
	modDir := filepath.Join(herdrHome, "mods", "chorus-launcher")
	// The overlay manifest must exist before Herdr can load the new UI script.
	for _, name := range []string{"mod.json", "launch.py", "main.js"} {
		content, err := skills.HerdrLauncher.ReadFile(source + "/" + name)
		if err != nil {
			return "", fmt.Errorf("read bundled Herdr launcher %s: %w", name, err)
		}
		target := filepath.Join(modDir, name)
		if err := writeIntegrationFile(target, content); err != nil {
			return "", err
		}
	}
	if runtime.GOOS == "darwin" {
		if err := setupHerdrShortcut(herdrHome, out); err != nil {
			writeHerdrLauncherSummary(out, modDir)
			return "", fmt.Errorf("add Herdr launcher shortcut: %w", err)
		}
	}
	return modDir, nil
}

// setupHerdrShortcut adds the launcher to Herdr's existing global bindings.
// A missing file is left for Herdr to create with its complete defaults.
// Existing bindings, including explicit nulls and object actions, are preserved;
// malformed files are reported without writing. Editing the file makes Herdr
// treat it as user-managed on future upgrades.
func setupHerdrShortcut(herdrHome string, out io.Writer) error {
	path := filepath.Join(herdrHome, "keys.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "Herdr keyboard file is missing; open Herdr and rerun chorus setup -global to add the shortcut.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Herdr keyboard settings %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect Herdr keyboard settings %s: %w", path, err)
	}
	if info.Mode().Perm()&0o222 == 0 {
		fmt.Fprintf(out, "Herdr keyboard settings %s are read-only; kept the file unchanged. Use the launcher command below.\n", path)
		return nil
	}
	updated, existing, err := addHerdrShortcut(raw)
	if err != nil {
		return fmt.Errorf("parse Herdr keyboard settings %s: %w", path, err)
	}
	if existing != nil {
		if existing.opensLauncher {
			fmt.Fprintf(out, "Herdr shortcut %s already opens Chorus in %q.\n", herdrLauncherShortcut, existing.section)
		} else {
			fmt.Fprintf(out, "Herdr shortcut %s is taken by command %q in %q; kept its existing binding. Use the launcher command below.\n", herdrLauncherShortcut, existing.command, existing.section)
		}
		return nil
	}
	if err := writeHerdrKeyboardFile(path, updated); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added Herdr shortcut %s to open Chorus. Herdr now treats keys.json as user-managed.\n", herdrLauncherShortcut)
	return nil
}

// addHerdrShortcut locates the global object while validating the complete
// keyboard document. Any matching chord in any section prevents an insertion.
// Unrelated bytes, including key order and numeric arguments, remain unchanged.
func addHerdrShortcut(raw []byte) ([]byte, *herdrShortcutBinding, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, errors.New("keyboard settings file is empty")
		}
		return nil, nil, err
	}
	if token != json.Delim('{') {
		return nil, nil, errors.New("keyboard settings must contain a JSON object")
	}
	var existing *herdrShortcutBinding
	var rootHasMembers, globalHasMembers bool
	globalClose := -1
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		section := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		rootHasMembers = true
		if section != "global" && section != "terminal" && !strings.HasPrefix(section, "layout:") && !strings.HasPrefix(section, "mod:") {
			continue
		}
		var bindings map[string]json.RawMessage
		if err := json.Unmarshal(value, &bindings); err != nil || bindings == nil {
			return nil, nil, fmt.Errorf("%s must contain a JSON object", section)
		}
		if section == "global" {
			if globalClose >= 0 {
				return nil, nil, errors.New("keyboard settings contain more than one global section")
			}
			globalClose = int(decoder.InputOffset()) - 1
			globalHasMembers = len(bindings) > 0
		}
		for chord, action := range bindings {
			if !herdrShortcutMatches(chord) {
				continue
			}
			var name string
			var object struct {
				// Command is the app action selected by this binding.
				Command string `json:"command"`
				// Args contains the action's named arguments without re-encoding them.
				Args map[string]json.RawMessage `json:"args"`
			}
			opens := false
			if json.Unmarshal(action, &name) == nil {
				opens = name == herdrLauncherAction
				name, _, _ = strings.Cut(name, ":")
			} else if json.Unmarshal(action, &object) == nil {
				name = object.Command
				var mod string
				opens = object.Command == "mod.open" && len(object.Args) == 1 && json.Unmarshal(object.Args["name"], &mod) == nil && mod == "chorus-launcher"
			}
			binding := &herdrShortcutBinding{
				section:       section,
				command:       name,
				opensLauncher: opens && len(strings.Fields(chord)) == 1,
			}
			// A conflicting assignment takes precedence over an existing launcher
			// assignment because it can intercept the key in a specific context.
			if existing == nil || !binding.opensLauncher {
				existing = binding
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, nil, err
	}
	rootClose := int(decoder.InputOffset()) - 1
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("keyboard settings must contain one JSON object")
	}
	if existing != nil {
		return raw, existing, nil
	}
	member := fmt.Sprintf("%q: %q", herdrLauncherShortcut, herdrLauncherAction)
	if globalClose >= 0 {
		return insertJSONMember(raw, globalClose, globalHasMembers, member), nil, nil
	}
	member = fmt.Sprintf("%q: {%s}", "global", member)
	return insertJSONMember(raw, rootClose, rootHasMembers, member), nil, nil
}

// herdrShortcutMatches recognizes Herdr's aliases and implicit shift for an
// uppercase letter. A sequence starting with this keypress also occupies it.
func herdrShortcutMatches(chord string) bool {
	strokes := strings.Fields(chord)
	if len(strokes) == 0 {
		return false
	}
	parts := strings.Split(strokes[0], "-")
	key := parts[len(parts)-1]
	if key != "c" && key != "C" {
		return false
	}
	held := make(map[string]bool)
	for _, modifier := range parts[:len(parts)-1] {
		modifier = strings.ToLower(modifier)
		switch modifier {
		case "secondary", "super", "win":
			modifier = "cmd"
		}
		held[modifier] = true
	}
	if key == "C" {
		held["shift"] = true
	}
	return len(held) == 3 && held["cmd"] && held["alt"] && held["shift"]
}

// insertJSONMember adds a member before a validated object's closing brace,
// reusing its existing closing indentation and leaving other bytes untouched.
func insertJSONMember(raw []byte, closeOffset int, hasMembers bool, member string) []byte {
	at := closeOffset
	for at > 0 && strings.ContainsRune(" \t\r\n", rune(raw[at-1])) {
		at--
	}
	separator := ""
	if hasMembers {
		separator = ","
	}
	var insertion string
	if bytes.ContainsRune(raw[at:closeOffset], '\n') {
		line := bytes.LastIndexByte(raw[:closeOffset], '\n') + 1
		indent := string(raw[line:closeOffset])
		memberIndent := indent + "  "
		if hasMembers {
			previous := bytes.LastIndexByte(raw[:at], '\n') + 1
			end := previous
			for end < at && (raw[end] == ' ' || raw[end] == '\t') {
				end++
			}
			memberIndent = string(raw[previous:end])
		}
		newline := "\n"
		if bytes.Contains(raw[at:closeOffset], []byte("\r\n")) {
			newline = "\r\n"
		}
		insertion = separator + newline + memberIndent + member + newline + indent
	} else {
		insertion = separator + member + string(raw[at:closeOffset])
	}
	updated := make([]byte, 0, len(raw)+len(insertion))
	updated = append(updated, raw[:at]...)
	updated = append(updated, insertion...)
	return append(updated, raw[closeOffset:]...)
}

// writeHerdrKeyboardFile replaces the complete keyboard file atomically,
// preserving permissions and following an existing symlink to its target.
func writeHerdrKeyboardFile(path string, content []byte) (err error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve Herdr keyboard settings %s: %w", path, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("inspect Herdr keyboard settings %s: %w", target, err)
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".chorus-keys-*")
	if err != nil {
		return fmt.Errorf("create Herdr keyboard update: %w", err)
	}
	defer func() {
		if cleanupErr := os.Remove(file.Name()); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove Herdr keyboard temporary file: %w", cleanupErr))
		}
	}()
	writeErr := file.Chmod(info.Mode().Perm())
	if writeErr == nil {
		_, writeErr = file.Write(content)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write Herdr keyboard settings %s: %w", target, err)
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return fmt.Errorf("replace Herdr keyboard settings %s: %w", target, err)
	}
	return nil
}

// writeIntegrationFile installs bundled skill or launcher content, creating
// directories as needed. Identical files are left untouched: Herdr reloads
// open dialogs when mod files are written, discarding unfinished choices.
func writeIntegrationFile(path string, content []byte) error {
	installed, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err == nil && bytes.Equal(installed, content) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
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
// skill next to them, installs the launcher when Herdr is detected, then writes
// a summary and the launcher command to out. Hand-edited installed skill and
// launcher files are replaced by the embedded version. The error names
// how to skip this step when a configuration file cannot be updated.
func setupForUp(homeDir, mcpURL string, out io.Writer) error {
	updated, err := setupMCPConfigs("", homeDir, mcpURL, true)
	if err != nil {
		return fmt.Errorf("register Chorus with your coding agents: %w (fix the file, or set CHORUS_NO_SETUP=1 to skip this step)", err)
	}
	fmt.Fprintf(out, "Registered Chorus with your coding agents (%d files)\n", len(updated))
	modDir, err := setupHerdrLauncher(homeDir, out)
	if err != nil {
		return fmt.Errorf("install Chorus launcher in Herdr: %w (fix this path, then rerun chorus up; or set CHORUS_NO_SETUP=1 to skip setup)", err)
	}
	writeHerdrLauncherSummary(out, modDir)
	return nil
}

// writeHerdrLauncherSummary names the installed mod directory once and gives
// the person a command to open it even when the shortcut key is already assigned.
func writeHerdrLauncherSummary(out io.Writer, modDir string) {
	if modDir != "" {
		fmt.Fprintf(out, "Installed Chorus launcher in %s\n", modDir)
		fmt.Fprintln(out, "Open it from a project terminal: herdr-app do mod.open --name chorus-launcher")
	}
}

// claudeInstalled reports whether Claude Code is set up for setupMCPConfigs:
// its configuration directory or file already exists, or homeDir is the real
// home directory and the claude executable is on PATH. The PATH check is
// limited to the real home so a setup run against another directory (such as
// a test's) never registers clients found on the machine.
func claudeInstalled(homeDir, claudeDir, claudeConfig string) bool {
	if info, err := os.Stat(claudeDir); err == nil && info.IsDir() {
		return true
	}
	if _, err := os.Stat(claudeConfig); err == nil {
		return true
	}
	realHome, err := os.UserHomeDir()
	if err != nil || realHome != homeDir {
		return false
	}
	_, err = exec.LookPath("claude")
	return err == nil
}
