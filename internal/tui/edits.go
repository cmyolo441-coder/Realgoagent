package tui

import (
	"fmt"
	"strings"
	"sync"

	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// The edit viewer: every file the agent touches is recorded with the text on
// both sides of the change, and `/edits` opens a list of them. The point is
// review, not decoration — a coding agent that edits a dozen files should be
// checkable without leaving the terminal or reading a git diff by hand.
//
// Edits are kept in the App rather than the TUI, because they outlive any one
// overlay and belong to the session, not to the current view.

const (
	// maxEditRecords caps the history. A long session can touch hundreds of
	// files, and holding every before/after pair in memory forever would grow
	// without bound for a list the user scrolls through a handful of entries
	// at a time.
	maxEditRecords = 200
	// editPreviewRows is how many diff lines an entry shows in the list before
	// it has to be opened.
	editPreviewRows = 3
)

// EditEntry is one recorded file change.
type EditEntry struct {
	// Tool is the tool that made the change.
	Tool string
	// Path is the file, relative to the workspace where possible.
	Path string
	// Before and After are the file's text either side of the change.
	Before string
	After  string
	// Dur is how long the tool call took.
	Dur string
	// IsErr is set when the call that made the change failed.
	IsErr bool
	// Output is the tool's own result text.
	Output string
	// Seq numbers entries so the newest is known without comparing times.
	Seq int
	// Added and Removed are the line counts, and Lines is the
	// context-trimmed change script, all computed once at record time.
	//
	// They are cached because computing them is an LCS over the lines that
	// differ, and both of their consumers are hot: the live diff panel
	// re-renders on every repaint — twenty times a second, for as long as the
	// panel is up — and the edit list re-renders on every keystroke. Diffing
	// the same pair of texts that often is the difference between a panel
	// that keeps up and one that pins a core.
	Added   int
	Removed int
	// Lines is the context-trimmed diff, shared read-only with the renderer.
	Lines []editdiff.Line
	// Rows is the coloured diff body, cached for the same reason.
	Rows []string
}

// Change returns the entry's context-trimmed diff, computing it if the entry
// was built without one. Entries normally arrive from record, which fills
// Lines in; the fallback keeps a hand-built entry renderable.
func (e EditEntry) Change() []editdiff.Line {
	if e.Lines != nil {
		return e.Lines
	}
	return editdiff.Context(e.Before, e.After, editdiff.DefaultContext)
}

// Stat returns the added/removed counts.
func (e EditEntry) Stat() editdiff.Stats {
	return editdiff.Stats{Added: e.Added, Removed: e.Removed}
}

// Created reports whether the file did not exist before the change.
func (e EditEntry) Created() bool { return e.Before == "" && e.After != "" }

// Diff renders the entry as a unified diff, which is what `/diff` shows and
// what a user can paste into a review.
func (e EditEntry) Diff() string {
	return editdiff.Unified(e.Path, e.Before, e.After, editdiff.DefaultContext)
}

// editLog is the session's edit history. It is guarded because edits are
// recorded on whichever goroutine ran the tool, while the overlay reads it
// from the event loop.
type editLog struct {
	mu      sync.Mutex
	entries []EditEntry
	seq     int
}

func newEditLog() *editLog { return &editLog{} }

// record files one tool call's changes, one entry per file, newest last.
//
// The change script is computed once, here, and every renderer reads it from
// the entry. It used to be recomputed per render: the live diff panel renders
// on every repaint and the edit list on every keystroke, and each of those
// paid for a full LCS over the two texts. Nothing about the file changes
// between two renders, so that work was identical every time.
//
// diffRows takes the script rather than the texts so the entry is diffed
// once instead of twice.
func (l *editLog) record(tool, dur, output string, isErr bool, files []editdiff.FileDiff) {
	if l == nil || len(files) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range files {
		lines := editdiff.Context(f.Before, f.After, editdiff.DefaultContext)
		added, removed := 0, 0
		for _, ln := range lines {
			switch ln.Op {
			case editdiff.OpAdd:
				added++
			case editdiff.OpDel:
				removed++
			}
		}
		if added == 0 && removed == 0 {
			// The text either side of the change is identical, so there is no
			// change to list. The tool layer filters this too, but a record is
			// the only place that can be sure, and a viewer entry for an
			// unchanged file would claim work that did not happen.
			continue
		}
		l.seq++
		rows := diffRows(f.Path, lines)
		l.entries = append(l.entries, EditEntry{
			Tool:    tool,
			Path:    f.Path,
			Before:  f.Before,
			After:   f.After,
			Dur:     dur,
			IsErr:   isErr,
			Output:  output,
			Seq:     l.seq,
			Added:   added,
			Removed: removed,
			Lines:   lines,
			Rows:    rows,
		})
	}
	if n := len(l.entries); n > maxEditRecords {
		l.entries = append([]EditEntry{}, l.entries[n-maxEditRecords:]...)
	}
}

// list returns a copy of the history, newest last.
func (l *editLog) list() []EditEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]EditEntry{}, l.entries...)
}

// count reports how many edits have been recorded.
func (l *editLog) count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// undo removes the newest entry, because /undo reverted it and the viewer
// must not keep showing a change that is no longer on disk.
func (l *editLog) undo(path string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.entries) - 1; i >= 0; i-- {
		if l.entries[i].Path == path {
			l.entries = append(l.entries[:i], l.entries[i+1:]...)
			return
		}
	}
}

// clear drops the history.
func (l *editLog) clear() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.entries = nil
	l.mu.Unlock()
}

// diffRows renders an already-computed change script as coloured rows with a
// line-number gutter. It takes the script rather than the two texts so a
// caller that already has it does not pay for a second diff.
func diffRows(path string, lines []editdiff.Line) []string {
	out := make([]string, 0, len(lines)+2)
	out = append(out, theme.Get("").Style("tool", "--- a/"+path))
	out = append(out, theme.Get("").Style("tool", "+++ b/"+path))
	for _, l := range lines {
		out = append(out, rowFor(l))
	}
	return out
}

// rowFor renders one change line with its gutter. A deleted line is numbered
// from the old file and an added one from the new, which is how a reader
// matches a line to a position in their editor.
func rowFor(l editdiff.Line) string {
	p := theme.Get("")
	// A leading space keeps the gutter aligned with the +/- marker.
	switch l.Op {
	case editdiff.OpAdd:
		return p.Style("dim", fmt.Sprintf("%5d ", l.New)) + p.Style("success", "+"+l.Text)
	case editdiff.OpDel:
		return p.Style("dim", fmt.Sprintf("%5d ", l.Old)) + p.Style("error", "-"+l.Text)
	default:
		return p.Style("dim", fmt.Sprintf("%5d ", l.New)) + p.Style("dim", " "+l.Text)
	}
}

// editViewer is the open overlay: a scrollable list of recorded edits with a
// preview of each, and a full diff for the highlighted one.
type editViewer struct {
	sel    int
	offset int
	// expanded is set when the user asked to see the whole diff rather than
	// the list, which is what `Enter` toggles.
	expanded bool
	// bodyOffset scrolls inside the expanded diff.
	bodyOffset int
}

func newEditViewer() *editViewer { return &editViewer{} }

// open reports whether the viewer is showing.
func (t *TUI) editViewerOpen() bool { return t.editView != nil }

// editLog is the session's edit history. It hangs off the App because the
// history belongs to the session, not to whichever overlay happens to be open.
func (t *TUI) editLog() *editLog { return t.app.edits }
func (t *TUI) closeEditViewer()  { t.editView = nil }

// editViewerKey handles a key while the viewer is open, reporting whether it
// consumed it. Called with t.mu held.
func (t *TUI) editViewerKey(k string) bool {
	entries := t.editLog().list()
	if len(entries) == 0 {
		// The history can empty out under the overlay — /undo, or /clear —
		// and a viewer with nothing to show is just a frame in the way.
		t.closeEditViewer()
		return true
	}
	v := t.editView
	switch k {
	case "up":
		v.move(-1, len(entries))
	case "down":
		v.move(1, len(entries))
	case "pageup":
		v.move(-palettePage, len(entries))
	case "pagedown":
		v.move(palettePage, len(entries))
	case "home":
		v.sel = 0
		v.bodyOffset = 0
	case "end":
		v.sel = len(entries) - 1
		v.bodyOffset = 0
	case "right", "enter", "tab":
		v.expanded = !v.expanded
		v.bodyOffset = 0
	case "left":
		if v.expanded {
			v.expanded = false
			v.bodyOffset = 0
			return true
		}
		return false
	case "esc":
		t.closeEditViewer()
	default:
		return false
	}
	return true
}

// move walks the highlight by delta, keeping it in range and pulling the
// scroll window with it.
func (v *editViewer) move(delta, total int) {
	if total == 0 {
		return
	}
	v.sel += delta
	if v.sel < 0 {
		v.sel = 0
	}
	if v.sel >= total {
		v.sel = total - 1
	}
	if v.sel < v.offset {
		v.offset = v.sel
	}
	// Returning to the list abandons any scroll position inside the expanded
	// diff, so the next Enter starts at the top of it.
	v.bodyOffset = 0
}

// selected returns the highlighted entry.
func (t *TUI) selectedEdit() (EditEntry, bool) {
	entries := t.editLog().list()
	if t.editView == nil || t.editView.sel < 0 || t.editView.sel >= len(entries) {
		return EditEntry{}, false
	}
	return entries[t.editView.sel], true
}

// editViewerRows renders the overlay into whatever vertical space is left
// above the prompt box.
func (t *TUI) editViewerRows(pal theme.Palette, w, budget int) []string {
	if !t.editViewerOpen() || budget < 3 {
		return nil
	}
	entries := t.editLog().list()
	if len(entries) == 0 {
		return []string{pal.Style("dim", " no edits recorded this session")}
	}
	if t.editView.expanded {
		return t.expandedEditRows(pal, w, budget)
	}
	return t.editListRows(pal, w, budget, entries)
}

// editListRows renders the scrollable list of edits with a short preview.
func (t *TUI) editListRows(pal theme.Palette, w, budget int, entries []EditEntry) []string {
	v := t.editView
	// One row for the header, one for the preview block under the highlight.
	visible := budget - 1 - (editPreviewRows + 1)
	if visible < 1 {
		visible = 1
	}
	if visible > len(entries) {
		visible = len(entries)
	}
	if v.sel < v.offset {
		v.offset = v.sel
	}
	if v.sel >= v.offset+visible {
		v.offset = v.sel - visible + 1
	}
	if v.offset+visible > len(entries) {
		v.offset = len(entries) - visible
	}
	if v.offset < 0 {
		v.offset = 0
	}

	more := ""
	if v.offset > 0 {
		more = " ↑" + itoa(v.offset)
	}
	if v.offset+visible < len(entries) {
		more = " ↓" + itoa(len(entries)-v.offset-visible) + more
	}
	head := util.Truncate(" edits — ↑↓ move · ⏎ expand · esc close", w-util.VisibleWidth(more))
	out := []string{pal.Style("dim", head) + pal.Style("dim", more)}

	for i := v.offset; i < v.offset+visible; i++ {
		out = append(out, pal.Style("dim", editRow(pal, entries[i], i == v.sel, w)))
	}
	// The highlighted entry shows its first few changed lines, so the list
	// answers "what did it change" without a second keystroke.
	if e, ok := t.selectedEdit(); ok {
		out = append(out, pal.Style("dim", ""))
		out = append(out, editPreviewRowsFor(pal, e, w)...)
	}
	return out
}

// editRow renders one list line: the tool, the path, and its line counts.
func editRow(pal theme.Palette, e EditEntry, sel bool, w int) string {
	marker := "  "
	style := pal.Style("text", e.Path)
	if sel {
		marker = pal.Style("accent", "❯ ")
		style = pal.Style("accent_bold", e.Path)
	}
	line := marker + style
	tool := e.Tool
	if tool == "" {
		tool = "edit"
	}
	line += pal.Style("dim", "  "+tool)
	if e.IsErr {
		line += pal.Style("error", "  failed")
	} else if e.Dur != "" {
		line += pal.Style("dim", "  "+e.Dur)
	}
	if e.Created() {
		line += pal.Style("success", "  new")
	}
	line += statSuffix(pal, e.Stat())
	return util.Truncate(line, w)
}

// statSuffix renders the +n/-m counts. They are colour-coded because they are
// the one number in the row that tells the user whether the change was small.
func statSuffix(pal theme.Palette, s editdiff.Stats) string {
	var b strings.Builder
	if s.Added > 0 {
		fmt.Fprintf(&b, "  %s", pal.Style("success", "+"+itoa(s.Added)))
	}
	if s.Removed > 0 {
		fmt.Fprintf(&b, "  %s", pal.Style("error", "-"+itoa(s.Removed)))
	}
	return b.String()
}

// editPreviewRowsFor renders the first few changed lines of an entry.
//
// Context lines are skipped: the preview answers "what changed", and a
// three-line window that happens to be all context tells the user nothing.
func editPreviewRowsFor(pal theme.Palette, e EditEntry, w int) []string {
	rows := make([]string, 0, editPreviewRows)
	for _, l := range e.Change() {
		if l.Op == editdiff.OpEqual {
			continue
		}
		if len(rows) >= editPreviewRows {
			break
		}
		rows = append(rows, "  "+util.Truncate(rowFor(l), w-2))
	}
	if len(rows) == 0 {
		rows = append(rows, pal.Style("dim", "  (no line-level change)"))
	}
	return rows
}

// expandedEditRows renders the whole diff of the highlighted entry.
func (t *TUI) expandedEditRows(pal theme.Palette, w, budget int) []string {
	e, ok := t.selectedEdit()
	if !ok {
		return nil
	}
	// The tool is named here as well as in the list row: this view is often
	// reached from Esc, and a diff on its own does not say what made it.
	tool := e.Tool
	if tool == "" {
		tool = "edit"
	}
	head := util.Truncate(" "+truncMiddle(e.Path, w-40)+"  "+pal.Style("dim", tool)+statSuffix(pal, e.Stat())+" — ↑↓ scroll · ⏎ back · esc close", w)
	visible := budget - 1
	if visible < 1 {
		visible = 1
	}
	rows := e.Rows
	v := t.editView
	if v.bodyOffset > len(rows)-1 {
		v.bodyOffset = len(rows) - 1
	}
	if v.bodyOffset < 0 {
		v.bodyOffset = 0
	}
	end := v.bodyOffset + visible
	if end > len(rows) {
		end = len(rows)
	}
	out := []string{pal.Style("dim", head)}
	for _, r := range rows[v.bodyOffset:end] {
		out = append(out, util.Truncate(r, w))
	}
	if end < len(rows) {
		out = append(out, pal.Style("dim", "    … "+itoa(len(rows)-end)+" more lines (↓ to scroll)"))
	}
	return out
}
