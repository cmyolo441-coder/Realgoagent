package tui

import (
	"bufio"
	"fmt"
	"strings"
	"unicode/utf8"
)

// readInput reads raw stdin and decodes keys, handing them to the key handler.
//
// It owns the terminal's input stream for the whole session, so it must not die
// quietly: a panic here takes the process down without unwinding Run, which
// would leave the user's shell in raw mode with no echo.
func (t *TUI) readInput(errs chan<- error) {
	rd := bufio.NewReaderSize(t.tty, 4096)
	// partial holds the leading bytes of a UTF-8 rune whose remainder has not
	// arrived yet. A paste is handed over in whatever chunks the tty decides
	// on, so a multi-byte character really can straddle two reads.
	var partial []byte
	defer func() {
		if rec := recover(); rec != nil {
			errs <- fmt.Errorf("input: %v", rec)
		}
	}()
	for {
		b, err := rd.ReadByte()
		if err != nil {
			errs <- err
			return
		}
		switch {
		case b == 0x1b:
			// escape sequence: try to read the rest without blocking
			if rd.Buffered() == 0 {
				// standalone ESC
				t.onKey("esc")
				continue
			}
			nb, err := rd.ReadByte()
			if err != nil {
				t.onKey("esc")
				continue
			}
			switch nb {
			case 0x0d, 0x0a:
				// ESC CR (ESC LF) is what a terminal sends for shift+enter.
				// It has to be claimed here, where the CR is still in hand:
				// read as Esc followed by Enter it clears the composer and
				// submits the empty line left behind.
				t.onKey("newline")
			case 'A':
				t.onKey("up")
			case 'B':
				t.onKey("down")
			case 'C':
				t.onKey("right")
			case 'D':
				t.onKey("left")
			case 'H':
				t.onKey("home")
			case 'F':
				t.onKey("end")
			case '[':
				t.readCSI(rd)
			case 'O':
				nb2, _ := rd.ReadByte()
				switch nb2 {
				case 'H':
					t.onKey("home")
				case 'F':
					t.onKey("end")
				}
			default:
				t.onKey("alt," + string(nb))
			}
		case b == 0x0d || b == 0x0a: // CR or LF
			if b == 0x0a {
				// a bare LF is a newline, not a submission
				t.onKey("newline")
				continue
			}
			t.onKey("enter")
		case b == 0x7f || b == 0x08: // DEL/BS
			t.onKey("backspace")
		case b == 0x03: // ^C
			t.onKey("ctrlc")
		case b == 0x04: // ^D
			t.onKey("ctrld")
		case b == 0x0c: // ^L
			t.onKey("redraw")
		case b == 0x17: // ^W
			t.onKey("delword")
		case b == 0x15: // ^U
			t.onKey("delline")
		case b == 0x0b: // ^K
			t.onKey("deltoend")
		case b == 0x01: // ^A
			t.onKey("home")
		case b == 0x05: // ^E
			t.onKey("end")
		case b == '\t':
			t.onKey("tab")
		case b < 0x20:
			// ignore other control chars
		default:
			partial = append(partial, b)
			// Only whole runes are inserted. Decoding a byte at a time stores
			// each byte of an accented letter or an emoji as a character of
			// its own, and the composer fills up with mojibake.
			for len(partial) > 0 {
				r, size := utf8.DecodeRune(partial)
				if r == utf8.RuneError && size <= 1 {
					if !utf8.FullRune(partial) {
						break // wait for the rest of the character
					}
					// Not valid UTF-8 at all: drop the byte rather than wedge
					// the decoder on it for the rest of the session.
					t.onRune(string(utf8.RuneError))
					partial = partial[1:]
					continue
				}
				t.onRune(string(partial[:size]))
				partial = partial[size:]
			}
		}
	}
}

func (t *TUI) readCSI(rd *bufio.Reader) {
	// after ESC [
	b, err := rd.ReadByte()
	if err != nil {
		return
	}
	switch b {
	case 'A':
		t.onKey("up")
	case 'B':
		t.onKey("down")
	case 'C':
		t.onKey("right")
	case 'D':
		t.onKey("left")
	case 'H':
		t.onKey("home")
	case 'F':
		t.onKey("end")
	case 'Z': // shift+tab
		t.onKey("shifttab")
	case '<':
		// SGR mouse report, ESC [ < b ; x ; y M. Nova does not enable mouse
		// reporting, so this only arrives from a terminal that sends it
		// unconditionally; swallow the whole report rather than letting its
		// tail reach the composer.
		for {
			c, err := rd.ReadByte()
			if err != nil {
				return
			}
			if c == 'M' || c == 'm' {
				return
			}
			if (c < '0' || c > '9') && c != ';' && c != '<' {
				return
			}
		}
	default:
		// Everything else begins a parameterised sequence,
		// ESC [ <params> <final>. The parameters have to be consumed: left in
		// the buffer they are typed into the composer, so ESC[1;5A (ctrl+up)
		// used to put ";5A" on screen.
		if !isCSIParam(b) {
			t.onCSI(b)
			return
		}
		params := []byte{b}
		for {
			c, err := rd.ReadByte()
			if err != nil {
				return
			}
			if isCSIParam(c) {
				params = append(params, c)
				continue
			}
			if c == '~' {
				t.tildeKey(rd, string(params))
				return
			}
			t.onCSI(c)
			return
		}
	}
}

// isCSIParam reports whether b is a CSI parameter byte.
func isCSIParam(b byte) bool {
	return (b >= '0' && b <= '9') || b == ';' || b == '?' || b == ':'
}

// onCSI dispatches a CSI sequence with numeric parameters to a key.
func (t *TUI) onCSI(final byte) {
	switch final {
	case 'A':
		t.onKey("up")
	case 'B':
		t.onKey("down")
	case 'C':
		t.onKey("right")
	case 'D':
		t.onKey("left")
	case 'H':
		t.onKey("home")
	case 'F':
		t.onKey("end")
	}
}

// tildeKey maps the parameters of an ESC [ <params> ~ sequence to a key.
func (t *TUI) tildeKey(rd *bufio.Reader, params string) {
	// A modifier arrives as extra parameters: ESC[3;5~ is ctrl+delete.
	if i := strings.IndexByte(params, ';'); i >= 0 {
		params = params[:i]
	}
	switch params {
	case "1", "7":
		t.onKey("home")
	case "4", "8":
		t.onKey("end")
	case "5":
		t.onKey("pageup")
	case "6":
		t.onKey("pagedown")
	case "2":
		t.onKey("insert")
	case "3":
		t.onKey("delete")
	case "200":
		t.readPaste(rd)
	}
}

// pasteEnd terminates a bracketed paste.
const pasteEnd = "\x1b[201~"

// readPaste consumes until ESC[201~ and inserts the payload.
func (t *TUI) readPaste(rd *bufio.Reader) {
	var sb strings.Builder
	// tail is the last len(pasteEnd) bytes seen, so the terminator can be
	// spotted without re-slicing the whole payload on every byte: a paste runs
	// to megabytes, and testing each prefix is quadratic in its length.
	var tail [len(pasteEnd)]byte
	n := 0
	for {
		b, err := rd.ReadByte()
		if err != nil {
			break
		}
		sb.WriteByte(b)
		copy(tail[:], tail[1:])
		tail[len(tail)-1] = b
		if n < len(tail) {
			n++
		}
		if n == len(tail) && string(tail[:]) == pasteEnd {
			s := sb.String()
			// The terminator is exactly the six bytes just matched. Trimming
			// suffixes of the payload instead would eat a pasted text that
			// happens to end in "1" or "20".
			t.insertText(s[:len(s)-len(pasteEnd)])
			return
		}
	}
	t.insertText(sb.String())
}

// insertText inserts text at the cursor, taking the mutex itself.
func (t *TUI) insertText(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.insertTextLocked(s)
}

// insertTextLocked is insertText for callers that already hold t.mu.
func (t *TUI) insertTextLocked(s string) {
	if s == "" {
		return
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if t.cursorLine >= len(t.inputLines) {
		t.inputLines = append(t.inputLines, "")
	}
	for i, ln := range lines {
		if i == 0 {
			cur := []rune(t.inputLines[t.cursorLine])
			// split at cursor
			if t.cursorCol > len(cur) {
				t.cursorCol = len(cur)
			}
			t.inputLines[t.cursorLine] = string(cur[:t.cursorCol]) + ln + string(cur[t.cursorCol:])
			t.cursorCol += countRunes(ln)
		} else {
			// One line per break, spliced in at the cursor. Appending a
			// placeholder and then overwriting it left a blank line behind for
			// every line of a multi-line paste, and the composer grew faster
			// than the text put into it.
			t.inputLines = insertLine(t.inputLines, t.cursorLine+i, ln)
			t.cursorCol = countRunes(ln)
		}
	}
	t.cursorLine += len(lines) - 1
	t.scheduleDraw()
}

// insertLineLocked is insertLine for callers that already hold t.mu.
func insertLineLocked(l []string, at int, s string) []string {
	if at > len(l) {
		at = len(l)
	}
	if at < 0 {
		at = 0
	}
	out := make([]string, 0, len(l)+1)
	out = append(out, l[:at]...)
	out = append(out, s)
	out = append(out, l[at:]...)
	return out
}

// onRune handles a printable rune.
func (t *TUI) onRune(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cursorLine >= len(t.inputLines) {
		t.inputLines = append(t.inputLines, "")
	}
	cur := []rune(t.inputLines[t.cursorLine])
	if t.cursorCol > len(cur) {
		t.cursorCol = len(cur)
	}
	out := string(cur[:t.cursorCol]) + s + string(cur[t.cursorCol:])
	t.inputLines[t.cursorLine] = out
	t.cursorCol += countRunes(s)
	t.scheduleDraw()
}

func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
