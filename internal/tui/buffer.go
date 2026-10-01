package tui

import (
	"strings"
	"sync"
)

// Buffer is a scrollback of rendered lines.
//
// It is capped, and the cap drops lines off the *front* as new ones arrive, so
// a position held by a reader is not stable: index 5000 is a different line
// after the next append. Lines therefore carry an absolute index — the count
// of everything ever appended — and readers work in those terms through
// Since. A reader that tracked its position as a plain slice index would, once
// the cap was reached, sit permanently at len(lines) and never see another
// line again.
type Buffer struct {
	mu sync.Mutex
	// lines is the retained window, oldest first.
	lines []string
	max   int
	// total counts every line ever appended, including those since dropped,
	// so absolute indices keep rising across Clear and across eviction.
	total int
}

// NewBuffer returns a buffer capped at max lines.
func NewBuffer(max int) *Buffer {
	if max <= 0 {
		max = 10000
	}
	return &Buffer{max: max}
}

// Append adds one or more lines.
func (b *Buffer) Append(lines ...string) {
	b.lines = append(b.lines, lines...)
	b.total += len(lines)
	if len(b.lines) > b.max {
		b.lines = append([]string{}, b.lines[len(b.lines)-b.max:]...)
	}
}

// AppendText splits a block into rendered lines and appends them.
func (b *Buffer) AppendText(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s = strings.TrimRight(s, "\n")
	added := strings.Split(s, "\n")
	b.lines = append(b.lines, added...)
	b.total += len(added)
	if len(b.lines) > b.max {
		b.lines = append([]string{}, b.lines[len(b.lines)-b.max:]...)
	}
}

// Lines returns a copy of the retained lines, oldest first.
//
// The copy is not incidental. Returning the live slice would hand the caller
// a view of memory the next Append is free to reallocate or overwrite, so a
// reader iterating it while a tool records an edit is a data race that
// -race reports and that, unchecked, corrupts a transcript mid-print.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

// Len returns the number of retained lines.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}

// Total returns how many lines have ever been appended, including those since
// dropped from the window. A reader uses it to place its position after a
// Clear, which empties the window without rewinding the count.
func (b *Buffer) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// Since returns the lines whose absolute index is at or after from, together
// with the absolute index of the first line returned.
//
// This is how a consumer follows the tail of an append-only log across a
// sliding window: `from` keeps rising, so reaching the cap does not stop the
// flow. A caller that tracked its position as an index into Lines() would
// instead sit at len(lines) forever once the cap was hit and never see
// another line. Lines already evicted are gone from the buffer, so a reader
// that falls that far behind skips them rather than blocking; base tells it
// where the window begins.
func (b *Buffer) Since(from int) (lines []string, base int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	base = b.total - len(b.lines)
	if from < base {
		from = base
	}
	if from >= b.total {
		return nil, base
	}
	out := make([]string, b.total-from)
	copy(out, b.lines[from-base:])
	return out, base
}

// Clear empties the retained lines. Absolute indices do not restart: total
// keeps counting, so a reader's position stays meaningful across a clear and
// it does not mistake the post-clear emptiness for output it has not printed.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total -= len(b.lines)
	b.lines = nil
}

// Tail returns a copy of the last n lines.
func (b *Buffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n >= len(b.lines) {
		return append([]string(nil), b.lines...)
	}
	return append([]string(nil), b.lines[len(b.lines)-n:]...)
}
