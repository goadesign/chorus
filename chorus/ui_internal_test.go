// Tests of the dashboard file identity that only package-internal code can
// reach.
package chorus

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUIDigestFollowsDashboardFilesOnly verifies that the dashboard identity
// changes when any served dashboard file changes, is the same for the same
// files, ignores files browsers are never served, cannot be confused by
// moving bytes between a path and its contents, and is computed from the
// embedded dashboard alone.
func TestUIDigestFollowsDashboardFilesOnly(t *testing.T) {
	digest := func(fsys fstest.MapFS) string {
		t.Helper()
		d, err := uiDigest(fsys)
		require.NoError(t, err)
		return d
	}
	base := fstest.MapFS{
		"index.html":  {Data: []byte("<html></html>")},
		"timeline.js": {Data: []byte("render()")},
	}
	same := fstest.MapFS{
		"timeline.js": {Data: []byte("render()")},
		"index.html":  {Data: []byte("<html></html>")},
	}
	changed := fstest.MapFS{
		"index.html":  {Data: []byte("<html></html>")},
		"timeline.js": {Data: []byte("render();")},
	}
	assert.Equal(t, digest(base), digest(same), "same files, same identity")
	assert.NotEqual(t, digest(base), digest(changed), "one changed byte changes the identity")
	assert.NotEqual(t,
		digest(fstest.MapFS{"a": {Data: []byte("bc")}}),
		digest(fstest.MapFS{"ab": {Data: []byte("c")}}),
		"bytes moved between a path and its contents give a different identity")
	notice := fstest.MapFS{
		"index.html":        {Data: []byte("<html></html>")},
		"timeline.js":       {Data: []byte("render()")},
		"brands/SOURCES.md": {Data: []byte("corrected license note")},
	}
	assert.Equal(t, digest(base), digest(notice), "a file browsers are never served does not change the identity")
	assert.Len(t, uiBuild, 64, "full SHA-256 in hex")
	assert.Equal(t, mustDigest(uiRoot), uiBuild, "the identity covers the embedded dashboard files only")
}
