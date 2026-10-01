package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/tools"
)

// turn builds one user exchange: the user message, the assistant reply that
// asked for tool calls, and the results.
func turn(user, reply string, n int) []llm.Message {
	out := []llm.Message{{Role: llm.RoleUser, Content: user}}
	if reply == "" {
		return out
	}
	msg := llm.Message{Role: llm.RoleAssistant, Content: reply}
	for i := range n {
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID: fmt.Sprintf("c%d", i), Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"a.go"}`},
		})
	}
	out = append(out, msg)
	for i := range n {
		out = append(out, llm.Message{
			Role: llm.RoleTool, ToolCallID: fmt.Sprintf("c%d", i),
			Name: "read", Content: "result",
		})
	}
	return out
}

// A conversation that outgrows the context window has to be cut back, or the
// session ends with every request refused: the history only ever grows, and
// the model call that could notice is the one carrying the too-long history.
func TestTrimHistoryDropsTheOldestTurns(t *testing.T) {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: "system"})
	for i := range 5 {
		msgs = append(msgs, turn(fmt.Sprintf("question %d", i), "reply", 2)...)
	}

	budget := estimateTokens(msgs) / 2
	trimmed, cut := trimHistory(msgs, budget)
	if !cut {
		t.Fatal("an oversized conversation was sent untrimmed")
	}
	if estimateTokens(trimmed) > budget {
		t.Fatalf("trimmed history is still %d tokens, over the %d budget",
			estimateTokens(trimmed), budget)
	}
	if len(trimmed) >= len(msgs) {
		t.Fatal("nothing was dropped")
	}
}

// The newest turn is the one being answered, so it is never the thing cut.
func TestTrimHistoryKeepsTheNewestTurn(t *testing.T) {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: "system"})
	for i := range 4 {
		msgs = append(msgs, turn(fmt.Sprintf("question %d", i), "reply", 1)...)
	}
	trimmed, _ := trimHistory(msgs, 200)

	// The newest user turn is the one being answered, so it is never the thing
	// cut.
	var newest string
	for _, m := range trimmed {
		if m.Role == llm.RoleUser {
			newest = m.Content
		}
	}
	if newest != "question 3" {
		t.Fatalf("the newest turn was cut, last user message = %q", newest)
	}
}

// A cut must never leave a tool result whose tool_calls were dropped: that is a
// protocol violation, and providers reject the whole request over it — which
// is the failure the trim was meant to prevent.
func TestTrimHistoryKeepsToolResultsPairedWithTheirCalls(t *testing.T) {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: "system"})
	for i := range 4 {
		msgs = append(msgs, turn(fmt.Sprintf("question %d", i), "reply", 3)...)
	}

	for _, budget := range []int{60, 200, 1000, 5000, 100000} {
		trimmed, _ := trimHistory(msgs, budget)
		pending := map[string]bool{}
		for _, m := range trimmed {
			switch m.Role {
			case llm.RoleAssistant:
				pending = map[string]bool{}
				for _, tc := range m.ToolCalls {
					pending[tc.ID] = true
				}
			case llm.RoleTool:
				if m.ToolCallID != "" && !pending[m.ToolCallID] {
					t.Fatalf("budget %d: orphaned tool result %q", budget, m.ToolCallID)
				}
			}
		}
	}
}

// A turn that does not fit on its own is reported rather than silently halved:
// there is nothing older to drop, and the caller has to say so.
func TestTrimHistoryReportsAnUnshrinkableTurn(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("x", 100_000)},
	}
	trimmed, cut := trimHistory(msgs, 1000)
	if !cut {
		t.Fatal("an oversized single turn must be reported as not trimmable")
	}
	if len(trimmed) != len(msgs) {
		t.Fatal("a single turn must be kept whole or reported, never split")
	}
}

// A history inside the budget is left exactly as it is — the model is not told
// anything was dropped when nothing was.
func TestTrimHistoryLeavesASmallConversationAlone(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "hi"}}
	trimmed, cut := trimHistory(msgs, 100_000)
	if cut || len(trimmed) != len(msgs) {
		t.Fatalf("a small conversation was rewritten: cut=%v", cut)
	}
}

// countingTool records how many times it ran.
type countingTool struct{ n *int }

func (countingTool) Name() string           { return "list_dir" }
func (countingTool) Description() string    { return "test tool" }
func (countingTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c countingTool) Run(ctx context.Context, a map[string]any) *tools.Result {
	*c.n++
	return &tools.Result{Output: "listed"}
}

// newLoopAgent points an agent at srv with reg installed, recording events.
func newLoopAgent(t *testing.T, srv *httptest.Server, reg *tools.Registry) (*Agent, func() []Event) {
	t.Helper()
	var mu sync.Mutex
	var seen []Event
	ag, err := New(Options{
		Config:   &config.Config{MaxIterations: 30},
		Provider: &config.Provider{Name: "stub", BaseURL: srv.URL, APIKey: "k"},
		Model:    &config.Model{ID: "stub-model"},
		Registry: reg,
		EventSink: func(e Event) {
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), seen...)
	}
}

// A model that asks for the same call over and over gets the same result back
// every time, and the turn is spent doing it. After a few identical calls the
// loop has to say so, so the model can do something else or report back.
func TestIdenticalToolCallsAreStopped(t *testing.T) {
	var mu sync.Mutex
	iterations := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		iterations++
		n := iterations
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= repeatLimit*2 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c\",\"function\":{\"name\":\"list_dir\",\"arguments\":\"{\\\"path\\\":\\\".\\\"}\"}}]}}]}\n\n")
		} else {
			fmt.Fprint(w, sse("done"))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	calls := 0
	reg := tools.NewRegistry()
	reg.Register(countingTool{n: &calls})
	ag, events := newLoopAgent(t, srv, reg)

	if err := ag.Run("list the workspace"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls > repeatLimit {
		t.Fatalf("the tool ran %d times, want it stopped after %d", calls, repeatLimit)
	}
	if calls < repeatLimit {
		t.Fatalf("the tool ran %d times, want the repeats allowed up to %d", calls, repeatLimit)
	}
	// The model has to be told why in its own context, and the user has to see
	// it too: a silent skip reads as a tool that did nothing.
	var denial string
	for _, e := range events() {
		if e.Kind == EvToolDenied && e.Text != "" {
			denial = e.Text
		}
	}
	if !strings.Contains(denial, "identical arguments") {
		t.Fatalf("the repeat was not explained, got %q", denial)
	}
	var told bool
	for _, m := range ag.Messages() {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "identical arguments") {
			told = true
		}
	}
	if !told {
		t.Fatal("the repeat was not reported back to the model as a tool result")
	}
}

// A model asking for the same call with different arguments is doing something
// new, and the guard must not stop it.
func TestDifferentArgumentsAreNotTreatedAsRepeats(t *testing.T) {
	counts := map[string]int{}
	// Far more calls than the limit, each one a different file.
	for i := range repeatLimit * 4 {
		args := fmt.Sprintf(`{"path":"file-%d.go"}`, i)
		if err := repeatedToolCall(counts, "read", args); err != nil {
			t.Fatalf("distinct calls were treated as repeats: %v", err)
		}
	}
}

// Whitespace is not a difference: a model re-emitting the same call with
// different formatting is still repeating itself.
func TestRepeatDetectionIgnoresFormatting(t *testing.T) {
	counts := map[string]int{}
	// The limit is allowed in either formatting; the call after it is not.
	for range repeatLimit {
		if err := repeatedToolCall(counts, "read", "{\n  \"path\": \"a.go\"\n}"); err != nil {
			t.Fatalf("reformatted call was counted as new: %v", err)
		}
	}
	if err := repeatedToolCall(counts, "read", `{"path":"a.go"}`); err == nil {
		t.Fatal("the same call, reformatted, was not detected as a repeat")
	}
}
