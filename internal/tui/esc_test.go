package tui

import (
	"testing"

	"github.com/nova-ai/nova/internal/editdiff"
)

func escTUI(lines ...string) *TUI {
	if len(lines) == 0 {
		lines = []string{""}
	}
	return &TUI{
		app:        &App{history: NewBuffer(100)},
		inputLines: lines,
		liveDiff:   newLiveDiffView(),
	}
}

// Esc on a typed prompt clears it, which is the fastest way back to a clean
// box without reaching for backspace a dozen times.
func TestEscClearsComposer(t *testing.T) {
	tui := escTUI("half a thought")
	tui.cursorCol = countRunes("half a thought")

	tui.handleEsc()

	if tui.inputLines[0] != "" {
		t.Errorf("composer = %q, want it cleared", tui.inputLines[0])
	}
	if tui.cursorCol != 0 || tui.cursorLine != 0 {
		t.Errorf("cursor = (%d,%d), want (0,0)", tui.cursorLine, tui.cursorCol)
	}
}

// Esc with nothing typed does nothing — it must not quit or otherwise surprise.
func TestEscOnEmptyComposerIsHarmless(t *testing.T) {
	tui := escTUI("")
	tui.handleEsc()
	if len(tui.inputLines) != 1 || tui.inputLines[0] != "" {
		t.Errorf("composer = %v, want it left alone", tui.inputLines)
	}
}

// Esc with the palette open closes the list but keeps what was typed, so the
// command can still be finished by hand. A second Esc then clears it.
func TestEscDismissesPaletteThenClears(t *testing.T) {
	tui := escTUI("/mod")
	if tui.paletteLen() == 0 {
		t.Fatal("palette should be open for /mod")
	}

	tui.handleEsc()
	if tui.paletteLen() != 0 {
		t.Error("Esc should have closed the palette")
	}
	if tui.inputLines[0] != "/mod" {
		t.Errorf("composer = %q, want the text kept", tui.inputLines[0])
	}

	tui.handleEsc()
	if tui.inputLines[0] != "" {
		t.Errorf("composer = %q, want the second Esc to clear it", tui.inputLines[0])
	}
}

// Typing after a dismissal brings the palette back.
func TestPaletteReopensAfterDismissalOnNewInput(t *testing.T) {
	tui := escTUI("/mod")
	tui.handleEsc()
	if tui.paletteLen() != 0 {
		t.Fatal("palette should be closed")
	}
	tui.inputLines[0] = "/mode"
	tui.syncPalette()
	if tui.paletteLen() == 0 {
		t.Error("palette should reopen once the composer changes")
	}
}

// While the agent waits on a question, Esc is ignored: the way out of that is
// to answer, and cancelling it silently would strand a tool call that still
// needs a decision.
func TestEscIgnoredWhileAsking(t *testing.T) {
	tui := escTUI("draft answer")
	tui.asking = true

	tui.handleEsc()

	if !tui.asking {
		t.Error("Esc cleared the asking state")
	}
	if tui.inputLines[0] != "draft answer" {
		t.Errorf("composer = %q, want it untouched while asking", tui.inputLines[0])
	}
}

// Esc is advertised to close the live diff panel, and the panel is up from the
// first edit onwards. It has to be answered before the composer, or Esc clears
// a half-typed prompt and leaves the panel on screen saying the opposite.
func TestEscDismissesLiveDiffThenClearsComposer(t *testing.T) {
	tui := escTUI("a half-typed prompt")
	tui.app.edits = newEditLog()
	tui.app.edits.record("edit", "1s", "", false, []editdiff.FileDiff{{
		Path:   "main.go",
		Before: "package main\n",
		After:  "package main\n\nfunc main() {}\n",
	}})
	if !tui.liveDiffOpen() {
		t.Fatal("the panel should be open after an edit")
	}

	tui.handleEsc()
	if tui.liveDiffOpen() {
		t.Error("Esc should have dismissed the live diff panel")
	}
	if tui.inputLines[0] != "a half-typed prompt" {
		t.Errorf("composer = %q, want the text kept", tui.inputLines[0])
	}

	tui.handleEsc()
	if tui.inputLines[0] != "" {
		t.Errorf("composer = %q, want the second Esc to clear it", tui.inputLines[0])
	}
}

// A turn in flight is stopped by Esc, through the handle Submit installed.
func TestEscStopsStreamingTurn(t *testing.T) {
	stopped := false
	tui := escTUI("a prompt I am still happy with")
	tui.streaming = true
	tui.cancelTurn = func() { stopped = true }

	tui.handleEsc()

	if !stopped {
		t.Error("Esc did not stop the turn in flight")
	}
	// The composer survives a stop: the point of stopping is usually to edit
	// the prompt and send it again.
	if tui.inputLines[0] != "a prompt I am still happy with" {
		t.Errorf("composer = %q, want it left alone", tui.inputLines[0])
	}
}

// Esc with no turn running has nothing to stop and must not crash.
func TestEscStreamingWithoutCancelHandle(t *testing.T) {
	tui := escTUI("text")
	tui.streaming = true
	tui.cancelTurn = nil
	tui.handleEsc()
}
