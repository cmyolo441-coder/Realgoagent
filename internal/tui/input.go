package tui

import (
	"bufio"
	"strings"
)

// readInput reads raw stdin and decodes keys, forwarding complete submissions.
func (t *TUI) readInput(out chan<- string, errs chan<- error) {
	rd := bufio.NewReaderSize(t.tty, 4096)
	var buf []byte
	for {
		b, err := rd.ReadByte()
		if err != nil {
			errs <- err
			return
		}
		buf = append(buf, b)
		switch {
		case b == 0x1b:
			// escape sequence: try to read the rest without blocking
			if rd.Buffered() == 0 {
				// standalone ESC
				t.onKey("esc")
				buf = buf[:0]
				continue
			}
			nb, err := rd.ReadByte()
			if err != nil {
				t.onKey("esc")
				buf = buf[:0]
				continue
			}
			switch nb {
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
			buf = buf[:0]
		case b == 0x0d || b == 0x0a: // CR or LF
			if b == 0x0a {
				// shift+enter detected via bracketed paste or LF: newline
				t.onKey("newline")
				buf = buf[:0]
				continue
			}
			if len(buf) >= 2 && buf[len(buf)-2] == 0x1b && buf[len(buf)-1] == 0x0d {
				// ESC CR = shift+enter
				t.onKey("newline")
				buf = buf[:0]
				continue
			}
			t.onKey("enter")
			buf = buf[:0]
		case b == 0x7f || b == 0x08: // DEL/BS
			t.onKey("backspace")
			buf = buf[:0]
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
		case b == 0x17:
			t.onKey("delword")
		case b == '\t':
			t.onKey("tab")
		case b == 0x16: // ^V paste start marker handled by bracketed paste
			t.onKey("paste")
		case b < 0x20:
			// ignore other control chars
			buf = buf[:0]
		default:
			t.onRune(b)
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
		// SGR mouse report. Nova does not enable mouse reporting, so this
		// only arrives from a terminal that sends it unconditionally; swallow
		// the three payload bytes rather than letting them reach the composer.
		for range 3 {
			if _, err := rd.ReadByte(); err != nil {
				return
			}
		}
	case '2', '3', '5', '6', '0', '1':
		// tilde sequences like ESC [ 3 ~ (delete), ESC[200~ (paste start)
		nb, _ := rd.ReadByte()
		if nb == '~' {
			switch b {
			case '3':
				t.onKey("delete")
			case '1', '7':
				t.onKey("home")
			case '4', '8':
				t.onKey("end")
			case '5':
				t.onKey("pageup")
			case '6':
				t.onKey("pagedown")
			case '2':
				// paste start: ESC[200~ is handled below
				t.onKey("insert")
			}
			return
		}
		// ESC[200~ : bracket paste
		if b == '2' && nb == '0' {
			nb2, _ := rd.ReadByte()
			if nb2 == '0' {
				nb3, _ := rd.ReadByte()
				if nb3 == '~' {
					t.readPaste(rd)
					return
				}
			}
			_ = nb2
		}
	default:
		// Numeric parameters, then a final byte. Only the final byte selects
		// the key, so the parameters are read and discarded.
		for {
			nb, err := rd.ReadByte()
			if err != nil {
				return
			}
			if (nb >= '0' && nb <= '9') || nb == ';' {
				continue
			}
			t.onCSI(nb)
			return
		}
	}
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

// readPaste consumes until ESC[201~ and inserts the payload.
func (t *TUI) readPaste(rd *bufio.Reader) {
	var sb strings.Builder
	var prev [3]byte
	for {
		b, err := rd.ReadByte()
		if err != nil {
			break
		}
		if b == '~' && prev[0] == 0x1b && prev[1] == '[' && prev[2] == '2' && false {
			break
		}
		// detect ESC [ 201~
		sb.WriteByte(b)
		s := sb.String()
		if strings.HasSuffix(s, "\x1b[201~") {
			content := strings.TrimSuffix(s, "\x1b[201~")
			// the leading content includes the 2 0 1 bytes written before ~
			content = strings.TrimSuffix(content, "\x1b[2")
			content = strings.TrimSuffix(content, "20")
			content = strings.TrimSuffix(content, "1")
			t.insertText(content)
			return
		}
		prev[0], prev[1], prev[2] = prev[1], prev[2], b
	}
	t.insertText(sb.String())
}

func (t *TUI) insertText(s string) {
	if s == "" {
		return
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, ln := range lines {
		if i == 0 {
			cur := t.inputLines[t.cursorLine]
			// split at cursor
			r := []rune(cur)
			if t.cursorCol > len(r) {
				t.cursorCol = len(r)
			}
			t.inputLines[t.cursorLine] = string(r[:t.cursorCol]) + ln + string(r[t.cursorCol:])
			t.cursorCol += len([]rune(ln))
		} else {
			t.inputLines = append(t.inputLines, "")
			t.inputLines = append(t.inputLines[:t.cursorLine+i], append([]string{ln}, t.inputLines[t.cursorLine+i:]...)...)
		}
	}
	if len(lines) > 1 {
		t.cursorLine += len(lines) - 1
	}
	t.scheduleDraw()
}

// onRune handles a printable rune.
func (t *TUI) onRune(b byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cursorLine >= len(t.inputLines) {
		t.inputLines = append(t.inputLines, "")
	}
	cur := []rune(t.inputLines[t.cursorLine])
	if t.cursorCol > len(cur) {
		t.cursorCol = len(cur)
	}
	s := string(b)
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
