package tui

import (
	"strings"
	"time"
)

// handleLine processes a submitted line: either a slash command or a prompt.
//
// The line always carries the composer's contents: the caller reads them under
// the mutex and hands them over, so nothing here touches the input state.
// Reading it again would race the goroutine still taking keystrokes, and
// clearing it would eat a keystroke that landed in between.
func (t *TUI) handleLine(line string) error {
	text := strings.TrimSpace(line)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/") {
		return t.runCommand(text)
	}
	t.app.history.Append(t.app.Theme.Style("accent", "❯ ") + text)
	t.app.history.Append("")
	return t.Submit(text)
}

// overlayOpen reports whether any full-panel overlay is showing. The command
// palette is not one of them: it is a completion list for the composer, and it
// only makes sense while the composer is being typed into. The live diff panel
// is not an overlay: it comes up on its own after an edit and renders above the
// prompt, so it does not block other views from opening. It is asked for keys
// separately, and only claims the ones it can act on.
func (t *TUI) overlayOpen() bool {
	return t.subViewOpen() || t.editViewerOpen() || t.modelPickerOpen()
}

// onKey dispatches a decoded key.
func (t *TUI) onKey(k string) {
	t.mu.Lock()
	// Overlays take the navigation keys while they are open, most specific
	// first: the subagent and edit viewers, then the model picker, then the
	// command palette. Only one is open at a time, because a screen that shows
	// two stacked lists is a screen nobody can read.
	if t.subViewOpen() {
		if handled := t.subViewKey(k); handled {
			t.mu.Unlock()
			t.scheduleDraw()
			return
		}
	}
	if t.editViewerOpen() {
		if handled := t.editViewerKey(k); handled {
			t.mu.Unlock()
			t.scheduleDraw()
			return
		}
	}
	if t.liveDiffOpen() {
		if handled := t.liveDiffKey(k); handled {
			t.mu.Unlock()
			t.scheduleDraw()
			return
		}
	}
	if t.modelPickerOpen() {
		if done := t.modelPickerKey(k); done {
			t.mu.Unlock()
			t.scheduleDraw()
			return
		}
	}
	if !t.overlayOpen() && t.paletteLen() > 0 {
		if handled, run := t.paletteKeyPress(k); handled {
			if run != "" {
				// The command is being run, so the composer is emptied here:
				// this path bypasses handleLine, which is what normally
				// clears it.
				t.inputLines = []string{""}
				t.cursorLine, t.cursorCol = 0, 0
			}
			t.mu.Unlock()
			if run != "" {
				t.submitLine(run)
			}
			t.scheduleDraw()
			return
		}
	}
	if k == "esc" {
		t.handleEsc()
		t.mu.Unlock()
		t.scheduleDraw()
		return
	}
	switch k {
	case "enter":
		if t.asking {
			ans := strings.Join(t.inputLines, " ")
			t.inputLines = []string{""}
			t.cursorLine, t.cursorCol = 0, 0
			t.asking = false
			// Take the channel the question published, and forget it: the next
			// question publishes a fresh one. A channel left behind here keeps
			// its reader parked, and the agent waits for an answer that has
			// already been typed.
			ch := t.pendingAnswer
			t.pendingAnswer = nil
			t.mu.Unlock()
			// Buffered, so this never parks the key handler. Nil when the
			// question was raised by something that has since gone away.
			if ch != nil {
				ch <- ans
			}
			t.scheduleDraw()
			return
		}
		text := strings.Join(t.inputLines, "\n")
		t.inputLines = []string{""}
		t.cursorLine, t.cursorCol = 0, 0
		t.mu.Unlock()
		t.submitLine(text)
		t.scheduleDraw()
		return
	case "newline":
		if t.cursorLine >= len(t.inputLines) {
			t.inputLines = append(t.inputLines, "")
		}
		cur := []rune(t.inputLines[t.cursorLine])
		if t.cursorCol > len(cur) {
			t.cursorCol = len(cur)
		}
		rest := string(cur[t.cursorCol:])
		t.inputLines[t.cursorLine] = string(cur[:t.cursorCol])
		t.inputLines = insertLine(t.inputLines, t.cursorLine+1, rest)
		t.cursorLine++
		t.cursorCol = 0
	case "backspace":
		if t.cursorCol > 0 {
			cur := []rune(t.inputLines[t.cursorLine])
			if t.cursorCol <= len(cur) {
				t.inputLines[t.cursorLine] = string(cur[:t.cursorCol-1]) + string(cur[t.cursorCol:])
				t.cursorCol--
			}
		} else if t.cursorLine > 0 {
			prev := t.inputLines[t.cursorLine-1]
			cur := t.inputLines[t.cursorLine]
			join := prev + cur
			t.cursorCol = countRunes(prev)
			t.inputLines[t.cursorLine-1] = join
			t.inputLines = removeLine(t.inputLines, t.cursorLine)
			t.cursorLine--
		}
	case "delete":
		cur := []rune(t.inputLines[t.cursorLine])
		if t.cursorCol < len(cur) {
			t.inputLines[t.cursorLine] = string(cur[:t.cursorCol]) + string(cur[t.cursorCol+1:])
		} else if t.cursorLine+1 < len(t.inputLines) {
			t.inputLines[t.cursorLine] += t.inputLines[t.cursorLine+1]
			t.inputLines = removeLine(t.inputLines, t.cursorLine+1)
		}
	case "delline":
		cur := []rune(t.inputLines[t.cursorLine])
		if len(cur) > 0 && t.cursorCol == len(cur) {
			t.inputLines[t.cursorLine] = ""
			t.cursorCol = 0
		} else {
			t.inputLines[t.cursorLine] = string(cur[t.cursorCol:])
			t.cursorCol = 0
		}
	case "deltoend":
		cur := []rune(t.inputLines[t.cursorLine])
		if t.cursorCol <= len(cur) {
			t.inputLines[t.cursorLine] = string(cur[:t.cursorCol])
		}
	case "delword":
		cur := []rune(t.inputLines[t.cursorLine])
		i := t.cursorCol
		for i > 0 && cur[i-1] == ' ' {
			i--
		}
		for i > 0 && cur[i-1] != ' ' {
			i--
		}
		t.inputLines[t.cursorLine] = string(cur[:i]) + string(cur[t.cursorCol:])
		t.cursorCol = i
	case "left":
		if t.cursorCol > 0 {
			t.cursorCol--
		} else if t.cursorLine > 0 {
			t.cursorLine--
			t.cursorCol = countRunes(t.inputLines[t.cursorLine])
		}
	case "right":
		cur := []rune(t.inputLines[t.cursorLine])
		if t.cursorCol < len(cur) {
			t.cursorCol++
		} else if t.cursorLine+1 < len(t.inputLines) {
			t.cursorLine++
			t.cursorCol = 0
		}
	case "up":
		if t.cursorLine > 0 {
			t.cursorLine--
			if t.cursorCol > countRunes(t.inputLines[t.cursorLine]) {
				t.cursorCol = countRunes(t.inputLines[t.cursorLine])
			}
		}
	case "down":
		if t.cursorLine+1 < len(t.inputLines) {
			t.cursorLine++
			if t.cursorCol > countRunes(t.inputLines[t.cursorLine]) {
				t.cursorCol = countRunes(t.inputLines[t.cursorLine])
			}
		}
	case "home":
		t.cursorCol = 0
	case "end":
		t.cursorCol = countRunes(t.inputLines[t.cursorLine])
	case "redraw":
		// ^L clears the display. The transcript stays in the terminal's own
		// scrollback, so this only repaints; it does not discard history.
		//
		// The buffer is emptied and the read position moved to its new total.
		// Leaving the position at zero would leave the reader behind the
		// window's new contents, and the next paint would reprint the
		// transcript from the top.
		//
		// forceDraw takes t.mu, which is held here, so the lock is dropped
		// across it and the bookkeeping that forceDraw's own draw depends on
		// is set before the unlock.
		t.app.history.Clear()
		t.flushed = t.app.history.Total()
		t.liveLines = 0
		t.caretRowLast = 0
		t.lastFrame = ""
		t.mu.Unlock()
		t.clearScreen()
		t.forceDraw()
		return
	case "ctrlc":
		t.mu.Unlock()
		t.Quit()
		return
	case "ctrld":
		if strings.TrimSpace(strings.Join(t.inputLines, "\n")) == "" {
			t.mu.Unlock()
			t.Quit()
			return
		}
	case "tab":
		// accept completion if any
		if comp := t.completeCommand(); comp != "" && len(t.inputLines) > 0 {
			t.inputLines[0] = comp + " "
			t.cursorCol = countRunes(t.inputLines[0])
		}
	case "pageup", "pagedown", "wheelup", "wheeldown":
		// Scrolling is the terminal's job now: Nova writes to the primary
		// screen buffer, so the wheel and Shift+PgUp scroll real scrollback.
		// Swallowing them here would make the mouse feel broken.
	default:
		// treat unknown alt combos as literal
		if strings.HasPrefix(k, "alt,") {
			// t.mu is held across this switch, so the locked-out insert is
			// the only one that can run here: insertText would take the same
			// non-reentrant mutex again and wedge the input goroutine for
			// good, leaving the terminal in raw mode with no way out but a
			// kill -9.
			t.insertTextLocked(k[4:])
		}
	}
	t.mu.Unlock()
	t.scheduleDraw()
}

// submitLine hands a finished line to the event loop.
//
// The loop is the only goroutine allowed to touch the display buffer: a
// command run from the input goroutine appends to app.history while the loop
// is reading that same slice to commit it, which is a torn read at best. It
// is also where the sticky error and the streaming state live, so a turn
// started from the reader raced every repaint.
//
// Outside a run loop there is nothing to hand the line to and it is handled
// here.
func (t *TUI) submitLine(line string) {
	if t.submissions != nil {
		select {
		case t.submissions <- line:
		case <-t.done:
		}
		return
	}
	if err := t.handleLine(line); err != nil {
		t.setLastErr(err)
	}
}

// setLastErr records the sticky error shown on the top rail.
func (t *TUI) setLastErr(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.lastErr = err
	t.mu.Unlock()
}

// Quit exits the application.
func (t *TUI) Quit() {
	t.mu.Lock()
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	t.mu.Unlock()
}

func (t *TUI) scheduleDraw() {
	select {
	case t.redrawReq <- struct{}{}:
	default:
	}
}

func joinLines(l []string) string { return strings.Join(l, "\n") }

func insertLine(l []string, at int, s string) []string {
	if at > len(l) {
		at = len(l)
	}
	out := make([]string, 0, len(l)+1)
	out = append(out, l[:at]...)
	out = append(out, s)
	out = append(out, l[at:]...)
	return out
}

func removeLine(l []string, at int) []string {
	if at >= len(l) {
		return l
	}
	out := make([]string, 0, len(l)-1)
	out = append(out, l[:at]...)
	out = append(out, l[at+1:]...)
	return out
}

func (t *TUI) completeCommand() string {
	if len(t.inputLines) == 0 || t.cursorLine < 0 || t.cursorLine >= len(t.inputLines) {
		return ""
	}
	return completeCommand(t.inputLines[t.cursorLine], t.app)
}

// time helpers
func nowMillis() int64 { return time.Now().UnixNano() / 1e6 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
