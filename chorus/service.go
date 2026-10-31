// Package chorus implements the Chorus multi-agent coordination service backed
// by Pulse replicated maps, bounded Redis streams, and leased semaphores.
//
// This file owns the Service lifecycle, the five Goa-AI MCP tool implementations
// (Who, Join, Leave, Send, Read), OpenTelemetry tracing, and HTTP handler
// assembly.
package chorus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	genchorus "github.com/goadesign/chorus/gen/chorus"
	genviewsrv "github.com/goadesign/chorus/gen/http/view/server"
	genmcpchorussrv "github.com/goadesign/chorus/gen/jsonrpc/mcp_chorus/server"
	genmcpchorus "github.com/goadesign/chorus/gen/mcp_chorus"
	genview "github.com/goadesign/chorus/gen/view"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goahttp "goa.design/goa/v3/http"
)

type (
	// Service implements genchorus.Service, coordinating multiple agents across
	// isolated swarms using Pulse replicated maps, streams, and semaphores.
	Service struct {
		// rdb is the shared Redis client backing all Pulse primitives.
		rdb *redis.Client
		// swarmTTL is the sliding TTL applied to swarm replicated maps and streams.
		swarmTTL time.Duration
		// streamMaxLen is the maximum number of messages retained per stream.
		streamMaxLen int
		// onlineWindow is the inactivity duration after which an agent's registration expires.
		onlineWindow time.Duration
		// clock returns the current time; injectable for deterministic tests.
		clock func() time.Time
		// tracer records OpenTelemetry spans for Chorus tool invocations.
		tracer trace.Tracer
		// mu guards swarms and closed.
		mu sync.Mutex
		// swarms holds active per-swarm replicated map handles keyed by swarm name.
		swarms map[string]*swarmHandle
		// closed reports whether Close has been called.
		closed bool
	}

	// Option configures a Service instance at construction time.
	Option func(*Service)
)

const (
	// defaultSwarmTTL is the default sliding TTL for swarm state and streams.
	defaultSwarmTTL = 24 * time.Hour
	// defaultStreamMaxLen is the default maximum retained messages per channel or DM stream.
	defaultStreamMaxLen = 500
	// defaultOnlineWindow is the default inactivity window after which an agent's
	// registration and channel subscriptions expire.
	defaultOnlineWindow = 5 * time.Minute
	// healthPingTimeout bounds the Redis ping executed during GET /healthz probes.
	healthPingTimeout = 500 * time.Millisecond
	// instrumentationName is the OpenTelemetry tracer name for Chorus.
	instrumentationName = "goa.design/chorus"
	// agentSwarmHashKey is the Redis hash key mapping active agent handles to
	// their bound swarm names across cluster replicas.
	agentSwarmHashKey = "chorus:agents"
)

var (
	// errPinRequiresChannel is returned when a caller sets pin=true on a direct
	// message target ("@agent") instead of a channel ("#channel").
	errPinRequiresChannel = errors.New("invalid arguments: pin is only valid for #channel targets")
	// errInvalidCallerState marks caller precondition errors (unregistered agent
	// handle, unjoined channel target, or duplicate concurrent watch) so spans
	// are not marked with codes.Error.
	errInvalidCallerState = errors.New("invalid caller state")
)

// New constructs a Chorus Service backed by rdb. Callers should invoke Close
// when shutting down to release active Pulse replicated map subscriptions.
func New(rdb *redis.Client, opts ...Option) (*Service, error) {
	if rdb == nil {
		return nil, errors.New("chorus: redis client is required")
	}
	svc := &Service{
		rdb:          rdb,
		swarmTTL:     defaultSwarmTTL,
		streamMaxLen: defaultStreamMaxLen,
		onlineWindow: defaultOnlineWindow,
		clock:        time.Now,
		tracer:       otel.Tracer(instrumentationName),
		swarms:       make(map[string]*swarmHandle),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(svc)
		}
	}
	if svc.swarmTTL <= 0 {
		return nil, errors.New("chorus: swarm TTL must be positive")
	}
	if svc.streamMaxLen <= 0 {
		return nil, errors.New("chorus: stream max length must be positive")
	}
	if svc.onlineWindow <= 0 {
		return nil, errors.New("chorus: online window must be positive")
	}
	if svc.clock == nil {
		return nil, errors.New("chorus: clock function must not be nil")
	}
	return svc, nil
}

// WithSwarmTTL configures the sliding expiration applied to swarm replicated
// maps and message streams.
func WithSwarmTTL(ttl time.Duration) Option {
	return func(s *Service) {
		s.swarmTTL = ttl
	}
}

// WithStreamMaxLen configures the maximum number of messages retained in each
// channel or direct-message stream.
func WithStreamMaxLen(maxLen int) Option {
	return func(s *Service) {
		s.streamMaxLen = maxLen
	}
}

// WithOnlineWindow configures how long after its last call or watcher heartbeat
// an agent remains registered in a swarm before expiring.
func WithOnlineWindow(window time.Duration) Option {
	return func(s *Service) {
		s.onlineWindow = window
	}
}

// WithClock overrides the time source used for agent liveness timestamps.
func WithClock(clock func() time.Time) Option {
	return func(s *Service) {
		s.clock = clock
	}
}

// NewHTTPHandler mounts the generated Goa-AI MCP 2026-07-28 JSON-RPC server for
// svc at POST /mcp, the embedded dashboard page at GET /, the read-only
// dashboard data routes GET /swarms and
// GET /swarms/{swarm}/view, and a liveness probe reporting X-Chorus-Version at
// GET /healthz, returning the resulting http.Handler. origins lists the
// browser Origin values allowed in addition to the dashboard's own loopback
// origin; the dashboard routes also require a loopback Host header.
func NewHTTPHandler(svc *Service, origins ...string) http.Handler {
	chorusEndpoints := genchorus.NewEndpoints(svc)
	mcpAdapter := genmcpchorus.NewMCPAdapter(chorusEndpoints, nil)
	mcpEndpoints := genmcpchorus.NewEndpoints(mcpAdapter)
	mux := goahttp.NewMuxer()
	srv := genmcpchorussrv.New(
		mcpEndpoints,
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		defaultErrorHandler,
		origins...,
	)
	genmcpchorussrv.Mount(mux, srv)
	viewSrv := genviewsrv.New(
		genview.NewEndpoints(&viewService{svc: svc}),
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		defaultErrorHandler,
		nil,
	)
	viewSrv.Use(viewGuard(origins))
	genviewsrv.Mount(mux, viewSrv)
	mountUI(mux)
	mux.Handle(http.MethodGet, "/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Chorus-Version", genchorus.APIVersion)
		if err := svc.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "redis unreachable: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Close stops all active Pulse replicated map subscriptions owned by s.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	handles := make([]*swarmHandle, 0, len(s.swarms))
	for _, h := range s.swarms {
		handles = append(handles, h)
	}
	clear(s.swarms)
	s.mu.Unlock()

	for _, h := range handles {
		h.close()
	}
	return nil
}

// Ping verifies connectivity to the backing Redis instance with a short bounded
// timeout, returning an error if the service is closed or Redis is unreachable.
func (s *Service) Ping(ctx context.Context) error {
	if s == nil || s.rdb == nil {
		return errors.New("chorus: service or redis client is nil")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("chorus: service is closed")
	}
	s.mu.Unlock()

	pingCtx, cancel := context.WithTimeout(ctx, healthPingTimeout)
	defer cancel()
	return s.rdb.Ping(pingCtx).Err()
}

// Who lists all participants, channels, subscriptions, and unread counts in the
// caller's swarm from the local in-memory Pulse replicated map.
func (s *Service) Who(ctx context.Context, p *genchorus.WhoPayload) (res *genchorus.TextResult, err error) {
	ctx, span := startToolSpan(ctx, s.tracer, "who", p.Swarm, p.Agent)
	defer func() {
		finishSpan(span, err)
	}()

	sw, err := s.checkAgentSwarmAvailable(ctx, p.Agent, p.Swarm)
	if err != nil {
		return nil, err
	}
	role := ""
	if p.Role != nil {
		role = *p.Role
	}
	out, err := sw.directory(ctx, p.Agent, role)
	if err != nil {
		return nil, err
	}
	if err := s.recordAgentSwarm(ctx, p.Agent, p.Swarm); err != nil {
		return nil, err
	}
	return textResult(out), nil
}

// Join creates or subscribes the calling agent to one or more channels,
// returning a compact summary and the most recent tail messages.
func (s *Service) Join(ctx context.Context, p *genchorus.JoinPayload) (res *genchorus.TextResult, err error) {
	ctx, span := startToolSpan(ctx, s.tracer, "join", p.Swarm, p.Agent)
	defer func() {
		finishSpan(span, err)
	}()

	sw, err := s.checkAgentSwarmAvailable(ctx, p.Agent, p.Swarm)
	if err != nil {
		return nil, err
	}
	topic := ""
	if p.Topic != nil {
		topic = *p.Topic
	}
	role := ""
	if p.Role != nil {
		role = *p.Role
	}
	out, err := sw.joinChannels(ctx, p.Agent, p.Channels, topic, role, p.Tail)
	if err != nil {
		return nil, err
	}
	if err := s.recordAgentSwarm(ctx, p.Agent, p.Swarm); err != nil {
		return nil, err
	}
	return textResult(out), nil
}

// Leave unsubscribes the calling agent from one or more channels and removes
// their server-side read watermarks.
func (s *Service) Leave(ctx context.Context, p *genchorus.LeavePayload) (res *genchorus.TextResult, err error) {
	ctx, span := startToolSpan(ctx, s.tracer, "leave", "", p.Agent)
	defer func() {
		finishSpan(span, err)
	}()

	sw, err := s.resolveAgentSwarm(ctx, p.Agent)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("chorus.swarm", sw.name))
	out, err := sw.leaveChannels(ctx, p.Agent, p.Channels)
	if err != nil {
		return nil, err
	}
	if !sw.isLiveAgent(p.Agent) {
		_ = s.rdb.HDel(ctx, agentSwarmHashKey, p.Agent).Err()
	}
	return textResult(out), nil
}

// Send publishes a message to a channel ("#channel") or direct recipient
// ("@agent"), optionally pinning it as the channel's collaboration manifest,
// piggybacks any unread messages waiting for the caller, and optionally blocks
// up to WaitSec seconds waiting for a reply.
func (s *Service) Send(ctx context.Context, p *genchorus.SendPayload) (res *genchorus.TextResult, err error) {
	ctx, span := startToolSpan(ctx, s.tracer, "send", "", p.Agent)
	span.SetAttributes(attribute.String("chorus.to", p.To))
	defer func() {
		finishSpan(span, err)
	}()

	if p.Pin && !strings.HasPrefix(p.To, "#") {
		return nil, errPinRequiresChannel
	}

	sw, err := s.resolveAgentSwarm(ctx, p.Agent)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("chorus.swarm", sw.name))
	replyTo := 0
	if p.ReplyTo != nil {
		replyTo = *p.ReplyTo
	}
	out, err := sw.publish(ctx, p.Agent, p.To, p.Text, replyTo, p.Pin, p.WaitSec)
	if err != nil {
		return nil, err
	}
	return textResult(out), nil
}

// Read drains unread messages across the caller's direct messages and
// subscribed channels, optionally blocking up to WaitSec seconds when no
// matching unread messages are immediately available.
func (s *Service) Read(ctx context.Context, p *genchorus.ReadPayload) (res *genchorus.TextResult, err error) {
	ctx, span := startToolSpan(ctx, s.tracer, "read", "", p.Agent)
	defer func() {
		finishSpan(span, err)
	}()

	sw, err := s.resolveAgentSwarm(ctx, p.Agent)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("chorus.swarm", sw.name))
	from := ""
	if p.From != nil {
		from = *p.From
	}
	out, err := sw.readUnread(ctx, p.Agent, from, p.WaitSec, p.Limit, p.MentionsOnly)
	if err != nil {
		return nil, err
	}
	return textResult(out), nil
}

// defaultErrorHandler writes uncaught transport-level errors as HTTP 500 responses.
func defaultErrorHandler(_ context.Context, w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// startToolSpan starts an OpenTelemetry span annotated with the swarm, agent,
// and tool name.
func startToolSpan(
	ctx context.Context,
	tracer trace.Tracer,
	tool string,
	swarm string,
	agent string,
) (context.Context, trace.Span) {
	return tracer.Start(
		ctx,
		"chorus."+tool,
		trace.WithAttributes(
			attribute.String("chorus.swarm", swarm),
			attribute.String("chorus.agent", agent),
			attribute.String("chorus.tool", tool),
		),
	)
}

// finishSpan records err on span if non-nil and not a context cancellation or
// caller input validation error, ending the span.
func finishSpan(span trace.Span, err error) {
	if err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, errPinRequiresChannel) &&
		!errors.Is(err, errInvalidCallerState) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// swarm returns the active swarmHandle for name, joining its Pulse replicated
// map on first access.
func (s *Service) swarm(ctx context.Context, name string) (*swarmHandle, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("chorus: service is closed")
	}
	if h, ok := s.swarms[name]; ok {
		s.mu.Unlock()
		return h, nil
	}
	s.mu.Unlock()

	created, err := openSwarm(ctx, s.rdb, name, s.swarmTTL, s.streamMaxLen, s.onlineWindow, s.clock)
	if err != nil {
		return nil, fmt.Errorf("open swarm %q: %w", name, err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		created.close()
		return nil, errors.New("chorus: service is closed")
	}
	if existing, ok := s.swarms[name]; ok {
		s.mu.Unlock()
		created.close()
		return existing, nil
	}
	s.swarms[name] = created
	s.mu.Unlock()
	return created, nil
}

// openSwarms returns a snapshot of all currently open swarmHandles on s.
func (s *Service) openSwarms() []*swarmHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	handles := make([]*swarmHandle, 0, len(s.swarms))
	for _, h := range s.swarms {
		handles = append(handles, h)
	}
	return handles
}

// checkAgentSwarmAvailable verifies that agent is not currently active in a
// different swarm and returns the swarmHandle for swarmName.
func (s *Service) checkAgentSwarmAvailable(ctx context.Context, agent, swarmName string) (*swarmHandle, error) {
	for _, other := range s.openSwarms() {
		if other.name != swarmName && other.isLiveAgent(agent) {
			return nil, fmt.Errorf(
				"%w: agent %q is already registered in swarm %q; choose a distinct handle or leave %q first",
				errInvalidCallerState, agent, other.name, other.name,
			)
		}
	}
	if boundSwarm, err := s.rdb.HGet(ctx, agentSwarmHashKey, agent).Result(); err == nil && boundSwarm != "" && boundSwarm != swarmName {
		if other, openErr := s.swarm(ctx, boundSwarm); openErr == nil && other.isLiveAgent(agent) {
			return nil, fmt.Errorf(
				"%w: agent %q is already registered in swarm %q; choose a distinct handle or leave %q first",
				errInvalidCallerState, agent, other.name, other.name,
			)
		}
	}
	return s.swarm(ctx, swarmName)
}

// recordAgentSwarm records the active agent-to-swarm binding in Redis after a
// successful who or join operation.
func (s *Service) recordAgentSwarm(ctx context.Context, agent, swarmName string) error {
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, agentSwarmHashKey, agent, swarmName)
	pipe.Expire(ctx, agentSwarmHashKey, s.swarmTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("bind agent %q to swarm %q: %w", agent, swarmName, err)
	}
	return nil
}

// resolveAgentSwarm returns the active swarmHandle in which agent is currently
// registered, or an errInvalidCallerState error when agent is unknown or expired.
func (s *Service) resolveAgentSwarm(ctx context.Context, agent string) (*swarmHandle, error) {
	for _, sw := range s.openSwarms() {
		if sw.isLiveAgent(agent) {
			return sw, nil
		}
	}
	if boundSwarm, err := s.rdb.HGet(ctx, agentSwarmHashKey, agent).Result(); err == nil && boundSwarm != "" {
		if sw, openErr := s.swarm(ctx, boundSwarm); openErr == nil {
			if sw.isLiveAgent(agent) {
				return sw, nil
			}
			_ = s.rdb.HDel(ctx, agentSwarmHashKey, agent).Err()
			if live := sw.liveAgents(); len(live) > 0 {
				return nil, fmt.Errorf(
					"%w: unknown or expired agent %q in swarm %q (registered: %s); call join first",
					errInvalidCallerState, agent, sw.name, formatHandleList(live),
				)
			}
			return nil, fmt.Errorf(
				"%w: unknown or expired agent %q in swarm %q; call join first",
				errInvalidCallerState, agent, sw.name,
			)
		}
	}

	open := s.openSwarms()
	if len(open) == 1 {
		if live := open[0].liveAgents(); len(live) > 0 {
			return nil, fmt.Errorf(
				"%w: unknown agent %q in swarm %q (registered: %s); call join first or use your joined handle",
				errInvalidCallerState, agent, open[0].name, formatHandleList(live),
			)
		}
	}
	return nil, fmt.Errorf(
		"%w: unknown agent %q; call join or who first",
		errInvalidCallerState, agent,
	)
}
