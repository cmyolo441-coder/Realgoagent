package agent

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/nova-ai/nova/internal/tools"
)

// While the model streams a write, the UI must get progress frames carrying
// the merged arguments so far — that is what renders the live lines.
func TestRunEmitsToolProgressWhileStreaming(t *testing.T) {
	var calls atomic.Int32
	ag, events := newTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\",\"function\":{\"name\":\"write\",\"arguments\":\"{\\\"path\\\":\\\"a\\\"\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\",\"function\":{\"arguments\":\", more text\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, sse("done"))
	})
	tools.RegisterDefaults(ag.opt.Registry)
	if err := ag.Run("hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := events()
	if n := countKind(evs, EvToolProgress); n < 2 {
		t.Fatalf("want >=2 tool_progress events, got %d", n)
	}
	var last Event
	for _, e := range evs {
		if e.Kind == EvToolProgress {
			last = e
		}
	}
	if last.Tool != "write" {
		t.Errorf("last progress tool = %q, want write", last.Tool)
	}
	if last.Output == "" || last.Text != "c1" {
		t.Errorf("last progress carries merged args and call id, got output=%q id=%q", last.Output, last.Text)
	}
}
