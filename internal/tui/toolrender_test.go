package tui

import (
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

func plain(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = util.Strip(r)
	}
	return out
}

// A tool call is one line: a marker, the tool, and what it was asked to do.
func TestToolCallIsOneLine(t *testing.T) {
	got := util.Strip(toolCall(theme.Get("nova"), "read", "/tmp/x.go"))
	if strings.Contains(got, "\n") {
		t.Errorf("tool call spans lines: %q", got)
	}
	if !strings.Contains(got, "read") || !strings.Contains(got, "/tmp/x.go") {
		t.Errorf("tool call = %q, want the tool and its argument", got)
	}
}

// A tool with no argument still renders, just without a trailing gap.
func TestToolCallWithoutArgs(t *testing.T) {
	got := util.Strip(toolCall(theme.Get("nova"), "list_dir", ""))
	if strings.TrimSpace(got) != "▸ list_dir" {
		t.Errorf("tool call = %q, want %q", got, "▸ list_dir")
	}
}

// A failure must be obvious at a glance and must not be hidden by styling.
func TestToolResultMarksFailure(t *testing.T) {
	ok := plain(toolResult(theme.Get("nova"), "read", "2ms", "file contents", false, 80))
	bad := plain(toolResult(theme.Get("nova"), "read", "2ms", "no such file", true, 80))

	if !strings.Contains(ok[0], "✓") {
		t.Errorf("success line = %q, want a check mark", ok[0])
	}
	if !strings.Contains(bad[0], "✗") {
		t.Errorf("failure line = %q, want a cross", bad[0])
	}
	if !strings.Contains(ok[0], "2ms") {
		t.Errorf("success line = %q, want the duration", ok[0])
	}
}

// The duration is a rounding artefact under a millisecond; showing "0s" is
// noise, so it is dropped.
func TestToolResultDropsZeroDuration(t *testing.T) {
	got := plain(toolResult(theme.Get("nova"), "read", "", "x", false, 80))
	if strings.Contains(got[0], "0s") {
		t.Errorf("line = %q, want no duration", got[0])
	}
}

// Output is indented under the status line, not appended to it. The old format
// glued the first output line onto the header with a "──" separator, which ran
// the line past the terminal width and wrapped.
func TestToolOutputIsIndentedNotAppended(t *testing.T) {
	rows := plain(toolResult(theme.Get("nova"), "bash", "1s", "line one\nline two", false, 80))
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %q", len(rows), rows)
	}
	if !strings.HasPrefix(rows[1], "    ") {
		t.Errorf("output row = %q, want it indented", rows[1])
	}
	for i, r := range rows {
		if len(r) > 80 {
			t.Errorf("row %d is %d columns, want it kept narrow", i, len(r))
		}
	}
}

// A chatty command must be summarised, not scrolled: the point of showing
// output is the shape of the result.
func TestToolOutputIsCapped(t *testing.T) {
	var b strings.Builder
	for i := range 200 {
		b.WriteString("output line number " + itoa(i) + "\n")
	}
	rows := plain(toolResult(theme.Get("nova"), "bash", "1s", b.String(), false, 80))

	if len(rows) > maxToolLines+2 {
		t.Fatalf("rendered %d rows, want at most %d plus a status line and a summary",
			len(rows), maxToolLines)
	}
	last := rows[len(rows)-1]
	if !strings.Contains(last, "more lines") {
		t.Errorf("last row = %q, want a note that output was cut", last)
	}
	if !strings.Contains(last, "192") {
		t.Errorf("last row = %q, want the number of hidden lines", last)
	}
}

// Short output is shown in full, with no cut note.
func TestToolOutputShortIsNotCut(t *testing.T) {
	rows := plain(toolResult(theme.Get("nova"), "read", "1ms", "one\ntwo", false, 80))
	if strings.Contains(strings.Join(rows, "\n"), "more lines") {
		t.Errorf("rows = %q, want no cut note for short output", rows)
	}
}

// A very long single line is truncated rather than left to wrap the layout.
func TestToolOutputTruncatesLongLines(t *testing.T) {
	long := strings.Repeat("x", 5000)
	rows := plain(toolResult(theme.Get("nova"), "bash", "1s", long, false, 80))
	for i, r := range rows {
		if len([]rune(r)) > maxToolLineWidth+8 {
			t.Errorf("row %d is %d columns, want it truncated to about %d", i, len([]rune(r)), maxToolLineWidth)
		}
	}
}

// Empty output leaves just the status line.
func TestToolOutputEmpty(t *testing.T) {
	rows := plain(toolResult(theme.Get("nova"), "write", "1ms", "", false, 80))
	if len(rows) != 1 {
		t.Errorf("rows = %q, want only the status line", rows)
	}
}

// Blank runs collapse to one marker, so whitespace does not become a wall.
func TestCollapseBlank(t *testing.T) {
	got := collapseBlank([]string{"a", "", "", "", "b"})
	want := []string{"a", "…", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Leading and trailing blank lines are dropped without leaving a marker.
func TestCollapseBlankTrimsEdges(t *testing.T) {
	got := collapseBlank([]string{"", "  ", "a", "b", ""})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %q, want [a b]", got)
	}
}

// Only the tail of a long reply is re-rendered per frame, which is what keeps
// long answers smooth. The tail must start on a line boundary.
func TestStreamTailBoundsWorkAndKeepsLineBoundary(t *testing.T) {
	long := strings.Repeat("line of prose\n", 2000)
	got := streamTail(long, maxStreamSource)

	if len(got) > maxStreamSource+8 {
		t.Errorf("tail is %d bytes, want at most about %d", len(got), maxStreamSource)
	}
	if !strings.HasPrefix(got, "…\n") {
		t.Errorf("tail = %q, want it marked as elided", got[:20])
	}
	// What follows the elision marker must begin at a line start, so the
	// renderer never sees half a word.
	// TrimPrefix, not got[2:]: the marker is a multi-byte rune, and slicing by
	// bytes would cut it in half.
	body := strings.TrimPrefix(got, "…\n")
	if !strings.HasPrefix(body, "line of prose\n") {
		t.Errorf("tail starts with %q, want it to start at a line boundary", body[:20])
	}
}

// A short reply is passed through untouched.
func TestStreamTailPassesShortText(t *testing.T) {
	if got := streamTail("short", maxStreamSource); got != "short" {
		t.Errorf("got %q, want it unchanged", got)
	}
}
