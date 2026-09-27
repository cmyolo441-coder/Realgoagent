package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/config"
)

// serve starts a fake provider and returns a config pointed at it.
func serve(t *testing.T, handler http.HandlerFunc) *config.Config {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := config.Default()
	c.Providers = []config.Provider{{
		Name:    "fake",
		BaseURL: srv.URL,
		APIKey:  "x",
		Enabled: true,
		Models:  []config.Model{{ID: "m1"}},
	}}
	c.DefaultModel = "fake/m1"
	return c
}

// sse writes one streamed reply and a usage chunk.
func sse(w http.ResponseWriter, text string, tokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	body := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}}
	b, _ := json.Marshal(body)
	fmt.Fprintf(w, "data: %s\n\n", b)
	usage := map[string]any{
		"choices": []any{map[string]any{"finish_reason": "stop", "delta": map[string]any{}}},
		"usage":   map[string]any{"prompt_tokens": tokens, "completion_tokens": tokens, "total_tokens": tokens * 2},
	}
	u, _ := json.Marshal(usage)
	fmt.Fprintf(w, "data: %s\n\n", u)
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func TestRunReturnsTheChildAnswer(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, "the file defines one function.", 42)
	})
	r := &Runner{Config: cfg}
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "what is in greet.go"})
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Reply != "the file defines one function." {
		t.Errorf("reply = %q", res.Reply)
	}
}

func TestRunAccountsTheChildTokens(t *testing.T) {
	// The parent is told what the delegation cost. Reading it from the child's
	// own usage is the only honest source, and it is what /agents shows.
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, "a short answer", 100)
	})
	r := &Runner{Config: cfg}
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "look"})
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Usage.TotalTokens != 200 {
		t.Errorf("usage = %+v, want 200 total tokens", res.Usage)
	}
}

func TestRunReportsAChildThatSaysNothing(t *testing.T) {
	// An empty tool result would read to the parent as "nothing found", which
	// is a different and wrong claim, so it has to be an error.
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, "", 1)
	})
	r := &Runner{Config: cfg}
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "look"})
	if res.Err == nil {
		t.Fatal("a silent subagent must be an error, not an empty answer")
	}
	if !strings.Contains(res.Err.Error(), "without answering") {
		t.Errorf("err = %v, want a missing-answer error", res.Err)
	}
}

func TestRunFailsWhenTheProviderIsUnreachable(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg.Providers[0].BaseURL = "http://127.0.0.1:1/v1"
	r := &Runner{Config: cfg}
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "look"})
	if res.Err == nil {
		t.Fatal("an unreachable provider must surface as an error")
	}
}

func TestRunEmitsLifecycleEvents(t *testing.T) {
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, "done", 1)
	})
	var kinds []EventKind
	r := &Runner{Config: cfg, OnEvent: func(ev Event) { kinds = append(kinds, ev.Kind) }}
	r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "look"})
	if len(kinds) < 2 || kinds[0] != EvSubStart {
		t.Fatalf("events = %v, want a start followed by a completion", kinds)
	}
	if kinds[len(kinds)-1] != EvSubDone {
		t.Errorf("last event = %v, want a completion", kinds[len(kinds)-1])
	}
}

func TestCancelledParentStopsTheChild(t *testing.T) {
	// A stopped parent must not leave a subagent running: it would keep
	// spending the user's tokens on a job nobody is waiting for.
	started := make(chan struct{})
	release := make(chan struct{})
	cfg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		sse(w, "late answer", 1)
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- (&Runner{Config: cfg}).Run(ctx, Spawn{Role: RoleExplore, Task: "slow"})
	}()
	<-started
	cancel()
	select {
	case res := <-done:
		if res.Err == nil {
			t.Error("a cancelled run must not report success")
		}
	case <-context.Background().Done():
	}
}
