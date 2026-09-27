package tui

// Esc is the one "get me out of here" key, so it resolves in priority order:
//
//  1. a turn in flight is stopped
//  2. an open overlay — model picker, then command palette — closes
//  3. the composer is cleared
//
// While the agent is blocked on a question or an approval, Esc is ignored:
// answering "no" is the way out of that, and silently cancelling it would let
// a pending tool call look abandoned.
func (t *TUI) handleEsc() {
	switch {
	case t.streaming:
		// Stop the turn. The agent reports it as stopped and clears the
		// streaming state; the composer is left alone so the prompt can be
		// edited and sent again.
		if t.cancelTurn != nil {
			t.cancelTurn()
		}
	case t.asking:
		return
	case t.subViewOpen():
		t.closeSubView()
	case t.editViewerOpen():
		// Esc backs out of the expanded diff before closing the list, because
		// "close what I am looking at" and "close everything" are different
		// intents and the first is the common one.
		if t.editView.expanded {
			t.editView.expanded = false
			t.editView.bodyOffset = 0
		} else {
			t.closeEditViewer()
		}
	case t.modelPickerOpen():
		t.closeModelPicker()
	case t.paletteLen() > 0:
		t.dismissPalette()
	case !t.composerEmpty():
		t.inputLines = []string{""}
		t.cursorLine, t.cursorCol = 0, 0
	}
}

// composerEmpty reports whether there is anything typed to clear.
func (t *TUI) composerEmpty() bool {
	return len(t.inputLines) == 0 || (len(t.inputLines) == 1 && t.inputLines[0] == "")
}
