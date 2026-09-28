// Package editdiff computes unified diffs for the files the agent changes,
// so the user can watch every write land as it happens and read exactly what
// it did.
//
// The algorithm is a plain LCS over the lines that actually differ, after the
// common prefix and suffix have been trimmed. That trim is what keeps it
// cheap: a one-line typo leaves a 1x1 table, not a table the size of the file.
package editdiff

import (
	"fmt"
	"strings"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// Op classifies one line of a diff.
type Op byte

const (
	// OpEqual is a context line, present before and after.
	OpEqual Op = ' '
	// OpAdd is a line present only in the new text.
	OpAdd Op = '+'
	// OpDel is a line present only in the old text.
	OpDel Op = '-'
)

// Line is one row of a change script.
type Line struct {
	Op   Op
	Text string
	// Old and New are 1-based line numbers in the before and after texts.
	// A line that exists in only one of them carries 0 for the other.
	Old, New int
}

// FileDiff holds one file's text either side of an edit.
type FileDiff struct {
	Path   string
	Before string
	After  string
}

// Stats counts changed lines.
type Stats struct {
	Added   int
	Removed int
}

// Diff is a completed edit: which files moved, and how.
type Diff struct {
	Tool  string
	Files []FileDiff
	Dur   string
	Err   bool
	Note  string
}

// Stat is the added/removed count for one file.
func (d FileDiff) Stat() Stats {
	a, r := Count(d.Before, d.After)
	return Stats{Added: a, Removed: r}
}

// lcsCeiling bounds each side of the LCS table. Past it the alignment costs
// more memory than it is worth and the honest answer is "this whole region
// changed", which is what the caller gets instead.
const lcsCeiling = 4000

// DefaultContext is how many unchanged lines a hunk carries either side of a
// change. Three is the usual convention and keeps a hunk readable at any
// terminal width.
const DefaultContext = 3

// Compute returns the complete change script turning before into after.
//
// The unchanged parts of the file are present as OpEqual lines, so this is
// the honest description of the edit; the context-trimmed form meant for
// display is Context.
func Compute(before, after string) []Line {
	return script(before, after)
}

// Count reports how many lines an edit adds and removes. It is the count a
// reviewer cares about, so it is computed without rendering any context.
func Count(before, after string) (added, removed int) {
	for _, l := range script(before, after) {
		switch l.Op {
		case OpAdd:
			added++
		case OpDel:
			removed++
		}
	}
	return
}

// Context returns the change script with unchanged lines kept only near a
// change, and the rest replaced by a single gap marker per run.
func Context(before, after string, context int) []Line {
	if context < 0 {
		context = 0
	}
	full := script(before, after)
	changed := false
	for _, l := range full {
		if l.Op != OpEqual {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}

	keep := make([]bool, len(full))
	for i, l := range full {
		if l.Op == OpEqual {
			continue
		}
		lo, hi := i-context, i+context
		if lo < 0 {
			lo = 0
		}
		if hi > len(full)-1 {
			hi = len(full) - 1
		}
		for j := lo; j <= hi; j++ {
			keep[j] = true
		}
	}

	out := make([]Line, 0, len(full))
	for i, l := range full {
		if keep[i] || l.Op != OpEqual {
			out = append(out, l)
			continue
		}
		// A gap only exists if something was dropped, so the marker goes on
		// the first skipped line of each run.
		if n := len(out); n > 0 && out[n-1].Op == OpEqual && out[n-1].Text == gapMarker {
			continue
		}
		out = append(out, Line{Op: OpEqual, Text: gapMarker, Old: l.Old, New: l.New})
	}
	return out
}

// script builds the full change script.
func script(before, after string) []Line {
	oldLines, newLines := splitLines(before), splitLines(after)

	// Trim the shared prefix and suffix before doing any work: an identical
	// region can never hold a change, so it is not worth a table cell. The
	// trimmed middle is what actually needs aligning.
	head := 0
	for head < len(oldLines) && head < len(newLines) && oldLines[head] == newLines[head] {
		head++
	}
	tailOld, tailNew := len(oldLines), len(newLines)
	for tailOld > head && tailNew > head && oldLines[tailOld-1] == newLines[tailNew-1] {
		tailOld--
		tailNew--
	}

	// Splice the aligned middle back between the shared head and the shared
	// tail. The tail is numbered separately in each text: the middle may have
	// added or removed lines, so equal offsets stop lining up after it.
	out := make([]Line, 0, len(oldLines)+len(newLines))
	for i := 0; i < head; i++ {
		out = append(out, Line{Op: OpEqual, Text: oldLines[i], Old: i + 1, New: i + 1})
	}
	// align numbers from 1, because the numbers it reports are the 1-based
	// lines a human sees in their editor.
	out = append(out, align(oldLines[head:tailOld], newLines[head:tailNew], head+1, head+1)...)
	tailShift := tailNew - tailOld
	for i := tailOld; i < len(oldLines); i++ {
		out = append(out, Line{Op: OpEqual, Text: oldLines[i], Old: i + 1, New: i + 1 + tailShift})
	}
	return out
}

// gapMarker stands in for elided unchanged lines.
const gapMarker = "⋯"

// mk numbers a run of lines of one kind, starting at the given 1-based
// position in whichever text they belong to.
func mk(op Op, lines []string, oldAt, newAt int) []Line {
	out := make([]Line, len(lines))
	o, n := oldAt, newAt
	for i, l := range lines {
		switch op {
		case OpDel:
			out[i] = Line{Op: OpDel, Text: l, Old: o}
			o++
		case OpAdd:
			out[i] = Line{Op: OpAdd, Text: l, New: n}
			n++
		default:
			out[i] = Line{Op: OpEqual, Text: l, Old: o, New: n}
			o++
			n++
		}
	}
	return out
}

// align turns two differing line runs into a change script via LCS. oldAt and
// newAt are 1-based line numbers where the runs start in the full texts.
func align(old, new []string, oldAt, newAt int) []Line {
	m, n := len(old), len(new)
	switch {
	case m == 0 && n == 0:
		return nil
	case m == 0:
		return mk(OpAdd, new, oldAt, newAt)
	case n == 0:
		return mk(OpDel, old, oldAt, newAt)
	case m > lcsCeiling || n > lcsCeiling:
		// Too large to align honestly. Report the old lines as gone and the
		// new lines as new: at this scale that is what happened.
		return append(mk(OpDel, old, oldAt, newAt), mk(OpAdd, new, oldAt, newAt)...)
	}

	// lcs[i][j] is the length of the longest common subsequence of old[i:]
	// and new[j:].
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if old[i] == new[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	script := make([]Line, 0, m+n)
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case old[i] == new[j]:
			script = append(script, Line{Op: OpEqual, Text: old[i], Old: oldAt + i, New: newAt + j})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			script = append(script, Line{Op: OpDel, Text: old[i], Old: oldAt + i})
			i++
		default:
			script = append(script, Line{Op: OpAdd, Text: new[j], New: newAt + j})
			j++
		}
	}
	script = append(script, mk(OpDel, old[i:], oldAt+i, newAt)...)
	script = append(script, mk(OpAdd, new[j:], oldAt+m, newAt+j)...)
	return script
}

// splitLines splits into lines, dropping the empty element a trailing newline
// produces so "a\n" and "a" compare equal. CRLF endings are normalised first
// so Windows-style text does not carry a stray \r on every line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	// A lone "\n" leaves a single empty line behind; treat it as zero lines
	// so it compares equal to "" — a file holding nothing is nothing.
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// Unified renders a standard unified diff for one file, which is what a user
// pastes into a bug report or a review comment. context <= 0 means three.
func Unified(path, before, after string, context int) string {
	if context <= 0 {
		context = DefaultContext
	}
	lines := Context(before, after, context)
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	// Each hunk is buffered before it is written, because its header carries
	// line ranges that are only known once the whole hunk has been read.
	body := make([]string, 0, len(lines))
	oldCount, newCount := 0, 0
	// hunkOldStart and hunkNewStart are the 1-based line numbers the hunk
	// ranges start at. Zero means "not yet seen a line from that side", which
	// is how a pure insertion (no old line) or pure deletion (no new line)
	// is detected once the hunk is complete.
	var hunkOldStart, hunkNewStart int
	// open reports whether a hunk is being accumulated. A gap marker sets it
	// false, because an elided region ends the hunk.
	open := false
	flush := func() {
		if !open {
			return
		}
		oldStart, newStart := hunkOldStart, hunkNewStart
		if oldStart == 0 {
			// A hunk with no old line is a pure insertion: its old range is
			// empty and the start is the line before which the new lines
			// land, the convention git uses as well.
			oldStart = newStart - 1
		}
		if newStart == 0 {
			// A hunk with no new line is a pure deletion: its new range is
			// empty and the start is the line before which the old lines
			// were removed.
			newStart = oldStart - 1
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", rangeSpec(oldStart, oldCount), rangeSpec(newStart, newCount))
		for _, l := range body {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		body = body[:0]
		open = false
	}
	for _, l := range lines {
		if l.Op == OpEqual && l.Text == gapMarker {
			flush()
			continue
		}
		if !open {
			open = true
			oldCount, newCount = 0, 0
			hunkOldStart, hunkNewStart = 0, 0
		}
		switch l.Op {
		case OpEqual:
			if hunkOldStart == 0 {
				hunkOldStart = l.Old
			}
			if hunkNewStart == 0 {
				hunkNewStart = l.New
			}
			oldCount++
			newCount++
		case OpDel:
			if hunkOldStart == 0 {
				hunkOldStart = l.Old
			}
			oldCount++
		case OpAdd:
			if hunkNewStart == 0 {
				hunkNewStart = l.New
			}
			newCount++
		}
		body = append(body, string(l.Op)+l.Text)
	}
	flush()
	return b.String()
}

// rangeSpec formats a hunk range. A zero count is written as "start,0" the
// way git writes it, because that is what every diff reader expects to parse.
func rangeSpec(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// RenderBody colours a raw unified diff for the screen.
func RenderBody(pal theme.Palette, body string, width int) []string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return nil
	}
	rows := strings.Split(body, "\n")
	out := make([]string, 0, len(rows))
	for _, ln := range rows {
		switch {
		case strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"):
			out = append(out, pal.Style("tool", util.Truncate(ln, width)))
		case strings.HasPrefix(ln, "@@"):
			out = append(out, pal.Style("accent2", util.Truncate(ln, width)))
		case strings.HasPrefix(ln, "+"):
			out = append(out, pal.Style("success", util.Truncate(ln, width)))
		case strings.HasPrefix(ln, "-"):
			out = append(out, pal.Style("error", util.Truncate(ln, width)))
		default:
			out = append(out, pal.Style("dim", util.Truncate(ln, width)))
		}
	}
	return out
}
