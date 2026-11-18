package chorus_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/goadesign/chorus/chorus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestActiveWatchPreservesMembership verifies presence through the HTTP
// consumers while a blocking read outlives the registration window. It uses
// the existing Redis fixture because last-member leave destroys real streams.
func TestActiveWatchPreservesMembership(t *testing.T) {
	c := newTestCluster(t, 1, []chorus.Option{chorus.WithOnlineWindow(2 * time.Minute)})
	watcher := c.newAgent(0, "watching", "watcher")
	peer := c.newAgent(0, "watching", "peer")
	watcher.call("join", map[string]any{"channels": []string{"#work"}})
	peer.call("join", map[string]any{"channels": []string{"#work"}})
	watcher.call("send", map[string]any{"to": "#work", "text": "keep this plan", "pin": true})
	done := make(chan string, 1)
	go func() {
		done <- watcher.call("read", map[string]any{"wait_sec": 30})
	}()
	t.Cleanup(func() {
		watcher.call("leave", map[string]any{"channels": []string{"#work"}})
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("watch did not terminate after leaving")
		}
	})
	base := c.replicas[0].httpServer.URL
	require.Eventually(t, func() bool {
		doc, _ := getView(t, base, "watching", "", 0, http.StatusOK)
		return len(doc.Agents) == 2 && doc.Agents[1].Watching
	}, 2*time.Second, 10*time.Millisecond, "blocking watch must be acquired before advancing time")

	c.advanceClock(3 * time.Minute)
	assert.Contains(t, peer.call("who", map[string]any{}), "@watcher\n")
	joined := peer.call("join", map[string]any{"channels": []string{"#work"}})
	assert.Contains(t, joined, "@watcher")
	peer.call("leave", map[string]any{"channels": []string{"#work"}})

	doc, _ := getView(t, base, "watching", "", 0, http.StatusOK)
	require.Len(t, doc.Agents, 1)
	assert.Equal(t, "watcher", doc.Agents[0].Handle)
	assert.True(t, doc.Agents[0].Watching)
	assert.Equal(t, []string{"#work"}, doc.Agents[0].Channels)
	require.Len(t, doc.Channels, 1)
	require.NotNil(t, doc.Channels[0].Pin)
	assert.Equal(t, "keep this plan", doc.Channels[0].Pin.Text)
	require.Len(t, doc.Channels[0].Positions, 1)
	assert.Equal(t, "watcher", doc.Channels[0].Positions[0].Handle)
	assert.Equal(t, []string{"watching"}, listSwarms(t, base))
}

// TestIdleChannelExpiresWithSwarmTTL waits for Redis expiry, then joins through
// a fresh server's consumer to prove TTL still removes history and pins. The
// two-second wait is necessary because the storage owner's clock is real time.
func TestIdleChannelExpiresWithSwarmTTL(t *testing.T) {
	c := newTestCluster(t, 2, []chorus.Option{chorus.WithSwarmTTL(2 * time.Second)})
	lead := c.newAgent(0, "ttl", "lead")
	lead.call("join", map[string]any{"channels": []string{"#work"}})
	lead.call("send", map[string]any{"to": "#work", "text": "expired plan", "pin": true})
	ctx := context.Background()
	const stateKey = "map:chorus:ttl:state:content"
	ttl, err := c.rdb.PTTL(ctx, stateKey).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.Eventually(t, func() bool {
		n, err := c.rdb.Exists(ctx, stateKey).Result()
		return err == nil && n == 0
	}, 5*time.Second, 20*time.Millisecond, "swarm state must expire without client activity")
	fresh := c.newAgent(1, "ttl", "fresh")
	joined := fresh.call("join", map[string]any{"channels": []string{"#work"}})
	assert.Contains(t, joined, "head #0")
	assert.NotContains(t, joined, "expired plan")
}
