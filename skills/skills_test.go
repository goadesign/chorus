package skills

import (
	"os"
	"strings"
	"testing"
)

// TestInstall verifies that Install writes the embedded SKILL.md into all
// target agent skill directories under the provided home directory.
func TestInstall(t *testing.T) {
	home := t.TempDir()
	paths, err := Install(home)
	if err != nil {
		t.Fatalf("Install(%q) failed: %v", home, err)
	}
	if len(paths) != len(targetSubdirs) {
		t.Fatalf("got %d installed paths, want %d", len(paths), len(targetSubdirs))
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read installed skill %q: %v", p, err)
		}
		if !strings.Contains(string(data), "name: chorus") {
			t.Fatalf("installed skill %q missing frontmatter:\n%s", p, string(data))
		}
	}
}
