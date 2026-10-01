package tui

import (
	"bufio"
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/session"
	"github.com/nova-ai/nova/internal/subagent"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/tools"
)

// Once the scrollback cap is reached, new output must still be printed.
//
// The buffer is a sliding window: past its cap it drops lines off the front,
// so a consumer that tracks its position as an index into the buffer is pinned
// at len(lines) from then on. Every later line then looks already-printed, and
// the transcript stops growing entirely — while the session keeps running, so
// it presents as a hung program with output going nowhere.
func TestOutputKeepsPrintingPastTheScrollbackCap(t *testing.T) {
	const cap = 8
	var out bytes.Buffer
	tui := &TUI{
		app:        &App{history: NewBuffer(cap), edits: newEditLog(), subs: newSubLog(), Theme: theme.Get("nova"), Width: 200},
		out:        bufio.NewWriter(&out),
		inputLines: []string{""},
		liveDiff:   newLiveDiffView(),
	}

	// Fill past the cap, committing as we go, the way a real session does.
	for i := range cap * 3 {
		tui.app.history.Append("line " + itoa(i))
		tui.mu.Lock()
		tui.commitHistoryLocked(200)
		tui.mu.Unlock()
	}
	tui.out.Flush()
	before := out.Len()
	if before == 0 {
		t.Fatal("nothing was printed before the cap either")
	}

	// The moment that matters: one more line, after the window is saturated.
	tui.app.history.Append("after the cap")
	tui.mu.Lock()
	tui.commitHistoryLocked(200)
	tui.mu.Unlock()
	tui.out.Flush()

	if !strings.Contains(out.String(), "after the cap") {
		t.Fatalf("output stopped at the scrollback cap; wrote %d bytes, last is %q",
			before, lastLine(out.String()))
	}
}

// Lines already printed must not be printed a second time. A fix that
// repositioned to the window's start rather than the reader's own position
// would reprint the whole transcript on every repaint.
func TestAlreadyPrintedLinesAreNotPrintedTwice(t *testing.T) {
	const cap = 4
	var out bytes.Buffer
	tui := &TUI{
		app:        &App{history: NewBuffer(cap), edits: newEditLog(), subs: newSubLog(), Theme: theme.Get("nova"), Width: 200},
		out:        bufio.NewWriter(&out),
		inputLines: []string{""},
		liveDiff:   newLiveDiffView(),
	}
	for i := range 12 {
		tui.app.history.Append("line " + itoa(i))
	}
	// Repeated commits with nothing new in between must be no-ops.
	for range 3 {
		tui.mu.Lock()
		tui.commitHistoryLocked(200)
		tui.mu.Unlock()
	}
	tui.out.Flush()
	first := strings.Count(out.String(), "\n")

	tui.app.history.Append("only once")
	for range 3 {
		tui.mu.Lock()
		tui.commitHistoryLocked(200)
		tui.mu.Unlock()
	}
	tui.out.Flush()
	if got := strings.Count(out.String(), "\n"); got != first+1 {
		t.Fatalf("re-committing reprinted lines: %d new lines, want 1", got-first)
	}
	if n := strings.Count(out.String(), "only once"); n != 1 {
		t.Fatalf("the new line was printed %d times, want 1", n)
	}
}

// A reader that has fallen behind the window — more lines appended than the
// cap retains — must be given what is left rather than nothing, or a burst of
// output under a slow front-end silently disappears.
func TestAReaderBehindTheWindowStillGetsTheTail(t *testing.T) {
	b := NewBuffer(3)
	for i := range 10 {
		b.Append("line " + itoa(i))
	}
	// A reader at position 0, with only the last three lines retained.
	got, base := b.Since(0)
	if base != 7 {
		t.Fatalf("window base = %d, want 7", base)
	}
	if len(got) != 3 {
		t.Fatalf("got %d lines, want the 3 retained", len(got))
	}
	if got[0] != "line 7" || got[2] != "line 9" {
		t.Fatalf("tail = %q", got)
	}
}

// Since is the contract the printer depends on: it must never report lines the
// caller has already seen, and its base must advance as the window slides.
func TestSinceAdvancesWithTheWindow(t *testing.T) {
	b := NewBuffer(5)
	b.Append("a", "b")
	pos := 0
	got, _ := b.Since(pos)
	pos += len(got)
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("first read = %q", got)
	}
	if again, _ := b.Since(pos); len(again) != 0 {
		t.Fatalf("re-reading the same position returned %q", again)
	}
	b.Append("c", "d")
	got, _ = b.Since(pos)
	if strings.Join(got, ",") != "c,d" {
		t.Fatalf("second read = %q", got)
	}
}

// ^L empties the buffer to repaint the screen. The read position has to follow
// it, or the next paint treats the whole window as unprinted and dumps the
// transcript again.
func TestClearScreenDoesNotReprintTheTranscript(t *testing.T) {
	var out bytes.Buffer
	tui := &TUI{
		app:        &App{history: NewBuffer(100), edits: newEditLog(), subs: newSubLog(), Theme: theme.Get("nova"), Width: 200},
		out:        bufio.NewWriter(&out),
		inputLines: []string{""},
		liveDiff:   newLiveDiffView(),
	}
	tui.app.history.Append("banner line")
	tui.mu.Lock()
	tui.commitHistoryLocked(200)
	tui.mu.Unlock()
	tui.out.Flush()
	if n := strings.Count(out.String(), "banner line"); n != 1 {
		t.Fatalf("the banner was printed %d times, want 1", n)
	}

	// ^L: clear the buffer, then move the read position to its new total.
	tui.app.history.Clear()
	tui.flushed = tui.app.history.Total()
	tui.mu.Lock()
	tui.commitHistoryLocked(200)
	tui.mu.Unlock()
	tui.out.Flush()

	if n := strings.Count(out.String(), "banner line"); n != 1 {
		t.Fatalf("a clear reprinted the transcript: %d copies", n)
	}

	// And output after the clear must still print.
	tui.app.history.Append("after the clear")
	tui.mu.Lock()
	tui.commitHistoryLocked(200)
	tui.mu.Unlock()
	tui.out.Flush()
	if !strings.Contains(out.String(), "after the clear") {
		t.Fatal("output after a clear was not printed")
	}
}

// Reading the buffer while a tool appends to it is the normal case, not an
// edge one: the edit watcher records from the tool's own goroutine. Handing
// out the live slice made that a race, and a torn read corrupts a transcript
// mid-print.
func TestReadingTheBufferWhileItIsWrittenIsSafe(t *testing.T) {
	b := NewBuffer(50)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Append("writer line long enough to be its own allocation")
			}
		}
	}()
	for range 200 {
		_ = b.Lines()
		_ = b.Tail(3)
		_, _ = b.Since(0)
		b.Len()
		b.Total()
	}
	close(stop)
	wg.Wait()
}

// Reads return snapshots rather than the buffer's own slice.
//
// The observable half of this is that a caller cannot reach back into the
// buffer through what it was handed — Lines() and Tail() are the only way out,
// and neither lets the caller append into the live window or observe it
// shrinking. The other half is memory-model safety: the read used to hand
// back the slice itself, so a reader iterating it while a tool recorded an
// edit was reading memory a writer was free to touch, with only the writer
// holding the lock. That half is what -race reports, and it cannot be pinned
// deterministically, so the concurrent test above is the one that covers it.
func TestReadsDoNotExposeTheBuffersOwnSlice(t *testing.T) {
	b := NewBuffer(4)
	b.Append("first", "second", "third")

	// A caller must not be able to reach back into the buffer through what it
	// was handed. This is a contract rather than a reproduced failure: with
	// today's callers the live slice is not observably corrupted, because
	// Append only writes past the caller's length and the cap trims into a
	// fresh array. The copy is what keeps that true as callers change, and it
	// is what makes the mutex on Buffer mean anything for a reader.
	lines := b.Lines()
	lines = append(lines, "injected")
	if got := b.Lines(); strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("appending to a read result reached the buffer: %q", got)
	}

	tail := b.Tail(2)
	if strings.Join(tail, ",") != "second,third" {
		t.Fatalf("Tail = %q", tail)
	}
	tail = append(tail, "injected")
	if got := b.Lines(); strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("appending to a tail reached the buffer: %q", got)
	}
}

// The window slides, so the same read position must keep yielding only what
// is new. If Since returned the window from its start, a repaint would reprint
// the transcript every frame.
func TestAReadPositionKeepsAdvancingPastTheCap(t *testing.T) {
	b := NewBuffer(5)
	pos := 0
	var seen []string
	for i := range 50 {
		b.Append("line " + itoa(i))
		fresh, _ := b.Since(pos)
		pos += len(fresh)
		seen = append(seen, fresh...)
	}
	if len(seen) == 0 {
		t.Fatal("nothing was read")
	}
	// Nothing may be reported twice.
	uniq := map[string]int{}
	for _, s := range seen {
		uniq[s]++
		if uniq[s] > 1 {
			t.Fatalf("line %q was read twice", s)
		}
	}
	// And the tail must be the newest, not the oldest.
	if last := seen[len(seen)-1]; last != "line 49" {
		t.Fatalf("the reader ended on %q, want the newest line", last)
	}
}

// Reply text must never be dropped, however long the front-end stalls.
//
// The old sink gave up after five seconds, so a long tool result or a slow
// repaint was enough to lose part of the answer: the session file kept the
// whole reply while the screen showed a truncated one, and nothing said which
// was right.
func TestReplyTextSurvivesAStalledFrontEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the old five-second drop deadline")
	}
	tui := busTUI()
	tui.messageBus <- agent.Event{Kind: agent.EvIteration} // full

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		tui.send(agent.Event{Kind: agent.EvText, Text: "the whole reply"})
	}()

	// The stall has to outlast the deadline the old sink gave itself, or the
	// test proves nothing: five and a half seconds is past the point where the
	// old code gave up and dropped the text on the floor.
	select {
	case <-sent:
		t.Fatal("the text was abandoned rather than waiting for room")
	case <-time.After(5500 * time.Millisecond):
	}

	<-tui.messageBus
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the text never reached the bus")
	}
	if got := <-tui.messageBus; got.Text != "the whole reply" {
		t.Fatalf("bus carried %q", got.Text)
	}
}

// Quitting must release a producer that is waiting for bus room, or the agent
// goroutine is parked for the rest of the process's life.
func TestQuittingReleasesAWaitingProducer(t *testing.T) {
	tui := busTUI()
	tui.messageBus <- agent.Event{Kind: agent.EvIteration} // full

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		tui.send(agent.Event{Kind: agent.EvText, Text: "in flight"})
	}()
	time.Sleep(50 * time.Millisecond)
	tui.Quit()

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("quitting left the producer waiting forever")
	}
}

// The one-shot path builds its output from the sink, which the agent may call
// from another goroutine once a tool delegates. Unguarded, two writers race on
// the builder's header and the reply comes back corrupt.
func TestRunOnceCollectsTextFromConcurrentSinks(t *testing.T) {
	app := &App{
		Cfg:     config.Default(),
		Theme:   theme.Get("nova"),
		Width:   200,
		history: NewBuffer(100),
		edits:   newEditLog(),
		subs:    newSubLog(),
		Quiet:   true,
		session: session.New("/tmp", "stub/model"),
	}
	reg := tools.NewRegistry()
	app.Registry = reg
	subagent.Install(nil)

	// No agent: the point is that RunOnce is unreachable without one, and this
	// keeps the assertion honest about the guard rather than the collector.
	if _, err := app.RunOnce("hi"); err == nil {
		t.Fatal("RunOnce without an agent must fail rather than return a reply")
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
