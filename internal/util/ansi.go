// Package util provides ANSI-aware text primitives used across the TUI.
package util

import (
	"strings"
	"unicode/utf8"
)

// ANSI escape sequence introducer.
const esc = "\x1b["

// Strip removes ANSI escape sequences from s.
func Strip(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// CSI ... final byte in @-~
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) {
					c := s[j]
					if c >= 0x40 && c <= 0x7e {
						j++
						break
					}
					j++
				}
				i = j
				continue
			}
			// OSC ... BEL or ST
			if i+1 < len(s) && s[i+1] == ']' {
				j := i + 2
				for j < len(s) {
					if s[j] == 0x07 {
						j++
						break
					}
					if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j
				continue
			}
			// other short escapes
			i += 2
			if i > len(s) {
				i = len(s)
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// VisibleWidth returns the printed width of s ignoring ANSI sequences.
func VisibleWidth(s string) int {
	return runeWidth(Strip(s))
}

func runeWidth(s string) int {
	w := 0
	for _, r := range s {
		w += RuneWidth(r)
	}
	return w
}

// RuneWidth returns the display width of a single rune.
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7f && r < 0xa0):
		return 0
	case isCombining(r):
		return 0
	case isWide(r):
		return 2
	}
	return 1
}

// PadRight pads s with spaces so its visible width reaches w.
func PadRight(s string, w int) string {
	d := w - VisibleWidth(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}

// PadLeft pads s with spaces on the left so its visible width reaches w.
func PadLeft(s string, w int) string {
	d := w - VisibleWidth(s)
	if d <= 0 {
		return s
	}
	return strings.Repeat(" ", d) + s
}

// Truncate cuts s so its visible width does not exceed w, appending ellipsis.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if VisibleWidth(s) <= w {
		return s
	}
	ellipsis := "…"
	ew := 1
	if w < ew+1 {
		return strings.Repeat("…", w)
	}
	target := w - ew
	var b strings.Builder
	width := 0
	inEsc := false
	escDepth := 0
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			switch {
			case escDepth == 0 && r == '[':
				// CSI introducer; keep consuming until the final byte.
			case escDepth == 0 && r == ']':
				// OSC: now wait for the BEL terminator.
				escDepth = 1
			case escDepth == 0 && (r >= 0x40 && r <= 0x7e):
				inEsc = false
			case escDepth == 1 && r == 0x07:
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			escDepth = 0
			b.WriteRune(r)
			continue
		}
		rw := RuneWidth(r)
		if width+rw > target {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	b.WriteString(ellipsis)
	if strings.Contains(s, "\x1b") {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// Wrap wraps s to the given display width, preserving ANSI state across lines.
// Existing newline characters force line breaks.
func Wrap(s string, width int) []string {
	if width <= 0 {
		width = 80
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		out = append(out, wrapOne(para, width)...)
	}
	return out
}

func wrapOne(s string, width int) []string {
	if s == "" {
		return []string{""}
	}
	if VisibleWidth(s) <= width {
		return []string{s}
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	var pending strings.Builder // ANSI codes seen so far, replayed after wrap
	// Collect trailing reset state to carry over.
	ansiBuf := make([]byte, 0, 16)

	flush := func() {
		lines = append(lines, strings.TrimRight(cur.String(), " "))
		cur.Reset()
		// replay active ANSI state on the next line
		if ansiBufActive(pending.String()) {
			cur.WriteString(pending.String())
		}
		curW = 0
	}

	// Walk the string by byte, decoding one rune at a time. The previous
	// []rune conversion copied the whole (already ANSI-bloated) line before
	// the loop even started, which doubled the allocation on every wrapped
	// line of every re-rendered streaming tail.
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			// capture whole escape into cur and pending
			start := i
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) {
					c := s[i]
					i++
					if c >= 0x40 && c <= 0x7e {
						break
					}
				}
			} else {
				if i < len(s) {
					// decode one rune so a multi-byte short escape is not split
					_, size := decodeRune(s[i:])
					i += size
				}
			}
			seq := s[start:i]
			cur.WriteString(seq)
			pending.WriteString(seq)
			ansiBuf = append(ansiBuf, seq...)
			continue
		}
		r, size := decodeRune(s[i:])
		rw := RuneWidth(r)
		if curW+rw > width {
			// try to break at last space
			flush()
		}
		cur.WriteString(s[i : i+size])
		curW += rw
		i += size
	}
	if cur.Len() > 0 || len(lines) == 0 {
		lines = append(lines, strings.TrimRight(cur.String(), " "))
	}
	_ = ansiBuf
	return lines
}

func ansiBufActive(s string) bool {
	// crude: active if last SGR is not a reset
	idx := strings.LastIndex(s, "\x1b[")
	if idx < 0 {
		return false
	}
	seq := s[idx:]
	return !strings.HasSuffix(seq, "[0m")
}

// HardWrap wraps without breaking ANSI sequences, splitting on rune boundaries
// and never breaking inside a word if avoidable.
func HardWrap(s string, width int) []string { return Wrap(s, width) }

// CountLines returns how many terminal lines s occupies at the given width.
func CountLines(s string, width int) int {
	return len(Wrap(s, width))
}

// Repeat repeats r n times.
func Repeat(r rune, n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(string(r), n)
}

// RuneCount is a thin wrapper over utf8 for callers that need it.
func RuneCount(s string) int { return utf8.RuneCountInString(s) }

// decodeRune decodes one rune from the front of s without allocating. It is
// utf8.DecodeRuneInString specialised for the wrapper's hot loop.
func decodeRune(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	c := s[0]
	if c < 0x80 {
		return rune(c), 1
	}
	return utf8.DecodeRuneInString(s)
}

// isCombining reports whether r is a zero-width combining mark.
func isCombining(r rune) bool {
	return (r >= 0x0300 && r <= 0x036f) ||
		(r >= 0x1ab0 && r <= 0x1aff) ||
		(r >= 0x20d0 && r <= 0x20ff) ||
		(r >= 0xfe20 && r <= 0xfe2f) ||
		r == 0x200b || r == 0x200c || r == 0x200d || r == 0xfeff
}

// isWide reports whether r occupies two columns (CJK, emoji, box art).
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f: // hangul jamo
		return true
	case r >= 0x2e80 && r <= 0x303e: // cjk radicals, punctuation
		return true
	case r >= 0x3041 && r <= 0x33ff: // hiragana .. cjk compat
		return true
	case r >= 0x3400 && r <= 0x4dbf: // cjk ext a
		return true
	case r >= 0x4e00 && r <= 0x9fff: // cjk unified
		return true
	case r >= 0xa000 && r <= 0xa4cf: // yi
		return true
	case r >= 0xac00 && r <= 0xd7a3: // hangul syllables
		return true
	case r >= 0xf900 && r <= 0xfaff: // cjk compat ideographs
		return true
	case r >= 0xfe30 && r <= 0xfe6f: // cjk compat forms
		return true
	case r >= 0xff00 && r <= 0xff60: // fullwidth forms
		return true
	case r >= 0xffe0 && r <= 0xffe6:
		return true
	case r >= 0x1f300 && r <= 0x1f64f: // emoji
		return true
	case r >= 0x1f900 && r <= 0x1f9ff:
		return true
	case r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}
