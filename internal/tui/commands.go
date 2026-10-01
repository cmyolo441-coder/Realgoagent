package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/session"
	"github.com/nova-ai/nova/internal/subagent"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/tools"
)

// command describes a slash command.
type command struct {
	Name string
	Desc string
	Run  func(t *TUI, args string) error
}

// commands is the slash-command registry.
var commands []command

func init() {
	commands = []command{
		{"/help", "Show this help", cmdHelp},
		{"/clear", "Clear the conversation history", cmdClear},
		{"/model", "Switch model, or open the picker", cmdModel},
		{"/models", "Pick a model with ↑↓ and ⏎", cmdModels},
		{"/theme", "Switch colour theme", cmdTheme},
		{"/themes", "List colour themes", cmdThemes},
		{"/mode", "Set agent mode: agent | plan", cmdMode},
		{"/status", "Show configuration and session status", cmdStatus},
		{"/doctor", "Check config, keys, model and workspace", cmdDoctor},
		{"/config", "Show the config file path", cmdConfig},
		{"/reload", "Re-read the config from disk", cmdReload},
		{"/tools", "List registered tools", cmdTools},
		{"/context", "Show the system prompt sent to the model", cmdContext},
		{"/init", "Ask the agent to write a project briefing", cmdInit},
		{"/copy", "Copy last reply to the clipboard", cmdCopy},
		{"/export", "Write the conversation to a markdown file", cmdExport},
		{"/undo", "Revert the last file change made this session", cmdUndo},
		{"/diff", "Show the uncommitted workspace diff", cmdDiff},
		{"/history", "List the prompts asked this session", cmdHistory},
		{"/tokens", "Show token usage and context fill", cmdTokens},
		{"/edits", "Browse the files the agent changed, with diffs", cmdEdits},
		{"/edits-show", "Print one recorded edit as a unified diff", cmdEditsShow},
		{"/edits-clear", "Forget the recorded edit history", cmdEditsClear},
		{"/agents", "Show the subagents this session has run", cmdAgents},
		{"/task", "Delegate a job to a subagent: /task explore <what to do>", cmdTask},
		{"/roles", "List the subagent roles", cmdRoles},
		{"/sessions", "List saved sessions", cmdSessions},
		{"/save", "Save this session", cmdSave},
		{"/load", "Load a saved session", cmdLoad},
		{"/run", "Run a shell command directly", cmdRun},
		{"/retry", "Re-send the last prompt", cmdRetry},
		{"/compact", "Summarise history to free context", cmdCompact},
		{"/quit", "Exit nova", cmdQuit},
	}
}

func (t *TUI) runCommand(text string) error {
	fields := strings.Fields(text)
	name := strings.ToLower(fields[0])
	args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	for _, c := range commands {
		if c.Name == name {
			return c.Run(t, args)
		}
	}
	t.app.history.Append(t.app.Theme.Style("error", "unknown command "+name) + t.app.Theme.Style("dim", "  try /help"))
	return nil
}

func cmdHelp(t *TUI, args string) error {
	p := t.app.Theme
	t.app.history.Append(p.Style("accent_bold", "Commands"))
	t.app.history.Append("")
	for _, c := range commands {
		t.app.history.Append(fmt.Sprintf("  %s %s", p.Style("accent2", pad(c.Name, 12)), p.Style("dim", c.Desc)))
	}
	t.app.history.Append("")
	t.app.history.Append(p.Style("dim", "Keys: ⏎ send · ⇧⏎ newline · Tab complete · ^C quit · ^L redraw · PgUp/PgDn scroll"))
	return nil
}

func cmdClear(t *TUI, args string) error {
	if t.app.Agent != nil {
		t.app.Agent.Reset()
	}
	// The recorded edits describe changes made in the conversation being
	// cleared. Keeping them would leave /edits describing work the session no
	// longer remembers making.
	t.app.edits.clear()
	t.app.subs.clear()
	t.closeEditViewer()
	t.closeSubView()
	t.app.history.Clear()
	t.app.history.Append(t.app.Theme.Style("dim", "context cleared"))
	return nil
}

// cmdModel switches model by name. With no argument it opens the picker, so
// the user does not have to remember the reference they want.
func cmdModel(t *TUI, args string) error {
	args = strings.TrimSpace(args)
	if args == "" {
		if p := newModelPicker(t.app.Cfg, t.app.SelectedModel); p != nil {
			t.openModelPicker(p)
			return nil
		}
		t.app.history.Append(t.app.Theme.Style("dim", "current model: "+t.app.SelectedModel))
		return nil
	}
	return applyModel(t, args)
}

// cmdModels opens the model picker. Switching model is a common enough
// operation, and scrolling a printed list to retype the name is a waste of
// both the user's time and the context window.
func cmdModels(t *TUI, args string) error {
	if p := newModelPicker(t.app.Cfg, t.app.SelectedModel); p != nil {
		t.openModelPicker(p)
		return nil
	}
	// No models configured: say so rather than opening an empty box.
	t.app.history.Append(t.app.Theme.Style("error", "no models configured"))
	return nil
}

// applyModel switches the session to the given provider/model reference.
func applyModel(t *TUI, ref string) error {
	prov, m, err := t.app.Cfg.ResolveModel(ref)
	if err != nil {
		t.app.history.Append(t.app.Theme.Style("error", err.Error()))
		return err
	}
	if err := rebuildAgent(t, prov, m); err != nil {
		t.app.history.Append(t.app.Theme.Style("error", err.Error()))
		return err
	}
	if t.app.session != nil {
		t.app.session.Model = t.app.SelectedModel
	}
	t.app.history.Append(t.app.Theme.Style("success", "switched to "+t.app.SelectedModel))
	return nil
}

func cmdTheme(t *TUI, args string) error {
	name := strings.TrimSpace(strings.ToLower(args))
	if name == "" {
		t.app.history.Append(t.app.Theme.Style("dim", "current theme: "+t.app.Theme.Name))
		return nil
	}
	pal := theme.Get(name)
	t.app.Theme = pal
	t.app.Cfg.Theme = pal.Name
	_ = t.app.Cfg.Save()
	t.app.history.Append(t.app.Theme.Style("success", "theme set to "+pal.Name))
	return nil
}

func cmdThemes(t *TUI, args string) error {
	for _, n := range theme.Names() {
		bullet := "  "
		if n == t.app.Theme.Name {
			bullet = "▸ "
		}
		t.app.history.Append(t.app.Theme.Style("accent", bullet) + n)
	}
	return nil
}

func cmdMode(t *TUI, args string) error {
	names := map[Mode]string{ModeAgent: "agent", ModePlan: "plan"}
	switch strings.TrimSpace(strings.ToLower(args)) {
	case "", "agent":
		t.app.Mode = ModeAgent
	case "plan", "plan-mode", "readonly":
		t.app.Mode = ModePlan
	default:
		t.app.history.Append(t.app.Theme.Style("error", "modes: agent, plan"))
		return nil
	}
	// Push the new mode into the live agent so the tool set and the system
	// prompt reflect it on the very next request.
	if t.app.Agent != nil {
		t.app.Agent.SetPlanMode(t.app.Mode == ModePlan)
	}
	t.app.history.Append(t.app.Theme.Style("success", "mode: "+names[t.app.Mode]))
	if t.app.Mode == ModePlan {
		t.app.history.Append(t.app.Theme.Style("dim", "read-only: write/edit/patch/bash are disabled"))
	}
	return nil
}

func cmdStatus(t *TUI, args string) error {
	p := t.app.Theme
	lines := []string{
		p.Style("accent_bold", "Status"),
		"  model    " + t.app.SelectedModel,
		"  theme    " + t.app.Theme.Name,
		"  workspace " + t.app.Cwd(),
		"  config   " + t.app.Cfg.Path(),
		"  messages " + fmt.Sprint(len(t.app.Agent.Messages())),
		"  usage    " + t.app.status,
	}
	t.app.history.Append(lines...)
	return nil
}

func cmdConfig(t *TUI, args string) error {
	t.app.history.Append(t.app.Theme.Style("dim", t.app.Cfg.Path()))
	return nil
}

func cmdTools(t *TUI, args string) error {
	if t.app.Agent == nil {
		return nil
	}
	p := t.app.Theme
	t.app.history.Append(p.Style("accent_bold", "Tools"))
	for _, name := range t.app.Registry.Names() {
		if tool, ok := t.app.Registry.Get(name); ok {
			t.app.history.Append("  " + p.Style("accent2", name) + p.Style("dim", "  "+firstLine(tool.Description())))
		}
	}
	return nil
}

func cmdCopy(t *TUI, args string) error {
	reply := t.app.LastReply
	if strings.TrimSpace(reply) == "" {
		t.app.history.Append(t.app.Theme.Style("dim", "nothing to copy yet"))
		return nil
	}
	if err := copyToClipboard(reply); err != nil {
		t.app.history.Append(t.app.Theme.Style("warning", "could not copy: "+err.Error()))
		t.app.history.Append(t.app.Theme.Style("dim", "the last reply is above; select it with your terminal"))
		return nil
	}
	t.app.history.Append(t.app.Theme.Style("success", "last reply copied to the clipboard"))
	return nil
}

func cmdUndo(t *TUI, args string) error {
	path, msg, err := tools.UndoLast()
	if err != nil {
		t.app.history.Append(t.app.Theme.Style("dim", "nothing to undo from this session"))
		return nil
	}
	// The file is back to its previous contents, so the recorded change is no
	// longer true. Leaving it in /edits would show a diff for a state that
	// does not exist.
	t.app.edits.undo(path)
	t.app.history.Append(t.app.Theme.Style("success", "undid the last edit: "+msg))
	return nil
}

func cmdDiff(t *TUI, args string) error {
	out, _ := shellExec("git diff --stat 2>&1", t.app.Cwd())
	t.app.history.Append(t.app.Theme.Style("dim", "workspace diff (stat):"))
	for _, l := range strings.Split(out, "\n") {
		t.app.history.Append(pStyle(t.app.Theme, l))
	}
	return nil
}

func cmdSessions(t *TUI, args string) error {
	p := t.app.Theme
	all, err := session.List()
	if err != nil {
		t.app.history.Append(p.Style("error", err.Error()))
		return nil
	}
	if len(all) == 0 {
		t.app.history.Append(p.Style("dim", "no saved sessions"))
		return nil
	}
	t.app.history.Append(p.Style("accent_bold", "Sessions"))
	for _, s := range all {
		current := ""
		if t.app.session != nil && s.ID == t.app.session.ID {
			current = p.Style("success", " ← current")
		}
		t.app.history.Append("  " + p.Style("accent2", s.ID) + p.Style("dim", "  "+s.Model) + current)
	}
	t.app.history.Append(p.Style("dim", "  /load <id> to open one"))
	return nil
}

func cmdSave(t *TUI, args string) error {
	if t.app.session == nil {
		t.app.history.Append(t.app.Theme.Style("dim", "no session to save"))
		return nil
	}
	if t.app.Agent != nil {
		t.app.session.Messages = t.app.Agent.Messages()
		t.app.session.Usage = t.app.Agent.Usage()
	}
	if err := t.app.session.Save(); err != nil {
		t.app.history.Append(t.app.Theme.Style("error", err.Error()))
		return nil
	}
	t.app.history.Append(t.app.Theme.Style("success", "session saved: "+t.app.session.ID))
	return nil
}

func cmdLoad(t *TUI, args string) error {
	p := t.app.Theme
	id := strings.TrimSpace(args)
	if id == "" {
		all, err := session.List()
		if err != nil || len(all) == 0 {
			t.app.history.Append(p.Style("dim", "no saved sessions to load"))
			return nil
		}
		id = all[0].ID
	}
	s, err := session.Load(id)
	if err != nil {
		if fixed, ferr := session.ResolvePrefix(id); ferr == nil {
			s, err = fixed, nil
		} else {
			t.app.history.Append(p.Style("error", err.Error()))
			return nil
		}
	}
	t.app.session = s
	t.app.Agent.Reset()
	t.app.Agent.InjectHistory(s.Messages)
	t.app.history.Append(p.Style("success", "loaded session "+s.ID+" ("+fmt.Sprint(len(s.Messages))+" messages)"))
	return nil
}

func cmdRun(t *TUI, args string) error {
	args = strings.TrimSpace(args)
	if args == "" {
		t.app.history.Append(t.app.Theme.Style("dim", "usage: /run <command>"))
		return nil
	}
	out, err := shellExec(args, t.app.Cwd())
	t.app.history.Append(t.app.Theme.Style("tool", "$ "+args))
	for _, l := range strings.Split(out, "\n") {
		t.app.history.Append(pStyle(t.app.Theme, l))
	}
	if err != nil {
		t.app.history.Append(t.app.Theme.Style("error", err.Error()))
	}
	return nil
}

func cmdRetry(t *TUI, args string) error {
	if t.app.PromptText == "" {
		return nil
	}
	return t.Submit(t.app.PromptText)
}

func cmdCompact(t *TUI, args string) error {
	if t.app.Agent == nil {
		return nil
	}
	p := t.app.Theme
	msgs := t.app.Agent.Messages()
	before := len(msgs)

	// Keep everything from the most recent user turn onwards; drop the earlier
	// middle of the conversation along with the tool output that produced it.
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		t.app.history.Append(p.Style("dim", "nothing to compact"))
		return nil
	}
	// Orphaned tool results (their assistant tool_call was dropped) can upset
	// the API, so strip them from the kept tail.
	tail := dropOrphanToolCalls(append([]llm.Message{}, msgs[last:]...))
	t.app.Agent.Reset()
	t.app.Agent.InjectHistory(tail)

	t.app.history.Append(p.Style("dim", fmt.Sprintf("compacted: dropped %d earlier messages, kept the last turn", before-len(tail))))
	if t.app.session != nil {
		t.app.session.Messages = tail
		_ = t.app.session.Save()
	}
	return nil
}

// dropOrphanToolCalls removes tool results whose assistant tool_call no longer
// precedes them, which can otherwise upset the API after a compaction.
func dropOrphanToolCalls(msgs []llm.Message) []llm.Message {
	pending := map[string]bool{}
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			pending = map[string]bool{}
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					pending[tc.ID] = true
				}
			}
		case "tool":
			if m.ToolCallID != "" && !pending[m.ToolCallID] {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func cmdQuit(t *TUI, args string) error {
	t.Quit()
	return nil
}

// completeCommand returns a completion for the current input line.
func completeCommand(line string, app *App) string {
	if !strings.HasPrefix(line, "/") {
		return ""
	}
	name := strings.Fields(line)[0]
	for _, c := range commands {
		if strings.HasPrefix(c.Name, name) && c.Name != name {
			return c.Name
		}
	}
	return ""
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func humanTokens(n int) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func pStyle(p theme.Palette, s string) string { return s }

// rebuildAgent swaps in an agent bound to a different provider/model while
// carrying over the conversation so switching models mid-session keeps context.
func rebuildAgent(t *TUI, p *config.Provider, m *config.Model) error {
	if t.app == nil || t.app.Cfg == nil {
		return fmt.Errorf("no config")
	}
	reg := t.app.Registry
	if reg == nil {
		reg = tools.NewRegistry()
		tools.RegisterDefaults(reg)
	}
	tools.SetWorkspace(t.app.Workspace, t.app.Cfg.Shell)

	// The edit watcher runs on the tool's own goroutine, so it only records:
	// the transcript is written by the event loop when the resulting event
	// comes back over the bus.
	tools.SetEditWatcher(func(rec tools.EditRecord) {
		t.send(agent.Event{Kind: agent.EvEdit, Tool: rec.Tool, Edit: &rec})
	})

	// Subagents get the same workspace as the main agent. What they do not get
	// is the parent's conversation: a delegated job is described entirely by
	// its own task.
	sub := &subagent.Runner{
		Config:    t.app.Cfg,
		Workspace: t.app.Workspace,
		OnEvent: func(ev subagent.Event) {
			t.app.subs.handleEvent(ev)
			t.scheduleDraw()
		},
	}
	subagent.Install(sub)

	ag, err := agent.New(agent.Options{
		Config:    t.app.Cfg,
		Provider:  p,
		Model:     m,
		Workspace: t.app.Workspace,
		Registry:  reg,
		PlanMode:  t.app.Mode == ModePlan,
		// The sink goes through the TUI's own send, so it applies the same
		// drop policy as every other producer on the bus. The inline
		// non-blocking send it replaces dropped reply text and the end-of-turn
		// event alike, which truncated answers and stranded the spinner.
		EventSink: t.send,
		OnAskUser: func(q string) string {
			t.mu.Lock()
			ch := make(chan string, 1)
			t.pendingAnswer = ch
			t.mu.Unlock()
			t.send(agent.Event{Kind: agent.EvAskUser, Text: q})
			select {
			case answer := <-ch:
				return answer
			case <-t.done:
				// Quitting with a question outstanding: the turn is over, so
				// unblock the agent rather than leave it parked on a channel
				// nobody is going to answer.
				return "(session closed before the question was answered)"
			}
		},
	})
	if err != nil {
		return err
	}
	t.app.SelectedModel = p.Name + "/" + m.ID
	t.app.Registry = reg

	// Carry over conversation history and the session persistence hook.
	if prev := t.app.Agent; prev != nil {
		ag.InjectHistory(prev.Messages())
	}
	if t.app.session != nil {
		s := t.app.session
		ag.SetOnDone(func() {
			s.Messages = t.app.Agent.Messages()
			s.Usage = t.app.Agent.Usage()
			_ = s.Save()
		})
	}
	t.app.Agent = ag
	return nil
}

// copyToClipboard pushes text to the system clipboard, trying the common
// terminal helpers in order and reporting a clear error when none is present.
func copyToClipboard(text string) error {
	candidates := [][]string{
		{"pbcopy"},
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"clip.exe"},
	}
	var lastErr error
	for _, c := range candidates {
		bin, err := exec.LookPath(c[0])
		if err != nil {
			lastErr = err
			continue
		}
		cmd := exec.Command(bin, c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no clipboard helper found")
	}
	return lastErr
}
