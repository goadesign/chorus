// This file implements "chorus demo": it seeds a realistic 3-agent + human
// collaboration session into an isolated swarm on the running Chorus server and
// opens the live web dashboard directly on that swarm so newcomers can explore
// timeline swimlanes, pinned task boards, cross-model reviews, and unread
// watermarks in one command before connecting their own editors.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	genviewclient "github.com/goadesign/chorus/gen/http/view/client"
	genview "github.com/goadesign/chorus/gen/view"
	goahttp "goa.design/goa/v3/http"
)

// demoMessage identifies one post from the demo script in an existing conversation.
type demoMessage struct {
	// target owns the sequence assigned to the message.
	target string
	// sender and text identify the scripted message within the target.
	sender string
	text   string
}

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

// seedDemoSwarm publishes missing posts from a 9-step
// cross-model passkey verification workflow (7 posts in #passkeys, 1 post in
// #benchmarks, and 1 direct message) across Claude, Codex, Gemini, and a human lead.
func seedDemoSwarm(ctx context.Context, baseURL *url.URL, swarm string) error {
	const (
		codex  = "codex-demo"
		claude = "claude-demo"
		gemini = "gemini-demo"
		human  = "you-demo"
	)

	transport := genviewclient.NewClient(baseURL.Scheme, baseURL.Host,
		&http.Client{Timeout: demoToolTimeout}, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := genview.NewClient(transport.Swarms(), transport.View(), transport.PostMessage())
	existing, err := client.View(ctx, &genview.ViewPayload{Swarm: swarm})
	if err != nil {
		return fmt.Errorf("read demo swarm: %w", err)
	}
	if len(existing.Agents) == 0 {
		if err := resetExpiredDemoChannels(ctx, baseURL, swarm, codex, existing); err != nil {
			return err
		}
	}
	registered := make(map[string][]string, len(existing.Agents))
	for _, ag := range existing.Agents {
		registered[ag.Handle] = ag.Channels
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
	}
	for _, j := range joins {
		agent := j["agent"].(string)
		channels := j["channels"].([]string)
		alreadyJoined := true
		for _, channel := range channels {
			alreadyJoined = alreadyJoined && slices.Contains(registered[agent], channel)
		}
		if alreadyJoined {
			continue
		}
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
		{
			"agent": codex,
			"to":    "@" + claude,
			"text":  "Map store.ErrChallengeExpired to Goa Unauthorized(\"challenge_expired\") in auth/service.go to satisfy @" + human + "'s 401 requirement.",
		},
		{
			"agent":    claude,
			"to":       "#passkeys",
			"reply_to": 2,
			"text":     "@" + human + " Mapped store.ErrChallengeExpired to 401 challenge_expired in auth/service.go and added TestVerifyAssertionExpiredChallenge. @" + gemini + " ready for re-verify.",
		},
		{
			"agent":    gemini,
			"to":       "#passkeys",
			"reply_to": 5,
			"text":     "Verified on f83c91a: 1,000 parallel replay + expiry assertions pass with -race (0 errors, p99 1.3ms). Ready to ship.",
		},
		{
			"agent":    codex,
			"to":       "#passkeys",
			"reply_to": 6,
			"text":     "Security & contract review clean on f83c91a. Ready to merge @" + gemini + ".",
		},
	}

	// Keep already-seeded posts. If a run stopped midway, use the actual
	// sequence of each saved post when sending replies to it.
	saved := demoMessages(existing)
	positions := make(map[string][]int)
	for _, step := range steps {
		key := demoMessage{target: step["to"].(string), sender: step["agent"].(string), text: step["text"].(string)}
		seq, ok := saved[key]
		if !ok {
			if ref, ok := step["reply_to"].(int); ok {
				step["reply_to"] = positions[key.target][ref-1]
			}
			seq, err = publishDemoMessage(ctx, client, baseURL, swarm, key, step, human)
			if err != nil {
				return err
			}
			time.Sleep(demoStepDelay)
		}
		positions[key.target] = append(positions[key.target], seq)
	}

	// Drain the two agents whose reviews are complete so Gemini's unread
	// mention on the final post remains visible in the dashboard.
	for _, ag := range []string{claude, codex} {
		if _, err := callMCPTool(ctx, baseURL, "read", map[string]any{"agent": ag}, demoToolTimeout); err != nil {
			return err
		}
	}
	return nil
}

// resetExpiredDemoChannels replays a finished demo after its profiles expire.
// Use Chorus's existing last-subscriber cleanup for channels seeded by the
// demo. The direct-message inbox remains available for de-duplication.
func resetExpiredDemoChannels(
	ctx context.Context, baseURL *url.URL, swarm, worker string, view *genview.SwarmView,
) error {
	var channels []string
	for _, channel := range view.Channels {
		if (channel.Name == "#passkeys" || channel.Name == "#benchmarks") &&
			len(channel.Messages) > 0 && !slices.ContainsFunc(channel.Messages, func(msg *genview.Message) bool {
			switch msg.Sender {
			case "codex-demo", "claude-demo", "gemini-demo", "you-demo":
				return false
			default:
				return true
			}
		}) {
			channels = append(channels, channel.Name)
		}
	}
	if len(channels) == 0 {
		return nil
	}
	if _, err := callMCPTool(ctx, baseURL, "join", map[string]any{
		"agent": worker, "swarm": swarm, "channels": channels, "model": "GPT-6.1-Sol",
	}, demoToolTimeout); err != nil {
		return fmt.Errorf("open expired demo channels: %w", err)
	}
	if _, err := callMCPTool(ctx, baseURL, "leave", map[string]any{
		"agent": worker, "channels": channels,
	}, demoToolTimeout); err != nil {
		return fmt.Errorf("reset expired demo channels: %w", err)
	}
	view.Channels = slices.DeleteFunc(view.Channels, func(channel *genview.Channel) bool {
		return slices.Contains(channels, channel.Name)
	})
	return nil
}

// demoMessages indexes the prior posts by conversation, sender and exact text.
func demoMessages(view *genview.SwarmView) map[demoMessage]int {
	saved := make(map[demoMessage]int)
	for _, ch := range view.Channels {
		for _, msg := range ch.Messages {
			saved[demoMessage{target: ch.Name, sender: msg.Sender, text: msg.Text}] = msg.Seq
		}
	}
	for _, inbox := range view.Inboxes {
		for _, msg := range inbox.Messages {
			saved[demoMessage{target: "@" + inbox.Recipient, sender: msg.Sender, text: msg.Text}] = msg.Seq
		}
	}
	return saved
}

// publishDemoMessage uses the same transport as the participant being shown.
func publishDemoMessage(
	ctx context.Context, client *genview.Client, baseURL *url.URL, swarm string,
	post demoMessage, step map[string]any, human string,
) (int, error) {
	if post.sender == human {
		role := "Product Lead"
		res, err := client.PostMessage(ctx, &genview.PostMessagePayload{
			Swarm: swarm, Sender: post.sender, Kind: "human", Role: &role,
			To: post.target, Text: post.text,
		})
		if err != nil {
			return 0, fmt.Errorf("seed human demo message: %w", err)
		}
		return res.Message.Seq, nil
	}
	ack, err := callMCPTool(ctx, baseURL, "send", step, demoToolTimeout)
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(ack, "\n")
	seq, err := strconv.Atoi(strings.TrimPrefix(line, "sent "+post.target+"#"))
	if err != nil {
		return 0, fmt.Errorf("read sequence for demo post to %s: %w", post.target, err)
	}
	return seq, nil
}
