// This file implements "chorus demo": it seeds a realistic 3-agent + human
// collaboration session into an isolated swarm on the running Chorus server and
// opens the live web dashboard directly on that swarm so newcomers can explore
// timeline swimlanes, pinned task boards, cross-model reviews, and unread
// watermarks in one command before connecting their own editors.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type (
	// demoViewSnapshot captures the subset of GET /swarms/<swarm>/view needed to
	// reset existing demo channels cleanly without duplicating direct messages.
	demoViewSnapshot struct {
		// Agents lists registered handles in the target swarm.
		Agents []struct {
			// Handle is the registered agent identifier without '@'.
			Handle string `json:"handle"`
		} `json:"agents"`
		// Inboxes lists non-empty direct-message inboxes in the target swarm.
		Inboxes []struct {
			// Recipient is the handle whose inbox holds Messages.
			Recipient string `json:"recipient"`
			// Messages holds retained direct messages in the inbox.
			Messages []json.RawMessage `json:"messages"`
		} `json:"inboxes"`
	}
)

const (
	// defaultDemoSwarm is the isolated swarm namespace populated by "chorus demo".
	defaultDemoSwarm = "demo"
	// demoToolTimeout bounds each MCP tool call issued while seeding the demo swarm.
	demoToolTimeout = 10 * time.Second
	// demoStepDelay spaces demo messages slightly so the timeline renders distinct points.
	demoStepDelay = 60 * time.Millisecond
)

var (
	// demoSwarmPattern matches valid Chorus swarm identifiers (design/design.go).
	demoSwarmPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// runDemo ensures a Chorus server is reachable, populates swarm with a sample
// multi-model passkey rollout session, and opens the dashboard in the browser.
func runDemo(args []string, out io.Writer, open func(string) error) error {
	var (
		addr   string
		swarm  string
		noOpen bool
	)
	fs := flag.NewFlagSet("chorus demo", flag.ContinueOnError)
	fs.StringVar(&addr, "addr", defaultListenAddr, "Chorus HTTP listen address")
	fs.StringVar(&swarm, "swarm", defaultDemoSwarm, "Swarm name to seed with sample multi-agent messages")
	fs.BoolVar(&noOpen, "no-open", false, "Seed the demo swarm without opening a browser window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	swarm = strings.TrimSpace(swarm)
	if !demoSwarmPattern.MatchString(swarm) {
		return fmt.Errorf("invalid -swarm %q: must be 1..64 alphanumeric, '-', or '_' characters", swarm)
	}

	baseURL, err := clientBaseURL(fs, addr)
	if err != nil {
		return err
	}
	dashboard, err := dashboardURL(baseURL)
	if err != nil {
		return err
	}
	if ok, _ := probeServer(baseURL); !ok {
		return fmt.Errorf("chorus is not running at %s; run 'chorus up' first", baseURL)
	}

	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse chorus URL %q: %w", baseURL, err)
	}
	if err := seedDemoSwarm(context.Background(), parsedURL, swarm); err != nil {
		return err
	}

	targetURL := strings.TrimRight(dashboard, "/") + "/#swarm=" + url.QueryEscape(swarm) + "&target=%23passkeys"
	fmt.Fprintf(out, "Seeded swarm %q (#passkeys, #benchmarks, and @codex-demo <-> @claude-demo)\n", swarm)
	fmt.Fprintf(out, "Dashboard: %s\n", targetURL)
	if noOpen {
		return nil
	}
	if err := open(targetURL); err != nil {
		return fmt.Errorf("open the demo dashboard in your browser at %s (%w)", targetURL, err)
	}
	return nil
}

// seedDemoSwarm resets any prior demo channels in swarm and publishes a 9-step
// cross-model passkey verification workflow (7 posts in #passkeys, 1 post in
// #benchmarks, and 1 direct message) across Claude, Codex, Gemini, and a human lead.
func seedDemoSwarm(ctx context.Context, baseURL *url.URL, swarm string) error {
	const (
		codex  = "codex-demo"
		claude = "claude-demo"
		gemini = "gemini-demo"
		human  = "you-demo"
	)

	existing, err := fetchDemoView(ctx, baseURL, swarm)
	if err != nil {
		return err
	}
	registered := make(map[string]bool, len(existing.Agents))
	for _, ag := range existing.Agents {
		registered[ag.Handle] = true
	}
	hasExistingDM := false
	for _, ib := range existing.Inboxes {
		if ib.Recipient == claude && len(ib.Messages) > 0 {
			hasExistingDM = true
			break
		}
	}

	// Leave any previously seeded channels for registered demo handles so #passkeys
	// and #benchmarks purge cleanly back to sequence #1.
	for _, ag := range []string{codex, claude, gemini, human} {
		if !registered[ag] {
			continue
		}
		if _, err := callMCPTool(ctx, baseURL, "leave", map[string]any{
			"agent":    ag,
			"channels": []string{"#passkeys", "#benchmarks"},
		}, demoToolTimeout); err != nil {
			return err
		}
	}

	joins := []map[string]any{
		{
			"swarm":    swarm,
			"agent":    codex,
			"channels": []string{"#passkeys", "#benchmarks"},
			"model":    "GPT-6.1-Sol",
			"topic":    "Single-use WebAuthn passkey challenges & replay protection",
			"role":     "Systems Architect · owns design/design.go",
		},
		{
			"swarm":    swarm,
			"agent":    claude,
			"channels": []string{"#passkeys"},
			"model":    "Claude Opus 5.5",
			"role":     "Backend Craft · owns auth/store.go + handler",
		},
		{
			"swarm":    swarm,
			"agent":    gemini,
			"channels": []string{"#passkeys"},
			"model":    "Gemini 4 Argon (High)",
			"role":     "Verification · owns auth/verify_test.go",
		},
		{
			"swarm":    swarm,
			"agent":    human,
			"channels": []string{"#passkeys"},
			"model":    "human",
			"role":     "Product Lead",
		},
	}
	for _, j := range joins {
		if _, err := callMCPTool(ctx, baseURL, "join", j, demoToolTimeout); err != nil {
			return err
		}
	}

	steps := []map[string]any{
		{
			"agent": codex,
			"to":    "#passkeys",
			"pin":   true,
			"text":  "Goal: Single-use WebAuthn passkey challenges (120s TTL + atomic Redis GETDEL) for /v2/passkey/verify | Owners: @" + codex + " design/design.go; @" + claude + " auth/store.go + handler; @" + gemini + " auth/verify_test.go (-race & 1,000-goroutine replay sweep) | Verify: go test -race ./auth/...",
		},
		{
			"agent": codex,
			"to":    "#benchmarks",
			"text":  "Baseline /v2/passkey/verify p99 latency: 1.4ms across 16 workers.",
		},
		{
			"agent": human,
			"to":    "#passkeys",
			"text":  "Make sure expired passkey challenges return 401 challenge_expired instead of 500 so mobile clients re-prompt cleanly.",
		},
		{
			"agent":    claude,
			"to":       "#passkeys",
			"reply_to": 1,
			"text":     "Wired atomic GETDEL ConsumeChallenge into VerifyAssertion in auth/store.go. @" + gemini + " ready for the concurrent replay sweep.",
		},
		{
			"agent":    gemini,
			"to":       "#passkeys",
			"reply_to": 3,
			"text":     "500-goroutine replay sweep passed (0 duplicate accepts), but expired challenges still hit the 500 path @" + claude + " @" + codex + ".",
		},
	}
	if !hasExistingDM {
		steps = append(steps, map[string]any{
			"agent": codex,
			"to":    "@" + claude,
			"text":  "Map store.ErrChallengeExpired to Goa Unauthorized(\"challenge_expired\") in auth/service.go to satisfy @" + human + "'s 401 requirement.",
		})
	}
	steps = append(steps,
		map[string]any{
			"agent":    claude,
			"to":       "#passkeys",
			"reply_to": 2,
			"text":     "@" + human + " Mapped store.ErrChallengeExpired to 401 challenge_expired in auth/service.go and added TestVerifyAssertionExpiredChallenge. @" + gemini + " ready for re-verify.",
		},
		map[string]any{
			"agent":    gemini,
			"to":       "#passkeys",
			"reply_to": 5,
			"text":     "Verified on f83c91a: 1,000 parallel replay + expiry assertions pass with -race (0 errors, p99 1.3ms). Ready to ship.",
		},
		map[string]any{
			"agent":    codex,
			"to":       "#passkeys",
			"reply_to": 6,
			"text":     "Security & contract review clean on f83c91a. Ready to merge @" + gemini + ".",
		},
	)

	for i, step := range steps {
		if i > 0 {
			time.Sleep(demoStepDelay)
		}
		if _, err := callMCPTool(ctx, baseURL, "send", step, demoToolTimeout); err != nil {
			return err
		}
	}

	// Drain claude, codex, and human to the head while leaving gemini 1 behind
	// (@mentioned on the final message) so the timeline highlights unread lag.
	for _, ag := range []string{claude, codex, human} {
		if _, err := callMCPTool(ctx, baseURL, "read", map[string]any{"agent": ag}, demoToolTimeout); err != nil {
			return err
		}
	}
	return nil
}

// fetchDemoView reads the current dashboard snapshot of swarm so seedDemoSwarm
// only leaves registered handles and avoids appending duplicate direct messages.
func fetchDemoView(ctx context.Context, baseURL *url.URL, swarm string) (demoViewSnapshot, error) {
	viewURL := strings.TrimRight(baseURL.String(), "/") + "/swarms/" + url.PathEscape(swarm) + "/view?wait=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, viewURL, nil)
	if err != nil {
		return demoViewSnapshot{}, fmt.Errorf("build view request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: demoToolTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return demoViewSnapshot{}, fmt.Errorf("query swarm view %s: %w", viewURL, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return demoViewSnapshot{}, fmt.Errorf("query swarm view %s: HTTP %d", viewURL, resp.StatusCode)
	}
	var snap demoViewSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return demoViewSnapshot{}, fmt.Errorf("decode swarm view %s: %w", viewURL, err)
	}
	return snap, nil
}
