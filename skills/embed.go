// Package skills embeds the model-agnostic Chorus Agent Skill (SKILL.md) so
// "chorus setup" can install it into project and global agent skill directories.
package skills

import _ "embed"

// ChorusSkillMD holds the raw markdown contents of skills/chorus/SKILL.md.
//
//go:embed chorus/SKILL.md
var ChorusSkillMD []byte
