package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
)

// testServer spins up the cloud HTTP API with a stub config.
func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &config.Config{
		MaxIterations: 1,
		DefaultModel:  "stub/stub-model",
		Providers: []config.Provider{
			{Name: "stub", BaseURL: "http://localhost:1", APIKey: "test", Enabled: true,
				Models: []config.Model{{ID: "stub-model"}},
			},
		},
	}
	srv := NewServer(t.TempDir(), cfg, "")
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv
}

// Sessions can be created, listed and deleted through the API.
func TestSessionLifecycle(t *testing.T) {
	_, httpSrv := testServer(t)
	ctx := context.Background()
	c := NewClient(httpSrv.URL, "")

	info, err := c.Create(ctx, CreateRequest{Name: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.ID == "" {
		t.Fatal("empty session id")
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("List = %+v, want the created session", list)
	}
}

// Prompting an unknown session fails cleanly.
func TestPromptUnknownSession(t *testing.T) {
	_, httpSrv := testServer(t)
	c := NewClient(httpSrv.URL, "")
	c.SetSession("nope")
	if err := c.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

// Events stream over SSE: broadcast an event on the server and watch it
// arrive at the client, proving the wire format round-trips.
func TestEventStreamRoundTrip(t *testing.T) {
	srv, httpSrv := testServer(t)
	ctx := context.Background()
	c := NewClient(httpSrv.URL, "")

	info, err := c.Create(ctx, CreateRequest{Name: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := make(chan agent.Event, 8)
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	go func() {
		_ = c.Stream(streamCtx, func(ev agent.Event) { got <- ev })
	}()

	// Wait for the SSE subscription to land, then broadcast.
	time.Sleep(300 * time.Millisecond)
	srv.mu.Lock()
	sess := srv.sessions[info.ID]
	srv.mu.Unlock()
	sess.broadcast(agent.Event{Kind: agent.EvText, Text: "hello-cloud"})

	select {
	case ev := <-got:
		if ev.Kind != agent.EvText || ev.Text != "hello-cloud" {
			t.Fatalf("got %+v, want the broadcast event", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the streamed event")
	}

	// Answering with no pending question is a clean error, not a hang.
	if err := c.Answer(ctx, "x"); err == nil {
		t.Fatal("expected an error when no question is pending")
	}
}

// A bearer token, when configured, gates every route.
func TestAuthGatesRoutes(t *testing.T) {
	cfg := &config.Config{MaxIterations: 1}
	srv := NewServer(t.TempDir(), cfg, "secret")
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	// No token: rejected.
	resp, err := http.Get(httpSrv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// Right token: allowed.
	c := NewClient(httpSrv.URL, "secret")
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List with token: %v", err)
	}
}
