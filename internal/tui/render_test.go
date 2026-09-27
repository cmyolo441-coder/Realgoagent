package tui

import (
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

func testPalette() theme.Palette { return theme.Get("nova") }

// Every row of the box must fit the terminal exactly. An earlier revision hung
// the status rail off the right edge, which made each row one column too wide;
// the terminal wrapped it and dragged the whole layout apart.
func TestPromptBoxRowsFitTerminalWidth(t *testing.T) {
	p := testPalette()
	for _, width := range []int{20, 24, 40, 80, 92, 200} {
		rows, _, _ := renderPromptBox(p, width, []string{""},
			p.Style("dim", "ready"), p.Style("dim", "nova · model"), "", 0, 0, 100)
		for i, r := range rows {
			if got := util.VisibleWidth(r); got != width {
				t.Errorf("width %d row %d: visible width = %d, want %d (%q)",
					width, i, got, width, util.Strip(r))
			}
		}
	}
}

// A long rail must be truncated, not allowed to push the row over the edge.
func TestPromptBoxTruncatesLongRails(t *testing.T) {
	p := testPalette()
	long := strings.Repeat("very long status text ", 12)
	rows, _, _ := renderPromptBox(p, 40, []string{""}, p.Style("dim", long), p.Style("dim", long), "", 0, 0, 100)
	for i, r := range rows {
		if got := util.VisibleWidth(r); got != 40 {
			t.Errorf("row %d: visible width = %d, want 40", i, got)
		}
	}
}

// The rails belong inside the border: the top row must open with ╭ and close
// with ╮ on the same line.
func TestPromptBoxRailsSitInsideBorder(t *testing.T) {
	p := testPalette()
	rows, _, _ := renderPromptBox(p, 40, []string{"hi"},
		p.Style("dim", "ready"), p.Style("dim", "nova"), "", 0, 0, 100)
	top := util.Strip(rows[0])
	if !strings.HasPrefix(top, tl) || !strings.HasSuffix(top, tr) {
		t.Errorf("top rail = %q, want it to start with %s and end with %s", top, tl, tr)
	}
	if !strings.Contains(top, "ready") {
		t.Errorf("top rail = %q, want it to carry the status", top)
	}
	// The bottom border is the last row starting with ╰; a hint line may sit
	// below it.
	var bottom string
	for _, r := range rows {
		if strings.HasPrefix(util.Strip(r), bl) {
			bottom = util.Strip(r)
		}
	}
	if bottom == "" {
		t.Fatalf("no bottom border rendered")
	}
	if !strings.HasSuffix(bottom, br) {
		t.Errorf("bottom rail = %q, want it to end with %s", bottom, br)
	}
	if !strings.Contains(bottom, "nova") {
		t.Errorf("bottom rail = %q, want it to carry the session info", bottom)
	}
}

// The caret has to land on the character it belongs to, which after wrapping
// is not the same index as the column in the source line.
func TestPromptBoxCaretFollowsWrappedText(t *testing.T) {
	p := testPalette()
	const width = 40
	inner := width - 4

	line := strings.Repeat("x", inner+10) // wraps onto two display rows
	rows, caretRow, caretCol := renderPromptBox(p, width, []string{line},
		p.Style("dim", "ready"), p.Style("dim", "nova"), "", 0, inner+5, 100)

	// rows[0] is the top border, so the first content row is rows[1].
	if caretRow != 2 {
		t.Fatalf("caretRow = %d, want 2 (the wrapped second row)", caretRow)
	}
	caretLine := util.Strip(rows[caretRow])
	if !strings.Contains(caretLine, "█") {
		t.Errorf("caret row = %q, want it to contain the caret", caretLine)
	}
	// The caret sits five runes into the second wrapped row, after "│ ".
	if got, want := caretCol, 2+5+1; got != want {
		t.Errorf("caretCol = %d, want %d", got, want)
	}
}

// On the first line of an empty composer the caret sits in the first content
// row, just inside the border.
func TestPromptBoxCaretOnEmptyComposer(t *testing.T) {
	p := testPalette()
	rows, caretRow, caretCol := renderPromptBox(p, 40, []string{""},
		p.Style("dim", "ready"), p.Style("dim", "nova"), "", 0, 0, 100)

	if caretRow != 1 {
		t.Errorf("caretRow = %d, want 1 (first content row)", caretRow)
	}
	if caretCol != 3 {
		t.Errorf("caretCol = %d, want 3 (just inside the border)", caretCol)
	}
	if !strings.Contains(util.Strip(rows[caretRow]), "█") {
		t.Errorf("caret row = %q, want the caret drawn", util.Strip(rows[caretRow]))
	}
}

// Moving to the second source line must move the caret down a row, even when
// the first line is empty.
func TestPromptBoxCaretTracksSecondLine(t *testing.T) {
	p := testPalette()
	_, caretRow, _ := renderPromptBox(p, 40, []string{"", "second"},
		p.Style("dim", "ready"), p.Style("dim", "nova"), "", 1, 0, 100)
	if caretRow != 2 {
		t.Errorf("caretRow = %d, want 2 (row after the empty first line)", caretRow)
	}
}

// A composer taller than the space available must scroll rather than overflow,
// and the caret must stay inside the capped frame.
func TestPromptBoxCapsHeightAndKeepsCaretVisible(t *testing.T) {
	p := testPalette()
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "line"
	}
	const budget = 6
	rows, caretRow, _ := renderPromptBox(p, 40, lines,
		p.Style("dim", "ready"), p.Style("dim", "nova"), "hint", 19, 4, budget)

	if len(rows) > budget {
		t.Fatalf("box rendered %d rows, budget was %d", len(rows), budget)
	}
	if caretRow < 0 || caretRow >= len(rows) {
		t.Fatalf("caretRow %d falls outside the %d-row frame", caretRow, len(rows))
	}
	if !strings.Contains(util.Strip(rows[caretRow]), "█") {
		t.Errorf("caret row = %q, want the caret drawn", util.Strip(rows[caretRow]))
	}
}

// A terminal too short for the box still gets a coherent frame.
func TestPromptBoxHandlesTinyBudget(t *testing.T) {
	p := testPalette()
	rows, caretRow, _ := renderPromptBox(p, 40, []string{"a", "b", "c"},
		p.Style("dim", "ready"), p.Style("dim", "nova"), "hint", 2, 0, 1)
	if len(rows) == 0 {
		t.Fatal("box rendered no rows")
	}
	if caretRow < 0 || caretRow >= len(rows) {
		t.Errorf("caretRow %d outside %d-row frame", caretRow, len(rows))
	}
}
