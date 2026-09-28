package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// maxLiveDiffRows caps the recorded-diff panel height. Without this the panel
// grows to fill every free row on the screen, which on a large terminal
// means the diff buries the conversation and the prompt box.
const maxLiveDiffRows = 8

// maxLiveStreamRows caps the streaming-write preview. It is taller than the
// recorded diff because this is the frame the user watches line by line: a
// 10-line write should be visible whole, not paged behind a scroll hint.
const maxLiveStreamRows = 14

// maxLiveDiffLineLen caps how much of a single diff line is shown. Long
// lines wrap and push the panel taller than its budget, so they are
// truncated with an ellipsis.
const maxLiveDiffLineLen = 80

// liveDiffFreshFor is how long the "new edit" marker stays on screen. It has
// to expire: drawn unconditionally it is on screen for the rest of the
// session, where it reads as decoration rather than as news.
const liveDiffFreshFor = 2 * time.Second

// liveDiffView is a real-time diff panel that automatically shows the most
// recent edit's diff as it happens. No toggling, no commands — whenever the
// agent writes or edits a file, the diff appears immediately and stays
// visible until the next edit replaces it or the user dismisses it.
//
// Green lines for additions, red for deletions, with the file path and
// stats in the header. The user can scroll through the diff with arrow keys.
type liveDiffView struct {
	// scroll is the scroll offset within the diff body.
	scroll int
	// maxScroll is the largest offset that still shows content, recomputed on
	// every render. handleKey consults it so the scroll keys are inert when
	// the whole diff already fits.
	maxScroll int
	// lastSeq is the seq of the last edit we rendered, so we know when a new
	// edit has arrived and the panel needs to refresh.
	lastSeq int
	// freshUntil is when the "new edit" marker stops being drawn.
	freshUntil time.Time
	// dismissed is set by Esc. Without it the panel is up for the rest of the
	// session and keeps first refusal on the navigation keys.
	dismissed bool
	// streaming holds the write still being produced by the model: the tool
	// name, target path and the content accumulated so far. It is set from
	// EvToolProgress and cleared on EvToolResult/EvEdit/EvDone, so the panel
	// shows lines the moment they stream in, then hands over to the real
	// recorded diff once the tool has run.
	streamTool string
	streamPath string
	streamText string
	streamID   string
	streaming  bool
}

func newLiveDiffView() *liveDiffView {
	return &liveDiffView{}
}

// handleKey processes a key press while the live diff panel is showing.
// Returns true if the key was consumed.
//
// Only navigation keys are claimed. `j`, `k`, `g` and `G` are ordinary
// typing, and since the panel is up for the rest of the session after the
// first edit, binding them made those letters impossible to type at all.
func (v *liveDiffView) handleKey(k string) bool {
	step := 0
	switch k {
	case "up":
		step = -1
	case "down":
		step = 1
	case "pageup":
		step = -5
	case "pagedown":
		step = 5
	case "home":
		v.scroll = 0
		return true
	default:
		return false
	}
	// Nothing to scroll: leave the key for the composer, which uses the arrow
	// keys to move between the lines of a multi-line prompt.
	if v.maxScroll == 0 {
		return false
	}
	v.scroll += step
	if v.scroll > v.maxScroll {
		v.scroll = v.maxScroll
	}
	if v.scroll < 0 {
		v.scroll = 0
	}
	return true
}

// close dismisses the panel. The next edit brings it back.
func (v *liveDiffView) close() {
	v.dismissed = true
	v.scroll = 0
	v.streaming = false
	v.streamText = ""
}

// liveDiffRows renders the live diff panel into the given budget of rows.
// Returns nil if there is nothing to show. A write still streaming from the
// model wins over the last recorded edit: that is the frame that grows line
// by line while the 10-line content is still arriving.
func (t *TUI) liveDiffRows(pal theme.Palette, w, budget int) []string {
	v := t.liveDiff
	if v == nil {
		return nil
	}
	cap := maxLiveDiffRows
	if v.streaming && !v.dismissed {
		cap = maxLiveStreamRows
	}
	// Cap the panel to a fixed maximum, regardless of available space.
	if budget > cap {
		budget = cap
	}
	if budget < 2 {
		return nil
	}
	if v.streaming {
		if v.dismissed {
			return nil
		}
		return t.buildLiveStreamRows(pal, w, budget)
	}

	entries := t.editLog().list()
	if len(entries) == 0 {
		return nil
	}

	// Always show the most recent edit
	latest := entries[len(entries)-1]

	if latest.Seq != v.lastSeq {
		v.lastSeq = latest.Seq
		v.scroll = 0
		v.maxScroll = 0
		v.dismissed = false
		v.freshUntil = time.Now().Add(liveDiffFreshFor)
	}
	if v.dismissed {
		return nil
	}

	return t.buildLiveDiffRows(pal, w, budget, latest)
}

// buildLiveDiffRows constructs the diff display for the latest edit.
func (t *TUI) buildLiveDiffRows(pal theme.Palette, w, budget int, e EditEntry) []string {
	v := t.liveDiff
	tool := e.Tool
	if tool == "" {
		tool = "edit"
	}

	// Header: file path, tool, stats
	header := pal.Style("accent_bold", " LIVE DIFF ")
	header += pal.Style("text", e.Path)
	header += pal.Style("dim", "  "+tool)
	header += statSuffix(pal, e.Stat())
	if e.Created() {
		header += pal.Style("success", "  new")
	}
	if e.IsErr {
		header += pal.Style("error", "  failed")
	}

	out := []string{header}

	// Build the diff body
	diffLines := editdiff.Context(e.Before, e.After, editdiff.DefaultContext)
	if len(diffLines) == 0 {
		return append(out, pal.Style("dim", "  (no line-level change)"))
	}

	// The marker is chrome, and it only earns its row when the body still has
	// one left to show.
	fresh := v.scroll == 0 && time.Now().Before(v.freshUntil)
	body := budget - len(out)
	if fresh && body > 2 {
		fresh, body = true, body-1
	} else {
		fresh = false
	}
	// The "… N more" row is reserved before the body is sized. Sizing the body
	// first and adding the indicator afterwards is how the panel came to
	// overrun its budget by two rows: that pushed the live region past the
	// last row of the screen, so every repaint scrolled the panel into
	// scrollback instead of repainting it in place.
	more := len(diffLines) > body
	if more {
		body--
	}
	if body < 1 {
		// Too small for a body line. The header alone still says what changed.
		return out
	}

	// Clamp scroll against the body we are actually about to draw, and record
	// the limit so handleKey knows when the keys have nothing to do.
	v.maxScroll = len(diffLines) - body
	if v.maxScroll < 0 {
		v.maxScroll = 0
	}
	if v.scroll > v.maxScroll {
		v.scroll = v.maxScroll
	}
	if v.scroll < 0 {
		v.scroll = 0
	}

	if fresh {
		out = append(out, pal.Style("dim", "  ── new edit ──"))
	}

	// Render diff lines
	end := v.scroll + body
	if end > len(diffLines) {
		end = len(diffLines)
	}

	for i := v.scroll; i < end; i++ {
		out = append(out, formatLiveDiffRow(diffLines[i], w))
	}

	if more {
		out = append(out, pal.Style("dim", fmt.Sprintf("  … %d more (↓ scroll)", len(diffLines)-end)))
	}

	// No status line: hint() already spells the keys out under the box, and
	// the box is drawn on every frame whether or not the panel is.
	return out
}

// buildLiveStreamRows renders the write still arriving from the model. The
// streamed content is diffed against the file on disk, so each new line the
// model produces appears as a green row the moment its chunk lands. The tail
// pins to the newest lines (auto-follow) once the preview overflows the
// budget, because the line being written right now is at the bottom.
func (t *TUI) buildLiveStreamRows(pal theme.Palette, w, budget int) []string {
	v := t.liveDiff
	tool := v.streamTool
	if tool == "" {
		tool = "edit"
	}
	path := v.streamPath
	if path == "" {
		path = "…"
	}
	header := pal.Style("accent_bold", " LIVE ")
	header += pal.Style("text", path)
	header += pal.Style("dim", "  "+tool+" · streaming…")
	out := []string{header}

	body := budget - len(out)
	lines := streamPreviewLines(v.streamText)
	more := len(lines) > body
	if more {
		body--
	}
	if body < 1 {
		return out
	}
	start := 0
	if len(lines) > body {
		start = len(lines) - body
	}
	v.maxScroll = 0
	end := start + body
	if end > len(lines) {
		end = len(lines)
	}
	for _, text := range lines[start:end] {
		out = append(out, formatLiveStreamRow(text, w))
	}
	if more {
		out = append(out, pal.Style("dim", fmt.Sprintf("  … %d earlier (streaming…)", start)))
	}
	return out
}

// streamPreviewLines splits streamed content into display lines. A trailing
// partial line is kept: it is the line being typed right now.
func streamPreviewLines(s string) []string {
	if s == "" {
		return []string{"…"}
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return []string{"…"}
	}
	return lines
}

// formatLiveStreamRow renders one streamed line as a pending addition.
func formatLiveStreamRow(text string, w int) string {
	p := theme.Get("")
	maxLen := maxLiveDiffLineLen
	if w > 0 && w-4 < maxLen {
		maxLen = w - 4
	}
	return p.Style("success", fmt.Sprintf("  + %s", util.Truncate(text, maxLen)))
}
// formatLiveDiffRow renders a single diff line with color coding.
// Long lines are truncated to prevent wrapping.
func formatLiveDiffRow(l editdiff.Line, w int) string {
	p := theme.Get("")
	maxLen := maxLiveDiffLineLen
	if w > 0 && w-4 < maxLen {
		maxLen = w - 4
	}
	text := util.Truncate(l.Text, maxLen)
	switch l.Op {
	case editdiff.OpAdd:
		return p.Style("success", fmt.Sprintf("  + %s", text))
	case editdiff.OpDel:
		return p.Style("error", fmt.Sprintf("  - %s", text))
	default:
		return p.Style("dim", fmt.Sprintf("    %s", text))
	}
}

// liveDiffKey handles a key press for the live diff panel.
// Returns true if the key was consumed.
func (t *TUI) liveDiffKey(k string) bool {
	if t.liveDiff == nil {
		return false
	}
	return t.liveDiff.handleKey(k)
}

// liveDiffOpen reports whether the live diff panel is showing. It comes up on
// the first edit of a turn — or the moment a write starts streaming — and
// Esc puts it away; the next edit brings it back.
//
// A nil liveDiff reads as closed: this is asked on every repaint and every key,
// including from tests that build a bare TUI.
func (t *TUI) liveDiffOpen() bool {
	if t.liveDiff == nil || t.liveDiff.dismissed {
		return false
	}
	if t.liveDiff.streaming {
		return true
	}
	return len(t.editLog().list()) > 0
}

// updateLiveStreamLocked feeds one EvToolProgress frame into the panel. The
// raw arguments are partial JSON while the model is still writing them, so
// the content field is extracted with a tolerant scan instead of a strict
// unmarshal: strict parsing shows nothing until the closing quote arrives,
// which is exactly the live lines the user asked to watch.
//
// Called with t.mu held, from the event loop.
func (t *TUI) updateLiveStreamLocked(ev streamFrame) {
	v := t.liveDiff
	if v == nil {
		return
	}
	path, text, ok := extractStreamWrite(ev.Tool, ev.Output)
	if !ok {
		return
	}
	if v.streamID != ev.Text {
		v.streamID = ev.Text
		v.scroll = 0
		v.maxScroll = 0
	}
	v.streamTool = ev.Tool
	v.streamPath = path
	v.streamText = text
	v.streaming = true
	v.dismissed = false
}

// streamFrame is the shape updateLiveStreamLocked needs from an event: the
// tool name, the raw streamed arguments and the call id.
type streamFrame struct {
	Tool   string
	Output string
	Text   string
}

// clearLiveStreamLocked drops the in-flight preview. The recorded EvEdit that
// follows takes over the panel, so the preview must not linger under it.
//
// Called with t.mu held, from the event loop.
func (t *TUI) clearLiveStreamLocked() {
	v := t.liveDiff
	if v == nil {
		return
	}
	v.streaming = false
	v.streamText = ""
	v.streamID = ""
}

// extractStreamWrite pulls the path and the in-progress content out of raw
// streamed tool arguments. Only tools that carry file content participate;
// anything else returns ok=false and leaves the panel alone.
func extractStreamWrite(tool, raw string) (path, text string, ok bool) {
	switch tool {
	case "write", "edit", "multi_edit", "patch":
	default:
		return "", "", false
	}
	path = streamStringField(raw, "path")
	if path == "" {
		path = streamStringField(raw, "file")
	}
	switch tool {
	case "write":
		text, _ = streamStringFieldRaw(raw, "content")
	case "edit", "multi_edit":
		text, _ = streamStringFieldRaw(raw, "new_string")
		if text == "" {
			text, _ = streamStringFieldRaw(raw, "content")
		}
	case "patch":
		text, _ = streamStringFieldRaw(raw, "patch")
	}
	if path == "" && text == "" {
		return "", "", false
	}
	return path, text, true
}

// streamStringField decodes one string field, tolerating the truncation of a
// stream in flight by falling back to a raw scan when JSON fails.
func streamStringField(raw, key string) string {
	s, _ := streamStringFieldRaw(raw, key)
	return s
}

// streamStringFieldRaw extracts a possibly-incomplete JSON string field. It
// first tries a strict unmarshal of the raw arguments; while the model is
// still producing the value that fails on the truncated tail, so it falls
// back to scanning for `"key": "` and decoding escape sequences up to the
// last complete chunk. The second return reports whether the value was
// complete (closing quote seen).
func streamStringFieldRaw(raw, key string) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		if s, _ := m[key].(string); s != "" {
			return s, true
		}
	}
	needle := `"` + key + `"`
	idx := strings.LastIndex(raw, needle)
	if idx < 0 {
		return "", false
	}
	rest := raw[idx+len(needle):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return "", false
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(rest, `"`) {
		return "", false
	}
	rest = rest[1:]
	var b strings.Builder
	esc := false
	for i := range len(rest) {
		c := rest[i]
		if esc {
			switch c {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\', '/':
				b.WriteByte(c)
			case 'u':
				if i+4 < len(rest) {
					var r rune
					_, _ = fmt.Sscanf(rest[i+1 : i+5], "%04x", &r)
					b.WriteRune(r)
					i += 4
				}
			default:
				b.WriteByte(c)
			}
			esc = false
			continue
		}
		if c == '\\' {
			esc = true
			continue
		}
		if c == '"' {
			return b.String(), true
		}
		b.WriteByte(c)
	}
	return b.String(), false
}
