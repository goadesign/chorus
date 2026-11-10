// Package skills embeds the Chorus Agent Skill and Herdr launcher so
// "chorus setup" installs integrations without downloading additional files.
package skills

import "embed"

// ChorusSkillMD holds the raw markdown contents of skills/chorus/SKILL.md.
//
//go:embed chorus/SKILL.md
var ChorusSkillMD []byte

// HerdrLauncher contains the launcher files that global setup installs into
// Herdr's mods directory. Preferences and terminal records stay owned by Herdr.
//
//go:embed herdr/chorus-launcher/mod.json herdr/chorus-launcher/main.js
//go:embed herdr/chorus-launcher/launch.py
var HerdrLauncher embed.FS
