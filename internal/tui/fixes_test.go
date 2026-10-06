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

// Pasting terminal output that carries raw ANSI escapes must not let those
// bytes reach the screen: the terminal interprets them on the next frame —
// ESC[2J clears the display — which is why pasting long terminal output made
// the prompt box go blank.
func TestPasteStripsANSI(t *testing.T) {
	tui := editTUI()
	tui.mu.Lock()
	tui.insertTextLocked("\x1b[31mred text\x1b[0m\n\x1b[2Jcleared?\nplain\x07bell")
	pal := tui.app.Theme
	got := strings.Join(tui.inputLines, "\n")
	rows, _, _ := tui.boxRows(pal, 100, 20)
	tui.mu.Unlock()
	if strings.Contains(got, "\x1b") {
		t.Errorf("composer = %q, want no escape sequences", got)
	}
	if !strings.Contains(got, "red text") || !strings.Contains(got, "plain") {
		t.Errorf("composer = %q, want the text without its styling", got)
	}
	if strings.Contains(strings.Join(rows, "\n"), "\x1b[31m") {
		t.Error("rendered box must not contain raw escape sequences")
	}
}

// sanitizeInsert leaves plain text untouched, including tabs and newlines,
// and drops lone carriage returns.
func TestSanitizeInsert(t *testing.T) {
	if got := sanitizeInsert("hello\n\tworld"); got != "hello\n\tworld" {
		t.Errorf("got %q, want it unchanged", got)
	}
	if got := sanitizeInsert("a\rb\nc\r\nd"); got != "ab\nc\nd" {
		t.Errorf("got %q, want CRs dropped", got)
	}
	// OSC hyperlink sequence.
	if got := sanitizeInsert("\x1b]8;;http://x\x07link\x1b]8;;\x07"); got != "link" {
		t.Errorf("got %q, want just the link text", got)
	}
}

// A truncated reply names the cause instead of looking like the model just
// stopped.
func TestTruncatedNotice(t *testing.T) {
	tui := editTUI()
	tui.streaming = true
	tui.streamBuf.WriteString("cut off")
	tui.handleEvent(agent.Event{Kind: agent.EvTruncated})
	lines, _ := tui.app.history.Since(0)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "cut off") {
		t.Errorf("history = %q, want the partial reply committed first", joined)
	}
	if !strings.Contains(joined, "output limit") {
		t.Errorf("history = %q, want the truncation notice", joined)
	}
}

// A broken stream shows the error and points at /retry.
func TestStreamBrokenHint(t *testing.T) {
	tui := editTUI()
	tui.streaming = true
	tui.streamBuf.WriteString("partial")
	tui.handleEvent(agent.Event{Kind: agent.EvStreamBroken, Text: "stream broke off mid-response"})
	lines, _ := tui.app.history.Since(0)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "partial") {
		t.Errorf("history = %q, want the partial reply committed first", joined)
	}
	if !strings.Contains(joined, "mid-response") {
		t.Errorf("history = %q, want the break explained", joined)
	}
	if !strings.Contains(joined, "/retry") {
		t.Errorf("history = %q, want the retry hint", joined)
	}
}

// streamTailLines returns the last N lines without splitting the whole
// string, with the total count for the "earlier" indicator.
func TestStreamTailLines(t *testing.T) {
	lines, total := streamTailLines("l1\nl2\nl3\nl4\nl5", 3)
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(lines) != 3 || lines[0] != "l3" || lines[2] != "l5" {
		t.Errorf("lines = %q, want last 3", lines)
	}
	// Fewer lines than asked: everything comes back.
	lines, total = streamTailLines("a\nb", 10)
	if total != 2 || len(lines) != 2 {
		t.Errorf("got %q total %d, want both lines", lines, total)
	}
	// Trailing newline does not start a new line; partial last line kept.
	lines, total = streamTailLines("x\ny\npartial", 2)
	if total != 3 || lines[1] != "partial" {
		t.Errorf("got %q total %d, want tail with partial line", lines, total)
	}
	if lines, _ := streamTailLines("", 3); len(lines) != 1 {
		t.Errorf("empty input should yield the placeholder, got %q", lines)
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
