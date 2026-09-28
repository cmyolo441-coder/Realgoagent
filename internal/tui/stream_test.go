package tui

import (
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// A write arriving in chunks must grow the panel line by line: frame 1 shows
// one line, frame 3 shows ten, and the tail pins to the newest lines.
func TestLiveStreamGrowsLineByLine(t *testing.T) {
	tui := editTUI()
	tui.liveDiff = newLiveDiffView()
	pal := theme.Get("nova")
	frames := []string{
		`{"path":"a.txt","content":"line1\n"}`,
		`{"path":"a.txt","content":"line1\nline2\nline3\n"}`,
		`{"path":"a.txt","content":"line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"}`,
	}
	var counts []int
	for _, f := range frames {
		tui.handleEvent(agent.Event{Kind: agent.EvToolProgress, Tool: "write", Output: f, Text: "call-1"})
		rows := tui.liveDiffRows(pal, 80, maxLiveStreamRows)
		counts = append(counts, len(rows)-1) // minus header
	}
	if !(counts[0] < counts[1] && counts[1] < counts[2]) {
		t.Fatalf("panel did not grow line by line: %v", counts)
	}
	if counts[2] != 10 {
		t.Fatalf("10-line write shows %d lines, want 10", counts[2])
	}
	if !tui.liveDiffOpen() {
		t.Fatal("streaming should open the panel with no recorded edits")
	}
}

// A truncated JSON tail mid-stream still yields the lines so far: strict
// parsing would show nothing until the closing quote arrives.
func TestLiveStreamToleratesTruncatedJSON(t *testing.T) {
	tui := editTUI()
	tui.liveDiff = newLiveDiffView()
	pal := theme.Get("nova")
	tui.handleEvent(agent.Event{Kind: agent.EvToolProgress, Tool: "write", Output: `{"path":"b.txt","content":"l1\nl2\nl3`, Text: "call-2"})
	rows := tui.liveDiffRows(pal, 80, maxLiveStreamRows)
	if len(rows) < 4 {
		t.Fatalf("truncated frame rendered %d rows, want header + 3 lines", len(rows))
	}
	last := util.Strip(rows[len(rows)-1])
	if !strings.Contains(last, "l3") {
		t.Fatalf("last row %q should hold the partial line l3", last)
	}
}

// The in-flight preview hands over to the recorded diff once the tool runs.
func TestLiveStreamClearsOnToolResult(t *testing.T) {
	tui := editTUI()
	tui.liveDiff = newLiveDiffView()
	tui.handleEvent(agent.Event{Kind: agent.EvToolProgress, Tool: "write", Output: `{"path":"a.txt","content":"x\n"}`, Text: "call-1"})
	if !tui.liveDiff.streaming {
		t.Fatal("progress should start streaming")
	}
	tui.handleEvent(agent.Event{Kind: agent.EvToolResult, Tool: "write", Output: "wrote a.txt"})
	if tui.liveDiff.streaming {
		t.Fatal("tool result should clear the streaming preview")
	}
}

// Non-content tools never open the panel on their own.
func TestLiveStreamIgnoresBash(t *testing.T) {
	tui := editTUI()
	tui.liveDiff = newLiveDiffView()
	tui.handleEvent(agent.Event{Kind: agent.EvToolProgress, Tool: "bash", Output: `{"command":"go test ./..."}`, Text: "call-9"})
	if tui.liveDiff.streaming {
		t.Fatal("bash progress must not start a diff preview")
	}
	if tui.liveDiffOpen() {
		t.Fatal("bash progress must not open the panel")
	}
}
