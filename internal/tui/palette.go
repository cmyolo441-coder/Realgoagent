package tui

import (
	"strings"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// The command palette: typing "/" opens a list of slash commands that can be
// walked with the arrow keys or PgUp/PgDn and put into the composer with
// Enter or Tab.
//
// It only opens for a single-line composer that starts with "/", so a
// multi-line prompt or ordinary prose is never interrupted by it.

// paletteCommands returns the commands matching the current composer line, in
// registry order. It returns nil when the palette should stay closed.
func (t *TUI) paletteCommands() []command {
	if t.paletteDismissed || len(t.inputLines) != 1 {
		return nil
	}
	// The space test runs on the raw line: TrimSpace would strip the trailing
	// space that paletteAccept appends, and the palette would never close.
	if strings.ContainsAny(t.inputLines[0], " \t") {
		return nil
	}
	line := t.inputLines[0]
	if !strings.HasPrefix(line, "/") {
		return nil
	}
	prefix := line
	var out []command
	for _, c := range commands {
		if strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// paletteLen is the number of commands currently offered.
func (t *TUI) paletteLen() int { return len(t.paletteCommands()) }

// syncPalette resets the highlight when the composer changes, so typing a new
// prefix always starts the walk from the top — and reopens a palette the user
// dismissed with Esc, once they start typing again.
func (t *TUI) syncPalette() int {
	key := ""
	if len(t.inputLines) == 1 {
		key = t.inputLines[0]
	}
	if key != t.paletteKey {
		t.paletteKey = key
		t.sel = 0
		t.paletteDismissed = false
	}
	cmds := t.paletteCommands()
	switch {
	case t.sel < 0:
		t.sel = 0
	case t.sel >= len(cmds):
		t.sel = len(cmds) - 1
	}
	return len(cmds)
}

// paletteMove walks the highlight by delta, keeping it in range.
func (t *TUI) paletteMove(delta int) {
	n := t.paletteLen()
	if n == 0 {
		return
	}
	t.sel += delta
	if t.sel < 0 {
		t.sel = 0
	}
	if t.sel >= n {
		t.sel = n - 1
	}
}

// paletteAccept puts the highlighted command into the composer with a trailing
// space, ready for its argument. It deliberately does not run the command:
// most of them take one, and Enter should not fire something the user has not
// finished typing.
func (t *TUI) paletteAccept() bool {
	cmds := t.paletteCommands()
	if t.sel < 0 || t.sel >= len(cmds) {
		return false
	}
	t.inputLines = []string{cmds[t.sel].Name + " "}
	t.cursorLine = 0
	t.cursorCol = countRunes(t.inputLines[0])
	t.paletteKey = ""
	t.sel = 0
	return true
}

// dismissPalette closes the list without touching the text, so the user can
// keep typing the command by hand. syncPalette reopens it the moment the
// composer changes.
func (t *TUI) dismissPalette() {
	t.paletteDismissed = true
	t.sel = 0
}

// paletteRows renders the command list, scrolled so the highlight is visible.
func (t *TUI) paletteRows(p theme.Palette, w, budget int) []string {
	cmds := t.paletteCommands()
	if len(cmds) == 0 || budget < 1 {
		return nil
	}
	// Two rows are spent on the header and the scroll indicator.
	visible := budget - 1

	if visible < 1 {
		visible = 1
	}
	if visible > len(cmds) {
		visible = len(cmds)
	}
	start := 0
	if t.sel >= visible {
		start = t.sel - visible + 1
	}

	rows := make([]string, 0, visible+1)
	more := ""
	if start > 0 {
		more = " ↑" + itoa(start)
	}
	if start+visible < len(cmds) {
		more = " ↓" + itoa(len(cmds)-start-visible) + more
	}
	// Truncate the header like any other row: an over-wide row wraps, and the
	// wrap drags the prompt box below it out of alignment.
	rows = append(rows, util.Truncate(p.Style("dim", " commands")+p.Style("dim", more), w))

	nameWidth := 0
	for _, c := range cmds {
		if n := util.VisibleWidth(c.Name); n > nameWidth {
			nameWidth = n
		}
	}
	for i := start; i < start+visible; i++ {
		c := cmds[i]
		marker := "  "
		style := p.Style("accent2", pad(c.Name, nameWidth))
		desc := p.Style("dim", c.Desc)
		if i == t.sel {
			marker = p.Style("accent", "❯ ")
			style = p.Style("text", pad(c.Name, nameWidth))
			desc = p.Style("dim", c.Desc)
		}
		row := marker + style + "  " + desc
		rows = append(rows, util.Truncate(row, w))
	}
	return rows
}

// palettePage is how far PgUp/PgDn move through the palette.
const palettePage = 5

// paletteKeyPress handles a key while the palette is open. It reports whether
// it consumed the key, and the text to run if Enter should execute a command
// outright. Called with t.mu held.
func (t *TUI) paletteKeyPress(k string) (handled bool, run string) {
	switch k {
	case "up":
		t.paletteMove(-1)
	case "down":
		t.paletteMove(1)
	case "pageup":
		t.paletteMove(-palettePage)
	case "pagedown":
		t.paletteMove(palettePage)
	case "home":
		t.sel = 0
	case "end":
		t.sel = t.paletteLen() - 1
	case "tab":
		// Tab only ever completes; it never runs a command.
		if t.paletteAccept() {
			return true, ""
		}
		return false, ""
	case "enter":
		// A command typed in full already runs on Enter. Anything shorter
		// completes first, because most commands need an argument and Enter
		// should not fire one the user has not finished typing.
		cmds := t.paletteCommands()
		line := strings.TrimSpace(t.inputLines[0])
		if t.sel >= 0 && t.sel < len(cmds) && cmds[t.sel].Name == line {
			t.dismissPalette()
			return true, line
		}
		if t.paletteAccept() {
			return true, ""
		}
		return false, ""
	default:
		return false, ""
	}
	return true, ""
}
