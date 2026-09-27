package tui

import "testing"

func paletteTUI(lines ...string) *TUI {
	if len(lines) == 0 {
		lines = []string{""}
	}
	return &TUI{app: &App{history: NewBuffer(100)}, inputLines: lines, cursorLine: 0}
}

// Typing "/" must offer every command, in registry order.
func TestPaletteOpensOnSlash(t *testing.T) {
	got := paletteTUI("/").paletteCommands()
	if len(got) != len(commands) {
		t.Fatalf("palette shows %d commands, want all %d", len(got), len(commands))
	}
	if got[0].Name != "/help" {
		t.Errorf("first entry = %q, want /help", got[0].Name)
	}
}

// A prefix narrows the list, and an exact match stays in it so the user can
// still see what else it could grow into.
func TestPaletteFiltersByPrefix(t *testing.T) {
	got := paletteTUI("/mod").paletteCommands()
	if len(got) == 0 {
		t.Fatal("no matches for /mod")
	}
	for _, c := range got {
		if len(c.Name) < 4 || c.Name[:4] != "/mod" {
			t.Errorf("entry %q does not match the /mod prefix", c.Name)
		}
	}
	if !containsName(got, "/model") || !containsName(got, "/models") {
		t.Errorf("/mod should offer /model and /models, got %v", names(got))
	}
}

// Ordinary prose must never open the palette.
func TestPaletteStaysClosedForProse(t *testing.T) {
	if got := paletteTUI("fix the failing test").paletteCommands(); got != nil {
		t.Errorf("palette opened for prose: %v", names(got))
	}
	if got := paletteTUI("").paletteCommands(); got != nil {
		t.Errorf("palette opened for an empty composer: %v", names(got))
	}
}

// A multi-line composer is a prompt, not a command line.
func TestPaletteStaysClosedForMultiline(t *testing.T) {
	if got := paletteTUI("/help", "more text").paletteCommands(); got != nil {
		t.Errorf("palette opened for a multi-line composer: %v", names(got))
	}
}

// Once a command is complete the user is typing its argument. Leaving the
// list open would let the next Enter complete a different command instead of
// running the one that was chosen.
func TestPaletteClosesAfterCompletion(t *testing.T) {
	if got := paletteTUI("/mode ").paletteCommands(); got != nil {
		t.Errorf("palette stayed open while typing an argument: %v", names(got))
	}
	if got := paletteTUI("/model grok").paletteCommands(); got != nil {
		t.Errorf("palette stayed open for a full command with its argument: %v", names(got))
	}
}

// Accepting puts the highlighted command in the composer with a trailing
// space, ready for its argument, and leaves the caret after it.
func TestPaletteAcceptFillsComposer(t *testing.T) {
	tui := paletteTUI("/")
	tui.sel = 2
	if !tui.paletteAccept() {
		t.Fatal("paletteAccept returned false with a selection")
	}
	want := commands[2].Name + " "
	if tui.inputLines[0] != want {
		t.Errorf("composer = %q, want %q", tui.inputLines[0], want)
	}
	if tui.cursorCol != countRunes(want) {
		t.Errorf("cursorCol = %d, want %d", tui.cursorCol, countRunes(want))
	}
	if tui.paletteLen() != 0 {
		t.Error("palette should be closed after accepting")
	}
}

// Accepting with nothing highlighted must not touch the composer.
func TestPaletteAcceptWithoutSelection(t *testing.T) {
	tui := paletteTUI("/help")
	before := tui.inputLines[0]
	tui.sel = 99 // out of range
	if tui.paletteAccept() {
		t.Error("paletteAccept accepted an out-of-range selection")
	}
	if tui.inputLines[0] != before {
		t.Errorf("composer changed to %q", tui.inputLines[0])
	}
}

// Walking past either end clamps instead of wrapping or running off.
func TestPaletteMoveClamps(t *testing.T) {
	tui := paletteTUI("/")
	n := tui.paletteLen()

	tui.paletteMove(-5)
	if tui.sel != 0 {
		t.Errorf("sel = %d after moving up from the top, want 0", tui.sel)
	}
	tui.paletteMove(1000)
	if tui.sel != n-1 {
		t.Errorf("sel = %d after moving down past the end, want %d", tui.sel, n-1)
	}
}

// PgUp and PgDn jump several rows at a time.
func TestPalettePageKeysJump(t *testing.T) {
	tui := paletteTUI("/")
	n := tui.paletteLen()
	if n <= palettePage {
		t.Skipf("only %d commands; not enough to page", n)
	}
	if handled, _ := tui.paletteKeyPress("pagedown"); !handled {
		t.Fatal("pagedown was not consumed by the palette")
	}
	if tui.sel != palettePage {
		t.Errorf("sel = %d after PgDn, want %d", tui.sel, palettePage)
	}
	if handled, _ := tui.paletteKeyPress("pageup"); !handled {
		t.Fatal("pageup was not consumed by the palette")
	}
	if tui.sel != 0 {
		t.Errorf("sel = %d after PgUp, want 0", tui.sel)
	}
}

// Typing a new prefix restarts the walk at the top.
func TestSyncPaletteResetsOnNewPrefix(t *testing.T) {
	tui := paletteTUI("/")
	tui.syncPalette()
	tui.sel = 3

	tui.inputLines[0] = "/mo"
	tui.syncPalette()
	if tui.sel != 0 {
		t.Errorf("sel = %d after the prefix changed, want the list reset to 0", tui.sel)
	}
}

// The palette never eats keys it has no use for.
func TestPaletteIgnoresUnrelatedKeys(t *testing.T) {
	tui := paletteTUI("/")
	for _, k := range []string{"backspace", "delete", "ctrlc", "left", "right"} {
		if handled, _ := tui.paletteKeyPress(k); handled {
			t.Errorf("palette consumed %q, which it has no use for", k)
		}
	}
}

func names(cs []command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func containsName(cs []command, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}
