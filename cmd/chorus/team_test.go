package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTeamCommandForwardsChoices uses one tiny process to observe the executable
// boundary, stdin and stdout; Python owner tests cover all launch semantics.
func TestTeamCommandForwardsChoices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_APP_HOME", home)
	_, err := setupHerdrLauncher(home, io.Discard)
	require.NoError(t, err)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "python3"), []byte("#!/bin/sh\ncase \"$3\" in team-launch|team-inspect) ;; *) exit 9 ;; esac\n[ \"$4\" = - ] || exit 9\n/bin/cat\n"), 0o700))
	t.Setenv("PATH", bin)
	const choices = `{"guide":{"client":"claude","program":"claude","model":null,"thinking":null,"yolo":false}}`
	var output bytes.Buffer
	for _, args := range [][]string{
		{"launch", "--space", "sp1", "--choices", "-"},
		{"inspect", "--space", "sp1", "--choices", "-"},
		{"inspect", "--space", "sp1"},
	} {
		output.Reset()
		require.NoError(t, runTeam(args, strings.NewReader(choices), &output))
		var result struct {
			Space   string          `json:"space"`
			Choices json.RawMessage `json:"choices"`
		}
		require.NoError(t, json.Unmarshal(output.Bytes(), &result))
		assert.Equal(t, "sp1", result.Space)
		if len(args) == 3 {
			assert.Nil(t, result.Choices, "bare inspect preserves its request shape")
		} else {
			assert.JSONEq(t, choices, string(result.Choices))
		}
	}

	// Setup owns stale integration recovery; a refused command does not repair it.
	script := filepath.Join(home, "mods", "chorus-launcher", "team_cli.py")
	require.NoError(t, os.WriteFile(script, []byte("old"), 0o600))
	output.Reset()
	err = runTeam([]string{"inspect", "--space", "sp1"}, strings.NewReader(""), &output)
	assert.ErrorContains(t, err, "chorus setup -global")
	assert.Empty(t, output.String())
	got, err := os.ReadFile(script)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}
