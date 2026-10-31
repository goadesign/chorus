package chorus_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// viewDoc mirrors the JSON body of GET /swarms/{swarm}/view for assertions.
	viewDoc struct {
		// Swarm is the swarm name echoed by the server.
		Swarm string `json:"swarm"`
		// Agents lists live agents sorted by handle.
		Agents []struct {
			Handle   string   `json:"handle"`
			Role     *string  `json:"role"`
			Model    *string  `json:"model"`
			Channels []string `json:"channels"`
		} `json:"agents"`
		// Channels lists channels sorted by name.
		Channels []struct {
			Name    string `json:"name"`
			HeadSeq int    `json:"head_seq"`
			Pin     *struct {
				Seq   int    `json:"seq"`
				Text  string `json:"text"`
				Human *bool  `json:"human"`
			} `json:"pin"`
			Positions []struct {
				Handle    string `json:"handle"`
				ReadSeq   int    `json:"read_seq"`
				Behind    int    `json:"behind"`
				Mentioned bool   `json:"mentioned"`
			} `json:"positions"`
			Messages []viewMsg `json:"messages"`
		} `json:"channels"`
		// Inboxes lists direct-message inboxes sorted by recipient.
		Inboxes []struct {
			Recipient string    `json:"recipient"`
			HeadSeq   int       `json:"head_seq"`
			ReadSeq   int       `json:"read_seq"`
			Behind    int       `json:"behind"`
			Messages  []viewMsg `json:"messages"`
		} `json:"inboxes"`
	}

	// viewMsg mirrors one retained message in the view body.
	viewMsg struct {
		// Seq is the message sequence number in its channel or inbox.
		Seq int `json:"seq"`
		// Sender is the sending handle.
		Sender string `json:"sender"`
		// Text is the message body.
		Text string `json:"text"`
		// ReplyTo is the reply_to the sender passed, if any.
		ReplyTo *int `json:"reply_to"`
		// SentAtMs is the Redis storage time in Unix milliseconds.
		SentAtMs int64 `json:"sent_at_ms"`
		// Human is true when the sender had role "human" when the message was sent.
		Human *bool `json:"human"`
	}
)

// TestSwarmViewReportsStateWithoutChangingIt verifies that GET view returns
// agents, channels, pins, read positions, mentions, and DM inboxes, and that
// reading it leaves every agent's unread messages in place.
func TestSwarmViewReportsStateWithoutChangingIt(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	lead := cluster.newAgent(0, "auth", "lead")
	api := cluster.newAgent(0, "auth", "api")
	lead.call("join", map[string]any{"channels": []string{"#auth"}, "role": "Tech Lead"})
	api.call("join", map[string]any{"channels": []string{"#auth"}})
	lead.call("send", map[string]any{"to": "#auth", "text": "Goal: passkeys | Verify: go test ./...", "pin": true})
	lead.send("#auth", "@api please wire ConsumeChallenge", 1)
	lead.send("@api", "use GETDEL", 0)
	human := cluster.newAgent(0, "auth", "raphael")
	human.call("join", map[string]any{"channels": []string{"#auth"}, "model": "human", "role": "Product Lead"})
	human.call("send", map[string]any{"to": "#auth", "text": "Please focus on contrast"})

	base := cluster.replicas[0].httpServer.URL
	before := time.Now().UnixMilli()
	doc, etag := getView(t, base, "auth", "", 0, http.StatusOK)

	assert.Equal(t, "auth", doc.Swarm)
	require.Len(t, doc.Agents, 3)
	assert.Equal(t, "api", doc.Agents[0].Handle)
	assert.Equal(t, "lead", doc.Agents[1].Handle)
	assert.Equal(t, "raphael", doc.Agents[2].Handle)
	require.NotNil(t, doc.Agents[1].Role)
	assert.Equal(t, "Tech Lead", *doc.Agents[1].Role)
	assert.Equal(t, []string{"#auth"}, doc.Agents[0].Channels)

	require.Len(t, doc.Channels, 1)
	ch := doc.Channels[0]
	assert.Equal(t, "#auth", ch.Name)
	assert.Equal(t, 3, ch.HeadSeq)
	require.NotNil(t, ch.Pin)
	assert.Equal(t, 1, ch.Pin.Seq)
	assert.Nil(t, ch.Pin.Human)
	require.Len(t, ch.Messages, 3)
	assert.Nil(t, ch.Messages[0].Human)
	assert.Nil(t, ch.Messages[1].Human)
	require.NotNil(t, ch.Messages[2].Human)
	assert.True(t, *ch.Messages[2].Human)
	require.NotNil(t, ch.Messages[1].ReplyTo)
	assert.Equal(t, 1, *ch.Messages[1].ReplyTo)
	assert.InDelta(t, before, ch.Messages[1].SentAtMs, float64(time.Minute.Milliseconds()))
	require.Len(t, ch.Positions, 3)
	assert.Equal(t, "api", ch.Positions[0].Handle)
	assert.Equal(t, 0, ch.Positions[0].ReadSeq)
	assert.Equal(t, 3, ch.Positions[0].Behind)
	assert.True(t, ch.Positions[0].Mentioned)
	assert.Equal(t, "lead", ch.Positions[1].Handle)
	assert.Equal(t, 1, ch.Positions[1].Behind)
	assert.False(t, ch.Positions[1].Mentioned)
	assert.Equal(t, "raphael", ch.Positions[2].Handle)
	assert.Equal(t, 0, ch.Positions[2].Behind)

	require.Len(t, doc.Inboxes, 1)
	assert.Equal(t, "api", doc.Inboxes[0].Recipient)
	assert.Equal(t, 1, doc.Inboxes[0].Behind)
	require.Len(t, doc.Inboxes[0].Messages, 1)
	assert.Equal(t, "lead", doc.Inboxes[0].Messages[0].Sender)

	// Viewing must not consume @api's unread messages.
	unread := api.call("read", map[string]any{"wait_sec": 0})
	assert.Contains(t, unread, "use GETDEL")
	assert.Contains(t, unread, "please wire ConsumeChallenge")

	// After @api reads, the view changes: its positions catch up.
	doc2, etag2 := getView(t, base, "auth", "", 0, http.StatusOK)
	assert.NotEqual(t, etag, etag2)
	assert.Equal(t, 0, doc2.Channels[0].Positions[0].Behind)
	assert.False(t, doc2.Channels[0].Positions[0].Mentioned)
	assert.Equal(t, 0, doc2.Inboxes[0].Behind)
}

// TestJoinRequiresDeclaredModel verifies that who and join reject a missing
// or blank model, that the declared model is stored with the agent and
// returned by the view, that ordinary calls keep it, and that a later join
// with another model replaces it.
func TestJoinRequiresDeclaredModel(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	api := cluster.newAgent(0, "models", "api")
	base := cluster.replicas[0].httpServer.URL

	api.callExpectingError("join", map[string]any{"channels": []string{"#m"}, "model": nil})
	api.callExpectingError("join", map[string]any{"channels": []string{"#m"}, "model": "   "})
	api.callExpectingError("who", map[string]any{"model": nil})

	api.call("join", map[string]any{"channels": []string{"#m"}, "model": "claude-opus-5-5"})
	api.send("#m", "hello", 0)
	api.call("read", map[string]any{"wait_sec": 0})
	doc, _ := getView(t, base, "models", "", 0, http.StatusOK)
	require.Len(t, doc.Agents, 1)
	require.NotNil(t, doc.Agents[0].Model)
	assert.Equal(t, "claude-opus-5-5", *doc.Agents[0].Model)

	// Real provider ids are kept exactly as given, including case, colons,
	// and slashes.
	const bedrock = "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	api.call("join", map[string]any{"channels": []string{"#m"}, "model": bedrock})
	doc, _ = getView(t, base, "models", "", 0, http.StatusOK)
	require.NotNil(t, doc.Agents[0].Model)
	assert.Equal(t, bedrock, *doc.Agents[0].Model)
}

// TestSwarmViewWaitsForChanges verifies the If-None-Match wait: it returns
// 304 when nothing changes, and returns the new snapshot as soon as a message
// is published during the wait.
func TestSwarmViewWaitsForChanges(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	lead := cluster.newAgent(0, "auth", "lead")
	lead.call("join", map[string]any{"channels": []string{"#auth"}})
	base := cluster.replicas[0].httpServer.URL

	_, etag := getView(t, base, "auth", "", 0, http.StatusOK)
	getView(t, base, "auth", etag, 0, http.StatusNotModified)
	start := time.Now()
	getView(t, base, "auth", etag, 1, http.StatusNotModified)
	assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond)

	done := make(chan viewDoc, 1)
	go func() {
		doc, _ := getView(t, base, "auth", etag, 30, http.StatusOK)
		done <- doc
	}()
	time.Sleep(200 * time.Millisecond)
	lead.send("#auth", "hello", 0)
	select {
	case doc := <-done:
		require.Len(t, doc.Channels[0].Messages, 1)
		assert.Equal(t, "hello", doc.Channels[0].Messages[0].Text)
	case <-time.After(10 * time.Second):
		t.Fatal("view wait did not return after a publication")
	}
}

// TestSwarmListAndUnknownSwarm verifies GET /swarms lists bound swarms and
// that an unknown swarm returns an empty snapshot instead of an error.
func TestSwarmListAndUnknownSwarm(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	cluster.newAgent(0, "zeta", "a").call("join", map[string]any{"channels": []string{"#z"}})
	cluster.newAgent(0, "alpha", "b").call("join", map[string]any{"channels": []string{"#a"}})
	base := cluster.replicas[0].httpServer.URL

	resp := viewRequest(t, base+"/swarms", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		Swarms []string `json:"swarms"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	assert.Equal(t, []string{"alpha", "zeta"}, list.Swarms)

	doc, _ := getView(t, base, "nobody-here", "", 0, http.StatusOK)
	assert.Empty(t, doc.Agents)
	assert.Empty(t, doc.Channels)
	assert.Empty(t, doc.Inboxes)
}

// TestUnboundSwarmViewHonorsWait verifies that a conditional request for a
// swarm no agent is bound to waits for the requested time before answering
// 304, so a polling page cannot spin, and that it stops waiting when the
// caller goes away.
func TestUnboundSwarmViewHonorsWait(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	base := cluster.replicas[0].httpServer.URL

	_, etag := getView(t, base, "nobody-here", "", 0, http.StatusOK)
	getView(t, base, "nobody-here", etag, 0, http.StatusNotModified)
	start := time.Now()
	getView(t, base, "nobody-here", etag, 1, http.StatusNotModified)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	assert.Less(t, elapsed, 3*time.Second)

	// The server, not just the client, must stop waiting when the caller
	// disconnects: Close blocks until every in-flight handler has returned.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/swarms/nobody-here/view?wait=30", nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", etag)
	_, err = http.DefaultClient.Do(req)
	require.Error(t, err)
	start = time.Now()
	cluster.replicas[0].httpServer.Close()
	assert.Less(t, time.Since(start), 2*time.Second, "view handler kept waiting after the caller disconnected")
}

// TestSwarmViewRejectsForeignHostAndOrigin verifies the DNS-rebinding and
// cross-site defenses on the dashboard routes.
func TestSwarmViewRejectsForeignHostAndOrigin(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: sharedRedisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	svc, err := chorus.New(rdb)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	// Start the listener first so the page's own origin can be allowed the way
	// cmd/chorus seeds it from the listen port.
	srv := httptest.NewUnstartedServer(nil)
	u, err := url.Parse("http://" + srv.Listener.Addr().String())
	require.NoError(t, err)
	srv.Config.Handler = chorus.NewHTTPHandler(svc, "http://"+u.Host, "http://allowed.example")
	srv.Start()
	t.Cleanup(srv.Close)
	base := srv.URL

	tests := []struct {
		name      string
		host      string
		origin    string
		fetchSite string
		want      int
	}{
		{name: "loopback without origin", want: http.StatusOK},
		{name: "same loopback origin", origin: "http://" + u.Host, want: http.StatusOK},
		{name: "configured origin", origin: "http://allowed.example", want: http.StatusOK},
		{name: "rebound attacker host", host: "evil.example:" + u.Port(), want: http.StatusForbidden},
		{name: "attacker origin", origin: "http://evil.example", want: http.StatusForbidden},
		{name: "other loopback port origin", origin: "http://127.0.0.1:1", want: http.StatusForbidden},
		{name: "cross-site script load", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "same-origin fetch", fetchSite: "same-origin", want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := viewRequest(t, base+"/swarms", func(r *http.Request) {
				if tt.host != "" {
					r.Host = tt.host
				}
				if tt.origin != "" {
					r.Header.Set("Origin", tt.origin)
				}
				if tt.fetchSite != "" {
					r.Header.Set("Sec-Fetch-Site", tt.fetchSite)
				}
			})
			defer resp.Body.Close()
			assert.Equal(t, tt.want, resp.StatusCode)
		})
	}
}

// getView fetches the view of swarm, sending etag as If-None-Match and wait as
// the wait query parameter, asserts the status, and returns the decoded body
// (zero for 304) and the response ETag.
func getView(t *testing.T, base, swarm, etag string, wait int, wantStatus int) (viewDoc, string) {
	t.Helper()
	target := base + "/swarms/" + swarm + "/view"
	if wait > 0 {
		target += "?wait=" + strconv.Itoa(wait)
	}
	resp := viewRequest(t, target, func(r *http.Request) {
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
	})
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, wantStatus, resp.StatusCode, string(body))
	var doc viewDoc
	if wantStatus == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &doc), string(body))
		require.NotEmpty(t, resp.Header.Get("ETag"))
	}
	return doc, resp.Header.Get("ETag")
}

// viewRequest sends a GET to target after applying edit to the request.
func viewRequest(t *testing.T, target string, edit func(*http.Request)) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	require.NoError(t, err)
	if edit != nil {
		edit(req)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestDashboardPageServed verifies that GET / serves the dashboard page and
// GET /ui/<file> its scripts, each with a Content-Security-Policy that allows
// only same-origin code, forbids other sites from framing the page, and
// allows no inline scripts, and that other paths are not answered with files.
func TestDashboardPageServed(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	base := cluster.replicas[0].httpServer.URL

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantType    string
		wantContent string
	}{
		{name: "page", path: "/", wantStatus: http.StatusOK, wantType: "text/html; charset=utf-8", wantContent: "<html"},
		{name: "timeline script", path: "/ui/timeline.js", wantStatus: http.StatusOK, wantType: "text/javascript; charset=utf-8", wantContent: "ChorusTimeline"},
		{name: "page under ui", path: "/ui/index.html", wantStatus: http.StatusNotFound},
		{name: "missing file", path: "/ui/missing.js", wantStatus: http.StatusNotFound},
		{name: "path escape", path: "/ui/..%2fui.go", wantStatus: http.StatusNotFound},
		{name: "unknown root path", path: "/nope", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := viewRequest(t, base+tt.path, nil)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, resp.StatusCode, string(body))
			if tt.wantStatus != http.StatusOK {
				return
			}
			assert.Equal(t, tt.wantType, resp.Header.Get("Content-Type"))
			assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
			csp := resp.Header.Get("Content-Security-Policy")
			assert.Contains(t, csp, "default-src 'self'")
			assert.Contains(t, csp, "frame-ancestors 'none'")
			assert.NotContains(t, csp, "unsafe-inline")
			assert.Contains(t, string(body), tt.wantContent)
		})
	}
}
