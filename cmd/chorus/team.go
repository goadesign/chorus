// This file exposes the roster launcher's existing owner to terminal users.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/goadesign/chorus/skills"
)

// runTeam parses an explicit space and forwards inspection or role choices to
// the installed dialog backend. It never changes Herdr preferences or discovers
// an agent catalog; the caller names each program whose client options it reads.
func runTeam(args []string, input io.Reader, out io.Writer) error {
	if len(args) == 0 || args[0] != "inspect" && args[0] != "launch" {
		return errors.New("use chorus team inspect --space <space>, or chorus team launch --space <space> --choices <file|->")
	}
	action := args[0]
	var space, choices string
	flags := flag.NewFlagSet("chorus team "+action, flag.ContinueOnError)
	flags.StringVar(&space, "space", "", "Required Herdr space ID from the project's team")
	flags.StringVar(&choices, "choices", "", "JSON choices file, or - for standard input; required for launch, optional for inspect")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: chorus team %s --space <space>", action)
		if action == "launch" {
			fmt.Fprint(flags.Output(), " --choices <file|->")
		} else {
			fmt.Fprint(flags.Output(), " [--choices <file|->]")
		}
		fmt.Fprintln(flags.Output(), "\n\nInspect roster roles, or launch explicit choices through the Herdr dialog's backend.")
		fmt.Fprintln(flags.Output(), "No agent catalog or saved preferences are read. Name your installed program.")
		fmt.Fprintln(flags.Output(), `Example choices: {"guide":{"client":"codex","program":"codex","model":null,"thinking":null,"yolo":false}}`)
		fmt.Fprintln(flags.Output(), "Use client claude with program claude for Claude Code. Null model/thinking use client defaults.")
		fmt.Fprintln(flags.Output(), "Use null for the client's configured model, or inspect your choices file to see that program's model choices before launch. Custom model IDs are checked by the client at startup.")
		fmt.Fprintln(flags.Output(), "yolo:false uses the client's permission settings; yolo:true bypasses its tool confirmations (Codex also disables command sandboxing); inspect shows the exact flag and effect.")
		fmt.Fprintln(flags.Output(), "Every missing role needs a choice; omitted running roles are reused. Refusals exit nonzero; inspect and retry after a partial launch.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if space == "" || flags.NArg() != 0 || action == "launch" && choices == "" {
		return errors.New("supply --space and, for launch, --choices <file|->; use --help for examples")
	}
	var request []byte
	var err error
	if action == "inspect" && choices == "" {
		request, err = json.Marshal(struct {
			Space string `json:"space"`
		}{space})
	} else {
		var raw []byte
		if choices == "-" {
			raw, err = io.ReadAll(input)
		} else {
			raw, err = os.ReadFile(choices)
		}
		if err != nil {
			return fmt.Errorf("read role choices: %w", err)
		}
		request, err = json.Marshal(struct {
			Space   string          `json:"space"`
			Choices json.RawMessage `json:"choices"`
		}{space, raw})
	}
	if err != nil {
		return fmt.Errorf("encode team request: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find Herdr home: %w", err)
	}
	appHome := cmp.Or(os.Getenv("HERDR_APP_HOME"), filepath.Join(home, ".config", "herdr-app"))
	mod := filepath.Join(appHome, "mods", "chorus-launcher")
	// Both entry points must execute this release's same backend. Setup owns
	// installation; a stale or hand-edited installation is never repaired here.
	for _, name := range []string{"launch.py", "team_cli.py"} {
		bundled, err := skills.HerdrLauncher.ReadFile("herdr/chorus-launcher/" + name)
		if err != nil {
			return fmt.Errorf("read bundled launcher: %w", err)
		}
		installed, err := os.ReadFile(filepath.Join(mod, name))
		if err != nil || !bytes.Equal(installed, bundled) {
			return errors.New("Herdr's Chorus launcher needs this release's files; run chorus setup -global, then retry")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", filepath.Join(mod, "launch.py"), appHome, "team-"+action, "-")
	command.Stdin = bytes.NewReader(request)
	command.Stdout, command.Stderr = out, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("team %s did not finish; inspect the team before retrying: %w", action, err)
	}
	return nil
}
