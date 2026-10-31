// This file implements the read-only "view" HTTP service that feeds the
// browser dashboard. It turns a swarm's replicated map snapshot and retained
// Pulse stream history into one JSON document, and never writes swarm state:
// it does not register handles, bind handles to swarms, refresh presence, or
// move read positions.

package chorus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"goa.design/pulse/rmap"

	genview "github.com/goadesign/chorus/gen/view"
)

type (
	// viewService implements genview.Service on top of a Service. It shares the
	// Service's Redis client, open swarm handles, and tracer, and only reads.
	viewService struct {
		// svc owns the swarm handles and configuration the view reads from.
		svc *Service
	}
)

// errNotModified is returned by viewService.View when the caller's ETag still
// matches the swarm after the wait ends. It is an expected outcome, so spans
// do not record it as an error.
var errNotModified = errors.New("swarm view not modified")

// Swarms lists the swarms named in the agent-to-swarm bindings that join and
// who record in Redis, sorted and without duplicates. A listed swarm may have
// no live agents left when its bindings have not yet been cleaned up.
func (v *viewService) Swarms(ctx context.Context) (*genview.SwarmsResult, error) {
	ctx, span := v.svc.tracer.Start(ctx, "chorus.view.swarms")
	names, err := v.swarmNames(ctx)
	span.SetAttributes(attribute.Int("chorus.swarm_count", len(names)))
	finishSpan(span, err)
	if err != nil {
		return nil, err
	}
	return &genview.SwarmsResult{Swarms: names}, nil
}

// View returns the snapshot of p.Swarm. When p.IfNoneMatch equals the current
// snapshot's ETag, View waits up to p.Wait seconds for the swarm's replicated
// map to change and returns the new snapshot as soon as its ETag differs; if
// nothing changes, it returns a not_modified error (HTTP 304).
//
// A swarm that no agent is bound to is returned as an empty snapshot without
// opening it, so requests for arbitrary names never make the server retain a
// replicated map for them.
func (v *viewService) View(ctx context.Context, p *genview.ViewPayload) (*genview.SwarmView, error) {
	ctx, span := v.svc.tracer.Start(ctx, "chorus.view", trace.WithAttributes(
		attribute.String("chorus.swarm", p.Swarm),
		attribute.Int("chorus.wait_sec", p.Wait),
	))
	res, err := v.view(ctx, p)
	if errors.Is(err, errNotModified) {
		span.SetAttributes(attribute.Bool("chorus.view.modified", false))
		span.End()
		return nil, genview.MakeNotModified(err)
	}
	if err == nil {
		span.SetAttributes(attribute.Bool("chorus.view.modified", true))
	}
	finishSpan(span, err)
	return res, err
}

// viewGuard returns middleware that rejects requests a DNS-rebinding page or
// another website could send. The Host header must name a loopback address
// (localhost, 127.0.0.1, or ::1), because a rebound attacker page sends its own
// hostname as Host. When an Origin header is present it must be one of origins,
// which the composition root seeds with the server's own loopback origins.
// Requests without Origin (curl, tests) pass once Host is loopback. A
// browser request whose Sec-Fetch-Site says another site started it is
// rejected even without Origin, which covers <script src> loads.
func viewGuard(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		allowed[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isLoopbackHost(r.Host) || !viewOriginAllowed(r, allowed) || !sameSiteFetch(r) {
				http.Error(w, "forbidden: request must be addressed to this machine", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopbackHost reports whether hostport (an HTTP Host header value, with or
// without a port) names localhost or a loopback IP address.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameSiteFetch reports whether a browser marked r as coming from the page's
// own origin or from the user directly. Browsers send Sec-Fetch-Site on every
// request, including <script src> loads that carry no Origin header; requests
// from non-browser clients omit it and are allowed.
func sameSiteFetch(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	default:
		return false
	}
}

// viewOriginAllowed reports whether r carries no Origin header, or exactly
// one whose value is in allowed.
func viewOriginAllowed(r *http.Request, allowed map[string]struct{}) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) > 1 {
		return false
	}
	_, ok := allowed[origins[0]]
	return ok
}

// viewETag returns a quoted strong ETag hashing every field of res except Etag.
func viewETag(res *genview.SwarmView) (string, error) {
	body, err := json.Marshal(struct {
		Swarm    string
		Agents   []*genview.Agent
		Channels []*genview.Channel
		Inboxes  []*genview.Inbox
	}{res.Swarm, res.Agents, res.Channels, res.Inboxes})
	if err != nil {
		return "", fmt.Errorf("hash swarm view: %w", err)
	}
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`, nil
}

// sentAtMs returns the Unix millisecond timestamp encoded in a Redis stream
// entry ID ("<unix_ms>-<sequence>").
func sentAtMs(id string) (int64, error) {
	ms, _, _ := strings.Cut(id, "-")
	v, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse stream entry ID %q: %w", id, err)
	}
	return v, nil
}

// emptyView returns the snapshot of a swarm that has no state.
func emptyView(swarm string) (*genview.SwarmView, error) {
	res := &genview.SwarmView{
		Swarm:    swarm,
		Agents:   []*genview.Agent{},
		Channels: []*genview.Channel{},
		Inboxes:  []*genview.Inbox{},
	}
	etag, err := viewETag(res)
	if err != nil {
		return nil, err
	}
	res.Etag = etag
	return res, nil
}

// view builds the snapshot and, when the caller already holds its ETag, waits
// for the next change. It subscribes to replicated map changes before
// building the first snapshot, so a change that lands while the snapshot is
// built still wakes the wait.
func (v *viewService) view(ctx context.Context, p *genview.ViewPayload) (*genview.SwarmView, error) {
	known := ""
	if p.IfNoneMatch != nil {
		known = *p.IfNoneMatch
	}
	sw, err := v.openBoundSwarm(ctx, p.Swarm)
	if err != nil {
		return nil, err
	}
	// An unbound swarm has nothing to subscribe to: notifyCh stays nil, which
	// never fires, so the wait below runs on the timer alone. If an agent
	// binds the swarm meanwhile, the caller sees it on its next request, at
	// most p.Wait seconds later.
	var (
		notifyCh <-chan rmap.EventKind
		res      *genview.SwarmView
	)
	if sw == nil {
		res, err = emptyView(p.Swarm)
	} else {
		if known != "" && p.Wait > 0 {
			ch := sw.state.Subscribe()
			defer sw.state.Unsubscribe(ch)
			notifyCh = ch
		}
		res, err = v.build(ctx, sw)
	}
	if err != nil {
		return nil, err
	}
	if res.Etag != known {
		return res, nil
	}
	if p.Wait == 0 {
		return nil, errNotModified
	}

	// Wait phase: rebuild on every change signal until the ETag differs. A
	// message stored without a later head update fires no signal; the caller
	// still sees it on its next request, after at most p.Wait seconds.
	timer := time.NewTimer(time.Duration(p.Wait) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errNotModified
		case _, ok := <-notifyCh:
			if !ok {
				return nil, errNotModified
			}
			res, err = v.build(ctx, sw)
			if err != nil {
				return nil, err
			}
			if res.Etag != known {
				return res, nil
			}
		}
	}
}

// swarmNames returns the sorted, de-duplicated swarm names stored in the
// agent-to-swarm binding hash.
func (v *viewService) swarmNames(ctx context.Context) ([]string, error) {
	vals, err := v.svc.rdb.HVals(ctx, agentSwarmHashKey).Result()
	if err != nil {
		return nil, fmt.Errorf("list swarm bindings: %w", err)
	}
	slices.Sort(vals)
	return slices.Compact(vals), nil
}

// openBoundSwarm returns the handle for name when at least one agent is bound
// to it, or nil when none is.
func (v *viewService) openBoundSwarm(ctx context.Context, name string) (*swarmHandle, error) {
	names, err := v.swarmNames(ctx)
	if err != nil {
		return nil, err
	}
	if _, found := slices.BinarySearch(names, name); !found {
		return nil, nil
	}
	return v.svc.swarm(ctx, name)
}

// build reads sw's replicated map snapshot and every channel and inbox stream,
// and returns the complete view with its ETag.
func (v *viewService) build(ctx context.Context, sw *swarmHandle) (*genview.SwarmView, error) {
	snap := sw.state.Map()
	now := sw.clock()

	// Phase 1: live agents and their subscriptions.
	subsByAgent := sw.channelSubscriptions(snap, now)
	agents := []*genview.Agent{}
	for key, raw := range snap {
		handle, ok := strings.CutPrefix(key, prefixAgent)
		if !ok {
			continue
		}
		rec, live := parseLiveAgentRecord(now, sw.onlineWindow, raw)
		if !live {
			continue
		}
		channels := slices.Clone(subsByAgent[handle])
		slices.Sort(channels)
		if channels == nil {
			channels = []string{}
		}
		a := &genview.Agent{
			Handle:               handle,
			LastSeenUnix:         rec.LastSeenUnix,
			Channels:             channels,
			WatchingOnThisServer: sw.hasActiveWatch(handle),
		}
		if rec.Role != "" {
			a.Role = &rec.Role
		}
		if rec.Model != "" {
			a.Model = &rec.Model
		}
		agents = append(agents, a)
	}
	slices.SortFunc(agents, func(a, b *genview.Agent) int { return strings.Compare(a.Handle, b.Handle) })

	// Phase 2: channels with pins, member read positions, and history.
	channels := []*genview.Channel{}
	for key, topic := range snap {
		name, ok := strings.CutPrefix(key, prefixChannel)
		if !ok {
			continue
		}
		ch, err := v.channel(ctx, sw, snap, subsByAgent, name, topic)
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	slices.SortFunc(channels, func(a, b *genview.Channel) int { return strings.Compare(a.Name, b.Name) })

	// Phase 3: direct-message inboxes, found from live agents and from every
	// head:@<recipient> key so messages sent before a recipient registers
	// still appear.
	recipients := map[string]struct{}{}
	for _, a := range agents {
		recipients[a.Handle] = struct{}{}
	}
	for key := range snap {
		if r, ok := strings.CutPrefix(key, prefixHead+"@"); ok {
			recipients[r] = struct{}{}
		}
	}
	inboxes := []*genview.Inbox{}
	for r := range recipients {
		target := "@" + r
		msgs, err := v.messages(ctx, sw, target)
		if err != nil {
			return nil, err
		}
		if len(msgs) == 0 {
			continue
		}
		head := parseSeq(snap[headMapKey(target)])
		read := parseSeq(snap[cursorMapKey(r, target)])
		inboxes = append(inboxes, &genview.Inbox{
			Recipient: r,
			HeadSeq:   head,
			ReadSeq:   read,
			Behind:    max(0, head-read),
			Messages:  msgs,
		})
	}
	slices.SortFunc(inboxes, func(a, b *genview.Inbox) int { return strings.Compare(a.Recipient, b.Recipient) })

	res := &genview.SwarmView{
		Swarm:    sw.name,
		Agents:   agents,
		Channels: channels,
		Inboxes:  inboxes,
	}
	etag, err := viewETag(res)
	if err != nil {
		return nil, err
	}
	res.Etag = etag
	return res, nil
}

// channel builds one channel's entry: its topic, pin, the read position and
// mention state of each live subscribed member, and its retained messages.
func (v *viewService) channel(
	ctx context.Context,
	sw *swarmHandle,
	snap map[string]string,
	subsByAgent map[string][]string,
	name, topic string,
) (*genview.Channel, error) {
	head := parseSeq(snap[headMapKey(name)])
	ch := &genview.Channel{
		Name:      name,
		HeadSeq:   head,
		Positions: []*genview.Position{},
	}
	if topic != "" {
		ch.Topic = &topic
	}
	if rawPin := snap[pinMapKey(name)]; rawPin != "" {
		var pin messageEvent
		if err := json.Unmarshal([]byte(rawPin), &pin); err == nil && pin.Seq > 0 {
			ch.Pin = &genview.Pin{Seq: pin.Seq, Sender: pin.Sender, Text: pin.Text}
		}
	}
	mentionAll := parseSeq(snap[mentionMapKey("all", name)])
	for handle, subs := range subsByAgent {
		if !slices.Contains(subs, name) {
			continue
		}
		read := parseSeq(snap[cursorMapKey(handle, name)])
		mention := max(parseSeq(snap[mentionMapKey(strings.ToLower(handle), name)]), mentionAll)
		ch.Positions = append(ch.Positions, &genview.Position{
			Handle:    handle,
			ReadSeq:   read,
			Behind:    max(0, head-read),
			Mentioned: mention > read,
		})
	}
	slices.SortFunc(ch.Positions, func(a, b *genview.Position) int { return strings.Compare(a.Handle, b.Handle) })
	msgs, err := v.messages(ctx, sw, name)
	if err != nil {
		return nil, err
	}
	ch.Messages = msgs
	return ch, nil
}

// messages returns every retained message in target's stream, oldest first.
// Entries whose payload is not a message are skipped. A send stores its
// message before it advances head, so a snapshot taken during a send can list
// a message whose seq is above head_seq; the next snapshot, taken when head
// is written, is consistent again.
func (v *viewService) messages(ctx context.Context, sw *swarmHandle, target string) ([]*genview.Message, error) {
	events, err := sw.snapshotTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	msgs := make([]*genview.Message, 0, len(events))
	for _, ev := range events {
		var m messageEvent
		if err := json.Unmarshal(ev.Payload(), &m); err != nil || m.Seq <= 0 {
			continue
		}
		sentAt, err := sentAtMs(ev.ID())
		if err != nil {
			return nil, err
		}
		msg := &genview.Message{
			Seq:      m.Seq,
			Sender:   m.Sender,
			Text:     m.Text,
			SentAtMs: sentAt,
		}
		if m.ReplyTo > 0 {
			replyTo := m.ReplyTo
			msg.ReplyTo = &replyTo
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}
