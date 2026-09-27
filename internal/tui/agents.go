package tui

import (
	"fmt"
	"strings"
	"sync"

	"github.com/nova-ai/nova/internal/subagent"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// The subagent view: `/agents` shows what the delegated work is doing, one row
// per subagent, and the report each one produced. A subagent has no UI of its
// own, so without this the only sign it exists is a pause in the transcript.

// maxSubagentRecords caps the session's subagent history, for the same reason
// the edit log is capped: a long session can delegate a great many jobs, and
// the view is a list the user scrolls, not an archive.
const maxSubagentRecords = 100

// SubRecord is one subagent run.
type SubRecord struct {
	// Agent is the subagent's name.
	Agent string
	// Role is the preset it ran under.
	Role string
	// Task is the instruction it was given.
	Task string
	// State is "running", "done" or "failed".
	State string
	// Steps is the running count of tool calls.
	Steps int
	// Report is the one-line summary shown in the list.
	Report string
	// Reply is the conclusion the parent received.
	Reply string
	// Err is the failure message, when there was one.
	Err string
	// Tokens is what it spent.
	Tokens int
	// Dur is how long it took.
	Dur string
	// Seq numbers the runs, so the newest is known without comparing times.
	Seq int
}

// subLog holds the session's subagent records. It is guarded because events
// arrive on the goroutine that ran the tool while the view reads it from the
// event loop.
type subLog struct {
	mu      sync.Mutex
	records []*SubRecord
	seq     int
}

func newSubLog() *subLog { return &subLog{} }

func (l *subLog) start(name, role, task string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.records = append(l.records, &SubRecord{
		Agent: name,
		Role:  role,
		Task:  task,
		State: "running",
		Seq:   l.seq,
	})
	if n := len(l.records); n > maxSubagentRecords {
		l.records = append([]*SubRecord{}, l.records[n-maxSubagentRecords:]...)
	}
}

func (l *subLog) step(name, text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec := l.findLocked(name); rec != nil && rec.State == "running" {
		rec.Steps++
		rec.Report = text
	}
}

func (l *subLog) done(name, report string, tokens int, dur string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec := l.findLocked(name); rec != nil {
		rec.State = "done"
		rec.Report = report
		rec.Tokens = tokens
		rec.Dur = dur
	}
}

func (l *subLog) fail(name, msg string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec := l.findLocked(name); rec != nil {
		rec.State = "failed"
		rec.Err = msg
	}
}

// findLocked returns the most recent record with the given name. The name is
// not unique across a session — two explore runs both go by "explore" — so
// the newest wins, which is the one the events are about.
func (l *subLog) findLocked(name string) *SubRecord {
	for i := len(l.records) - 1; i >= 0; i-- {
		if l.records[i].Agent == name {
			return l.records[i]
		}
	}
	return nil
}

func (l *subLog) list() []*SubRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*SubRecord{}, l.records...)
}

func (l *subLog) count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

func (l *subLog) clear() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.records = nil
	l.mu.Unlock()
}

// handleEvent folds a subagent event into the log. It runs on the tool's own
// goroutine, so it must not draw; the event loop repaints from the log.
func (l *subLog) handleEvent(ev subagent.Event) {
	switch ev.Kind {
	case subagent.EvSubStart:
		l.start(ev.Agent, roleOf(ev.Text), "")
	case subagent.EvSubTool:
		l.step(ev.Agent, ev.Text)
	case subagent.EvSubDone:
		l.done(ev.Agent, ev.Text, 0, "")
	case subagent.EvSubFail:
		l.fail(ev.Agent, ev.Text)
	}
}

// roleOf recovers the role from a start event's text ("<name> · <model>"),
// which is the only place the name and the model are joined.
func roleOf(text string) string {
	if i := strings.IndexByte(text, '·'); i > 0 {
		return strings.TrimSpace(text[:i])
	}
	return ""
}

// subView is the open overlay.
type subView struct {
	sel    int
	offset int
}

func (t *TUI) subViewOpen() bool { return t.agents != nil }

func (t *TUI) closeSubView() { t.agents = nil }

// subViewKey handles a key while the view is open. Called with t.mu held.
func (t *TUI) subViewKey(k string) bool {
	recs := t.app.subs.list()
	if len(recs) == 0 {
		t.closeSubView()
		return true
	}
	v := t.agents
	switch k {
	case "up":
		v.sel--
	case "down":
		v.sel++
	case "pageup":
		v.sel -= palettePage
	case "pagedown":
		v.sel += palettePage
	case "home":
		v.sel = 0
	case "end":
		v.sel = len(recs) - 1
	case "esc":
		t.closeSubView()
	default:
		return false
	}
	if v.sel < 0 {
		v.sel = 0
	}
	if v.sel >= len(recs) {
		v.sel = len(recs) - 1
	}
	if v.sel < v.offset {
		v.offset = v.sel
	}
	return true
}

// subViewRows renders the overlay above the prompt box.
func (t *TUI) subViewRows(pal theme.Palette, w, budget int) []string {
	if !t.subViewOpen() || budget < 3 {
		return nil
	}
	recs := t.app.subs.list()
	if len(recs) == 0 {
		return []string{pal.Style("dim", " no subagents have run this session")}
	}
	v := t.agents
	visible := budget - 1
	if visible < 1 {
		visible = 1
	}
	if visible > len(recs) {
		visible = len(recs)
	}
	if v.sel < v.offset {
		v.offset = v.sel
	}
	if v.sel >= v.offset+visible {
		v.offset = v.sel - visible + 1
	}
	if v.offset+visible > len(recs) {
		v.offset = len(recs) - visible
	}
	if v.offset < 0 {
		v.offset = 0
	}

	more := ""
	if v.offset > 0 {
		more = " ↑" + itoa(v.offset)
	}
	if v.offset+visible < len(recs) {
		more = " ↓" + itoa(len(recs)-v.offset-visible) + more
	}
	head := util.Truncate(" subagents — ↑↓ move · esc close", w-util.VisibleWidth(more))
	out := []string{pal.Style("dim", head) + pal.Style("dim", more)}
	for i := v.offset; i < v.offset+visible; i++ {
		out = append(out, util.Truncate(subRow(pal, recs[i], i == v.sel), w))
	}
	return out
}

// subRow renders one subagent line: its state, name, and what it is doing.
func subRow(pal theme.Palette, r *SubRecord, sel bool) string {
	marker := "  "
	style := pal.Style("text", r.Agent)
	if sel {
		marker = pal.Style("accent", "❯ ")
		style = pal.Style("accent_bold", r.Agent)
	}
	var state string
	switch r.State {
	case "done":
		state = pal.Style("success", "✓")
	case "failed":
		state = pal.Style("error", "✗")
	default:
		state = pal.Style("accent", spinFrame())
	}
	line := marker + state + " " + style
	if r.Role != "" && r.Role != r.Agent {
		line += pal.Style("dim", "  "+r.Role)
	}
	if r.Steps > 0 {
		line += pal.Style("dim", "  "+itoa(r.Steps)+" calls")
	}
	if r.Tokens > 0 {
		line += pal.Style("dim", "  "+humanTokens(r.Tokens)+" tok")
	}
	if r.State == "failed" && r.Err != "" {
		line += pal.Style("error", "  "+truncMiddle(r.Err, 48))
	} else if r.Report != "" {
		line += pal.Style("dim", "  "+truncMiddle(r.Report, 48))
	}
	return line
}

// describeRoleLine is the one-line summary shown in /help-adjacent output.
func describeRoleLine(name, desc string) string {
	return fmt.Sprintf("  %-8s %s", name, desc)
}
