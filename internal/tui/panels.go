package tui

import (
	"github.com/nova-ai/nova/internal/theme"
)

// panelRows renders whichever panel is open, in one place so the draw loop
// does not have to know the list of them.
//
// Exactly one panel shows at a time. The subagent view and the edit viewer are
// full panels, and while one is up the model picker and the command palette
// stand down: two stacked lists on one screen is a screen nobody can read.
// The ordering is the precedence, most specific first.
func (t *TUI) panelRows(p theme.Palette, w, budget int) []string {
	switch {
	case t.subViewOpen():
		return t.subViewRows(p, w, budget)
	case t.editViewerOpen():
		return t.editViewerRows(p, w, budget)
	case t.liveDiffOpen():
		return t.liveDiffRows(p, w, budget)
	case t.modelPickerOpen():
		// The picker takes the space it needs; the command palette is not
		// useful at the same time.
		return t.modelPickerRows(p, w, budget)
	default:
		return t.paletteRows(p, w, budget)
	}
}
