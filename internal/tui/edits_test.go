package tui

import (
	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
	"strings"
	"testing"
)

// editTUI builds a TUI over a real App, with an edit log holding one entry
// per (path, before, after) triple.
func editTUI(entries ...EditEntry) *TUI {
	app := &App{
		history: NewBuffer(100),
		edits:   newEditLog(),
		subs:    newSubLog(),
		Theme:   theme.Get("nova"),
		Width:   100,
	}
	for _, e := range entries {
		app.edits.record(e.Tool, e.Dur, e.Output, e.IsErr, []editdiff.FileDiff{
			{Path: e.Path, Before: e.Before, After: e.After},
		})
	}
	return &TUI{app: app, inputLines: []string{""}, messageBus: make(chan agent.Event, 1)}
}

func sampleEdit() EditEntry {
	return EditEntry{
		Tool:   "edit",
		Path:   "internal/tui/keys.go",
		Before: "package tui\n\nfunc a() {}\n",
		After:  "package tui\n\nfunc b() {}\nfunc c() {}\n",
	}
}

// An edit that changed nothing must not appear in the viewer: listing it would
// show a diff for work that did not happen.
func TestEditLogIgnoresUnchangedFiles(t *testing.T) {
	l := newEditLog()
	l.record("write", "1ms", "", false, []editdiff.FileDiff{{Path: "a.go", Before: "same\n", After: "same\n"}})
	if l.count() != 0 {
		t.Fatalf("an unchanged file must not be recorded, got %d", l.count())
	}
}

func TestEditLogRecordsBothCounts(t *testing.T) {
	l := newEditLog()
	l.record("edit", "1ms", "", false, []editdiff.FileDiff{
		{Path: "a.go", Before: "one\ntwo\n", After: "one\ntwo\nthree\n"},
	})
	entries := l.list()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Added != 1 || entries[0].Removed != 0 {
		t.Errorf("counts = +%d -%d, want +1 -0", entries[0].Added, entries[0].Removed)
	}
}

func TestEditLogMarksCreatedFiles(t *testing.T) {
	l := newEditLog()
	l.record("write", "1ms", "", false, []editdiff.FileDiff{{Path: "new.go", Before: "", After: "package x\n"}})
	entries := l.list()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if !entries[0].Created() {
		t.Error("a file with no before-text must be reported as created")
	}
}

func TestEditLogDropsUndoneEntries(t *testing.T) {
	l := newEditLog()
	l.record("edit", "1ms", "", false, []editdiff.FileDiff{{Path: "a.go", Before: "x\n", After: "y\n"}})
	l.record("edit", "1ms", "", false, []editdiff.FileDiff{{Path: "b.go", Before: "x\n", After: "y\n"}})
	l.undo("b.go")
	entries := l.list()
	if len(entries) != 1 || entries[0].Path != "a.go" {
		t.Fatalf("undo must drop only the reverted file, got %+v", entries)
	}
}

func TestEditLogIsCapped(t *testing.T) {
	l := newEditLog()
	for i := 0; i < maxEditRecords+25; i++ {
		l.record("edit", "1ms", "", false, []editdiff.FileDiff{
			{Path: "a.go", Before: "x\n", After: "y\n"},
		})
	}
	if l.count() > maxEditRecords {
		t.Fatalf("log grew to %d, cap is %d", l.count(), maxEditRecords)
	}
}

func TestEditViewerShowsRecordedEdits(t *testing.T) {
	tui := editTUI(sampleEdit())
	tui.editView = newEditViewer()
	rows := tui.editViewerRows(tui.app.Theme, 100, 20)
	joined := util.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "internal/tui/keys.go") {
		t.Errorf("the viewer must name the file, got:\n%s", joined)
	}
	if !strings.Contains(joined, "+func c") {
		t.Errorf("the preview must show the added line, got:\n%s", joined)
	}
}

func TestEditViewerShowsNothingForAnEmptyLog(t *testing.T) {
	tui := editTUI()
	tui.editView = newEditViewer()
	rows := tui.editViewerRows(tui.app.Theme, 100, 20)
	joined := util.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "no edits recorded") {
		t.Errorf("an empty log must say so, got:\n%s", joined)
	}
}

func TestEditViewerExpandsToTheFullDiff(t *testing.T) {
	tui := editTUI(sampleEdit())
	tui.editView = newEditViewer()
	tui.editView.expanded = true
	rows := tui.editViewerRows(tui.app.Theme, 100, 20)
	joined := util.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "-func a") || !strings.Contains(joined, "+func b") {
		t.Errorf("the expanded diff must show both sides, got:\n%s", joined)
	}
}

func TestEditViewerNavigationStaysInRange(t *testing.T) {
	tui := editTUI(sampleEdit(), sampleEdit())
	tui.editView = newEditViewer()
	// Walking past either end must clamp rather than index out of bounds or
	// wrap, both of which would panic in a key handler.
	tui.editViewerKey("up")
	tui.editViewerKey("up")
	if tui.editView.sel != 0 {
		t.Errorf("sel = %d, want 0", tui.editView.sel)
	}
	tui.editViewerKey("down")
	tui.editViewerKey("down")
	tui.editViewerKey("down")
	if tui.editView.sel != 1 {
		t.Errorf("sel = %d, want 1", tui.editView.sel)
	}
	tui.editViewerKey("home")
	if tui.editView.sel != 0 {
		t.Errorf("home did not reset sel: %d", tui.editView.sel)
	}
	tui.editViewerKey("end")
	if tui.editView.sel != 1 {
		t.Errorf("end did not reach the last entry: %d", tui.editView.sel)
	}
}

func TestEditViewerClosesWhenTheLogEmpties(t *testing.T) {
	tui := editTUI(sampleEdit())
	tui.editView = newEditViewer()
	// /undo can empty the log out from under an open viewer; the overlay must
	// close rather than render against a slice that no longer exists.
	tui.app.edits.clear()
	tui.editViewerKey("down")
	if tui.editViewerOpen() {
		t.Error("the viewer must close when there is nothing left to show")
	}
}

func TestEditViewerIgnoresTyping(t *testing.T) {
	tui := editTUI(sampleEdit())
	tui.editView = newEditViewer()
	// An unhandled key has to fall through to the composer, otherwise the
	// viewer would swallow the prompt while it is open.
	if tui.editViewerKey("a") {
		t.Error("a plain character must not be consumed by the viewer")
	}
}

func TestEscStepsBackOutOfTheExpandedDiff(t *testing.T) {
	tui := editTUI(sampleEdit())
	tui.editView = newEditViewer()
	tui.editView.expanded = true
	tui.handleEsc()
	if !tui.editViewerOpen() {
		t.Error("the first Esc must close the diff, not the viewer")
	}
	if tui.editView.expanded {
		t.Error("the expanded diff should be closed after the first Esc")
	}
	tui.handleEsc()
	if tui.editViewerOpen() {
		t.Error("the second Esc must close the viewer")
	}
}

func TestEditRowsRespectTheWidth(t *testing.T) {
	tui := editTUI(EditEntry{
		Tool:   "edit",
		Path:   "a.go",
		Before: strings.Repeat("x", 400) + "\n",
		After:  strings.Repeat("y", 400) + "\n",
	})
	tui.editView = newEditViewer()
	// A row wider than the terminal wraps, and the wrapped half lands under
	// the next row and drags the whole live region out of alignment.
	for _, row := range tui.editViewerRows(tui.app.Theme, 80, 20) {
		if w := util.VisibleWidth(row); w > 80 {
			t.Fatalf("row is %d columns wide, limit is 80", w)
		}
	}
}

func TestSubLogTracksARunToCompletion(t *testing.T) {
	l := newSubLog()
	l.start("explore", "explore", "")
	l.step("explore", "grep  internal/")
	l.step("explore", "read  internal/agent/agent.go")
	l.done("explore", "explore · 2 tool calls · 3s", 900, "3s")
	recs := l.list()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.State != "done" {
		t.Errorf("state = %q, want done", r.State)
	}
	if r.Steps != 2 {
		t.Errorf("steps = %d, want 2", r.Steps)
	}
}

func TestSubLogRecordsFailures(t *testing.T) {
	l := newSubLog()
	l.start("plan", "plan", "")
	l.fail("plan", "model unavailable")
	recs := l.list()
	if len(recs) != 1 || recs[0].State != "failed" {
		t.Fatalf("want a failed record, got %+v", recs)
	}
	if !strings.Contains(recs[0].Err, "model unavailable") {
		t.Errorf("the failure reason must be kept: %q", recs[0].Err)
	}
}

// Two runs of the same role share a name, and the events after a restart refer
// to the newer one.
func TestSubLogAttributesEventsToTheNewestRun(t *testing.T) {
	l := newSubLog()
	l.start("explore", "explore", "")
	l.done("explore", "first", 0, "")
	l.start("explore", "explore", "")
	l.step("explore", "grep x")
	recs := l.list()
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].State != "done" {
		t.Errorf("the finished run must stay done, got %q", recs[0].State)
	}
	if recs[1].Steps != 1 {
		t.Errorf("the step must land on the newest run, got %d", recs[1].Steps)
	}
}

func TestSubViewShowsRuns(t *testing.T) {
	tui := editTUI()
	tui.app.subs.start("explore", "explore", "")
	tui.agents = &subView{}
	rows := tui.subViewRows(tui.app.Theme, 100, 20)
	joined := util.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "explore") {
		t.Errorf("the view must name the subagent, got:\n%s", joined)
	}
}

func TestSubViewClosesWhenEmpty(t *testing.T) {
	tui := editTUI()
	tui.agents = &subView{}
	tui.subViewKey("down")
	if tui.subViewOpen() {
		t.Error("the view must close when nothing has run")
	}
}

func TestPanelPrecedencePutsFullPanelsFirst(t *testing.T) {
	// A full panel and the composer palette open at once would be unreadable,
	// so the panel wins and the palette is not drawn.
	tui := editTUI(sampleEdit())
	tui.editView = newEditViewer()
	tui.inputLines = []string{"/"}
	tui.app.Width = 100
	tui.app.Theme = theme.Get("nova")
	rows := tui.panelRows(tui.app.Theme, 100, 20)
	joined := util.Strip(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "edits —") {
		t.Errorf("the edit viewer must win over the palette, got:\n%s", joined)
	}
}

func TestNewCommandsAreRegistered(t *testing.T) {
	// The registry is the only place a command becomes reachable, so a command
	// that is implemented but not listed would be silently dead.
	want := []string{"/edits", "/edits-show", "/edits-clear", "/agents", "/task", "/roles"}
	for _, name := range want {
		found := false
		for _, c := range commands {
			if c.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is implemented but not registered", name)
		}
	}
}

func TestNewCommandsHaveDescriptions(t *testing.T) {
	// The palette is a list of names and one-line descriptions; a blank one
	// makes the command undiscoverable.
	for _, c := range commands {
		if strings.TrimSpace(c.Desc) == "" {
			t.Errorf("%s has no description", c.Name)
		}
	}
}

func TestEditViewerCommandsAreDistinct(t *testing.T) {
	// A duplicate name in the registry makes the second unreachable, and the
	// first matching entry wins silently.
	seen := map[string]bool{}
	for _, c := range commands {
		if seen[c.Name] {
			t.Errorf("%s is registered twice", c.Name)
		}
		seen[c.Name] = true
	}
}

func TestEditEntryDiffRoundTrips(t *testing.T) {
	e := sampleEdit()
	body := e.Diff()
	if body == "" {
		t.Fatal("an edit with real changes must render a diff")
	}
	if !strings.Contains(body, "--- a/"+e.Path) {
		t.Errorf("the diff must be labelled with its path, got:\n%s", body)
	}
}

var _ = config.Default
