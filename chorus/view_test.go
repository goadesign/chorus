package chorus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
			Watching bool     `json:"watching_on_this_server"`
			LastSeen int64    `json:"last_seen_unix"`
		} `json:"agents"`
		// Channels lists channels sorted by name.
		Channels []struct {
			Name    string `json:"name"`
			HeadSeq int    `json:"head_seq"`
			Pin     *struct {
				Seq   int     `json:"seq"`
				Text  string  `json:"text"`
				Human *bool   `json:"human"`
				Model *string `json:"model"`
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
		// Human is true when the sender had declared model "human" when the message was sent.
		Human *bool `json:"human"`
		// Model is the model the sender had declared when the message was sent.
		Model *string `json:"model"`
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
	base := cluster.replicas[0].httpServer.URL
	postLocalMessage(t, base, "auth", "raphael", "human", "#auth", "Please focus on contrast", "Product owner")
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

// TestLocalMessageIdentity checks HTTP provenance, MCP isolation, and the
// recipient's unread view across two server replicas.
func TestLocalMessageIdentity(t *testing.T) {
	cluster := newTestCluster(t, 2, nil)
	const swarm = "review"
	first, second := cluster.replicas[0].httpServer.URL, cluster.replicas[1].httpServer.URL
	agent := cluster.newAgent(0, swarm, "agent")
	agent.call("join", map[string]any{"channels": []string{"#review"}})
	agent.send("#review", "initial post", 0)

	body := map[string]any{
		"sender": "person", "kind": "human", "to": "#review", "text": "  Please revise.\n",
		"reply_to": 1, "pin": true,
	}
	msg, _ := postMessage(t, second, swarm, body, http.StatusCreated)
	assert.Equal(t, 2, msg.Seq)
	assert.Equal(t, body["text"], msg.Text)
	assert.Equal(t, "person", msg.Sender)
	require.NotNil(t, msg.ReplyTo)
	assert.Equal(t, 1, *msg.ReplyTo)
	require.NotNil(t, msg.Model)
	assert.Equal(t, "human", *msg.Model)
	require.NotNil(t, msg.Human)
	assert.True(t, *msg.Human)
	assert.Positive(t, msg.SentAtMs)
	view, _ := getView(t, second, swarm, "", 0, http.StatusOK)
	require.Len(t, view.Channels, 1)
	require.NotNil(t, view.Channels[0].Pin)
	assert.Equal(t, msg.Seq, view.Channels[0].Pin.Seq)
	assert.Equal(t, msg, view.Channels[0].Messages[1])
	agent.send("#review", "reply to @person", msg.Seq)
	view, _ = getView(t, first, swarm, "", 0, http.StatusOK)
	require.Len(t, view.Channels[0].Positions, 2)
	assert.True(t, view.Channels[0].Positions[1].Mentioned)
	answer := postLocalMessage(t, second, swarm, "person", "human", "#review", "Revision ready", "")
	view, _ = getView(t, second, swarm, "", 0, http.StatusOK)
	position := view.Channels[0].Positions[1]
	assert.Equal(t, "person", position.Handle)
	assert.Equal(t, answer.Seq, position.ReadSeq)
	assert.Zero(t, position.Behind)
	assert.False(t, position.Mentioned)

	service := postLocalMessage(t, first, swarm, "coordinator", "service", "@agent", "ready", "Workflow status")
	require.NotNil(t, service.Human)
	assert.False(t, *service.Human)
	require.NotNil(t, service.Model)
	assert.Equal(t, "service", *service.Model)
	assert.Contains(t, agent.call("read", map[string]any{"mentions_only": true}), "@coordinator->me#1: ready")

	// Reserved handles cannot enter any MCP method under any model claim.
	for _, handle := range []string{"person", "coordinator"} {
		fake := cluster.newAgent(0, swarm, handle)
		fake.callExpectingError("who", map[string]any{})
		fake.callExpectingError("join", map[string]any{"channels": []string{"#review"}})
		fake.callExpectingError("send", map[string]any{"to": "#review", "text": "fake"})
		fake.callExpectingError("read", map[string]any{})
		fake.callExpectingError("leave", map[string]any{"channels": []string{"#review"}})
	}
	newcomer := cluster.newAgent(1, swarm, "newcomer")
	newcomer.callExpectingError("who", map[string]any{"model": " HuMaN "})
	newcomer.callExpectingError("join", map[string]any{"model": "SERVICE", "channels": []string{"#review"}})

	for _, tc := range []struct {
		name, swarm, sender, kind, target, code string
		pin                                     bool
		status                                  int
	}{
		{"agent handle", swarm, "agent", "human", "#review", "handle_in_use", false, http.StatusConflict},
		{"different kind", swarm, "person", "service", "#review", "handle_in_use", false, http.StatusConflict},
		{"different swarm", "other", "person", "human", "#review", "handle_in_use", false, http.StatusConflict},
		{"pinned direct message", swarm, "person", "human", "@agent", "invalid_target", true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, code := postMessage(t, first, tc.swarm, map[string]any{
				"sender": tc.sender, "kind": tc.kind, "to": tc.target, "text": "bad post", "pin": tc.pin,
			}, tc.status)
			assert.Equal(t, tc.code, code)
		})
	}
}

// TestStaleReplicaUsesCurrentBinding simulates a state change whose pub/sub
// notification was lost. The stale server must resolve a handle through the
// current binding before accepting another agent operation.
func TestStaleReplicaUsesCurrentBinding(t *testing.T) {
	cluster := newTestCluster(t, 2, nil)
	old := cluster.newAgent(0, "old", "moving")
	old.call("join", map[string]any{"channels": []string{"#work"}})
	const oldState = "map:chorus:old:state:content"
	ctx := t.Context()
	require.NoError(t, cluster.rdb.HDel(ctx, oldState, "agent:moving", "model:moving", "sub:moving").Err())
	require.NoError(t, cluster.rdb.HIncrBy(ctx, oldState, "=rev", 1).Err())
	first, second := cluster.replicas[0].httpServer.URL, cluster.replicas[1].httpServer.URL
	assert.Equal(t, "test-model", viewedModel(t, first, "old", "moving"), "replica deliberately remains stale")
	postLocalMessage(t, second, "new", "moving", "human", "#work", "Moved", "")
	old.callExpectingError("send", map[string]any{"to": "@peer", "text": "stale publisher"})
	old.callExpectingError("leave", map[string]any{"channels": []string{"#work"}})
	view, _ := getView(t, second, "new", "", 0, http.StatusOK)
	require.Len(t, view.Agents, 1)
	require.NotNil(t, view.Agents[0].Model)
	assert.Equal(t, "human", *view.Agents[0].Model)
}

// TestLocalMessageRejectsRemotePeer verifies that a caller reaching a
// non-loopback listener cannot make a post by setting a loopback Host.
func TestLocalMessageRejectsRemotePeer(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	handler := chorus.NewHTTPHandler(cluster.replicas[0].svc)
	req := httptest.NewRequest(http.MethodPost, "http://localhost/swarms/demo/messages",
		strings.NewReader(`{"sender":"person","kind":"human","to":"@worker","text":"remote post"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:12345"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	assert.Equal(t, http.StatusForbidden, resp.Code)
}

// TestTerminalActivity verifies presence and wake facts without reading a
// message stream or advancing any read position on the terminal's behalf.
func TestTerminalActivity(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	svc := cluster.replicas[0].svc
	ctx := t.Context()
	const swarm = "terminal"
	worker := cluster.newAgent(0, swarm, "worker")
	writer := cluster.newAgent(0, swarm, "writer")
	worker.call("join", map[string]any{"channels": []string{"#work"}})
	writer.call("join", map[string]any{"channels": []string{"#work"}})
	writer.send("#work", "passive channel update", 0)
	sequences, err := svc.AgentWakeSequences(ctx, swarm, "worker")
	require.NoError(t, err)
	assert.Empty(t, sequences)

	writer.send("#work", "ready for @worker", 0)
	sequences, err = svc.AgentWakeSequences(ctx, swarm, "worker")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"#work": 2}, sequences)
	assert.Contains(t, worker.call("read", map[string]any{"mentions_only": true}), "passive channel update")
	sequences, err = svc.AgentWakeSequences(ctx, swarm, "worker")
	require.NoError(t, err)
	assert.Empty(t, sequences)

	writer.send("@worker", "direct assignment", 0)
	sequences, err = svc.AgentWakeSequences(ctx, swarm, "worker")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"@worker": 1}, sequences)

	cluster.advanceClock(4 * time.Minute)
	require.NoError(t, svc.TouchAgent(ctx, swarm, "worker"))
	base := cluster.replicas[0].httpServer.URL
	assert.Eventually(t, func() bool {
		view, _ := getView(t, base, swarm, "", 0, http.StatusOK)
		for _, participant := range view.Agents {
			if participant.Handle == "worker" {
				return participant.LastSeen == cluster.currentTime().Unix()
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)
	cluster.advanceClock(2 * time.Minute)
	require.NoError(t, svc.TouchAgent(ctx, "other", "worker"))
	require.NoError(t, svc.TouchAgent(ctx, swarm, "never-joined"))
	view, _ := getView(t, base, swarm, "", 0, http.StatusOK)
	require.Len(t, view.Agents, 1)
	assert.Equal(t, "worker", view.Agents[0].Handle)
}

// TestExpiredProfileCannotRemoveNewBinding verifies that pruning a participant
// from its previous swarm keeps the binding that other replicas and hc use.
func TestExpiredProfileCannotRemoveNewBinding(t *testing.T) {
	cluster := newTestCluster(t, 2, nil)
	old := cluster.newAgent(0, "old", "moving")
	old.call("who", map[string]any{})
	cluster.advanceClock(6 * time.Minute)
	moved := cluster.newAgent(1, "new", "moving")
	moved.call("join", map[string]any{"channels": []string{"#work"}})
	observer := cluster.newAgent(0, "old", "observer")
	observer.call("who", map[string]any{})
	bound, err := cluster.rdb.HGet(t.Context(), "chorus:agents", "moving").Result()
	require.NoError(t, err)
	assert.Equal(t, "new", bound)
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

	// The cap counts characters, not bytes: 2048 characters are accepted and
	// kept exactly, including multibyte ones, and 2049 are rejected.
	atCap := strings.Repeat("é", 2047) + "x"
	api.call("join", map[string]any{"channels": []string{"#m"}, "model": atCap})
	doc, _ = getView(t, base, "models", "", 0, http.StatusOK)
	require.NotNil(t, doc.Agents[0].Model)
	assert.Equal(t, atCap, *doc.Agents[0].Model)
	below := strings.Repeat("m", 2047)
	api.call("join", map[string]any{"channels": []string{"#m"}, "model": below})
	doc, _ = getView(t, base, "models", "", 0, http.StatusOK)
	require.NotNil(t, doc.Agents[0].Model)
	assert.Equal(t, below, *doc.Agents[0].Model)
	api.callExpectingError("join", map[string]any{"channels": []string{"#m"}, "model": strings.Repeat("m", 2049)})
	doc, _ = getView(t, base, "models", "", 0, http.StatusOK)
	require.NotNil(t, doc.Agents[0].Model)
	assert.Equal(t, below, *doc.Agents[0].Model)
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

// TestSwarmListOmitsSwarmsWithoutLiveAgents verifies that GET /swarms leaves
// out a swarm whose only agent went idle past the online window, while a
// dashboard that names that swarm directly still gets its history.
func TestSwarmListOmitsSwarmsWithoutLiveAgents(t *testing.T) {
	cluster := newTestCluster(t, 1, []chorus.Option{chorus.WithOnlineWindow(2 * time.Minute)})
	idle := cluster.newAgent(0, "idle-swarm", "sleeper")
	idle.call("join", map[string]any{"channels": []string{"#old"}})
	idle.send("#old", "last words", 0)
	active := cluster.newAgent(0, "busy-swarm", "worker")
	active.call("join", map[string]any{"channels": []string{"#now"}})
	base := cluster.replicas[0].httpServer.URL
	assert.Equal(t, []string{"busy-swarm", "idle-swarm"}, listSwarms(t, base))

	cluster.advanceClock(3 * time.Minute)
	active.call("who", map[string]any{})

	assert.Equal(t, []string{"busy-swarm"}, listSwarms(t, base))
	doc, _ := getView(t, base, "idle-swarm", "", 0, http.StatusOK)
	require.Len(t, doc.Channels, 1)
}

// TestSwarmListReadsLatestPresenceWithoutKeepingSwarmsOpen verifies that
// listing a swarm this server never opened leaves no subscription to it
// behind, and that GET /swarms decides from the presence stored in Redis even
// when this server's open copy missed the change. The test saves each change
// the way another server's write does (new value and next revision) but never
// publishes it, so the open copy stays behind, in both directions.
func TestSwarmListReadsLatestPresenceWithoutKeepingSwarmsOpen(t *testing.T) {
	cluster := newTestCluster(t, 2, []chorus.Option{chorus.WithOnlineWindow(2 * time.Minute)})
	cluster.replicas[0].httpServer.Client().Timeout = 5 * time.Second
	ctx := context.Background()
	hashKey := "map:chorus:cold-swarm:state:content"
	updates := "map:chorus:cold-swarm:state:updates"
	cluster.newAgent(1, "cold-swarm", "remote").call("join", map[string]any{"channels": []string{"#c"}})
	listing := cluster.replicas[0].httpServer.URL
	subscribers := func() int64 {
		counts, err := cluster.rdb.PubSubNumSub(ctx, updates).Result()
		require.NoError(t, err)
		return counts[updates]
	}
	before := subscribers()
	assert.Equal(t, []string{"cold-swarm"}, listSwarms(t, listing))
	assert.Equal(t, before, subscribers(), "listing must not keep the swarm open")

	// Open the swarm on the listing server, then let its registration go
	// stale there while Redis holds a fresh one.
	getView(t, listing, "cold-swarm", "", 0, http.StatusOK)
	cluster.advanceClock(3 * time.Minute)
	fresh := fmt.Sprintf(`{"ts":%d}`, cluster.currentTime().Unix())
	require.NoError(t, cluster.rdb.HSet(ctx, hashKey, "agent:remote", fresh).Err())
	require.NoError(t, cluster.rdb.HIncrBy(ctx, hashKey, "=rev", 1).Err())
	assert.Equal(t, []string{"cold-swarm"}, listSwarms(t, listing), "a fresh registration in Redis is live")

	// Remove the registration in Redis only; the open copy still holds it.
	require.NoError(t, cluster.rdb.HDel(ctx, hashKey, "agent:remote").Err())
	require.NoError(t, cluster.rdb.HIncrBy(ctx, hashKey, "=rev", 1).Err())
	assert.Equal(t, []string{}, listSwarms(t, listing), "a registration gone from Redis is not live")
}

// TestMessagesKeepSenderModelAfterSenderLeaves verifies that a message and a
// pin record the model their sender had declared when they were sent, and
// that the dashboard still reports it after the sender leaves, so a departed
// agent keeps its identity in the message list and timeline.
func TestMessagesKeepSenderModelAfterSenderLeaves(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	base := cluster.replicas[0].httpServer.URL
	stayer := cluster.newAgent(0, "model-swarm", "stayer")
	stayer.call("join", map[string]any{"channels": []string{"#m"}})
	leaver := cluster.newAgent(0, "model-swarm", "leaver")
	leaver.call("join", map[string]any{"channels": []string{"#m"}, "model": "sol-6.1"})
	leaver.call("send", map[string]any{"to": "#m", "text": "Plan: ship it", "pin": true})
	leaver.call("leave", map[string]any{"channels": []string{"#m"}})

	doc, _ := getView(t, base, "model-swarm", "", 0, http.StatusOK)
	require.Len(t, doc.Channels, 1)
	ch := doc.Channels[0]
	require.Len(t, ch.Messages, 1)
	require.NotNil(t, ch.Messages[0].Model)
	assert.Equal(t, "sol-6.1", *ch.Messages[0].Model)
	require.NotNil(t, ch.Pin)
	require.NotNil(t, ch.Pin.Model)
	assert.Equal(t, "sol-6.1", *ch.Pin.Model)
	for _, a := range doc.Agents {
		assert.NotEqual(t, "leaver", a.Handle, "the sender has left")
	}
}

// listSwarms returns the swarm names that GET /swarms reports.
func listSwarms(t *testing.T, base string) []string {
	t.Helper()
	resp := viewRequest(t, base+"/swarms", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		Swarms []string `json:"swarms"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	return list.Swarms
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

// TestDashboardCarriesUIBuild verifies that the dashboard page records the
// identity of the dashboard files it was built from, and that every
// dashboard data response, including 304 Not Modified, names the files this
// server embeds, so an open page can compare the two and offer a reload
// after the server starts serving a different dashboard.
func TestDashboardCarriesUIBuild(t *testing.T) {
	cluster := newTestCluster(t, 1, nil)
	cluster.newAgent(0, "build-swarm", "probe").call("join", map[string]any{"channels": []string{"#b"}})
	base := cluster.replicas[0].httpServer.URL

	page := viewRequest(t, base+"/", nil)
	body, err := io.ReadAll(page.Body)
	page.Body.Close()
	require.NoError(t, err)
	match := regexp.MustCompile(`<meta name="chorus-ui-build" content="([0-9a-f]{64})">`).FindSubmatch(body)
	require.NotNil(t, match, "page must carry its dashboard identity")
	loaded := string(match[1])
	assert.Equal(t, "no-store", page.Header.Get("Cache-Control"))
	assert.Contains(t, string(body), `id="btn-update-chorus" hidden`)
	assert.Contains(t, string(body), `id="update-release"`)
	assert.Contains(t, string(body), `src="/ui/update.js"`)

	list := viewRequest(t, base+"/swarms", nil)
	list.Body.Close()
	assert.Equal(t, loaded, list.Header.Get("X-Chorus-UI-Build"))

	_, etag := getView(t, base, "build-swarm", "", 0, http.StatusOK)
	unchanged := viewRequest(t, base+"/swarms/build-swarm/view?wait=1", func(r *http.Request) {
		r.Header.Set("If-None-Match", etag)
	})
	unchanged.Body.Close()
	require.Equal(t, http.StatusNotModified, unchanged.StatusCode)
	assert.Equal(t, loaded, unchanged.Header.Get("X-Chorus-UI-Build"))
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
		{name: "chat script", path: "/ui/chat.js", wantStatus: http.StatusOK, wantType: "text/javascript; charset=utf-8", wantContent: "ChorusChat"},
		{name: "mention script", path: "/ui/mention.js", wantStatus: http.StatusOK, wantType: "text/javascript; charset=utf-8", wantContent: "ChorusMention"},
		{name: "update script", path: "/ui/update.js", wantStatus: http.StatusOK, wantType: "text/javascript; charset=utf-8", wantContent: "ChorusUpdate"},
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

// viewedModel returns the model that the server at base reports for handle in
// swarm's view, which it builds from its own local copy of the swarm state.
func viewedModel(t *testing.T, base, swarm, handle string) string {
	t.Helper()
	doc, _ := getView(t, base, swarm, "", 0, http.StatusOK)
	for _, a := range doc.Agents {
		if a.Handle == handle && a.Model != nil {
			return *a.Model
		}
	}
	return ""
}

// postLocalMessage drives the same local HTTP endpoint the dashboard uses.
func postLocalMessage(t *testing.T, base, swarm, sender, kind, to, text, role string) viewMsg {
	t.Helper()
	msg, _ := postMessage(t, base, swarm, map[string]any{
		"sender": sender, "kind": kind, "to": to, "text": text, "role": role,
	}, http.StatusCreated)
	return msg
}

// postMessage sends a browser-style request and decodes its typed result or error.
func postMessage(t *testing.T, base, swarm string, body map[string]any, status int) (viewMsg, string) {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(base+"/swarms/"+swarm+"/messages", "application/json", bytes.NewReader(data))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, status, resp.StatusCode, "%s", raw)
	if status != http.StatusCreated {
		var failure struct {
			Name string `json:"name"`
		}
		require.NoError(t, json.Unmarshal(raw, &failure))
		return viewMsg{}, failure.Name
	}
	var result struct {
		Target  string  `json:"target"`
		Message viewMsg `json:"message"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, body["to"], result.Target)
	return result.Message, ""
}
