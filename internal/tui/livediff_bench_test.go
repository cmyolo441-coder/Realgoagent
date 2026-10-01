package tui

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/theme"
)

// benchEntry builds a file of the given size with half its lines changed, the
// shape a real edit to a real file has.
func benchEntry(tb testing.TB, lines, changed int) EditEntry {
	tb.Helper()
	var before, after strings.Builder
	for i := range lines {
		if i%2 == 0 && i/2 < changed {
			before.WriteString(fmt.Sprintf("original line %d\n", i))
			after.WriteString(fmt.Sprintf("rewritten line %d\n", i))
			continue
		}
		if i%2 == 0 {
			after.WriteString(fmt.Sprintf("original line %d\n", i))
		} else {
			after.WriteString(fmt.Sprintf("context line %d\n", i))
		}
		before.WriteString(fmt.Sprintf("%s line %d\n", map[bool]string{true: "context", false: "original"}[i%2 == 1], i))
	}
	return EditEntry{
		Tool: "edit", Path: "big.go", Before: before.String(), After: after.String(),
	}
}

// The live panel repaints twenty times a second for as long as it is up, so the
// cost of one of its renders is multiplied by every second the panel stays on
// screen. It reads the recorded script rather than recomputing one.
func BenchmarkLiveDiffRows(b *testing.B) {
	for _, size := range []int{200, 1000, 4000} {
		b.Run(fmt.Sprintf("lines=%d", size), func(b *testing.B) {
			pal := theme.Get("")
			tui := &TUI{liveDiff: newLiveDiffView()}
			entry := benchEntry(b, size, size/4)
			// Recorded once, as editLog.record does.
			entry.Lines = editdiff.Context(entry.Before, entry.After, editdiff.DefaultContext)
			entry.Added, entry.Removed = editdiff.Count(entry.Before, entry.After)
			tui.liveDiff.maxScroll = 0

			b.ResetTimer()
			for range b.N {
				tui.buildLiveDiffRows(pal, 100, maxLiveDiffRows, entry)
			}
		})
	}
}

// What the panel used to do: re-diff the same pair of texts on every repaint.
// Kept as the comparison the caching fix is measured against.
func BenchmarkLiveDiffRowsRecomputingTheDiff(b *testing.B) {
	for _, size := range []int{200, 1000, 4000} {
		b.Run(fmt.Sprintf("lines=%d", size), func(b *testing.B) {
			pal := theme.Get("")
			tui := &TUI{liveDiff: newLiveDiffView()}
			entry := benchEntry(b, size, size/4)
			tui.liveDiff.maxScroll = 0

			b.ResetTimer()
			for range b.N {
				// The pre-fix body: a full LCS on every frame.
				entry.Lines = nil
				tui.buildLiveDiffRows(pal, 100, maxLiveDiffRows, entry)
			}
		})
	}
}

// Recording an edit does the diff once, so the win has to cover the cost of
// that single computation as well as the per-repaint saving.
func BenchmarkEditLogRecord(b *testing.B) {
	for _, size := range []int{200, 1000, 4000} {
		b.Run(fmt.Sprintf("lines=%d", size), func(b *testing.B) {
			entry := benchEntry(b, size, size/4)
			files := []editdiff.FileDiff{{Path: entry.Path, Before: entry.Before, After: entry.After}}
			b.ResetTimer()
			for range b.N {
				l := newEditLog()
				l.record("edit", "1ms", "", false, files)
			}
		})
	}
}

// A whole repaint of the live region with the panel up: the panel is rendered
// once per frame, so this is the number that governs whether typing and
// scrolling stay responsive while a diff is on screen.
func BenchmarkFrameWithLiveDiff(b *testing.B) {
	pal := theme.Get("")
	entry := benchEntry(b, 1000, 250)
	entry.Lines = editdiff.Context(entry.Before, entry.After, editdiff.DefaultContext)
	entry.Added, entry.Removed = editdiff.Count(entry.Before, entry.After)

	b.ResetTimer()
	for range b.N {
		tui := &TUI{liveDiff: newLiveDiffView()}
		tui.liveDiff.maxScroll = 0
		for range 20 {
			tui.buildLiveDiffRows(pal, 100, maxLiveDiffRows, entry)
		}
	}
}

// Streaming a write shows a preview that grows with the content, and it
// re-renders on every frame too.
func BenchmarkLiveStreamRows(b *testing.B) {
	for _, size := range []int{50, 500, 5000} {
		b.Run(fmt.Sprintf("lines=%d", size), func(b *testing.B) {
			pal := theme.Get("")
			tui := &TUI{liveDiff: newLiveDiffView()}
			var content strings.Builder
			for i := range size {
				content.WriteString(fmt.Sprintf("streamed line %d\n", i))
			}
			tui.liveDiff.streaming = true
			tui.liveDiff.streamText = content.String()
			tui.liveDiff.streamPath = "a.go"
			tui.liveDiff.streamTool = "write"

			b.ResetTimer()
			for range b.N {
				tui.buildLiveStreamRows(pal, 100, maxLiveStreamRows)
			}
		})
	}
}

// Committing history reads only the lines since the last print, so a long
// transcript does not make every repaint re-walk the whole buffer.
func BenchmarkCommitHistory(b *testing.B) {
	for _, size := range []int{100, 10000} {
		b.Run(fmt.Sprintf("buffer=%d", size), func(b *testing.B) {
			var sink bytes.Buffer
			tui := &TUI{
				app: &App{history: NewBuffer(10000), edits: newEditLog(),
					subs: newSubLog(), Theme: theme.Get("nova"), Width: 100},
				out:      bufio.NewWriter(&sink),
				liveDiff: newLiveDiffView(),
			}
			// Commit once so the position is current; each iteration then adds
			// the single line a real frame would carry, which is the hot path.
			tui.mu.Lock()
			tui.commitHistoryLocked(100)
			tui.mu.Unlock()

			b.ResetTimer()
			for range b.N {
				tui.app.history.Append("a new line of output")
				tui.mu.Lock()
				tui.commitHistoryLocked(100)
				tui.mu.Unlock()
			}
		})
	}
}
