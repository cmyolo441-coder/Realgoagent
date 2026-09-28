package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/theme"
)

// TestLiveDiffAutoOpen verifies the live diff panel shows automatically
// when there are edits.
func TestLiveDiffAutoOpen(t *testing.T) {
	v := newLiveDiffView()
	if v == nil {
		t.Fatal("liveDiffView should not be nil")
	}
	if v.scroll != 0 {
		t.Fatal("scroll should start at 0")
	}
	if v.lastSeq != 0 {
		t.Fatal("lastSeq should start at 0")
	}
}

// TestLiveDiffScrollKeys verifies the scroll keys work correctly.
func TestLiveDiffScrollKeys(t *testing.T) {
	v := newLiveDiffView()
	v.maxScroll = 10

	v.handleKey("down")
	if v.scroll != 1 {
		t.Fatalf("expected scroll 1, got %d", v.scroll)
	}

	v.handleKey("up")
	if v.scroll != 0 {
		t.Fatalf("expected scroll 0, got %d", v.scroll)
	}

	v.handleKey("pagedown")
	if v.scroll != 5 {
		t.Fatalf("expected scroll 5, got %d", v.scroll)
	}

	v.handleKey("pageup")
	if v.scroll != 0 {
		t.Fatalf("expected scroll 0, got %d", v.scroll)
	}

	v.handleKey("home")
	if v.scroll != 0 {
		t.Fatalf("expected scroll 0, got %d", v.scroll)
	}

	// Scrolling past the end stops at the end rather than running off it.
	for range 20 {
		v.handleKey("down")
	}
	if v.scroll != v.maxScroll {
		t.Fatalf("scroll should stop at maxScroll %d, got %d", v.maxScroll, v.scroll)
	}
	for range 20 {
		v.handleKey("up")
	}
	if v.scroll != 0 {
		t.Fatalf("scroll should stop at 0, got %d", v.scroll)
	}
}

// The panel is up from the first edit onwards, so every key it claims is a key
// the user cannot type. With nothing to scroll it must claim none of them.
func TestLiveDiffClaimsNoKeysWhenNothingToScroll(t *testing.T) {
	v := newLiveDiffView()
	for _, k := range []string{"up", "down", "pageup", "pagedown"} {
		if v.handleKey(k) {
			t.Errorf("%s consumed with an empty diff; the composer needs it", k)
		}
	}
	if v.scroll != 0 {
		t.Errorf("scroll moved with an empty diff: %d", v.scroll)
	}
}

// j, k, g and G are ordinary typing. Binding them made those letters
// impossible to type in the prompt after the agent's first edit.
func TestLiveDiffLeavesTypingKeysAlone(t *testing.T) {
	v := newLiveDiffView()
	v.maxScroll = 100
	for _, k := range []string{"j", "k", "g", "G", "a", "esc", "enter", "backspace"} {
		if v.handleKey(k) {
			t.Errorf("live diff consumed the typing key %q", k)
		}
	}
}

// TestLiveDiffRowFormat verifies row formatting.
func TestLiveDiffRowFormat(t *testing.T) {
	addLine := editdiff.Line{Op: editdiff.OpAdd, Text: "new line"}
	row := formatLiveDiffRow(addLine, 80)
	if row == "" {
		t.Fatal("add line row should not be empty")
	}

	delLine := editdiff.Line{Op: editdiff.OpDel, Text: "old line"}
	row = formatLiveDiffRow(delLine, 80)
	if row == "" {
		t.Fatal("delete line row should not be empty")
	}

	ctxLine := editdiff.Line{Op: editdiff.OpEqual, Text: "same line"}
	row = formatLiveDiffRow(ctxLine, 80)
	if row == "" {
		t.Fatal("context line row should not be empty")
	}
}

// TestLiveDiffWithEditLog verifies the live diff panel works with the edit log.
func TestLiveDiffWithEditLog(t *testing.T) {
	log := newEditLog()
	files := []editdiff.FileDiff{
		{
			Path:   "main.go",
			Before: "package main\n",
			After:  "package main\n\nfunc main() {}\n",
		},
	}
	log.record("write", "1s", "", false, files)
	entries := log.list()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	v := newLiveDiffView()
	v.lastSeq = entries[0].Seq
	if v.lastSeq != 1 {
		t.Fatalf("expected lastSeq 1, got %d", v.lastSeq)
	}
}

// The panel renders into the live region, which the draw loop is careful never
// to let grow past the last terminal row: when it does, the write scrolls the
// screen and the panel is committed to scrollback on every frame instead of
// being repainted in place. So the row count has to hold for every budget,
// including the ones that leave no room for a body.
func TestLiveDiffRespectsRowBudget(t *testing.T) {
	var before, after strings.Builder
	for range 60 {
		before.WriteString("keep\n")
		after.WriteString("add\n")
	}
	e := EditEntry{Tool: "edit", Path: "big.go", Before: before.String(), After: after.String()}

	tui := &TUI{liveDiff: newLiveDiffView()}
	pal := theme.Get("")

	for budget := 1; budget <= maxLiveDiffRows+4; budget++ {
		tui.liveDiff.scroll = 0
		// Render twice: the first pass marks the edit fresh and the second
		// runs with the marker expired, so both shapes are checked.
		for range 2 {
			rows := tui.buildLiveDiffRows(pal, 80, budget, e)
			if len(rows) > budget {
				t.Fatalf("budget %d: rendered %d rows: %q", budget, len(rows), rows)
			}
		}
	}
}

// The marker is a notification, so it has to go away. Left in, it sat under
// every diff for the rest of the session.
func TestLiveDiffNewEditMarkerExpires(t *testing.T) {
	e := EditEntry{Tool: "edit", Path: "a.go",
		Before: "one\ntwo\n", After: "one\ntwo\nthree\n"}

	tui := &TUI{liveDiff: newLiveDiffView()}
	pal := theme.Get("")
	v := tui.liveDiff
	v.freshUntil = time.Now().Add(liveDiffFreshFor)
	v.maxScroll = 10

	if rows := tui.buildLiveDiffRows(pal, 80, 8, e); !hasRow(rows, "new edit") {
		t.Error("a fresh edit should show the marker")
	}
	v.freshUntil = time.Now().Add(-time.Second)
	if rows := tui.buildLiveDiffRows(pal, 80, 8, e); hasRow(rows, "new edit") {
		t.Error("the marker outlived its deadline")
	}
}

func hasRow(rows []string, sub string) bool {
	for _, r := range rows {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
