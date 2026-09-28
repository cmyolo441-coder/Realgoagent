package tui

import (
	"strings"

	"github.com/nova-ai/nova/internal/md"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// maxStreamRows caps how much of the in-flight reply is previewed above the
// prompt box. The preview is re-rendered on every token, so it has to stay
// small; the full text is committed to scrollback when the turn ends.
const maxStreamRows = 12

// draw commits any finished output to scrollback and then repaints the live
// region — the streaming preview plus the prompt box — in place.
//
// It is safe to call as often as you like: the frame is compared against the
// previous one and an unchanged frame writes nothing at all.
func (t *TUI) draw() {
	if t.app == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.drawLocked()
}

// drawLocked is draw with t.mu held.
func (t *TUI) drawLocked() {
	p := t.app.Theme
	w := t.app.Width
	if w < 24 {
		w = 24
	}
	h := t.app.Height
	if h < 8 {
		h = 8
	}

	// Anything the agent or a command appended is finished output: print it
	// once, permanently, and the terminal owns it from then on.
	t.commitHistoryLocked(w)

	// The live region must never be taller than the screen, or writing its
	// last row scrolls the terminal and the offsets stop lining up. One row is
	// kept spare so the final write cannot trigger that scroll.
	avail := h - 1
	t.syncPalette()

	rows := t.streamRows(p, w, min(avail-6, maxStreamRows))
	boxRows, caretRow, caretCol := t.boxRows(p, w, avail-len(rows)-1)
	// Overlays render *above* the box. Putting them below would push the prompt
	// up the screen, which is the one place it must stay still.
	overhead := avail - len(rows) - len(boxRows)
	// Only one panel is drawn at a time. The subagent view and the edit viewer
	// are full panels, so while one is up the model picker and the command
	// palette stand down: two stacked lists on one screen is unreadable.
	if panel := t.panelRows(p, w, overhead); len(panel) > 0 {
		rows = append(rows, panel...)
		caretRow += len(panel)
	}
	rows = append(rows, boxRows...)

	frame := strings.Join(rows, "\r\n")
	if frame == t.lastFrame {
		return
	}
	t.lastFrame = frame

	var b strings.Builder
	// Wipe the previous frame. This has to account for where the cursor
	// actually is: after a repaint it sits on the caret, which is inside the
	// box, not on the last row of the region. Stepping up liveLines rows from
	// the caret overshoots, erases committed history above the region, and
	// drags the prompt to the top of the screen.
	t.moveToLiveTop(&b)
	b.WriteString("\x1b[J")

	// Pad so this frame finishes on the last row of the terminal. Done here,
	// after the wipe and before the write, because the caret sits in the
	// middle of the box: padding emitted from the caret would split it.
	if t.pinPending {
		t.pinPending = false
		if gap := h - 1 - t.rowsWritten - len(rows); gap > 0 {
			b.WriteString(strings.Repeat("\r\n", gap))
			t.rowsWritten += gap
		}
	}

	b.WriteString(frame)
	// The cursor lands at the end of the last row, so the caret is placed by
	// offset only. It is always inside the box, never in the preview above.
	up := len(rows) - 1 - caretRow
	if up < 0 {
		up = 0
	}
	if up > 0 {
		b.WriteString("\x1b[" + itoa(up) + "A")
	}
	b.WriteString("\x1b[" + itoa(caretCol) + "G")
	b.WriteString("\x1b[?25h")

	t.rowsWritten += len(rows)
	t.liveLines = len(rows)
	// Remember where the cursor was left, so the next wipe can find the top of
	// the region from wherever the caret happens to be.
	t.caretRowLast = caretRow
	t.out.WriteString(b.String())
	t.out.Flush()
}

// commitHistory writes every history line that has not reached the terminal
// yet. The live region is torn down first so the new output lands directly
// above it instead of interleaving with it.
func (t *TUI) commitHistory(w int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commitHistoryLocked(w)
}

// commitHistoryLocked is commitHistory with t.mu held.
func (t *TUI) commitHistoryLocked(w int) {
	all := t.app.history.Lines()
	if len(all) < t.flushed {
		// The buffer was cleared under us. Forget the old bookkeeping rather
		// than treating every surviving line as new output.
		t.flushed = len(all)
		t.liveLines = 0
		t.lastFrame = ""
		return
	}
	if len(all) == t.flushed {
		return
	}
	t.eraseLiveLocked()
	for _, ln := range all[t.flushed:] {
		line := util.Truncate(ln, w)
		t.out.WriteString(line)
		t.out.WriteString("\r\n")
		t.rowsWritten += util.CountLines(line, w)
	}
	t.flushed = len(all)
	t.lastFrame = ""
}

// moveToLiveTop appends the sequence that puts the cursor on the first row of
// the live region, and clears the region's bookkeeping.
//
// The cursor is not necessarily at the bottom of the region: a repaint leaves
// it on the caret, which sits inside the box. Stepping up from there would
// overshoot into committed history, so the region is left from the bottom
// row instead.
//
// Called with t.mu held.
func (t *TUI) moveToLiveTop(b *strings.Builder) {
	if t.liveLines <= 0 {
		return
	}
	// The cursor is left on the caret, caretRowLast rows into the region, so
	// stepping up exactly that many rows lands on the region's first row.
	//
	// Stepping up liveLines instead overshoots by one, and a repaint that
	// overshoots walks the prompt up the screen a row at a time until it
	// reaches the top. The same off-by-one in the other direction would wipe
	// the last committed line.
	up := t.caretRowLast
	if up > t.liveLines-1 {
		up = t.liveLines - 1
	}
	if up > 0 {
		b.WriteString("\x1b[" + itoa(up) + "A")
	}
	// Cursor-up preserves the column; new output starts at the left edge.
	b.WriteString("\r")
	t.rowsWritten -= t.liveLines
	t.liveLines = 0
	t.caretRowLast = 0
}

// eraseLiveLocked removes the repainted region so output can be written in its place.
// Called with t.mu held.
func (t *TUI) eraseLiveLocked() {
	var b strings.Builder
	t.moveToLiveTop(&b)
	b.WriteString("\x1b[J")
	t.out.WriteString(b.String())
}

// requestPin asks the next repaint to push the live region down to the last
// row of the terminal.
//
// Without it the prompt sits wherever the last committed line happened to
// end, which on a fresh terminal is a third of the way down and looks broken.
// Once the screen is full the box tracks the bottom on its own, because output
// is always written directly above it.
func (t *TUI) requestPin() {
	t.pinPending = true
}

// maxStreamSource caps how much of the in-flight reply is re-rendered per
// frame. The preview only ever shows the last dozen rows, so re-parsing a
// reply that has grown to tens of kilobytes is pure waste — and it grows
// quadratically, which is what made long answers crawl.
const maxStreamSource = 8 << 10

// streamRows renders the tail of the reply being streamed right now, capped
// at budget rows.
func (t *TUI) streamRows(p theme.Palette, w, budget int) []string {
	if !t.streaming || t.streamBuf.Len() == 0 || budget < 1 {
		return nil
	}
	rendered := md.New(p, w).Render(streamTail(t.streamBuf.String(), maxStreamSource))
	if len(rendered) > budget {
		rendered = rendered[len(rendered)-budget:]
		rendered[0] = p.Style("dim", "…")
	}
	return rendered
}

// streamTail returns at most max bytes of s, starting at a line boundary so
// the renderer never sees a half line. The first line is marked as elided
// because text before it is on screen already.
func streamTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	tail := s[len(s)-max:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return "…\n" + tail
}

// boxRows renders the prompt box and its hint line, capped at budget rows.
func (t *TUI) boxRows(p theme.Palette, w, budget int) ([]string, int, int) {
	return renderPromptBox(p, w, t.inputLines, t.topRail(p), t.bottomRail(p),
		p.Style("dim", t.hint(p)), t.cursorLine, t.cursorCol, budget)
}

// hint is the key legend under the box. It describes whatever is actually
// live: an open panel, a turn in flight, or the composer.
func (t *TUI) hint(p theme.Palette) string {
	switch {
	case t.subViewOpen():
		return "↑↓ move · PgUp/PgDn jump · esc close"
	case t.editViewerOpen():
		if t.editView.expanded {
			return "↑↓ scroll · PgUp/PgDn jump · ⏎ back · esc close"
		}
		return "↑↓ move · ⏎ expand diff · PgUp/PgDn jump · esc close"
	case t.liveDiffOpen():
		return "↑↓ scroll · esc dismiss"
	case t.modelPickerOpen():
		return "↑↓ move · PgUp/PgDn jump · ⏎ switch model · esc close"
	case t.paletteLen() > 0:
		return "↑↓ select · PgUp/PgDn jump · ⇥/⏎ complete · esc close"
	case t.streaming:
		return "esc stop · ^C quit"
	default:
		return "⏎ send · ⇧⏎ newline · / for commands · esc clear · ^C quit"
	}
}

// topRail is the status shown on the box's top border.
func (t *TUI) topRail(p theme.Palette) string {
	switch {
	case t.asking:
		return p.Style("warning", "? "+truncMiddle(t.question, 60))
	case t.streaming:
		rail := p.Style("accent", spinFrame()) + " working…"
		if t.toolActive != "" {
			rail += "  " + p.Style("tool", t.toolActive)
		}
		if t.liveDiff != nil && t.liveDiff.streaming && t.liveDiff.streamPath != "" {
			rail += "  " + p.Style("text", truncMiddle(t.liveDiff.streamPath, 40))
		}
		if t.activeTask != "" {
			rail += "  " + p.Style("accent2", truncMiddle(t.activeTask, 40))
		}
		return rail + p.Style("dim", "  (esc to stop)")
	case t.lastErr != nil:
		return p.Style("error", "error — see above")
	default:
		return p.Style("dim", "ready")
	}
}

// bottomRail carries the session identity on the box's bottom border.
func (t *TUI) bottomRail(p theme.Palette) string {
	rail := "nova · " + t.app.SelectedModel
	if cwd := truncMiddle(t.app.Cwd(), 28); cwd != "" {
		rail += " · " + cwd
	}
	rail += modeDot(t.app.Mode)
	if t.iter > 0 {
		rail += " · it " + itoa(t.iter)
	}
	if s := t.app.status; s != "" {
		rail += " · " + s
	}
	return p.Style("dim", rail)
}

func modeDot(m Mode) string {
	if m == ModePlan {
		return " · plan"
	}
	return ""
}

func spinFrame() string {
	frames := md.Frames(appSpinner)
	if frames == nil {
		return "*"
	}
	idx := int(nowMillis()/100) % len(frames)
	return frames[idx]
}

const appSpinner = "braille"
