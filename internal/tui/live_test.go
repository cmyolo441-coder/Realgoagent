package tui

import (
	"strings"
	"testing"
)

// The live region is repainted in place, so every repaint has to find the top
// of the region again from wherever the cursor was left.
//
// Getting this wrong by a single row is invisible in a screenshot but obvious
// in use: the prompt walks up the screen one row per repaint until it reaches
// the top, or the last committed line gets wiped.
func TestMoveToLiveTopStepsUpByCaretRowOnly(t *testing.T) {
	for _, tc := range []struct {
		live  int
		caret int
		want  string
	}{
		{live: 4, caret: 1, want: "\x1b[1A"},
		{live: 5, caret: 2, want: "\x1b[2A"},
		{live: 1, caret: 0, want: ""}, // caret on the region's only row
		{live: 6, caret: 5, want: "\x1b[5A"},
	} {
		tui := &TUI{liveLines: tc.live, caretRowLast: tc.caret, rowsWritten: 40}
		var b strings.Builder
		tui.moveToLiveTop(&b)
		got := b.String()

		if tc.want == "" {
			if strings.Contains(got, "A") {
				t.Errorf("live=%d caret=%d: emitted %q, want no cursor movement", tc.live, tc.caret, got)
			}
		} else if !strings.HasPrefix(got, tc.want) {
			t.Errorf("live=%d caret=%d: emitted %q, want it to start with %q", tc.live, tc.caret, got, tc.want)
		}
		// The column is always reset: a cursor-up preserves it.
		if !strings.Contains(got, "\r") {
			t.Errorf("live=%d caret=%d: emitted %q, want a carriage return to home the column", tc.live, tc.caret, got)
		}
		// The old region must not be counted twice.
		if tui.rowsWritten != 40-tc.live {
			t.Errorf("live=%d: rowsWritten = %d, want %d", tc.live, tui.rowsWritten, 40-tc.live)
		}
		if tui.liveLines != 0 || tui.caretRowLast != 0 {
			t.Errorf("live=%d: bookkeeping not cleared: liveLines=%d caretRowLast=%d",
				tc.live, tui.liveLines, tui.caretRowLast)
		}
	}
}

// With nothing on screen there is nothing to step over.
func TestMoveToLiveTopNoopWhenEmpty(t *testing.T) {
	tui := &TUI{liveLines: 0, rowsWritten: 10}
	var b strings.Builder
	tui.moveToLiveTop(&b)
	if b.Len() != 0 {
		t.Errorf("emitted %q for an empty region, want nothing", b.String())
	}
	if tui.rowsWritten != 10 {
		t.Errorf("rowsWritten = %d, want it untouched", tui.rowsWritten)
	}
}

// A caret row that somehow exceeds the region must be clamped, or the cursor
// would fly up into the transcript and wipe it.
func TestMoveToLiveTopClampsBadCaretRow(t *testing.T) {
	tui := &TUI{liveLines: 3, caretRowLast: 99, rowsWritten: 10}
	var b strings.Builder
	tui.moveToLiveTop(&b)
	if !strings.Contains(b.String(), "\x1b[2A") {
		t.Errorf("emitted %q, want the step clamped to the region's 2 rows", b.String())
	}
	if tui.rowsWritten != 7 {
		t.Errorf("rowsWritten = %d, want 7", tui.rowsWritten)
	}
}
