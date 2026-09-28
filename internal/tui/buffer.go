package tui

import (
	"strings"
	"sync"
)

// Buffer is a scrollback of rendered lines.
type Buffer struct {
	mu    sync.Mutex
	lines []string
	max   int
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, lines...)
	if len(b.lines) > b.max {
		b.lines = append([]string{}, b.lines[len(b.lines)-b.max:]...)
	}
}

// AppendText splits a block into rendered lines and appends them.
func (b *Buffer) AppendText(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s = strings.TrimRight(s, "\n")
	b.lines = append(b.lines, strings.Split(s, "\n")...)
	if len(b.lines) > b.max {
		b.lines = append([]string{}, b.lines[len(b.lines)-b.max:]...)
	}
}

// Lines returns the current lines.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lines
}

// Len returns the number of buffered lines.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}

// Clear empties the buffer.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = nil
}

// Tail returns the last n lines.
func (b *Buffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n >= len(b.lines) {
		return b.lines
	}
	return b.lines[len(b.lines)-n:]
}
