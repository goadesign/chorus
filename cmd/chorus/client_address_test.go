// This file tests the managed address contract at the CLI boundary.
package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClientBaseURL owns saved-address precedence and invalid-file handling.
func TestClientBaseURL(t *testing.T) {
	for _, test := range []struct {
		name, saved, want, wantErr string
		args                       []string
	}{
		{name: "no saved settings", want: "http://localhost:9440"},
		{name: "saved address", saved: `["-addr",":9550"]`, want: "http://localhost:9550"},
		{name: "explicit wins", saved: `["-addr",":9550"]`, args: []string{"-addr", ":9660"}, want: "http://localhost:9660"},
		{name: "explicit default wins", saved: `["-addr",":9550"]`, args: []string{"-addr", defaultListenAddr}, want: "http://localhost:9440"},
		{name: "explicit bypasses invalid saved settings", saved: `{`, args: []string{"-addr", ":9660"}, want: "http://localhost:9660"},
		{name: "invalid flags", saved: `["-unknown"]`, wantErr: "flag provided but not defined"},
		{name: "invalid saved address", saved: `["-addr","bad"]`, wantErr: "parse Chorus listen address"},
		{name: "invalid explicit address", args: []string{"-addr", "bad"}, wantErr: "parse Chorus listen address"},
		{name: "positional before address", saved: `["serve","-addr",":9550"]`, wantErr: "unexpected argument"},
		{name: "positional after flag", saved: `["-redis-db","1","bad","-addr",":9550"]`, wantErr: "unexpected argument"},
		{name: "flags after terminator", saved: `["--","-addr",":9550"]`, wantErr: "unexpected argument"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if test.saved != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(home, ".chorus", serveArgsFile), []byte(test.saved), 0o644))
			}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			addr := fs.String("addr", defaultListenAddr, "server address")
			require.NoError(t, fs.Parse(test.args))
			got, err := clientBaseURL(fs, *addr)
			if test.wantErr != "" {
				assert.ErrorContains(t, err, test.wantErr)
				assert.Empty(t, got)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestClientCommandsRejectBadSavedAddress verifies all six entry points stop
// before making requests, opening a browser, or writing client configuration.
func TestClientCommandsRejectBadSavedAddress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".chorus"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".chorus", serveArgsFile), []byte("{"), 0o644))
	opened := false
	for name, run := range map[string]func() error{
		"status": func() error { return runStatus([]string{"-json"}, &bytes.Buffer{}) },
		"watch":  func() error { return runWatch([]string{"agent", "-wait", "0s"}, &bytes.Buffer{}) },
		"ui": func() error {
			return runUI(nil, &bytes.Buffer{}, func(string) error { opened = true; return nil })
		},
		"demo": func() error {
			return runDemo(nil, &bytes.Buffer{}, func(string) error { opened = true; return nil })
		},
		"stdio": func() error { return runStdio(nil, strings.NewReader(""), &bytes.Buffer{}) },
		"setup": func() error { return runSetup(nil, &bytes.Buffer{}) },
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorContains(t, run(), "read saved server flags")
		})
	}
	assert.False(t, opened)
	assert.NoFileExists(t, ".mcp.json")
}
