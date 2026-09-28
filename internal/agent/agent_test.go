package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/tools"
)

// newTestAgent points an agent at a stub OpenAI-compatible endpoint that
// replies with the given SSE body, and records every event it emits.
func newTestAgent(t *testing.T, handler http.HandlerFunc) (*Agent, func() []Event) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{MaxIterations: 3}
	prov := &config.Provider{Name: "stub", BaseURL: srv.URL, APIKey: "test"}
	model := &config.Model{ID: "stub-model"}

	var mu sync.Mutex
	var events []Event
	ag, err := New(Options{
		Config:   cfg,
		Provider: prov,
		Model:    model,
		Registry: tools.NewRegistry(),
		EventSink: func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), events...)
	}
}

// sse builds a streaming chat-completion response body.
func sse(content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", content)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// A turn that completes normally must announce itself exactly once.
func TestRunSignalsCompletionOnSuccess(t *testing.T) {
	ag, events := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse("hello"))
	})

	if err := ag.Run("hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := countKind(events(), EvDone)
	if done != 1 {
		t.Fatalf("EvDone emitted %d times, want exactly 1", done)
	}
	if !strings.Contains(doneText(events()), "completed in") {
		t.Errorf("EvDone text = %q, want a completion summary", doneText(events()))
	}
}

// A failing turn must also announce completion. Before this contract existed
// the front-end kept spinning forever, because only the success path spoke.
func TestRunSignalsCompletionOnStreamError(t *testing.T) {
	ag, events := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		// A non-2xx status makes the client report a stream error, which is
		// the path that used to leave the UI spinning.
		http.Error(w, "bad request", http.StatusBadRequest)
	})

	_ = ag.Run("hi") // the error is reported through the event stream

	if got := countKind(events(), EvDone); got != 1 {
		t.Fatalf("EvDone emitted %d times after a stream error, want exactly 1", got)
	}
	if countKind(events(), EvError) == 0 {
		t.Error("no EvError reported for a failed stream")
	}
}

// OnDone persists the session, so it must fire on the failure path too —
// otherwise a turn that died is lost from the transcript.
func TestRunFiresOnDoneAfterFailure(t *testing.T) {
	ag, _ := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		// A non-2xx status makes the client report a stream error, which is
		// the path that used to leave the UI spinning.
		http.Error(w, "bad request", http.StatusBadRequest)
	})

	var mu sync.Mutex
	fired := 0
	ag.SetOnDone(func() {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	_ = ag.Run("hi")

	mu.Lock()
	defer mu.Unlock()
	if fired != 1 {
		t.Fatalf("OnDone fired %d times after a failed turn, want 1", fired)
	}
}

// Usage is reported for the turn regardless of how it ended.
func TestRunReportsUsageOnFailure(t *testing.T) {
	ag, events := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		// A non-2xx status makes the client report a stream error, which is
		// the path that used to leave the UI spinning.
		http.Error(w, "bad request", http.StatusBadRequest)
	})

	_ = ag.Run("hi")

	if countKind(events(), EvUsage) != 1 {
		t.Fatalf("EvUsage emitted %d times, want exactly 1", countKind(events(), EvUsage))
	}
}

func TestRunAppendsAssistantReply(t *testing.T) {
	ag, _ := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse("hello there"))
	})

	if err := ag.Run("hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := ag.Messages()
	var last llm.Message
	if len(msgs) > 0 {
		last = msgs[len(msgs)-1]
	}
	if last.Role != llm.RoleAssistant || last.Content != "hello there" {
		t.Errorf("last message = %+v, want the assistant reply %q", last, "hello there")
	}
}

func countKind(events []Event, k EventKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func doneText(events []Event) string {
	for _, e := range events {
		if e.Kind == EvDone {
			return e.Text
		}
	}
	return ""
}

// A user-initiated stop must read as "stopped", not as a failure. Cancelling
// the context aborts the stream, and an aborted stream surfaces as an error —
// printing it would tell the user something broke when they simply hit Esc.
func TestCancelStopsTurnWithoutError(t *testing.T) {
	release := make(chan struct{})
	ag, events := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"the printing press\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// Hold the stream open so the turn is genuinely in flight.
		<-release
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ag.Run("write an essay")
	}()

	// Let the first chunk arrive, then stop. The handler stays blocked: only
	// the cancellation may end this turn, so releasing it early would let the
	// stream finish on its own and prove nothing.
	waitFor(t, func() bool { return countKind(events(), EvText) > 0 })
	ag.Cancel()
	waitTurnEnd(t, done)
	close(release)

	if got := countKind(events(), EvError); got != 0 {
		t.Errorf("EvError emitted %d times after a user stop, want 0", got)
	}
	if got := countKind(events(), EvDone); got != 1 {
		t.Fatalf("EvDone emitted %d times after a user stop, want 1", got)
	}
	if txt := doneText(events()); txt != "stopped" {
		t.Errorf("EvDone text = %q, want %q", txt, "stopped")
	}
}

// Stopping must still close the turn and persist the session.
func TestCancelFiresOnDone(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ag, _ := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		close(started)
		<-release
	})

	var mu sync.Mutex
	fired := 0
	ag.SetOnDone(func() {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ag.Run("go")
	}()

	// Cancel only reaches a turn that has installed its context, so wait for
	// the request to actually be in flight first.
	<-started
	ag.Cancel()
	waitTurnEnd(t, done)
	close(release)

	mu.Lock()
	defer mu.Unlock()
	if fired != 1 {
		t.Errorf("OnDone fired %d times after a stop, want 1", fired)
	}
}

// Cancelling with nothing running is harmless.
func TestCancelIsSafeWhenIdle(t *testing.T) {
	ag, _ := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sse("hi"))
	})
	ag.Cancel()
	if err := ag.Run("hi"); err != nil {
		t.Fatalf("a turn after an idle Cancel failed: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within the timeout")
}

// waitTurnEnd fails the test if a turn does not finish promptly, so a
// cancellation that does not take effect is reported rather than hung on.
func waitTurnEnd(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not end after Cancel")
	}
}
