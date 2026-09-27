package tui

import (
	"fmt"
	"strings"

	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// box drawing glyphs
const (
	tl = "╭"
	tr = "╮"
	bl = "╰"
	br = "╯"
	h  = "─"
	v  = "│"
)

// renderPromptBox draws Nova's signature prompt: a rounded rectangle whose top
// and bottom borders carry status information, with the editable area inside.
//
// Layout (width W):
//
//	╭─ working… ─────────────────────────────╮
//	│ what I want to build                    │
//	╰─ nova · kiosai/grok · ~/src ────────────╯
//	  ⏎ send · ⇧⏎ newline · /help · ^C quit
//
// The rails sit *inside* the border. Hanging them off the right edge, as an
// earlier revision did, made every frame one line wider than the terminal, so
// the row wrapped and dragged the layout apart.
// It returns the rendered rows plus the position of the caret within them:
// caretRow indexes rows, caretCol is a 1-based terminal column. maxRows caps
// the total height so a long composer cannot push the live region past the
// bottom of the screen; the view scrolls to keep the caret in sight.
func renderPromptBox(p theme.Palette, width int, content []string, topRail, bottomRail, hint string, cursorLine, cursorCol, maxRows int) (rows []string, caretRow, caretCol int) {
	if width < 20 {
		width = 20
	}
	inner := width - 4 // 2 border + 2 padding
	if inner < 4 {
		inner = 4
	}

	// Wrap the composer into display rows, remembering where each source line
	// starts so the caret can be located after wrapping.
	type segment struct {
		text  string
		start int // rune offset of this row within its source line
	}
	var segs []segment
	// lineStart[i] is the index of the first display row for source line i.
	lineStart := make([]int, len(content)+1)

	for i, ln := range content {
		lineStart[i] = len(segs)
		if strings.TrimSpace(ln) == "" {
			segs = append(segs, segment{text: "", start: 0})
			continue
		}
		n := 0
		for _, w := range util.Wrap(ln, inner) {
			segs = append(segs, segment{text: w, start: n})
			n += countRunes(w)
		}
		// A caret past the end of a short line still needs a row to sit on.
		for n < countRunes(ln) {
			segs = append(segs, segment{text: "", start: n})
			n++
		}
	}
	lineStart[len(content)] = len(segs)
	if len(segs) == 0 {
		segs = []segment{{}}
		lineStart = []int{0, 1}
	}

	// Locate the caret.
	if cursorLine < 0 {
		cursorLine = 0
	}
	if cursorLine >= len(content) {
		cursorLine = len(content) - 1
	}
	seg := lineStart[cursorLine]
	if seg >= len(segs) {
		seg = len(segs) - 1
	}
	off := cursorCol - segs[seg].start
	for seg+1 < lineStart[cursorLine+1] && off >= countRunes(segs[seg].text) {
		off -= countRunes(segs[seg].text)
		seg++
	}
	if off < 0 {
		off = 0
	}

	// The chrome is three rows: top border, bottom border, hint.
	chrome := 3
	if hint == "" {
		chrome = 2
	}
	view := segs
	if maxRows > chrome && len(view) > maxRows-chrome {
		// Scroll the composer so the caret stays visible, leaving a little
		// context above it.
		room := maxRows - chrome
		start := seg - room + 1
		if start < 0 {
			start = 0
		}
		view = segs[start : seg+1]
		seg -= start
	}

	rows = make([]string, 0, len(view)+chrome)
	rows = append(rows, borderRow(p, tl, tr, topRail, width))
	for i, sg := range view {
		cell := sg.text
		if i == seg {
			cell = insertRune(cell, off, "█")
		}
		rows = append(rows, p.Style("border", v)+" "+util.PadRight(cell, inner)+" "+p.Style("border", v))
	}
	rows = append(rows, borderRow(p, bl, br, bottomRail, width))
	if hint != "" {
		rows = append(rows, p.Style("dim", util.Truncate(hint, width)))
	}

	// rows[0] is the top border, so display row `seg` sits at seg+1.
	caretRow = 1 + seg
	caretCol = 2 + off + 1 // "│ " occupies two columns before the text
	if caretRow >= len(rows) {
		caretRow = len(rows) - 1
	}
	return rows, caretRow, caretCol
}

// borderRow builds a horizontal border with text sitting inside it, flush to
// the closing corner.
func borderRow(p theme.Palette, left, right, text string, width int) string {
	span := width - 2
	if span < 2 {
		span = 2
	}
	label := util.Truncate(text, span-2)
	gap := span - util.VisibleWidth(label) - 2
	if gap < 0 {
		gap = 0
	}
	return p.Style("border", left) +
		p.Style("border", strings.Repeat(h, gap)) +
		" " + p.Style("dim", label) + " " +
		p.Style("border", right)
}

// insertRune places r at rune index at, clamped to the end of s.
func insertRune(s string, at int, r string) string {
	runes := []rune(s)
	if at > len(runes) {
		at = len(runes)
	}
	return string(runes[:at]) + r + string(runes[at:])
}

// fmtUsageTokens formats token usage for display.
func fmtUsageTokens(u *llm.Usage) string {
	if u == nil {
		return ""
	}
	return fmt.Sprintf("in %s · out %s", humanTokens(u.PromptTokens), humanTokens(u.CompletionTokens))
}
