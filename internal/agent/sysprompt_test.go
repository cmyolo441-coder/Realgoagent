package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/tools"
)

// newCachingAgent builds an agent over a workspace holding a large AGENTS.md,
// so rendering the system prompt is expensive enough for its frequency to be
// observable, and counts how often that render actually happens.
func newCachingAgent(t *testing.T, handler http.HandlerFunc) (*Agent, *int32) {
	t.Helper()
	dir := t.TempDir()
	body := strings.Repeat("The project prefers Y and never does Z. ", 2000) // ~100KB
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	reg := tools.NewRegistry()
	tools.RegisterDefaults(reg)
	tools.SetWorkspace(dir, "/bin/sh")

	ag, err := New(Options{
		Config:    &config.Config{MaxIterations: 10},
		Provider:  &config.Provider{Name: "stub", BaseURL: srv.URL, APIKey: "k"},
		Model:     &config.Model{ID: "stub-model"},
		Registry:  reg,
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag.setClient(llm.NewClient(srv.URL, "k", "stub-model"))
	ag.cwd = dir

	var renders int32
	var mu sync.Mutex
	ag.onRender = func() { mu.Lock(); renders++; mu.Unlock() }
	return ag, &renders
}

func renderCount(n *int32) int {
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	return int(*n)
}

// A turn that takes several model calls must render the system prompt once,
// not once per call. Re-rendering shells out to git and re-reads the
// workspace's instruction files, so doing it per request put that cost in
// front of every step the model asked for.
func TestSystemPromptRenderedOncePerTurn(t *testing.T) {
	var mu sync.Mutex
	iter := 0
	ag, renders := newCachingAgent(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		iter++
		n := iter
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		// Three rounds that each end in a tool call, then a final answer.
		if n <= 3 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c%d\",\"function\":{\"name\":\"list_dir\",\"arguments\":\"{\\\"path\\\":\\\".\\\"}\"}}]}}]}\n\n", n)
		}
		fmt.Fprint(w, sse("done"))
	})

	if err := ag.Run("do the thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	got := iter
	mu.Unlock()
	if got != 4 {
		t.Fatalf("server saw %d requests, want 4 (3 tool rounds + final answer)", got)
	}
	if n := renderCount(renders); n != 1 {
		t.Errorf("system prompt rendered %d times for a 4-request turn, want exactly 1", n)
	}
}

// A mode switch changes what the prompt says, so the cache must not go on
// serving the old text.
func TestSystemPromptFollowsPlanMode(t *testing.T) {
	ag, _ := newCachingAgent(t, func(w http.ResponseWriter, r *http.Request) {})

	if got := ag.SystemPrompt(); strings.Contains(got, "## Plan mode") {
		t.Fatalf("prompt mentions plan mode before plan mode was enabled")
	}

	ag.SetPlanMode(true)
	if got := ag.SystemPrompt(); !strings.Contains(got, "## Plan mode") {
		t.Errorf("after SetPlanMode(true) the prompt still lacks the plan-mode section")
	}

	ag.SetPlanMode(false)
	if got := ag.SystemPrompt(); strings.Contains(got, "## Plan mode") {
		t.Errorf("after SetPlanMode(false) the prompt still claims plan mode")
	}
}

// Two calls in quick succession must return byte-identical text, timestamp
// line included. The prompt is a single system message, and a body that
// changes between the two requests of one turn is a different system message
// each time.
func TestSystemPromptStableWithinTTL(t *testing.T) {
	ag, _ := newCachingAgent(t, func(w http.ResponseWriter, r *http.Request) {})
	first := ag.SystemPrompt()
	second := ag.SystemPrompt()
	if first != second {
		t.Errorf("two SystemPrompt calls differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// The git context is read at most once per TTL, so a turn's iterations do not
// each pay for two git subprocesses.
func TestGitContextReusedWithinTTL(t *testing.T) {
	ag, _ := newCachingAgent(t, func(w http.ResponseWriter, r *http.Request) {})

	for i := 0; i < 5; i++ {
		_ = ag.SystemPrompt()
	}

	ag.mu.Lock()
	stamp := ag.gitStamp
	ag.mu.Unlock()
	if stamp.IsZero() {
		t.Fatalf("git context was never read")
	}

	// Drop the rendered-prompt cache so the next call has to go back through
	// renderSystemPrompt, and confirm it reuses the git read rather than
	// spawning git again.
	ag.mu.Lock()
	ag.sysCache = sysCacheEntry{}
	ag.gitStamp = stamp
	ag.mu.Unlock()

	_ = ag.SystemPrompt()
	ag.mu.Lock()
	after := ag.gitStamp
	ag.mu.Unlock()
	if !after.Equal(stamp) {
		t.Errorf("git was re-read within the TTL: stamp moved from %s to %s", stamp, after)
	}
}

// A rate-limited provider must reach the front-end as an event. The turn
// produces nothing while the client waits, so a silent twenty-second gap is
// the difference between "the model is slow" and "the program hung".
func TestAgentSurfacesRateLimitWait(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ag, events := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {})
	cl := llm.NewClient(srv.URL, "k", "stub-model")
	cl.MaxRetries = 1
	ag.setClient(cl)

	var got []string
	ag.opt.EventSink = func(e Event) {
		if e.Kind == EvModelWait {
			mu.Lock()
			got = append(got, e.Text)
			mu.Unlock()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ag.RunTask(ctx, "hi")

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("no EvModelWait reached the sink after %d rate-limited attempts", calls)
	}
	if !strings.Contains(got[0], "rate limit") {
		t.Errorf("EvModelWait text = %q, want it to name the rate limit", got[0])
	}
	_ = events
}
