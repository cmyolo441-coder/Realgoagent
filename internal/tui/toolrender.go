package tui

import (
	"strings"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// Tool output is shown for the user to check what the agent actually did, so
// it has to be readable at a glance rather than a raw dump. These limits keep
// a chatty command from burying the conversation.

// maxToolLines caps how many output lines are shown.
const maxToolLines = 8

// maxToolLineWidth caps any single output line regardless of terminal width;
// compiler and test output has lines thousands of characters long.
const maxToolLineWidth = 200

// toolIndent is the gutter the output sits in, so a wrapped line is obviously
// a continuation.
const toolIndent = 4

// toolCall renders the line announcing a tool call.
// brief is the argument summary and must not repeat the tool name.
func toolCall(p theme.Palette, name, brief string) string {
	line := p.Style("dim", "  ▸ ") + p.Style("tool", name)
	if brief != "" {
		line += "  " + p.Style("text", brief)
	}
	return line
}

// toolResult renders the outcome: a status line, then the output, indented
// under it. Blank runs are collapsed and long output is summarised rather than
// scrolled, so the shape of the result stays visible.
func toolResult(p theme.Palette, name, dur, output string, isErr bool, width int) []string {
	head := "  " + p.Style("success", "✓")
	if isErr {
		head = "  " + p.Style("error", "✗")
	}
	head += " " + p.Style("tool", name)
	if dur != "" {
		head += p.Style("dim", "  "+dur)
	}

	rows := []string{head}
	rows = append(rows, summariseOutput(p, output, isErr, width)...)
	return rows
}

// summariseOutput renders tool output as indented lines, collapsed and capped.
func summariseOutput(p theme.Palette, out string, isErr bool, width int) []string {
	// Truncate to the terminal, not just to a constant: a line wider than the
	// screen wraps, and the wrapped half lands under the next line and makes
	// the whole transcript unreadable.
	limit := width - toolIndent
	if limit > maxToolLineWidth {
		limit = maxToolLineWidth
	}
	if limit < 20 {
		limit = 20
	}

	lines := collapseBlank(strings.Split(strings.TrimRight(out, "\n"), "\n"))
	if len(lines) == 0 {
		return nil
	}

	style := p.Style("dim", "    ")
	if isErr {
		style = p.Style("error", "    ")
	}

	shown := lines
	var more string
	if len(shown) > maxToolLines {
		shown = shown[:maxToolLines]
		more = p.Style("dim", "    … "+itoa(len(lines)-maxToolLines)+" more lines")
	}

	rows := make([]string, 0, len(shown)+1)
	for _, ln := range shown {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			continue
		}
		rows = append(rows, style+util.Truncate(ln, limit))
	}
	if more != "" {
		rows = append(rows, more)
	}
	return rows
}

// collapseBlank drops empty lines and runs of them, so a wall of whitespace
// between two real lines does not become a wall on screen.
func collapseBlank(lines []string) []string {
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			// Keep a single separator, and only if something follows.
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "…")
			blank = false
		}
		out = append(out, ln)
	}
	return out
}
