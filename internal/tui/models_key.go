package tui

import (
	"github.com/nova-ai/nova/internal/theme"
)

// openModelPicker puts the session into model-picking mode. The picker is part
// of the live region, drawn above the prompt box, and it takes over the
// navigation keys until the user picks or backs out.
func (t *TUI) openModelPicker(p *modelPicker) {
	t.modelPick = p
	t.paletteDismissed = true
	t.scheduleDraw()
}

// closeModelPicker leaves model-picking mode.
func (t *TUI) closeModelPicker() {
	t.modelPick = nil
}

// modelPickerOpen reports whether the picker is showing.
func (t *TUI) modelPickerOpen() bool { return t.modelPick != nil }

// modelPickerKey handles a key while the picker is open, reporting whether it
// consumed the key. Called with t.mu held.
func (t *TUI) modelPickerKey(k string) bool {
	switch k {
	case "up":
		t.modelPick.move(-1)
	case "down":
		t.modelPick.move(1)
	case "pageup":
		t.modelPick.move(-palettePage)
	case "pagedown":
		t.modelPick.move(palettePage)
	case "home":
		t.modelPick.sel = 0
	case "end":
		t.modelPick.sel = len(t.modelPick.items) - 1
	case "enter", "tab":
		it := t.modelPick.selected()
		t.closeModelPicker()
		if it == nil {
			return true
		}
		if err := applyModel(t, it.ref); err != nil {
			// The failure is already on screen; just make sure it lands.
			t.scheduleDraw()
		}
	case "esc":
		// Backing out must not change the model.
		t.closeModelPicker()
	default:
		return false
	}
	return true
}

// modelPickerRows renders the picker into whatever vertical space is left in
// the live region, above the prompt box.
func (t *TUI) modelPickerRows(pal theme.Palette, w, budget int) []string {
	if !t.modelPickerOpen() {
		return nil
	}
	rows := t.modelPick.rows(pal, w, budget)
	// A blank line keeps the list off the box border.
	return append(rows, pal.Style("dim", ""))
}

// modelPickerHint reports whether the key legend should describe the picker.
func (t *TUI) modelPickerHint() bool { return t.modelPickerOpen() }
