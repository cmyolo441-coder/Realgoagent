package tui

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/md"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
	"golang.org/x/term"
)

// TUI is the interactive terminal front-end.
//
// Nova renders into the terminal's own scrollback rather than the alternate
// screen: finished output is printed once and stays put, so selection,
// copying and native scrollback all behave the way the shell taught the user.
// Only the "live region" — the streaming tail plus the prompt box — is ever
// repainted in place.
type TUI struct {
	app      *App
	out      *bufio.Writer
	in       *bufio.Reader
	tty      *os.File
	oldState *term.State

	mu   sync.Mutex
	done chan struct{}

	// streaming state
	streaming  bool
	streamBuf  strings.Builder
	toolActive string
	activeTask string

	// pending confirmation
	asking   bool
	question string

	// iteration counter for the current turn
	iter int

	// answerChan receives the typed answer to a tool approval prompt.
	answerChan chan string
	// pendingAnswer receives the typed answer to an ask_user question. It is
	// kept separate from answerChan so the two flows cannot swallow each other.
	pendingAnswer chan string

	// cursor for rendering input
	inputLines []string
	cursorLine int
	cursorCol  int

	// sel is the highlighted row of the command palette; paletteKey is the
	// composer text that selection was last computed for, so typing a new
	// prefix resets the walk to the top.
	sel        int
	paletteKey string

	// rowsWritten counts terminal rows emitted since startup. It is only
	// meaningful while the screen has not yet scrolled, which is exactly when
	// it is needed to push the prompt down to the bottom.
	rowsWritten int

	// pinPending asks the next repaint to push the live region to the bottom.
	pinPending bool

	// paletteDismissed closes the list with Esc while keeping the text.
	paletteDismissed bool

	// cancelTurn stops the turn in flight; nil when no turn is running.
	cancelTurn func()

	// modelPick is the open model picker, or nil when not picking.
	modelPick *modelPicker

	// editView is the open edit viewer, or nil when it is closed.
	editView *editViewer

	// agents is the open subagent view, or nil when it is closed.
	agents *subView

	// The live region is the block pinned to the bottom of the screen that Nova
	// repaints in place. liveLines is how many terminal rows it currently
	// occupies; flushed is how many history lines have already been committed
	// to scrollback; caretRowLast is the row the cursor was left on, which is
	// the caret inside the box.
	liveLines    int
	flushed      int
	caretRowLast int
	// lastFrame is the last payload written to the terminal. Repaints that
	// would produce identical bytes are skipped, which keeps the idle spinner
	// from writing to the tty twelve times a second.
	lastFrame string

	// needsDraw records that agent output changed since the last repaint, so
	// the ticker can coalesce a burst of events into a single frame.
	needsDraw bool

	// lastErr is the sticky error shown on the top rail.
	lastErr error

	// redrawReq coalesces repaint requests from the key handler.
	redrawReq  chan struct{}
	messageBus chan agent.Event
}

// frameInterval caps how often the screen is repainted. 20fps is smooth
// enough for text and cheap enough to leave the terminal responsive to typing.
const frameInterval = 50 * time.Millisecond

// NewTUI creates a TUI bound to cfg.
func NewTUI(cfg *config.Config) (*TUI, error) {
	app := NewApp(cfg)
	t := &TUI{
		app:        app,
		out:        bufio.NewWriterSize(os.Stdout, 1<<16),
		in:         bufio.NewReaderSize(os.Stdin, 1<<12),
		done:       make(chan struct{}),
		redrawReq:  make(chan struct{}, 1),
		messageBus: make(chan agent.Event, 256),
		inputLines: []string{""},
	}
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		app.SetSize(w, h)
	}
	return t, nil
}

// EnterRaw switches the terminal into raw mode.
//
// Nova deliberately does not switch to the alternate screen and does not
// enable mouse reporting: both would take the scrollback away from the user.
// Bracketed paste is kept because it lets a multi-line paste land in the
// composer as text instead of as a burst of keystrokes that each submit.
func (t *TUI) EnterRaw() error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	t.tty = os.Stdin
	st, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	t.oldState = st
	fmt.Fprint(t.out, "\x1b[?25l\x1b[?2004h")
	t.out.Flush()
	return nil
}

// Exit restores the terminal.
func (t *TUI) Exit() {
	if t.oldState != nil && t.tty != nil {
		fmt.Fprint(t.out, "\x1b[?2004l\x1b[?25h")
		t.out.Flush()
		_ = term.Restore(int(t.tty.Fd()), t.oldState)
		t.oldState = nil
	}
}

// Run starts the event loop. It blocks until the user quits.
// initialPrompt, when non-empty, is submitted immediately after startup.
func (t *TUI) Run(initialPrompt string) error {
	if err := t.EnterRaw(); err != nil {
		return err
	}
	defer t.Exit()

	// A resize changes the wrap width, so re-measure and repaint on SIGWINCH.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	t.resize()
	t.printBanner()
	// Draw once so the banner is committed and the live region has a measured
	// height, then pad so the box finishes at the bottom of the terminal.
	t.draw()
	t.requestPin()
	t.forceDraw()

	// input reader goroutine
	inputCh := make(chan string, 32)
	errCh := make(chan error, 8)
	go t.readInput(inputCh, errCh)

	if initialPrompt != "" {
		t.inputLines = strings.Split(initialPrompt, "\n")
		t.cursorLine = len(t.inputLines) - 1
		t.cursorCol = countRunes(t.inputLines[t.cursorLine])
		t.onKey("enter")
	}

	// Agent events are coalesced instead of drawn one by one. A fast model can
	// emit hundreds of tokens a second, and re-rendering the whole reply for
	// each one made the UI crawl on long answers. Repaints are capped by the
	// ticker, which also drives the spinner; keystrokes still draw at once so
	// typing never feels laggy.
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return t.lastErr
		case ev := <-t.messageBus:
			t.handleEvent(ev)
			t.needsDraw = true
		case s := <-inputCh:
			if err := t.handleLine(s); err != nil {
				t.lastErr = err
			}
			t.draw()
		case err := <-errCh:
			t.lastErr = err
			t.Quit()
		case <-winch:
			t.resize()
			t.requestPin()
			t.forceDraw()
		case <-ticker.C:
			if t.needsDraw {
				t.needsDraw = false
			}
			// Always tick, so the spinner animates even with no new events.
			t.draw()
		case <-t.redrawReq:
			t.draw()
		}
	}
}

// Run starts the interactive session on the given App.
// initialPrompt, when non-empty, is submitted immediately after startup.
func (a *App) Run(initialPrompt string) error {
	t, err := a.newTUI()
	if err != nil {
		return err
	}
	return t.Run(initialPrompt)
}

// RunWithPrompt starts the session and immediately submits prompt.
func (a *App) RunWithPrompt(prompt string) error {
	t, err := a.newTUI()
	if err != nil {
		return err
	}
	return t.Run(prompt)
}

// newTUI wires app + agent into a TUI instance.
//
// The agent built by New carries no event sink and cannot ask the user
// anything, which is right for -p and useless interactively. Re-binding it
// here gives the session its UI: events flow to the message bus, risky tools
// block on an approval prompt, and ask_user reaches the composer.
func (a *App) newTUI() (*TUI, error) {
	if a.Agent == nil {
		return nil, fmt.Errorf("agent not initialised")
	}
	t, err := NewTUI(a.Cfg)
	if err != nil {
		return nil, err
	}
	t.app = a

	p, m, err := a.Cfg.ResolveModel(a.SelectedModel)
	if err != nil {
		return nil, fmt.Errorf("no model available: %w", err)
	}
	if err := rebuildAgent(t, p, m); err != nil {
		return nil, err
	}
	return t, nil
}

// resize re-reads the terminal dimensions.
func (t *TUI) resize() {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		t.app.SetSize(w, h)
	}
}

// clearScreen erases the display and homes the cursor. It is only used for an
// explicit ^L, never on startup: wiping the user's scrollback unasked is the
// one thing this UI must not do.
func (t *TUI) clearScreen() {
	t.out.WriteString("\x1b[2J\x1b[H")
	t.out.Flush()
}

// forceDraw repaints even when the frame is byte-identical to the last one.
// Used after operations that invalidate our idea of the cursor position.
func (t *TUI) forceDraw() {
	t.lastFrame = ""
	t.draw()
}

// printBanner renders the startup header.
func (t *TUI) printBanner() {
	p := t.app.Theme
	logo := []string{
		p.Style("accent_bold", "  ███╗   ██╗ ██████╗ ██╗   ██╗ █████╗ "),
		p.Style("accent_bold", "  ████╗  ██║██╔═══██╗██║   ██║██╔══██╗"),
		p.Style("accent2", "  ██╔██╗ ██║██║   ██║██║   ██║███████║"),
		p.Style("accent2", "  ██║╚██╗██║██║   ██║╚██╗ ██╔╝██╔══██║"),
		p.Style("tool", "  ██║ ╚████║╚██████╔╝ ╚████╔╝ ██║  ██║"),
		p.Style("tool", "  ╚═╝  ╚═══╝ ╚═════╝   ╚═══╝  ╚═╝  ╚═╝"),
	}
	for _, l := range logo {
		t.app.history.Append(l)
	}
	t.app.history.Append("")
	t.app.history.Append(p.Style("dim", "  autonomous terminal coding agent"))
	t.app.history.Append(p.Style("dim", "  model: ") + p.Style("accent2", t.app.SelectedModel) + p.Style("dim", "   workspace: ") + t.app.Cwd())
	t.app.history.Append(p.Style("dim", "  type /help for commands, or just describe what to build"))
	t.app.history.Append("")
}

// Submit sends text to the agent. The turn runs on its own goroutine and
// reports back over the message bus, so the event loop stays responsive to
// keystrokes while the model works.
func (t *TUI) Submit(text string) error {
	t.app.PromptText = text
	t.streaming = true
	t.iter = 0
	t.streamBuf.Reset()
	// Esc stops the turn through this handle rather than reaching into the
	// agent, so the key handler stays free of agent plumbing.
	t.cancelTurn = t.app.Agent.Cancel
	t.scheduleDraw()
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.send(agent.Event{Kind: agent.EvError, Text: fmt.Sprintf("internal error: %v", rec), IsErr: true})
			}
		}()
		// The agent reports its own failures through the sink and always
		// closes the turn with EvDone, so there is nothing to forward here.
		_ = t.app.Agent.Run(text)
	}()
	return nil
}

// send hands an event to the event loop.
//
// Text is the one kind that must never be dropped: EvText carries reply
// content that also goes into the saved session, so losing one to a full bus
// would silently truncate the answer and corrupt the conversation on resume.
// Everything else is a status update, and a dropped frame of those costs
// nothing but a missing animation tick.
func (t *TUI) send(ev agent.Event) {
	if ev.Kind != agent.EvText {
		select {
		case t.messageBus <- ev:
		default:
		}
		return
	}
	// Blocking on the bus is safe because the event loop is the only reader
	// and it is never blocked on the agent: every path that waits on the
	// agent does so on the agent's own goroutine. The done case keeps a
	// quitting session from parking this goroutine forever.
	select {
	case t.messageBus <- ev:
	case <-t.done:
	}
}

// handleEvent routes agent events into the display buffer. The run loop
// repaints once per event, so nothing here may draw.
func (t *TUI) handleEvent(ev agent.Event) {
	p := t.app.Theme
	switch ev.Kind {
	case agent.EvText:
		t.streamBuf.WriteString(ev.Text)
	case agent.EvThinking:
		t.app.history.Append(p.Style("dim", "· "+ev.Text))
	case agent.EvToolStart:
		t.flushStream()
		t.app.history.Append(toolCall(p, ev.Tool, md.FormatArgs(ev.Tool, ev.Args)))
		t.toolActive = ev.Tool
	case agent.EvToolResult:
		t.flushStream()
		dur := ev.Dur.Round(time.Millisecond).String()
		if dur == "0s" {
			dur = ""
		}
		t.app.history.Append(toolResult(p, ev.Tool, dur, ev.Output, ev.IsErr, t.app.Width)...)
		t.toolActive = ""
		if ev.Task != "" {
			t.activeTask = ev.Task
		}
	case agent.EvToolApproval:
		t.showApproval(ev.Tool, ev.Args)
	case agent.EvToolDenied:
		t.app.history.Append(p.Style("warning", "⊘ denied "+ev.Tool))
	case agent.EvAskUser:
		t.startAsk(ev.Text)
	case agent.EvIteration:
		t.iter++
	case agent.EvError:
		t.flushStream()
		t.app.history.Append(p.Style("error", "✗ "+ev.Text))
	case agent.EvReasoningHidden:
		// Reasoning is stripped before it reaches the screen. Say so once,
		// rather than letting the user think the model ignored their question.
		t.app.history.Append(p.Style("dim", "· "+ev.Text+" chars of model reasoning hidden"))
	case agent.EvDone:
		if reply := t.streamBuf.String(); strings.TrimSpace(reply) != "" {
			t.app.LastReply = reply
		}
		t.flushStream()
		t.app.history.Append(p.Style("dim", "· "+ev.Text))
		t.streaming = false
		t.activeTask = ""
		t.toolActive = ""
		t.iter = 0
		t.cancelTurn = nil
	case agent.EvEdit:
		t.showEdit(ev)
	case agent.EvUsage:
		t.app.status = fmtUsageTokens(ev.Usage)
	}
}

// showEdit records a file change and prints it to the transcript.
//
// The transcript shows a short preview rather than the whole diff: a
// large change would bury the conversation, and the full diff is one keystroke
// away in /edits. What it must show is the count and the first few lines, so
// the user sees the agent changing a file the moment it happens.
func (t *TUI) showEdit(ev agent.Event) {
	if ev.Edit == nil {
		return
	}
	rec := *ev.Edit
	t.app.edits.record(rec.Tool, rec.Dur, rec.Output, rec.IsErr, rec.Files)
	p := t.app.Theme
	for _, f := range rec.Files {
		added, removed := editdiff.Count(f.Before, f.After)
		head := "  " + p.Style("tool", "✎ ") + p.Style("text", f.Path)
		stat := editdiff.Stats{Added: added, Removed: removed}
		head += editStat(p, stat)
		if rec.Dur != "" {
			head += p.Style("dim", "  "+rec.Dur)
		}
		if rec.IsErr {
			head += p.Style("error", "  failed")
		}
		t.app.history.Append(head)
		for _, row := range editPreview(p, f.Path, f.Before, f.After, t.app.Width) {
			t.app.history.Append(row)
		}
	}
}

// editStat renders the +n/-m counts for a change.
func editStat(p theme.Palette, s editdiff.Stats) string {
	var b strings.Builder
	if s.Added > 0 {
		b.WriteString("  " + p.Style("success", "+"+itoa(s.Added)))
	}
	if s.Removed > 0 {
		b.WriteString("  " + p.Style("error", "-"+itoa(s.Removed)))
	}
	return b.String()
}

// editPreviewLines is how many changed lines the transcript shows for an edit.
const editPreviewLines = 4

// editPreview renders the first few changed lines of a file.
func editPreview(p theme.Palette, path, before, after string, width int) []string {
	lines := editdiff.Context(before, after, editdiff.DefaultContext)
	var out []string
	for _, l := range lines {
		if l.Op == editdiff.OpEqual {
			continue
		}
		if len(out) >= editPreviewLines {
			break
		}
		out = append(out, "    "+util.Truncate(rowFor(l), width-4))
	}
	return out
}

// flushStream commits whatever the model has streamed so far to the history
// buffer, so the next permanent write picks it up.
func (t *TUI) flushStream() {
	if t.streamBuf.Len() == 0 {
		return
	}
	p := t.app.Theme
	rendered := md.New(p, t.app.Width).Render(t.streamBuf.String())
	t.app.history.Append(rendered...)
	t.streamBuf.Reset()
}

func (t *TUI) startAsk(q string) {
	t.asking = true
	t.question = q
}
