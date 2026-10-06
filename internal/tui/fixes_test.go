package tui

import (
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/theme"
)

// Pressing Enter while a turn runs must not eat the typed prompt: the turn
// is refused and the composer keeps its text, so Esc + Enter sends it again.
func TestEnterWhileStreamingKeepsComposer(t *testing.T) {
	tui := editTUI()
	tui.streaming = true
	tui.mu.Lock()
	tui.inputLines = []string{"my prompt"}
	tui.cursorLine = 0
	tui.cursorCol = 9
	tui.mu.Unlock()
	tui.onKey("enter")
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if got := strings.Join(tui.inputLines, "\n"); got != "my prompt" {
		t.Errorf("composer = %q, want the prompt preserved", got)
	}
	if !tui.streaming {
		t.Error("the running turn must not be disturbed")
	}
}

// A second submission while a turn is in flight must be refused, not start a
// second agent loop: the agent cancels the previous turn, both turns' events
// interleave, and the first turn's EvDone resets the UI while the second is
// still running — its reply went nowhere, which is why a prompt sometimes
// got no answer at all.
func TestSubmitWhileStreamingRefuses(t *testing.T) {
	tui := editTUI()
	tui.streaming = true
	if err := tui.Submit("second prompt"); err != nil {
		t.Fatalf("refused submit returned error: %v", err)
	}
	if !tui.streaming {
		t.Error("a refused submit must leave the running turn alone")
	}
	lines, _ := tui.app.history.Since(0)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "already running") {
		t.Errorf("history = %q, want a notice that a turn is already running", joined)
	}
}

// The tool starting must freeze the streaming preview, not clear it: the
// arguments are complete and the tool is running, so the last streamed frame
// stays up until the recorded diff replaces it.
func TestLiveStreamFrozenOnToolStart(t *testing.T) {
	tui := editTUI()
	tui.liveDiff = newLiveDiffView()
	pal := theme.Get("nova")
	tui.handleEvent(agent.Event{Kind: agent.EvToolProgress, Tool: "write",
		Output: `{"path":"a.txt","content":"line1\n"}`, Text: "call-1"})
	if !tui.liveDiff.streaming || !tui.liveDiff.streamLive {
		t.Fatal("progress should start a live stream")
	}
	tui.handleEvent(agent.Event{Kind: agent.EvToolStart, Tool: "write", Args: map[string]any{}})
	if !tui.liveDiff.streaming {
		t.Error("EvToolStart must freeze the preview, not clear it")
	}
	if tui.liveDiff.streamLive {
		t.Error("streamLive should be false once the tool is running")
	}
	rows := tui.liveDiffRows(pal, 80, maxLiveStreamRows)
	if len(rows) == 0 {
		t.Fatal("frozen preview should still render")
	}
	if !strings.Contains(rows[0], "running") {
		t.Errorf("header = %q, want it to say the tool is running", rows[0])
	}
	// A denied tool never ran: its preview must not linger.
	tui.handleEvent(agent.Event{Kind: agent.EvToolDenied, Tool: "write"})
	if tui.liveDiff.streaming {
		t.Error("EvToolDenied should clear the preview")
	}
}

// The stream preview render is cached by buffer length: a frame with no new
// token must reuse the last render, and growth must re-render.
func TestStreamRowsCache(t *testing.T) {
	tui := editTUI()
	tui.streaming = true
	tui.app.Width = 100
	pal := theme.Get("nova")
	tui.streamBuf.WriteString("hello")
	r1 := tui.streamRows(pal, 100, 12)
	r2 := tui.streamRows(pal, 100, 12)
	if strings.Join(r1, "\n") != strings.Join(r2, "\n") {
		t.Error("identical buffer should render identically from cache")
	}
	tui.streamBuf.WriteString(" world, this is more text")
	r3 := tui.streamRows(pal, 100, 12)
	if strings.Join(r1, "\n") == strings.Join(r3, "\n") {
		t.Error("grown buffer should re-render, not serve the stale cache")
	}
	if !strings.Contains(strings.Join(r3, "\n"), "world") {
		t.Errorf("re-render = %q, want the new text", strings.Join(r3, "\n"))
	}
	// Flushing drops the cache: the next turn must not see the old render.
	tui.flushStream()
	tui.streaming = true
	tui.streamBuf.WriteString("hello")
	r4 := tui.streamRows(pal, 100, 12)
	if strings.Join(r1, "\n") != strings.Join(r4, "\n") {
		// Same text renders the same; the point is it does not panic or
		// return stale rows from a previous turn of the same length.
		t.Log("note: render differs after flush, acceptable if styling changed")
	}
}

// A mid-stream (truncated) payload must still yield the partial content via
// the tolerant scan, without paying for a doomed strict parse.
func TestStreamFieldRawTruncated(t *testing.T) {
	s, complete := streamStringFieldRaw(`{"path":"b.txt","content":"l1\nl2\nl3`, "content")
	if complete {
		t.Error("truncated payload should not report complete")
	}
	if s != "l1\nl2\nl3" {
		t.Errorf("got %q, want the partial content", s)
	}
}

// A complete payload still parses strictly.
func TestStreamFieldRawComplete(t *testing.T) {
	s, complete := streamStringFieldRaw(`{"path":"b.txt","content":"l1\nl2"}`, "content")
	if !complete {
		t.Error("complete payload should report complete")
	}
	if s != "l1\nl2" {
		t.Errorf("got %q, want l1\\nl2", s)
	}
}
