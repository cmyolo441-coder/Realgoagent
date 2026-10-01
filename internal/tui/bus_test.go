package tui

import (
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/session"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/tools"
)

// busTUI builds a TUI whose event bus has room for exactly one event, so the
// second send finds it full.
func busTUI() *TUI {
	app := &App{
		history:  NewBuffer(100),
		edits:    newEditLog(),
		subs:     newSubLog(),
		Theme:    theme.Get("nova"),
		Width:    80,
		session:  session.New("/tmp", "stub/model"),
		Registry: tools.NewRegistry(),
	}
	return &TUI{app: app, done: make(chan struct{}), messageBus: make(chan agent.Event, 1)}
}

// The end of a turn is the event that clears the UI's working state, and it is
// sent once. A bus that was full when it was produced dropped it, and the
// spinner then ran for the rest of the session with no turn in flight to stop
// it — there was nothing to press Esc against.
func TestEndOfTurnIsNotDroppedByAFullBus(t *testing.T) {
	for _, kind := range []agent.EventKind{
		agent.EvDone, agent.EvError, agent.EvToolResult, agent.EvText, agent.EvAskUser,
	} {
		t.Run(string(kind), func(t *testing.T) {
			tui := busTUI()
			tui.messageBus <- agent.Event{Kind: agent.EvIteration} // fill it

			sent := make(chan struct{})
			go func() {
				defer close(sent)
				tui.send(agent.Event{Kind: kind, Text: "keep me"})
			}()

			select {
			case <-sent:
				t.Fatalf("%s was discarded by a full bus", kind)
			case <-time.After(50 * time.Millisecond):
				// Still held: the event is waiting for room rather than
				// thrown away.
			}

			<-tui.messageBus
			select {
			case <-sent:
			case <-time.After(time.Second):
				t.Fatalf("%s never reached the bus", kind)
			}
			if got := <-tui.messageBus; got.Kind != kind {
				t.Fatalf("bus carried %q, want %q", got.Kind, kind)
			}
		})
	}
}

// A partial tool call is superseded by the next one, so it is the one kind
// that may be dropped rather than queued: a model streaming a large write
// produces hundreds of these, and holding every frame behind a slow front-end
// would put the reply behind them.
func TestPartialToolCallIsDroppedRatherThanQueued(t *testing.T) {
	tui := busTUI()
	tui.messageBus <- agent.Event{Kind: agent.EvIteration} // full

	done := make(chan struct{})
	go func() {
		defer close(done)
		tui.send(agent.Event{Kind: agent.EvToolProgress, Tool: "write"})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a superseded frame blocked the agent goroutine")
	}
	if got := <-tui.messageBus; got.Kind != agent.EvIteration {
		t.Fatalf("bus carried %q, want the queued event untouched", got.Kind)
	}
}

// The panel re-renders on every repaint, so it has to read the diff it was
// given rather than recompute one. If it recomputes, it ignores the entry's
// cached script — and a panel showing something other than what was recorded
// is the observable consequence.
func TestLivePanelRendersTheRecordedDiff(t *testing.T) {
	entry := EditEntry{
		Tool: "edit", Path: "a.go",
		Before: "one\n", After: "two\n",
		// Deliberately not derivable from Before/After: a renderer that
		// re-diffs produces "real", not "recorded".
		Lines: []editdiff.Line{{Op: editdiff.OpAdd, Text: "recorded", New: 1}},
	}
	tui := &TUI{liveDiff: newLiveDiffView()}
	rows := tui.buildLiveDiffRows(theme.Get(""), 80, 8, entry)

	if !hasRow(rows, "recorded") {
		t.Fatalf("panel did not render the recorded diff: %q", rows)
	}
	if hasRow(rows, "real") {
		t.Fatalf("panel re-diffed the entry instead of using it: %q", rows)
	}
}

// An entry built by hand, with no cached script, still has to render.
func TestLivePanelRendersAnEntryWithoutACachedDiff(t *testing.T) {
	entry := EditEntry{
		Tool: "edit", Path: "a.go",
		Before: "one\n", After: "one\ntwo\n",
	}
	tui := &TUI{liveDiff: newLiveDiffView()}
	if rows := tui.buildLiveDiffRows(theme.Get(""), 80, 8, entry); !hasRow(rows, "two") {
		t.Fatalf("an uncached entry did not render: %q", rows)
	}
}

// The edit list re-renders on every keystroke, so its preview has to read the
// recorded script too.
func TestEditPreviewUsesTheRecordedDiff(t *testing.T) {
	e := EditEntry{
		Path:   "a.go",
		Before: "one\n",
		After:  "two\n",
		Lines:  []editdiff.Line{{Op: editdiff.OpAdd, Text: "recorded", New: 1}},
	}
	rows := editPreviewRowsFor(theme.Get(""), e, 80)
	if !hasRow(rows, "recorded") {
		t.Fatalf("preview did not use the recorded diff: %q", rows)
	}
}

// record must fill the cache, or the two tests above would pass while the real
// path — every render after a record — still re-diffs.
func TestRecordCachesTheChangeScript(t *testing.T) {
	l := newEditLog()
	l.record("edit", "1ms", "", false, []editdiff.FileDiff{
		{Path: "a.go", Before: "one\n", After: "one\ntwo\n"},
	})
	e := l.list()[0]
	if len(e.Lines) == 0 {
		t.Fatal("record did not cache the change script")
	}
	// The counts have to agree with the cached script, since they are now
	// derived from it rather than from a second pass over the same texts.
	if e.Added != 1 || e.Removed != 0 {
		t.Fatalf("counts = +%d -%d, want +1 -0", e.Added, e.Removed)
	}
	if len(e.Rows) == 0 {
		t.Fatal("record did not render the rows")
	}
}
