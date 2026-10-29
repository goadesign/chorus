// Package skills embeds the Chorus agent skill and installs it into standard
// coding-agent skill discovery directories.
package skills

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// chorusSkillMarkdown holds the embedded contents of skills/chorus/SKILL.md.
//
//go:embed chorus/SKILL.md
var chorusSkillMarkdown []byte

// targetSubdirs lists the global skill directories (relative to the user's home
// directory) where coding agents discover the Chorus skill.
var targetSubdirs = []string{
	filepath.Join(".gemini", "config", "skills", "chorus"),
	filepath.Join(".claude", "skills", "chorus"),
	filepath.Join(".agents", "skills", "chorus"),
}

// Install writes the embedded Chorus SKILL.md into the standard global agent
// skill directories under homeDir and returns the written file paths.
func Install(homeDir string) ([]string, error) {
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user home directory: %w", err)
		}
	}

	written := make([]string, 0, len(targetSubdirs))
	for _, subdir := range targetSubdirs {
		dir := filepath.Join(homeDir, subdir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create skill directory %s: %w", dir, err)
		}
		dest := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(dest, chorusSkillMarkdown, 0o644); err != nil {
			return nil, fmt.Errorf("write skill file %s: %w", dest, err)
		}
		written = append(written, dest)
	}
	return written, nil
}
