// Package chorus_test verifies the Chorus MCP server end-to-end over HTTP and
// Redis using emulated LLM agent loops and the generated Goa-AI MCP 2026-07-28
// client.
package chorus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goadesign/chorus/chorus"
	genmcpchorusclient "github.com/goadesign/chorus/gen/jsonrpc/mcp_chorus/client"
	genmcpchorus "github.com/goadesign/chorus/gen/mcp_chorus"
	"github.com/redis/go-redis/v9"
	goahttp "goa.design/goa/v3/http"
)

type (
	// testCluster manages a shared Redis instance and one or more Chorus HTTP
	// server replicas for multi-agent integration tests.
	testCluster struct {
		// t is the owning test handle.
		t *testing.T
		// rdb is the Redis client connected to the test Redis server.
		rdb *redis.Client
		// replicas holds the active Chorus HTTP server replicas sharing rdb.
		replicas []*testReplica
		// clockMu guards now.
		clockMu sync.Mutex
		// now is the synthetic clock time shared across replicas in the cluster.
		now time.Time
	}

	// testReplica is a single Chorus Service and HTTP server instance.
	testReplica struct {
		// svc is the Chorus Service instance for this replica.
		svc *chorus.Service
		// httpServer is the in-process HTTP server serving POST /mcp.
		httpServer *httptest.Server
		// mcpClient is the generated Goa-AI MCP 2026-07-28 client targeting httpServer.
		mcpClient *genmcpchorusclient.Client
	}

	// emulatedAgent simulates an LLM agent connected to a Chorus replica via
	// POST /mcp. It discovers tool schemas via tools/list, validates outgoing
	// arguments against the model-visible InputSchema, and parses compact Chorus
	// wire responses to drive coordination decisions.
	emulatedAgent struct {
		// t is the owning test handle.
		t *testing.T
		// swarm is the swarm namespace passed in tool arguments (or "" to use the default swarm).
		swarm string
		// handle is the agent's handle (without '@').
		handle string
		// replica is the Chorus server replica this agent is connected to.
		replica *testReplica
		// schemas maps tool names to their advertised JSON Schema properties.
		schemas map[string]map[string]any
	}

	// parsedSendResult represents the structured information an LLM extracts
	// from a Chorus "send" wire response.
	parsedSendResult struct {
		// target is the destination echoed in "sent <target>#<seq>".
		target string
		// seq is the sequence number assigned to the sent message.
		seq int
		// unread holds any piggybacked messages returned under "--- unread ---".
		unread []parsedMessage
	}

	// parsedMessage represents a single channel or direct message parsed from
	// Chorus's compact line-oriented wire format.
	parsedMessage struct {
		// raw is the unparsed wire line.
		raw string
		// target is "#channel" for channel posts or "@me" for direct messages.
		target string
		// seq is the message's sequence number within target.
		seq int
		// replyTo is the referenced sequence number when "↩#<n>" is present, or 0.
		replyTo int
		// sender is the author's handle (without '@').
		sender string
		// text is the message body.
		text string
	}
)

var (
	// sharedRedisAddr holds the address of the Redis server started by TestMain.
	sharedRedisAddr string

	// channelMsgRegex matches "#chan#14 @sender: text", "#chan#14↩#12 @sender: text",
	// "#chan#14 @sender (human): text", and pinned manifest lines "pin #chan#1 @sender: text".
	channelMsgRegex = regexp.MustCompile(`^(?:pin )?(#[a-zA-Z0-9_-]+)#(\d+)(?:↩#(\d+))? @([a-zA-Z0-9_-]+)(?: \(human\))?: (.*)$`)

	// dmMsgRegex matches "@sender->me#3: text", "@sender->me#3↩#2: text", and "@sender (human)->me#3: text".
	dmMsgRegex = regexp.MustCompile(`^@([a-zA-Z0-9_-]+)(?: \(human\))?->me#(\d+)(?:↩#(\d+))?: (.*)$`)
)

// TestMain boots a shared local redis-server instance for all integration tests
// in chorus_test and tears it down after the suite finishes.
func TestMain(m *testing.M) {
	addr, cleanup, err := startSharedRedis()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start test redis: %v\n", err)
		os.Exit(1)
	}
	sharedRedisAddr = addr
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestMCPProtocolAndValidationFlows verifies the GET /healthz probe, MCP
// discovery and tool schemas at POST /mcp, default swarm resolution, Origin
// header enforcement, and boundary input validation.
func TestMCPProtocolAndValidationFlows(t *testing.T) {
	c := newTestCluster(t, 1, nil, "https://allowed.example.com")
	replica := c.replicas[0]
	ctx := context.Background()

	// 1. Verify GET /healthz liveness probe and X-Chorus-Version header.
	healthResp, err := replica.httpServer.Client().Get(replica.httpServer.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /healthz, got %d", healthResp.StatusCode)
	}
	if gotVer := healthResp.Header.Get("X-Chorus-Version"); gotVer != genmcpchorus.APIVersion {
		t.Fatalf("expected X-Chorus-Version %q from /healthz, got %q", genmcpchorus.APIVersion, gotVer)
	}

	// 2. Verify server/discover and tools/list schemas via generated MCP client.
	discoverAny, err := replica.mcpClient.ServerDiscover()(ctx, &genmcpchorus.DiscoverPayload{})
	if err != nil {
		t.Fatalf("ServerDiscover failed: %v", err)
	}
	discoverRes := discoverAny.(*genmcpchorus.DiscoverResult)
	if discoverRes.Capabilities == nil || discoverRes.Capabilities.Tools == nil {
		t.Fatalf("expected tools capability in ServerDiscover, got %+v", discoverRes)
	}

	lead := c.newAgent(0, "", "lead")
	wantTools := []string{"who", "join", "leave", "send", "read"}
	if len(lead.schemas) != len(wantTools) {
		t.Fatalf("expected %d tools, got %d", len(wantTools), len(lead.schemas))
	}
	for _, name := range wantTools {
		props, ok := lead.schemas[name]
		if !ok {
			t.Fatalf("missing tool %q in tools/list", name)
		}
		if _, hasAgent := props["agent"]; !hasAgent {
			t.Fatalf("tool %q missing 'agent' property in InputSchema", name)
		}
		_, hasSwarm := props["swarm"]
		if name == "who" || name == "join" {
			if !hasSwarm {
				t.Fatalf("tool %q missing 'swarm' property in InputSchema", name)
			}
		} else if hasSwarm {
			t.Fatalf("tool %q must not expose 'swarm' property in InputSchema", name)
		}
	}
	if _, hasWait := lead.schemas["send"]["wait_sec"]; !hasWait {
		t.Fatal("tool 'send' missing 'wait_sec' property in InputSchema")
	}

	// Verify who registers @lead in the specified swarm.
	defaultWho := lead.call("who", map[string]any{
		"role": "Default Swarm Lead",
	})
	if !strings.Contains(defaultWho, "@lead (Default Swarm Lead)\n") {
		t.Fatalf("expected default swarm who output to contain @lead, got:\n%s", defaultWho)
	}

	// 3. Verify legacy MCP 2025-11-25 initialize & ping compatibility at POST /mcp.
	initResp := postRawJSONRPC(t, replica.httpServer.URL+"/mcp", "", "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-llm-host", "version": "1.0.0"},
		},
	})
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for legacy initialize, got %d", initResp.StatusCode)
	}
	_ = initResp.Body.Close()

	pingResp := postRawJSONRPC(t, replica.httpServer.URL+"/mcp", "", "2025-11-25", map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "ping",
	})
	if pingResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for legacy ping, got %d", pingResp.StatusCode)
	}
	_ = pingResp.Body.Close()

	// 4. Verify HTTP Origin guard (allowed origin vs disallowed origin) and HTTP method guard.
	allowedResp := postRawJSONRPC(t, replica.httpServer.URL+"/mcp", "https://allowed.example.com", "2025-11-25", map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "ping",
	})
	if allowedResp.StatusCode != http.StatusOK {
		t.Fatalf("expected allowed Origin to return 200, got %d", allowedResp.StatusCode)
	}
	_ = allowedResp.Body.Close()

	forbiddenResp := postRawJSONRPC(t, replica.httpServer.URL+"/mcp", "https://evil.example.com", "2025-11-25", map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "ping",
	})
	if forbiddenResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected disallowed Origin to return 403 Forbidden, got %d", forbiddenResp.StatusCode)
	}
	_ = forbiddenResp.Body.Close()

	getReq, err := http.NewRequest(http.MethodGet, replica.httpServer.URL+"/mcp", nil)
	if err != nil {
		t.Fatalf("new GET request: %v", err)
	}
	getResp, err := replica.httpServer.Client().Do(getReq)
	if err != nil {
		t.Fatalf("do GET request: %v", err)
	}
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected GET on MCP endpoint to return 405 Method Not Allowed, got %d", getResp.StatusCode)
	}

	// 5. Verify input validation errors on malformed tool arguments.
	lead.callExpectingError("join", map[string]any{
		"channels": []string{"missing_hash_prefix"},
	})
	lead.callExpectingError("send", map[string]any{
		"to":   "neither_channel_nor_agent",
		"text": "hello",
	})
	lead.callExpectingError("send", map[string]any{
		"to":   "@api",
		"text": "cannot pin a direct message",
		"pin":  true,
	})
	lead.callExpectingError("read", map[string]any{
		"from": "@dm_not_allowed_in_from",
	})
	lead.callExpectingError("read", map[string]any{
		"wait_sec": 601,
	})
	lead.callExpectingError("read", map[string]any{
		"limit": 0,
	})
	lead.callExpectingError("who", map[string]any{
		"agent": "@invalid-agent-handle",
	})
	lead.callExpectingError("who", map[string]any{
		"swarm": "invalid/swarm",
	})

	// 6. Verify identity, channel membership, and single-active-watch enforcement.
	lead.call("join", map[string]any{
		"channels": []string{"#auth"},
	})
	ghost := c.newAgent(0, "", "ghost")
	ghost.callExpectingError("read", map[string]any{})
	ghost.callExpectingError("send", map[string]any{
		"to":   "@lead",
		"text": "unregistered dm",
	})
	ghost.callExpectingError("leave", map[string]any{
		"channels": []string{"#auth"},
	})
	lead.callExpectingError("send", map[string]any{
		"to":   "#unjoined",
		"text": "must join channel before sending",
	})
	lead.callExpectingError("read", map[string]any{
		"from": "#unjoined",
	})

	// Start a blocking watch for @lead and verify:
	//  - a second concurrent watch for @lead is rejected immediately,
	//  - non-blocking read(wait_sec=0) succeeds, and
	//  - send by @lead omits the "! no active watch" warning while its watch is active,
	//    whereas send without an active watch includes the warning.
	watchStarted := make(chan struct{})
	watchDone := make(chan string, 1)
	go func() {
		close(watchStarted)
		watchDone <- lead.call("read", map[string]any{
			"wait_sec": 2,
		})
	}()
	<-watchStarted
	time.Sleep(40 * time.Millisecond)

	lead.callExpectingError("read", map[string]any{
		"wait_sec": 5,
	})
	if got := lead.call("read", map[string]any{"wait_sec": 0}); got != "none" {
		t.Fatalf("expected non-blocking read during active watch to return 'none', got %q", got)
	}
	activeSendRaw := lead.call("send", map[string]any{
		"to":   "#auth",
		"text": "posting while my watch is active",
	})
	if strings.Contains(activeSendRaw, "! watch:") {
		t.Fatalf("expected send during active watch to omit watch warning, got:\n%s", activeSendRaw)
	}

	peer := c.newAgent(0, "", "peer")
	peer.call("join", map[string]any{
		"channels": []string{"#auth"},
	})
	peer.send("#auth", "wake @lead from active watch", 0)
	if got := <-watchDone; !strings.Contains(got, "wake @lead from active watch") {
		t.Fatalf("expected active watch to wake on peer message, got %q", got)
	}

	unarmedSendRaw := lead.call("send", map[string]any{
		"to":   "#auth",
		"text": "posting after watch exited without re-arming",
	})
	if !strings.Contains(unarmedSendRaw, "! watch: chorus watch lead") {
		t.Fatalf("expected send without active watch to include re-arm hint, got:\n%s", unarmedSendRaw)
	}

	// Verify send with wait_sec > 0 blocks until a peer replies and returns the
	// reply in the same send response.
	sendWaitCh := make(chan string, 1)
	go func() {
		sendWaitCh <- lead.call("send", map[string]any{
			"to":       "#auth",
			"text":     "@peer ready for review?",
			"wait_sec": 3,
		})
	}()
	time.Sleep(60 * time.Millisecond)
	peer.send("#auth", "@lead LGTM!", 0)
	if got := <-sendWaitCh; !strings.Contains(got, "--- unread ---\n#auth#") || !strings.Contains(got, "@peer: @lead LGTM!") {
		t.Fatalf("expected send(wait_sec=3) to return peer reply under --- unread ---, got:\n%s", got)
	}
}

// TestHealthzAndPing verifies that Service.Ping and GET /healthz report 200 OK
// when Redis is healthy and 503 Service Unavailable when Redis is unreachable or closed.
func TestHealthzAndPing(t *testing.T) {
	// 1. Healthy service backed by shared test Redis.
	rdb := redis.NewClient(&redis.Options{Addr: sharedRedisAddr})
	defer func() { _ = rdb.Close() }()
	svc, err := chorus.New(rdb)
	if err != nil {
		t.Fatalf("chorus.New: %v", err)
	}
	defer func() { _ = svc.Close() }()

	srv := httptest.NewServer(chorus.NewHTTPHandler(svc))
	defer srv.Close()

	if err := svc.Ping(context.Background()); err != nil {
		t.Fatalf("svc.Ping failed on healthy Redis: %v", err)
	}
	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /healthz, got %d", resp.StatusCode)
	}
	if gotVer := resp.Header.Get("X-Chorus-Version"); gotVer != genmcpchorus.APIVersion {
		t.Fatalf("expected X-Chorus-Version %q on /healthz, got %q", genmcpchorus.APIVersion, gotVer)
	}

	// 2. Closed service returns 503.
	_ = svc.Close()
	if err := svc.Ping(context.Background()); err == nil {
		t.Fatal("expected svc.Ping to fail on closed service")
	}
	respClosed, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed on closed service: %v", err)
	}
	_ = respClosed.Body.Close()
	if respClosed.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from /healthz on closed service, got %d", respClosed.StatusCode)
	}

	// 3. Service with unreachable Redis returns 503.
	deadRDB := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = deadRDB.Close() }()
	deadSvc, err := chorus.New(deadRDB)
	if err != nil {
		t.Fatalf("chorus.New with dead Redis: %v", err)
	}
	defer func() { _ = deadSvc.Close() }()
	deadSrv := httptest.NewServer(chorus.NewHTTPHandler(deadSvc))
	defer deadSrv.Close()

	if err := deadSvc.Ping(context.Background()); err == nil {
		t.Fatal("expected deadSvc.Ping to fail on unreachable Redis")
	}
	respDead, err := deadSrv.Client().Get(deadSrv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed on unreachable Redis: %v", err)
	}
	_ = respDead.Body.Close()
	if respDead.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from /healthz on unreachable Redis, got %d", respDead.StatusCode)
	}
}

// TestSelfMessageSuppressionAndWatchNoSelfWake verifies that an agent never
// receives its own sent messages back on send, read, or a concurrent blocking
// read(wait_sec, mentions_only) watcher—even when sending @all or posting after
// unmentioned peer chatter.
func TestSelfMessageSuppressionAndWatchNoSelfWake(t *testing.T) {
	c := newTestCluster(t, 2, nil)
	swarm := "no-self-echo"

	arch := c.newAgent(0, swarm, "arch")
	impl := c.newAgent(1, swarm, "impl")

	arch.call("join", map[string]any{"channels": []string{"#foundry"}})
	impl.call("join", map[string]any{"channels": []string{"#foundry"}})

	// 1. @arch starts a blocking mentions_only read (simulating background
	// `chorus watch arch`).
	watchCh := make(chan string, 1)
	go func() {
		watchCh <- arch.call("read", map[string]any{
			"wait_sec":      5,
			"mentions_only": true,
		})
	}()
	time.Sleep(80 * time.Millisecond)

	// 2. @impl posts unmentioned chatter (#foundry#1), then @arch posts an @all
	// broadcast (#foundry#2) and a self-mention (#foundry#3).
	impl.send("#foundry", "routine build progress", 0)
	sendRes := arch.send("#foundry", "@all starting architecture review", 0)
	if sendRes.seq != 2 {
		t.Fatalf("expected @arch post to be #foundry#2, got %d", sendRes.seq)
	}
	// @arch receives @impl's #foundry#1 piggybacked on send, and never its own #foundry#2.
	if len(sendRes.unread) != 1 || sendRes.unread[0].seq != 1 || sendRes.unread[0].sender != "impl" {
		t.Fatalf("expected send piggyback to contain only #foundry#1 from @impl, got %+v", sendRes.unread)
	}

	archSelfMention := arch.send("#foundry", "note for @arch and @all", 0)
	if archSelfMention.seq != 3 || len(archSelfMention.unread) != 0 {
		t.Fatalf("expected #foundry#3 with 0 piggybacked unread, got %+v", archSelfMention)
	}

	// Verify @arch's concurrent watcher did NOT wake up on @arch's own @all or @arch messages!
	select {
	case unexpected := <-watchCh:
		t.Fatalf("expected @arch watcher not to wake on self-sent @all/@arch messages, but it woke with: %q", unexpected)
	case <-time.After(150 * time.Millisecond):
	}

	// 3. Now @impl mentions @arch (#foundry#4) -> @arch's watcher must wake immediately with only #foundry#4.
	impl.send("#foundry", "@arch ready for review", 0)
	select {
	case raw := <-watchCh:
		msgs := parseWireMessages(t, raw)
		if len(msgs) != 1 || msgs[0].seq != 4 || msgs[0].sender != "impl" {
			t.Fatalf("expected @arch watcher to wake with only #foundry#4 from @impl, got %+v", msgs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for @arch watcher to wake on peer mention")
	}
}

// TestEmulatedLLMAgentSwarmWorkflow simulates four LLM agents (@lead, @db,
// @api, and @qa) collaborating across two Chorus server replicas to design,
// implement, and verify a feature using who, join, send, read, and leave.
func TestEmulatedLLMAgentSwarmWorkflow(t *testing.T) {
	// Deploy a 2-replica Chorus cluster sharing Redis to verify cross-replica
	// state replication, distributed semaphore fencing, and cross-node wakeups.
	c := newTestCluster(t, 2, nil)
	swarm := "feature-auth"

	// Four emulated LLM agents connected across the two Chorus replicas:
	// - @lead and @qa connect to replica 0
	// - @db and @api connect to replica 1
	lead := c.newAgent(0, swarm, "lead")
	db := c.newAgent(1, swarm, "db")
	api := c.newAgent(1, swarm, "api")
	qa := c.newAgent(0, swarm, "qa")

	// Turn 1: @lead creates #arch and #release on replica 0 and posts the spec.
	joinLead := lead.call("join", map[string]any{
		"channels": []string{"#arch", "#release"},
		"topic":    "Auth v2 architecture and release gate",
		"role":     "Tech Lead",
	})
	if !strings.Contains(joinLead, "joined #arch (head #0)") || !strings.Contains(joinLead, "joined #release (head #0)") {
		t.Fatalf("unexpected join output for @lead:\n%s", joinLead)
	}

	specSend := lead.send("#arch", "spec: add users.session_version column and /v2/login endpoint", 0)
	if specSend.seq != 1 || len(specSend.unread) != 0 {
		t.Fatalf("expected #arch#1 with 0 unread, got %+v", specSend)
	}

	// Turn 2: @db on replica 1 calls who(), parses the wire directory to discover
	// #arch and @lead, joins #arch, reads the tail message (#arch#1), and replies.
	whoDB := db.call("who", map[string]any{
		"role": "Database Engineer",
	})
	if !strings.Contains(whoDB, "@lead (Tech Lead)\n") {
		t.Fatalf("@db on replica 1 did not see @lead from replica 0:\n%s", whoDB)
	}
	if !strings.Contains(whoDB, "#arch [unsub, head #1] - Auth v2 architecture and release gate") {
		t.Fatalf("@db on replica 1 did not see #arch head #1:\n%s", whoDB)
	}

	joinDBLines := parseWireMessages(t, db.call("join", map[string]any{
		"channels": []string{"#arch"},
	}))
	if len(joinDBLines) != 1 || joinDBLines[0].seq != 1 || joinDBLines[0].sender != "lead" {
		t.Fatalf("expected @db join tail to contain #arch#1 from @lead, got %+v", joinDBLines)
	}

	// @db replies to the spec message (#arch#1) it parsed from the join tail.
	dbReply := db.send("#arch", "migration 012 applied: users.session_version added", joinDBLines[0].seq)
	if dbReply.seq != 2 {
		t.Fatalf("expected @db reply to be #arch#2, got #%d", dbReply.seq)
	}

	// Turn 3: @lead on replica 0 sends a DM to @api; in the same turn, @lead
	// automatically receives @db's #arch#2 reply from replica 1!
	leadDMSend := lead.send("@api", "please wire /v2/login now that migration 012 is live", 0)
	if leadDMSend.seq != 1 {
		t.Fatalf("expected @api#1 DM sequence, got #%d", leadDMSend.seq)
	}
	if len(leadDMSend.unread) != 1 || leadDMSend.unread[0].target != "#arch" || leadDMSend.unread[0].seq != 2 || leadDMSend.unread[0].replyTo != 1 {
		t.Fatalf("expected @lead send to piggyback #arch#2↩#1 from @db, got %+v", leadDMSend.unread)
	}

	// Turn 4: @api on replica 1 joins #arch and #release, while @qa on replica 0
	// joins #arch (advancing its watermark to #2) and starts a blocking
	// read(from="#arch", wait_sec=5) waiting for @api's update.
	apiJoinMsgs := parseWireMessages(t, api.call("join", map[string]any{
		"channels": []string{"#arch", "#release"},
		"role":     "API Engineer",
	}))
	if len(apiJoinMsgs) != 2 || apiJoinMsgs[1].seq != 2 {
		t.Fatalf("expected @api join tail to include #arch#1 and #arch#2, got %+v", apiJoinMsgs)
	}

	qaJoinMsgs := parseWireMessages(t, qa.call("join", map[string]any{
		"channels": []string{"#arch", "#release"},
		"role":     "QA Engineer",
	}))
	if len(qaJoinMsgs) != 2 || qaJoinMsgs[1].seq != 2 {
		t.Fatalf("expected @qa join tail to include #arch#1 and #arch#2, got %+v", qaJoinMsgs)
	}

	qaWakeCh := make(chan []parsedMessage, 1)
	go func() {
		raw := qa.call("read", map[string]any{
			"from":     "#arch",
			"wait_sec": 5,
		})
		qaWakeCh <- parseWireMessages(t, raw)
	}()
	time.Sleep(40 * time.Millisecond)

	// @api posts #arch#3 on replica 1. In the same response, @api discovers
	// @lead's pending DM (@lead->me#1) via piggybacked unread delivery!
	apiArchSend := api.send("#arch", "/v2/login handler implemented and ready for QA", apiJoinMsgs[1].seq)
	if apiArchSend.seq != 3 {
		t.Fatalf("expected #arch#3, got #%d", apiArchSend.seq)
	}
	if len(apiArchSend.unread) != 1 || apiArchSend.unread[0].target != "@me" || apiArchSend.unread[0].sender != "lead" || apiArchSend.unread[0].seq != 1 {
		t.Fatalf("expected @api send to piggyback DM @lead->me#1, got %+v", apiArchSend.unread)
	}

	// Reacting to the piggybacked DM, @api sends a direct reply to @lead.
	apiDMReply := api.send("@"+apiArchSend.unread[0].sender, "ack, /v2/login is up", apiArchSend.unread[0].seq)
	if apiDMReply.seq != 1 || len(apiDMReply.unread) != 0 {
		t.Fatalf("expected @lead#1 with no unread, got %+v", apiDMReply)
	}

	// Verify @qa on replica 0 woke up immediately with only the new delta (#arch#3)
	// after @api published on replica 1.
	select {
	case qaMsgs := <-qaWakeCh:
		if len(qaMsgs) != 1 || qaMsgs[0].seq != 3 || qaMsgs[0].sender != "api" {
			t.Fatalf("expected @qa wakeup to deliver delta #arch#3 from @api, got %+v", qaMsgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("@qa blocking read on replica 0 did not wake up after @api published on replica 1")
	}

	// Turn 5: @lead on replica 0 starts a blocking read(wait_sec=5) across all
	// subscribed channels and DMs. Since @api already sent @lead a DM (#1), @lead
	// immediately drains that DM without blocking!
	leadImmediate := parseWireMessages(t, lead.call("read", map[string]any{
		"wait_sec": 5,
	}))
	if len(leadImmediate) != 2 || leadImmediate[0].target != "@me" || leadImmediate[1].target != "#arch" || leadImmediate[1].seq != 3 {
		t.Fatalf("expected @lead read to return @api's DM and #arch#3, got %+v", leadImmediate)
	}

	// Now @lead is caught up and blocks waiting for @qa's release sign-off.
	leadReleaseCh := make(chan []parsedMessage, 1)
	go func() {
		raw := lead.call("read", map[string]any{
			"wait_sec": 5,
		})
		leadReleaseCh <- parseWireMessages(t, raw)
	}()
	time.Sleep(40 * time.Millisecond)

	// @qa posts sign-off to #release.
	qaReleaseSend := qa.send("#release", "QA suite passed 42/42 checks, ship it", 0)
	if qaReleaseSend.seq != 1 {
		t.Fatalf("expected #release#1, got #%d", qaReleaseSend.seq)
	}

	select {
	case leadMsgs := <-leadReleaseCh:
		if len(leadMsgs) != 1 || leadMsgs[0].target != "#release" || leadMsgs[0].seq != 1 || leadMsgs[0].sender != "qa" {
			t.Fatalf("expected @lead wakeup with #release#1 from @qa, got %+v", leadMsgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("@lead blocking read did not wake up on #release#1")
	}

	// Turn 6: @db and @api leave #arch and verify clean unsubscription.
	if got := db.call("leave", map[string]any{"channels": []string{"#arch"}}); got != "left #arch" {
		t.Fatalf("unexpected leave response: %q", got)
	}
	whoDBAfter := db.call("who", map[string]any{})
	if !strings.Contains(whoDBAfter, "#arch [unsub, head #3]") {
		t.Fatalf("expected #arch to be marked unsub after leave, got:\n%s", whoDBAfter)
	}
}

// TestPaginationFilteringAndRetentionFlows verifies read limit pagination,
// channel-filtered reads, blocking read timeouts, and stream max-length
// trimming when an agent's watermark falls behind trimmed history.
func TestPaginationFilteringAndRetentionFlows(t *testing.T) {
	// Configure streamMaxLen = 5 so we can also test bounded stream retention.
	c := newTestCluster(t, 1, []chorus.Option{
		chorus.WithStreamMaxLen(5),
		chorus.WithOnlineWindow(2 * time.Minute),
	})
	swarm := "retention"

	writer := c.newAgent(0, swarm, "writer")
	reader := c.newAgent(0, swarm, "reader")

	// 1. Channel-filtered read(from="#alpha") must not drain DMs or "#beta".
	reader.call("join", map[string]any{
		"channels": []string{"#alpha", "#beta"},
	})
	writer.call("join", map[string]any{
		"channels": []string{"#alpha", "#beta"},
	})
	writer.send("@reader", "direct ping", 0)
	writer.send("#alpha", "alpha one", 0)
	writer.send("#beta", "beta one", 0)

	alphaOnly := parseWireMessages(t, reader.call("read", map[string]any{
		"from": "#alpha",
	}))
	if len(alphaOnly) != 1 || alphaOnly[0].target != "#alpha" || alphaOnly[0].text != "alpha one" {
		t.Fatalf("expected only #alpha#1 from filtered read, got %+v", alphaOnly)
	}

	// Reading again with from="#alpha" returns "none".
	if got := reader.call("read", map[string]any{"from": "#alpha"}); got != "none" {
		t.Fatalf("expected second filtered read on #alpha to return 'none', got %q", got)
	}

	// Unfiltered read({}) now drains the remaining DM and #beta#1 (withDM first).
	remaining := parseWireMessages(t, reader.call("read", map[string]any{}))
	if len(remaining) != 2 || remaining[0].target != "@me" || remaining[1].target != "#beta" {
		t.Fatalf("expected DM followed by #beta#1, got %+v", remaining)
	}

	// 2. Bounded read(limit=2) pagination across 5 messages.
	for i := 1; i <= 5; i++ {
		writer.send("#beta", fmt.Sprintf("batch-%d", i), 0)
	}
	page1 := parseWireMessages(t, reader.call("read", map[string]any{"limit": 2}))
	if len(page1) != 2 || page1[0].seq != 2 || page1[1].seq != 3 {
		t.Fatalf("expected page 1 to return #beta#2..#3, got %+v", page1)
	}
	page2 := parseWireMessages(t, reader.call("read", map[string]any{"limit": 2}))
	if len(page2) != 2 || page2[0].seq != 4 || page2[1].seq != 5 {
		t.Fatalf("expected page 2 to return #beta#4..#5, got %+v", page2)
	}
	page3 := parseWireMessages(t, reader.call("read", map[string]any{"limit": 2}))
	if len(page3) != 1 || page3[0].seq != 6 {
		t.Fatalf("expected page 3 to return #beta#6, got %+v", page3)
	}
	if got := reader.call("read", map[string]any{}); got != "none" {
		t.Fatalf("expected 'none' after draining all pages, got %q", got)
	}

	// 3. Stream MaxLen trimming (streamMaxLen=5): #beta now has 6 messages (#1..#6),
	// so Redis retains only #2..#6. Verify join(tail=0), join(tail=2), and join(tail=10).
	lateZero := c.newAgent(0, swarm, "late0")
	joinZero := lateZero.call("join", map[string]any{
		"channels": []string{"#beta"},
		"tail":     0,
	})
	wantJoinZero := "joined #beta (head #6)\nmembers: @late0, @reader, @writer\n! watch: chorus watch late0"
	if joinZero != wantJoinZero {
		t.Fatalf("expected tail=0 join to return header, members, and watch hint only, got:\n%s", joinZero)
	}

	lateTwo := c.newAgent(0, swarm, "late2")
	joinTwoMsgs := parseWireMessages(t, lateTwo.call("join", map[string]any{
		"channels": []string{"#beta"},
		"tail":     2,
	}))
	if len(joinTwoMsgs) != 2 || joinTwoMsgs[0].seq != 5 || joinTwoMsgs[1].seq != 6 {
		t.Fatalf("expected tail=2 join to return #beta#5..#6, got %+v", joinTwoMsgs)
	}

	lateFull := c.newAgent(0, swarm, "latefull")
	joinFullMsgs := parseWireMessages(t, lateFull.call("join", map[string]any{
		"channels": []string{"#beta"},
		"tail":     10,
	}))
	if len(joinFullMsgs) != 5 || joinFullMsgs[0].seq != 2 || joinFullMsgs[4].seq != 6 {
		t.Fatalf("expected streamMaxLen=5 to retain #beta#2..#6, got %+v", joinFullMsgs)
	}

	// 4. Read timeout when no message arrives within wait_sec=1.
	if got := lateFull.call("read", map[string]any{"wait_sec": 1}); got != "none" {
		t.Fatalf("expected timeout read to return 'none', got %q", got)
	}
}

// TestConcurrentMultiAgentHammer verifies that concurrent publishes from
// multiple agents across multiple replicas produce strictly contiguous sequence
// numbers with zero lost or duplicated messages.
func TestConcurrentMultiAgentHammer(t *testing.T) {
	c := newTestCluster(t, 2, nil)
	swarm := "hammer"

	const (
		numAgents       = 6
		msgsPerAgent    = 5
		totalExpected   = numAgents * msgsPerAgent
		channel         = "#bus"
		observerAgentID = "observer"
	)

	observer := c.newAgent(0, swarm, observerAgentID)
	observer.call("join", map[string]any{"channels": []string{channel}})

	agents := make([]*emulatedAgent, numAgents)
	for i := range numAgents {
		agents[i] = c.newAgent(i%2, swarm, fmt.Sprintf("worker-%d", i))
		agents[i].call("join", map[string]any{"channels": []string{channel}})
	}

	var wg sync.WaitGroup
	for i, ag := range agents {
		wg.Add(1)
		go func(idx int, a *emulatedAgent) {
			defer wg.Done()
			for m := 1; m <= msgsPerAgent; m++ {
				a.send(channel, fmt.Sprintf("w%d-m%d", idx, m), 0)
			}
		}(i, ag)
	}
	wg.Wait()

	// Observer drains all 30 messages and verifies dense, gap-free sequence numbers 1..30.
	allMsgs := parseWireMessages(t, observer.call("read", map[string]any{
		"limit": 50,
	}))
	if len(allMsgs) != totalExpected {
		t.Fatalf("expected observer to read %d messages, got %d", totalExpected, len(allMsgs))
	}
	for i, msg := range allMsgs {
		wantSeq := i + 1
		if msg.seq != wantSeq {
			t.Fatalf("message %d has seq %d, want dense sequence %d", i, msg.seq, wantSeq)
		}
	}
}

// TestMultiSwarmIsolationAndLiveness verifies that distinct swarm namespaces
// are completely isolated and that agent registrations and abandoned channels
// expire after the configured online activity window.
func TestMultiSwarmIsolationAndLiveness(t *testing.T) {
	c := newTestCluster(t, 1, []chorus.Option{
		chorus.WithOnlineWindow(2 * time.Minute),
	})

	alphaLead := c.newAgent(0, "swarm-alpha", "alpha-lead")
	betaLead := c.newAgent(0, "swarm-beta", "beta-lead")

	alphaLead.call("join", map[string]any{
		"channels": []string{"#arch"},
		"topic":    "Alpha Topic",
		"role":     "Alpha Lead",
	})
	alphaLead.send("#arch", "alpha secret message", 0)

	// Attempting to join a different swarm using an already-active handle must fail.
	collision := c.newAgent(0, "swarm-beta", "alpha-lead")
	collision.callExpectingError("join", map[string]any{
		"channels": []string{"#arch"},
	})

	// When @alpha-lead has an active blocking watch and leaves its last channel,
	// the watch wakes immediately, the Redis swarm binding is released, and
	// @alpha-lead can re-join swarm-alpha cleanly.
	watchDone := make(chan string, 1)
	go func() {
		watchDone <- alphaLead.call("read", map[string]any{"wait_sec": 5})
	}()
	time.Sleep(80 * time.Millisecond)
	alphaLead.call("leave", map[string]any{"channels": []string{"#arch"}})
	select {
	case out := <-watchDone:
		if out != "none" {
			t.Fatalf("expected active watch to exit cleanly with 'none' upon leaving last channel, got %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for active watch to terminate upon leaving last channel")
	}
	alphaLead.call("join", map[string]any{
		"channels": []string{"#arch"},
		"topic":    "Alpha Topic",
		"role":     "Alpha Lead",
	})

	betaLead.call("join", map[string]any{
		"channels": []string{"#arch"},
		"topic":    "Beta Topic",
		"role":     "Beta Lead",
	})

	whoBeta := betaLead.call("who", map[string]any{})
	if strings.Contains(whoBeta, "Alpha") || !strings.Contains(whoBeta, "#arch [sub, 0 unread, head #0] - Beta Topic") {
		t.Fatalf("swarm-beta observed state from swarm-alpha:\n%s", whoBeta)
	}

	// Advance synthetic clock past onlineWindow (2m) and verify @alpha-lead's stale
	// registration and abandoned #arch channel are automatically reaped both when
	// observed by a peer (swarm-alpha) and when the expired agent itself calls
	// who (swarm-beta).
	c.advanceClock(3 * time.Minute)
	alphaLead.callExpectingError("send", map[string]any{
		"to":   "#arch",
		"text": "stale agent must re-join after expiring",
	})
	alphaObserver := c.newAgent(0, "swarm-alpha", "observer")
	whoAlpha := alphaObserver.call("who", map[string]any{"role": "Observer"})
	if strings.Contains(whoAlpha, "@alpha-lead") || strings.Contains(whoAlpha, "#arch") {
		t.Fatalf("expected expired @alpha-lead and its abandoned #arch channel to be reaped, got:\n%s", whoAlpha)
	}
	if !strings.Contains(whoAlpha, "@observer (Observer)\n") {
		t.Fatalf("expected @observer in directory, got:\n%s", whoAlpha)
	}

	whoBetaExpiredSelf := betaLead.call("who", map[string]any{"role": "Beta Lead"})
	if strings.Contains(whoBetaExpiredSelf, "#arch") {
		t.Fatalf("expected expired @beta-lead calling who to reap its own stale #arch subscription, got:\n%s", whoBetaExpiredSelf)
	}
}

// TestMentionsOnlyWakeupAndFullContextDrain verifies that read with
// mentions_only=true ignores unrelated channel chatter and partial handle
// substrings without advancing watermarks, then wakes and drains full channel
// context when the agent is @mentioned, addressed via @all, replied to via
// reply_to, or sent a direct message.
func TestMentionsOnlyWakeupAndFullContextDrain(t *testing.T) {
	c := newTestCluster(t, 2, nil)
	swarm := "selective-wake"

	lead := c.newAgent(0, swarm, "lead")
	db := c.newAgent(0, swarm, "db")
	api := c.newAgent(1, swarm, "api")
	qa := c.newAgent(1, swarm, "qa")

	for _, ag := range []*emulatedAgent{lead, db, api, qa} {
		ag.call("join", map[string]any{"channels": []string{"#arch"}})
	}

	// 1. Unrelated chatter and partial substrings (@api-v2, user@api.com) must
	// not trigger mentions_only or advance @api's watermark.
	db.send("#arch", "starting schema migration 004", 0)
	lead.send("#arch", "checking @api-v2 compatibility and user@api.com fixture", 0)

	if got := api.call("read", map[string]any{"mentions_only": true}); got != "none" {
		t.Fatalf("expected mentions_only read to return 'none' on unrelated chatter, got:\n%s", got)
	}

	// 2. Mentioning @api wakes mentions_only and drains all 3 unread messages
	// (#arch#1..#3) so @api receives the surrounding conversation context.
	db.send("#arch", "migration complete, (@api) you can wire the handler now", 0)
	mentionDrain := parseWireMessages(t, api.call("read", map[string]any{"mentions_only": true}))
	if len(mentionDrain) != 3 || mentionDrain[0].seq != 1 || mentionDrain[1].seq != 2 || mentionDrain[2].seq != 3 {
		t.Fatalf("expected mentions_only wakeup to drain #arch#1..#3 for full context, got %+v", mentionDrain)
	}

	// 3. Blocking mentions_only read wakes when a peer replies (reply_to) to a
	// message authored by @api, even without typing "@api" in the reply body.
	apiPost := api.send("#arch", "VerifyAssertion handler is wired", 0)
	if apiPost.seq != 4 {
		t.Fatalf("expected @api post to be #arch#4, got %d", apiPost.seq)
	}

	replyWakeCh := make(chan []parsedMessage, 1)
	go func() {
		raw := api.call("read", map[string]any{
			"wait_sec":      5,
			"mentions_only": true,
		})
		replyWakeCh <- parseWireMessages(t, raw)
	}()

	time.Sleep(80 * time.Millisecond)
	// Unrelated post #arch#5 from @db while @api is blocking on mentions_only.
	db.send("#arch", "vacuuming index tables", 0)
	// @qa replies to @api's #arch#4 without mentioning "@api" in text (#arch#6↩#4).
	qa.send("#arch", "500 concurrent replay checks pass", apiPost.seq)

	select {
	case wokenMsgs := <-replyWakeCh:
		if len(wokenMsgs) != 2 || wokenMsgs[0].seq != 5 || wokenMsgs[1].seq != 6 || wokenMsgs[1].replyTo != 4 {
			t.Fatalf("expected reply wakeup to drain #arch#5 and #arch#6↩#4, got %+v", wokenMsgs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mentions_only read to wake on reply_to")
	}

	// 4. Case-insensitive @ALL mention and direct messages (@lead->@api) both wake mentions_only.
	lead.send("#arch", "@ALL freeze merges for release tag", 0)
	allDrain := parseWireMessages(t, api.call("read", map[string]any{"mentions_only": true}))
	if len(allDrain) != 1 || allDrain[0].seq != 7 {
		t.Fatalf("expected @ALL mention to wake mentions_only and drain #arch#7, got %+v", allDrain)
	}

	lead.send("@api", "tag v0.2.0 when ready", 0)
	dmDrain := parseWireMessages(t, api.call("read", map[string]any{"mentions_only": true}))
	if len(dmDrain) != 1 || dmDrain[0].target != "@me" || dmDrain[0].seq != 1 {
		t.Fatalf("expected direct message to wake mentions_only, got %+v", dmDrain)
	}

	// 5. Multi-channel starvation protection and FIFO pagination across mentions:
	// @api joins #alerts and #noise. #noise receives 25 unrelated messages while
	// #alerts receives #alerts#1 (mentioning @api), #alerts#2 (context), and
	// #alerts#3 (uppercase @API mention). Calling read(mentions_only: true,
	// limit: 2) must skip #noise completely, deliver #alerts#1..#2 on the first
	// call, and deliver #alerts#3 on the second call without losing any mention.
	for _, ag := range []*emulatedAgent{lead, db, api} {
		ag.call("join", map[string]any{"channels": []string{"#alerts", "#noise"}})
	}
	for i := 1; i <= 25; i++ {
		db.send("#noise", fmt.Sprintf("background job %d", i), 0)
	}
	lead.send("#alerts", "first ping: @api check auth logs", 0)
	db.send("#alerts", "correlating request IDs", 0)
	lead.send("#alerts", "second ping: @API check the latency spike", 0)

	page1 := parseWireMessages(t, api.call("read", map[string]any{
		"mentions_only": true,
		"limit":         2,
	}))
	if len(page1) != 2 ||
		page1[0].target != "#alerts" || page1[0].seq != 1 ||
		page1[1].target != "#alerts" || page1[1].seq != 2 {
		t.Fatalf("expected FIFO page 1 #alerts#1..#2 (skipping #noise), got %+v", page1)
	}

	page2 := parseWireMessages(t, api.call("read", map[string]any{
		"mentions_only": true,
		"limit":         2,
	}))
	if len(page2) != 1 || page2[0].target != "#alerts" || page2[0].seq != 3 {
		t.Fatalf("expected FIFO page 2 #alerts#3 (skipping #noise), got %+v", page2)
	}
}

// TestPinnedManifestAndChannelMembersOnJoin verifies that join returns the
// subscribed channel members (with roles) and the channel's active
// pinned collaboration manifest (deduplicated when inside tail, preserved in
// rmap even after scrolling past tail and streamMaxLen trimming, overwritten in
// place when a newer message sets pin=true, and waking mentions_only watchers).
func TestPinnedManifestAndChannelMembersOnJoin(t *testing.T) {
	c := newTestCluster(t, 2, []chorus.Option{
		chorus.WithStreamMaxLen(5),
	})
	swarm := "manifest-pin"

	arch := c.newAgent(0, swarm, "arch")
	api := c.newAgent(1, swarm, "api")
	qa := c.newAgent(0, swarm, "qa")
	db := c.newAgent(1, swarm, "db")

	// 1. @arch creates #auth and pins the initial collaboration manifest (#auth#1).
	archJoin := arch.call("join", map[string]any{
		"channels": []string{"#auth"},
		"topic":    "Passkey replay protection",
		"role":     "Architect · owns design/design.go",
	})
	if !strings.Contains(archJoin, "members: @arch (Architect · owns design/design.go)") {
		t.Fatalf("expected @arch in join members, got:\n%s", archJoin)
	}

	arch.call("send", map[string]any{
		"to":   "#auth",
		"text": "Goal: single-use challenge. @api owns service.go, @db owns store.go, @qa owns service_test.go",
		"pin":  true,
	})
	arch.send("#auth", "starting threat model review", 0)

	// 2. @api joins #auth while #auth#1 is still within tail=5. Verify members
	// includes both @api and @arch, and #auth#1 is emitted once with the "pin "
	// prefix (not duplicated in the tail list).
	apiJoinRaw := api.call("join", map[string]any{
		"channels": []string{"#auth"},
		"role":     "API · owns chorus/service.go",
		"tail":     5,
	})
	wantMembers := "members: @api (API · owns chorus/service.go), @arch (Architect · owns design/design.go)"
	if !strings.Contains(apiJoinRaw, wantMembers) {
		t.Fatalf("expected join output to include %q, got:\n%s", wantMembers, apiJoinRaw)
	}
	if !strings.Contains(apiJoinRaw, "\npin #auth#1 @arch: Goal: single-use challenge.") {
		t.Fatalf("expected join output to include pinned #auth#1 line, got:\n%s", apiJoinRaw)
	}
	apiJoinMsgs := parseWireMessages(t, apiJoinRaw)
	if len(apiJoinMsgs) != 2 || apiJoinMsgs[0].seq != 1 || apiJoinMsgs[1].seq != 2 {
		t.Fatalf("expected deduplicated #auth#1 (pinned) and #auth#2 on join, got %+v", apiJoinMsgs)
	}

	// 3. Publish 6 more messages (#auth#3..#8) so #auth#1 scrolls past tail=2
	// AND is trimmed from the Redis stream (streamMaxLen=5 retains only #4..#8).
	for i := 3; i <= 8; i++ {
		api.send("#auth", fmt.Sprintf("progress update %d", i), 0)
	}

	// 4. @db joins late with tail=2. Even though #auth#1 was trimmed from the
	// Redis stream, join still returns the pinned manifest from rmap above #7..#8.
	dbJoinRaw := db.call("join", map[string]any{
		"channels": []string{"#auth"},
		"role":     "DB · owns store/passkey.go",
		"tail":     2,
	})
	if !strings.Contains(dbJoinRaw, "\npin #auth#1 @arch: Goal: single-use challenge.") {
		t.Fatalf("expected pinned #auth#1 to survive streamMaxLen trimming on join, got:\n%s", dbJoinRaw)
	}
	dbJoinMsgs := parseWireMessages(t, dbJoinRaw)
	if len(dbJoinMsgs) != 3 || dbJoinMsgs[0].seq != 1 || dbJoinMsgs[1].seq != 7 || dbJoinMsgs[2].seq != 8 {
		t.Fatalf("expected [#1 (pin), #7, #8] from late join(tail=2), got %+v", dbJoinMsgs)
	}

	// Verify that replying (reply_to=1) to the pinned manifest (#auth#1) still
	// resolves @arch as the author from rmap and wakes @arch's mentions_only
	// watcher even after #auth#1 was trimmed from the Redis stream.
	arch.call("read", map[string]any{})
	archWakeCh := make(chan []parsedMessage, 1)
	go func() {
		raw := arch.call("read", map[string]any{
			"wait_sec":      5,
			"mentions_only": true,
		})
		archWakeCh <- parseWireMessages(t, raw)
	}()
	time.Sleep(80 * time.Millisecond)

	db.send("#auth", "store/passkey.go GETDEL ready for review", 1)
	select {
	case archWoken := <-archWakeCh:
		if len(archWoken) != 1 || archWoken[0].seq != 9 || archWoken[0].replyTo != 1 || archWoken[0].sender != "db" {
			t.Fatalf("expected @arch to wake on reply_to=#1 (pinned manifest), got %+v", archWoken)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for @arch to wake on reply_to trimmed pinned manifest")
	}

	// 5. @qa joins and blocks on read(mentions_only=true). When @arch pins an
	// updated manifest (#auth#10) with pin=true, @qa wakes immediately even
	// without an explicit @qa/@all token in the text, and the new pin replaces
	// #auth#1 in place.
	qa.call("join", map[string]any{
		"channels": []string{"#auth"},
		"role":     "QA · owns chorus/service_test.go",
	})
	qaWakeCh := make(chan []parsedMessage, 1)
	go func() {
		raw := qa.call("read", map[string]any{
			"wait_sec":      5,
			"mentions_only": true,
		})
		qaWakeCh <- parseWireMessages(t, raw)
	}()
	time.Sleep(80 * time.Millisecond)

	arch.call("send", map[string]any{
		"to":   "#auth",
		"text": "Updated manifest: 401 challenge_expired landed; ready for final QA sign-off",
		"pin":  true,
	})

	select {
	case woken := <-qaWakeCh:
		if len(woken) != 1 || woken[0].seq != 10 || woken[0].sender != "arch" {
			t.Fatalf("expected mentions_only watcher to wake on pinned #auth#10, got %+v", woken)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mentions_only watcher to wake on pinned manifest update")
	}

	rejoinRaw := qa.call("join", map[string]any{
		"channels": []string{"#auth"},
		"tail":     2,
	})
	if strings.Contains(rejoinRaw, "pin #auth#1 ") || !strings.Contains(rejoinRaw, "\npin #auth#10 @arch: Updated manifest:") {
		t.Fatalf("expected #auth#10 to overwrite #auth#1 as the single channel pin, got:\n%s", rejoinRaw)
	}

	// 6. When all subscribed members leave #auth, the channel, its pin, and its
	// stream history are removed so who no longer lists #auth and re-creating
	// #auth starts cleanly at head #0.
	for _, ag := range []*emulatedAgent{arch, api, db, qa} {
		ag.call("leave", map[string]any{"channels": []string{"#auth"}})
	}
	whoAfterAllLeft := arch.call("who", map[string]any{})
	if strings.Contains(whoAfterAllLeft, "#auth") ||
		strings.Contains(whoAfterAllLeft, "@api") ||
		strings.Contains(whoAfterAllLeft, "@db") ||
		strings.Contains(whoAfterAllLeft, "@qa") {
		t.Fatalf("expected #auth and departed agents (@api, @db, @qa) to be removed from who after leaving their last channel, got:\n%s", whoAfterAllLeft)
	}
	recreatedJoin := arch.call("join", map[string]any{
		"channels": []string{"#auth"},
		"topic":    "Fresh auth channel",
	})
	if !strings.HasPrefix(recreatedJoin, "joined #auth (head #0) - Fresh auth channel\n") ||
		len(parseWireMessages(t, recreatedJoin)) != 0 {
		t.Fatalf("expected re-created #auth to start at head #0 with no old messages, got:\n%s", recreatedJoin)
	}
	freshSend := arch.send("#auth", "first message in fresh channel", 0)
	if freshSend.seq != 1 {
		t.Fatalf("expected first message in re-created #auth to get seq #1, got #%d", freshSend.seq)
	}

	// 7. Participants that declared model "human" (such as from the browser UI)
	// are annotated with "(human)" on channel and direct-message wire lines so
	// agents immediately distinguish human steering from peer worker messages.
	// The role is free text and never decides it.
	arch.call("join", map[string]any{
		"channels": []string{"#ui-review"},
	})
	human := c.newAgent(0, swarm, "raphael")
	human.call("join", map[string]any{
		"channels": []string{"#ui-review"},
		"model":    "human",
	})
	impostor := c.newAgent(0, swarm, "worker")
	impostor.call("join", map[string]any{
		"channels": []string{"#ui-review"},
		"role":     "human",
	})
	impostor.call("send", map[string]any{
		"to":   "#ui-review",
		"text": "role text alone",
	})
	human.call("send", map[string]any{
		"to":   "#ui-review",
		"text": "Please prioritize accessibility and contrast",
	})
	human.call("send", map[string]any{
		"to":   "@arch",
		"text": "Can you check the header spacing?",
	})
	archRead := arch.call("read", map[string]any{})
	if !strings.Contains(archRead, "#ui-review#2 @raphael (human): Please prioritize accessibility and contrast") {
		t.Fatalf("expected channel message from model=human to carry (human) tag, got:\n%s", archRead)
	}
	if !strings.Contains(archRead, "@raphael (human)->me#1: Can you check the header spacing?") {
		t.Fatalf("expected direct message from model=human to carry (human) tag, got:\n%s", archRead)
	}
	if !strings.Contains(archRead, "@worker: role text alone") {
		t.Fatalf("expected role=human without model=human to stay untagged, got:\n%s", archRead)
	}

	// Directory and member lines mark a person from the declared model only,
	// outside the role's parentheses: a person with a descriptive role still
	// reads as human, and an agent whose role says "human" does not.
	human.call("who", map[string]any{"model": "human", "role": "Reviewer"})
	dir := arch.call("who", map[string]any{})
	if !strings.Contains(dir, "\nhuman @raphael (Reviewer)\n") {
		t.Fatalf("expected person listed as human with their role, got:\n%s", dir)
	}
	if !strings.Contains(dir, "\n@worker (human)\n") {
		t.Fatalf("expected agent with role \"human\" listed as an agent, got:\n%s", dir)
	}
	members := human.call("join", map[string]any{"channels": []string{"#ui-review"}, "model": "human"})
	if !strings.Contains(members, "human @raphael (Reviewer)") {
		t.Fatalf("expected members line to keep the declared role and mark the person, got:\n%s", members)
	}
	parsedHuman := parseWireMessages(t, archRead)
	if len(parsedHuman) != 3 {
		t.Fatalf("expected parseWireMessages to parse all three messages, got %d", len(parsedHuman))
	}
}

// newTestCluster flushes the shared Redis database and starts numReplicas
// Chorus HTTP server replicas sharing Redis and a synthetic clock.
func newTestCluster(t *testing.T, numReplicas int, opts []chorus.Option, origins ...string) *testCluster {
	t.Helper()

	rdb := redis.NewClient(&redis.Options{Addr: sharedRedisAddr})
	t.Cleanup(func() {
		_ = rdb.Close()
	})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("FlushDB failed: %v", err)
	}

	cluster := &testCluster{
		t:   t,
		rdb: rdb,
		now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	}

	baseOpts := []chorus.Option{
		chorus.WithClock(cluster.currentTime),
	}
	baseOpts = append(baseOpts, opts...)

	for range numReplicas {
		svc, err := chorus.New(rdb, baseOpts...)
		if err != nil {
			t.Fatalf("chorus.New: %v", err)
		}
		httpSrv := httptest.NewServer(chorus.NewHTTPHandler(svc, origins...))
		parsedURL, err := url.Parse(httpSrv.URL)
		if err != nil {
			t.Fatalf("parse replica URL: %v", err)
		}
		mcpClient := genmcpchorusclient.NewClient(
			parsedURL.Scheme,
			parsedURL.Host,
			httpSrv.Client(),
			goahttp.RequestEncoder,
			goahttp.ResponseDecoder,
			false,
		)
		rep := &testReplica{
			svc:        svc,
			httpServer: httpSrv,
			mcpClient:  mcpClient,
		}
		cluster.replicas = append(cluster.replicas, rep)
		t.Cleanup(func() {
			httpSrv.Close()
			_ = svc.Close()
		})
	}
	return cluster
}

// parseWireMessages extracts all channel and direct message lines from a Chorus
// wire response (skipping headers like "joined #arch (head #2)" or "none").
func parseWireMessages(t *testing.T, raw string) []parsedMessage {
	t.Helper()

	if raw == "" || raw == "none" {
		return nil
	}
	var out []parsedMessage
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" ||
			strings.HasPrefix(line, "joined ") ||
			strings.HasPrefix(line, "members: ") ||
			strings.HasPrefix(line, "! watch:") {
			continue
		}
		if m := channelMsgRegex.FindStringSubmatch(line); m != nil {
			seq, _ := strconv.Atoi(m[2])
			replyTo, _ := strconv.Atoi(m[3])
			out = append(out, parsedMessage{
				raw:     line,
				target:  m[1],
				seq:     seq,
				replyTo: replyTo,
				sender:  m[4],
				text:    m[5],
			})
			continue
		}
		if m := dmMsgRegex.FindStringSubmatch(line); m != nil {
			seq, _ := strconv.Atoi(m[2])
			replyTo, _ := strconv.Atoi(m[3])
			out = append(out, parsedMessage{
				raw:     line,
				target:  "@me",
				seq:     seq,
				replyTo: replyTo,
				sender:  m[1],
				text:    m[4],
			})
			continue
		}
		t.Fatalf("unrecognized Chorus message wire line: %q", line)
	}
	return out
}

// postRawJSONRPC sends a raw JSON-RPC HTTP POST request with optional Origin
// and MCP-Protocol-Version headers to verify protocol-level transport guards.
func postRawJSONRPC(t *testing.T, endpoint, origin, protocolVersion string, body any) *http.Response {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal JSON-RPC body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new HTTP request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post JSON-RPC request: %v", err)
	}
	return resp
}

// startSharedRedis starts an isolated Redis server for the test binary using:
//  1. REDIS_ADDR if explicitly provided by the environment,
//  2. a local "redis-server" subprocess on a free loopback port if installed, or
//  3. an ephemeral "docker run redis:7-alpine" container if redis-server is absent.
func startSharedRedis() (string, func(), error) {
	if envAddr := os.Getenv("REDIS_ADDR"); envAddr != "" {
		return envAddr, func() {}, nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("allocate free port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	if redisBin, err := exec.LookPath("redis-server"); err == nil {
		tmpDir, err := os.MkdirTemp("", "chorus-redis-*")
		if err != nil {
			return "", nil, err
		}
		cmd := exec.Command(
			redisBin,
			"--bind", "127.0.0.1",
			"--port", strconv.Itoa(port),
			"--save", "",
			"--appendonly", "no",
			"--dir", tmpDir,
			"--loglevel", "warning",
		)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", nil, fmt.Errorf("start redis-server: %w", err)
		}
		cleanup := func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			_ = os.RemoveAll(tmpDir)
		}
		if err := waitForRedis(addr); err != nil {
			cleanup()
			return "", nil, err
		}
		return addr, cleanup, nil
	}

	out, err := exec.Command(
		"docker", "run", "--rm", "-d",
		"-p", fmt.Sprintf("127.0.0.1:%d:6379", port),
		"redis:7-alpine",
		"redis-server", "--save", "", "--appendonly", "no",
	).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("start redis container (%s): %w", strings.TrimSpace(string(out)), err)
	}
	containerID := strings.TrimSpace(string(out))
	cleanup := func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	}
	if err := waitForRedis(addr); err != nil {
		cleanup()
		return "", nil, err
	}
	return addr, cleanup, nil
}

// waitForRedis pings addr until Redis responds or 10 seconds elapse.
func waitForRedis(addr string) error {
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer func() {
		_ = client.Close()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := client.Ping(ctx).Err()
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("redis at %s did not become ready within 10s", addr)
}

// newAgent creates an emulatedAgent bound to replicaIdx and discovers the
// server's MCP tool schemas via tools/list.
func (c *testCluster) newAgent(replicaIdx int, swarm, handle string) *emulatedAgent {
	c.t.Helper()
	rep := c.replicas[replicaIdx]

	listAny, err := rep.mcpClient.ToolsList()(context.Background(), &genmcpchorus.ToolsListPayload{})
	if err != nil {
		c.t.Fatalf("ToolsList for @%s in %s failed: %v", handle, swarm, err)
	}
	listRes := listAny.(*genmcpchorus.ToolsListResult)

	schemas := make(map[string]map[string]any, len(listRes.Tools))
	for _, tool := range listRes.Tools {
		if len(tool.OutputSchema) != 0 {
			c.t.Fatalf("tool %q must omit OutputSchema, got %s", tool.Name, string(tool.OutputSchema))
		}
		var parsed struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &parsed); err != nil {
			c.t.Fatalf("unmarshal InputSchema for %q: %v", tool.Name, err)
		}
		schemas[tool.Name] = parsed.Properties
	}

	return &emulatedAgent{
		t:       c.t,
		swarm:   swarm,
		handle:  handle,
		replica: rep,
		schemas: schemas,
	}
}

// currentTime returns the cluster's synthetic clock time.
func (c *testCluster) currentTime() time.Time {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	return c.now
}

// advanceClock advances the cluster's synthetic clock by d.
func (c *testCluster) advanceClock(d time.Duration) {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	c.now = c.now.Add(d)
}

// withIdentity copies args and populates "agent" (and "swarm" for who and join)
// if not already set by the caller.
func (a *emulatedAgent) withIdentity(toolName string, args map[string]any) map[string]any {
	merged := make(map[string]any, len(args)+2)
	for k, v := range args {
		merged[k] = v
	}
	if _, ok := merged["agent"]; !ok {
		merged["agent"] = a.handle
	}
	if toolName == "who" || toolName == "join" {
		if _, ok := merged["model"]; !ok {
			merged["model"] = "test-model"
		}
		if _, ok := merged["swarm"]; !ok {
			swarm := a.swarm
			if swarm == "" {
				swarm = "default"
			}
			merged["swarm"] = swarm
		}
	}
	return merged
}

// call validates args against the tool's discovered InputSchema, invokes
// tools/call over HTTP, asserts that StructuredContent is omitted, and returns
// the plain-text wire output.
func (a *emulatedAgent) call(toolName string, args map[string]any) string {
	a.t.Helper()
	args = a.withIdentity(toolName, args)

	props, ok := a.schemas[toolName]
	if !ok {
		a.t.Fatalf("agent @%s attempted to call unknown tool %q", a.handle, toolName)
	}
	for k := range args {
		if _, declared := props[k]; !declared {
			a.t.Fatalf("agent @%s passed undeclared argument %q to tool %q", a.handle, k, toolName)
		}
	}

	rawArgs, err := json.Marshal(args)
	if err != nil {
		a.t.Fatalf("marshal tool args: %v", err)
	}
	resAny, err := a.replica.mcpClient.ToolsCall()(context.Background(), &genmcpchorus.ToolsCallPayload{
		Name:      toolName,
		Arguments: rawArgs,
	})
	if err != nil {
		a.t.Fatalf("ToolsCall(%s) for @%s in %s failed: %v", toolName, a.handle, a.swarm, err)
	}
	res := resAny.(*genmcpchorus.ToolsCallResult)
	complete, ok := res.Outcome.AsComplete()
	if !ok {
		a.t.Fatalf("expected completed tool outcome for %s", toolName)
	}
	if complete.IsError != nil && *complete.IsError {
		errText := ""
		if len(complete.Content) > 0 && complete.Content[0].Text != nil {
			errText = *complete.Content[0].Text
		}
		a.t.Fatalf("tool %s returned IsError=true: %s", toolName, errText)
	}
	if len(complete.StructuredContent) != 0 {
		a.t.Fatalf("tool %s must omit StructuredContent, got %s", toolName, string(complete.StructuredContent))
	}
	if len(complete.Content) != 1 || complete.Content[0].Type != "text" || complete.Content[0].Text == nil {
		a.t.Fatalf("tool %s must return 1 text block, got %+v", toolName, complete.Content)
	}
	return *complete.Content[0].Text
}

// callExpectingError invokes toolName with invalid arguments and asserts that
// the MCP server rejects the call.
func (a *emulatedAgent) callExpectingError(toolName string, args map[string]any) {
	a.t.Helper()
	args = a.withIdentity(toolName, args)

	rawArgs, err := json.Marshal(args)
	if err != nil {
		a.t.Fatalf("marshal tool args: %v", err)
	}
	resAny, err := a.replica.mcpClient.ToolsCall()(context.Background(), &genmcpchorus.ToolsCallPayload{
		Name:      toolName,
		Arguments: rawArgs,
	})
	if err != nil {
		return
	}
	res := resAny.(*genmcpchorus.ToolsCallResult)
	complete, ok := res.Outcome.AsComplete()
	if ok && complete.IsError != nil && *complete.IsError {
		return
	}
	a.t.Fatalf("expected error from tool %s with args %v, but call succeeded", toolName, args)
}

// send invokes the "send" tool and parses both the sent acknowledgement
// ("sent <target>#<seq>") and any piggybacked "--- unread ---" message lines.
func (a *emulatedAgent) send(to, text string, replyTo int) parsedSendResult {
	a.t.Helper()

	args := map[string]any{
		"to":   to,
		"text": text,
	}
	if replyTo > 0 {
		args["reply_to"] = replyTo
	}
	raw := a.call("send", args)
	header, unreadBlock, hasUnread := strings.Cut(raw, "\n--- unread ---\n")
	header, _, _ = strings.Cut(header, "\n")
	if !strings.HasPrefix(header, "sent ") {
		a.t.Fatalf("unexpected send header %q in response:\n%s", header, raw)
	}
	targetSeq := strings.TrimPrefix(header, "sent ")
	hashIdx := strings.LastIndexByte(targetSeq, '#')
	if hashIdx <= 0 {
		a.t.Fatalf("missing #<seq> in send header %q", header)
	}
	seq, err := strconv.Atoi(targetSeq[hashIdx+1:])
	if err != nil {
		a.t.Fatalf("invalid sequence in send header %q: %v", header, err)
	}

	var unread []parsedMessage
	if hasUnread {
		unread = parseWireMessages(a.t, unreadBlock)
	}
	return parsedSendResult{
		target: targetSeq[:hashIdx],
		seq:    seq,
		unread: unread,
	}
}
