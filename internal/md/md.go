// Package md renders Markdown to ANSI-styled terminal text.
package md

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// Renderer converts Markdown to styled terminal lines.
type Renderer struct {
	Palette theme.Palette
	Width   int
}

// New returns a renderer bound to a palette.
func New(p theme.Palette, width int) *Renderer {
	return &Renderer{Palette: p, Width: width}
}

var (
	reCodeFence = regexp.MustCompile("^\\s*```")
	reHeading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet    = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reOrdered   = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	reQuote     = regexp.MustCompile(`^>\s?(.*)$`)
	reRule      = regexp.MustCompile(`^\s*(---|\*\*\*|___|===)\s*$`)
	reTableSep  = regexp.MustCompile(`^\s*\|?[\s:|-]+\|[\s:|-]*$`)
	reInline    = regexp.MustCompile("`([^`]+)`")
	reBold      = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reBoldU     = regexp.MustCompile(`__([^_]+)__`)
	reItalic    = regexp.MustCompile(`\*([^*\n]+)\*`)
	reItalicU   = regexp.MustCompile(`\b_([^_\n]+)_\b`)
	reStrike    = regexp.MustCompile(`~~([^~]+)~~`)
	reLink      = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reImage     = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	reAutoLink  = regexp.MustCompile(`<(https?://[^>]+)>`)
	reFileRef   = regexp.MustCompile("`([\\w./~-]+[./][\\w./~-]*:\\d+(-\\d+)?)`")
)

// Render converts a Markdown document into styled terminal lines.
// Width may be 0 for no wrapping (caller handles it).
func (r *Renderer) Render(src string) []string {
	if r.Width <= 0 {
		r.Width = 80
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var out []string
	p := r.Palette

	inCode := false
	var codeLang string
	var codeBuf []string
	inTable := false
	var tableBuf []string

	flushTable := func() {
		if len(tableBuf) == 0 {
			return
		}
		out = append(out, r.renderTable(tableBuf)...)
		tableBuf = nil
		inTable = false
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if reCodeFence.MatchString(line) {
			if inCode {
				out = append(out, r.renderCodeBlock(codeLang, codeBuf)...)
				inCode = false
				codeLang = ""
				codeBuf = nil
			} else {
				flushTable()
				inCode = true
				codeLang = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "```"))
			}
			continue
		}
		if inCode {
			codeBuf = append(codeBuf, line)
			continue
		}

		// tables
		if strings.Contains(line, "|") && strings.TrimSpace(line) != "" && looksTableish(line) {
			if !inTable {
				inTable = true
			}
			tableBuf = append(tableBuf, line)
			// peek next
			if i+1 < len(lines) && looksTableish(lines[i+1]) {
				continue
			}
			flushTable()
			continue
		}
		flushTable()

		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			out = append(out, "")
			continue
		}

		if m := reHeading.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			text := r.inline(m[2])
			switch level {
			case 1:
				out = append(out, p.Style("accent_bold", text))
				out = append(out, p.Style("border", strings.Repeat("─", min(r.Width-2, util.VisibleWidth(text)))))
			case 2:
				out = append(out, p.Style("accent_bold", text))
			case 3:
				out = append(out, p.Style("accent2", text))
			default:
				out = append(out, p.Style("accent2", text))
			}
			continue
		}

		if m := reRule.FindStringSubmatch(line); m != nil {
			out = append(out, p.Style("dim", strings.Repeat("─", r.Width-2)))
			continue
		}

		if m := reQuote.FindStringSubmatch(line); m != nil {
			quoted := r.inline(m[1])
			wrapped := wrapText(quoted, r.Width-4)
			for _, w := range wrapped {
				out = append(out, p.Style("dim", "│ ")+w)
			}
			continue
		}

		if m := reOrdered.FindStringSubmatch(line); m != nil {
			indent := len(m[1])
			text := r.inline(m[3])
			bullet := p.Style("accent", fmt.Sprintf("%s. ", strings.TrimSpace(m[2])))
			prefix := strings.Repeat(" ", indent*2)
			for _, w := range wrapText(text, r.Width-util.VisibleWidth(prefix)-3) {
				out = append(out, prefix+bullet+w)
				bullet = ""
			}
			continue
		}

		if m := reBullet.FindStringSubmatch(line); m != nil {
			indent := len(m[1])
			text := m[2]
			prefix := strings.Repeat(" ", indent*2)
			bullet := p.Style("accent", "• ")
			checkbox := ""
			if strings.HasPrefix(text, "[ ] ") {
				checkbox = p.Style("dim", "☐ ") + strings.TrimPrefix(text, "[ ] ")
			} else if strings.HasPrefix(text, "[x] ") {
				checkbox = p.Style("success", "☑ ") + strings.TrimPrefix(text, "[x] ")
			} else if strings.HasPrefix(text, "[X] ") {
				checkbox = p.Style("success", "☑ ") + strings.TrimPrefix(text, "[X] ")
			}
			var content string
			if checkbox != "" {
				content = checkbox
			} else {
				content = r.inline(text)
			}
			first := true
			for _, w := range wrapText(content, r.Width-util.VisibleWidth(prefix)-2) {
				if first {
					out = append(out, prefix+bullet+w)
					first = false
				} else {
					out = append(out, prefix+strings.Repeat(" ", 2)+w)
				}
			}
			continue
		}

		// plain paragraph: consecutive lines joined
		para := []string{line}
		for i+1 < len(lines) {
			nxt := lines[i+1]
			if strings.TrimSpace(nxt) == "" || reCodeFence.MatchString(nxt) || reHeading.MatchString(nxt) ||
				reBullet.MatchString(nxt) || reOrdered.MatchString(nxt) || reQuote.MatchString(nxt) ||
				strings.Contains(nxt, "|") {
				break
			}
			para = append(para, strings.TrimSpace(nxt))
			i++
		}
		text := r.inline(strings.Join(para, " "))
		for _, w := range wrapText(text, r.Width) {
			out = append(out, w)
		}
	}

	if inCode {
		out = append(out, r.renderCodeBlock(codeLang, codeBuf)...)
	}
	flushTable()
	return out
}

func looksTableish(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") || strings.Contains(t, " | ")
}

// inline applies inline formatting: code, bold, italic, links.
// File references are processed first so their backtick delimiters are still
// present; code spans are tokenised next and restored last so that the emphasis
// and link regexes can never match ANSI bytes emitted for a code span.
func (r *Renderer) inline(s string) string {
	p := r.Palette

	// 1. file references with line numbers get accent colour
	s = reFileRef.ReplaceAllStringFunc(s, func(m string) string {
		inner := reFileRef.FindStringSubmatch(m)[1]
		return p.Style("accent2", inner)
	})

	// 2. tokenise code spans so nothing else touches their contents
	var codes []string
	protect := func(in string) string {
		spans := reInline.FindAllStringSubmatch(in, -1)
		if len(spans) == 0 {
			return in
		}
		codes = codes[:0]
		var b strings.Builder
		last := 0
		for _, sp := range spans {
			b.WriteString(in[last : strings.Index(in[last:], sp[0])+last])
			b.WriteString(placeholder(len(codes), len(sp[1])))
			codes = append(codes, sp[1])
			last = strings.Index(in[last:], sp[0]) + last + len(sp[0])
		}
		b.WriteString(in[last:])
		return b.String()
	}
	s = protect(s)

	// 3. images (before links)
	s = reImage.ReplaceAllStringFunc(s, func(m string) string {
		sub := reImage.FindStringSubmatch(m)
		return p.Style("dim", "[img] "+sub[1])
	})
	// 4. links
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		return p.Style("accent2", sub[1]) + p.Style("dim", " ("+sub[2]+")")
	})
	s = reAutoLink.ReplaceAllStringFunc(s, func(m string) string {
		return p.Style("accent2", m[1:len(m)-1])
	})
	// 5. emphasis (placeholders contain only digits, so bold/italic/regex safe)
	s = reBold.ReplaceAllString(s, "\x1b[1m$1\x1b[0m")
	s = reBoldU.ReplaceAllString(s, "\x1b[1m$1\x1b[0m")
	s = reStrike.ReplaceAllString(s, "\x1b[9m$1\x1b[0m")
	s = reItalicU.ReplaceAllString(s, "\x1b[3m$1\x1b[0m")
	s = reItalic.ReplaceAllString(s, "\x1b[3m$1\x1b[0m")

	// 6. restore code spans, now styled
	for i, c := range codes {
		s = strings.ReplaceAll(s, placeholder(i, len(c)), p.Style("warning", c))
	}
	return s
}

// placeholder is a run of digits that cannot be re-matched by the emphasis
// regexes and cannot collide with literal text (markdown rarely has \x01).
func placeholder(idx, spanLen int) string {
	return "\x01" + strings.Repeat("0", spanLen) + "\x01"
}

// renderCodeBlock renders a fenced block with line numbers.
func (r *Renderer) renderCodeBlock(lang string, lines []string) []string {
	var out []string
	p := r.Palette
	header := " code"
	if lang != "" {
		header = " " + lang
	}
	out = append(out, p.Style("dim", "╭─"+strings.Repeat("─", max(1, r.Width-6-util.VisibleWidth(header)))+header))
	nw := len(fmt.Sprint(len(lines)))
	for i, ln := range lines {
		num := fmt.Sprintf("%*d ", nw, i+1)
		body := highlight(lang, ln)
		out = append(out, p.Style("dim", "│ ")+p.Style("dim", num)+body)
	}
	out = append(out, p.Style("dim", "╰"+strings.Repeat("─", max(1, r.Width-2))))
	return out
}

// wrapText wraps a string to width, preserving ANSI escapes.
func wrapText(s string, width int) []string {
	return util.Wrap(s, width)
}

func (r *Renderer) renderTable(rows []string) []string {
	var out []string
	p := r.Palette
	if len(rows) == 0 {
		return out
	}
	// parse cells
	parsed := make([][]string, 0, len(rows))
	for _, row := range rows {
		if reTableSep.MatchString(row) {
			continue
		}
		t := strings.TrimSpace(row)
		t = strings.TrimPrefix(t, "|")
		t = strings.TrimSuffix(t, "|")
		cells := strings.Split(t, "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		parsed = append(parsed, cells)
	}
	if len(parsed) == 0 {
		return out
	}
	cols := 0
	for _, row := range parsed {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for _, row := range parsed {
		for i, c := range row {
			if i < cols {
				if w := util.VisibleWidth(r.inline(c)); w > widths[i] {
					widths[i] = w
				}
			}
		}
	}
	// clamp to render width
	avail := r.Width - (cols + 1)
	if avail < cols*4 {
		avail = cols * 4
	}
	maxCol := avail / cols
	for i := range widths {
		if widths[i] > maxCol {
			widths[i] = maxCol
		}
	}
	renderRow := func(cells []string, bold bool) {
		var b strings.Builder
		b.WriteString(" ")
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			styled := r.inline(cell)
			if bold {
				styled = "\x1b[1m" + styled + "\x1b[0m"
			}
			b.WriteString(styled)
			b.WriteString(strings.Repeat(" ", max(1, widths[i]-util.VisibleWidth(r.inline(cell)))))
			if i < cols-1 {
				b.WriteString(p.Style("dim", " │ "))
			}
		}
		out = append(out, b.String())
	}
	renderRow(parsed[0], true)
	if len(parsed) > 1 {
		var sep strings.Builder
		sep.WriteString(" ")
		for i := 0; i < cols; i++ {
			sep.WriteString(strings.Repeat("─", widths[i]))
			if i < cols-1 {
				sep.WriteString("─┼─")
			}
		}
		out = append(out, p.Style("dim", sep.String()))
		for _, row := range parsed[1:] {
			renderRow(row, false)
		}
	}
	return out
}

func (r *Renderer) renderTableEntry(rows []string) []string { return r.renderTable(rows) }
