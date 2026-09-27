package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/theme"
)

// Commands that report on the session or act on it rather than on the
// conversation. Grouped here so the registry in commands.go stays readable.

// cmdDoctor checks the things that silently break a session: a missing or
// unreadable config, a provider with no usable key, an empty workspace, and a
// selected model that no longer resolves.
func cmdDoctor(t *TUI, args string) error {
	p := t.app.Theme
	ok := p.Style("success", "✓")
	bad := p.Style("error", "✗")
	warn := p.Style("warning", "!")

	t.app.history.Append(p.Style("accent_bold", "Doctor"))

	path := config.Path()
	if _, err := os.Stat(path); err != nil {
		t.app.history.Append("  " + bad + " config missing at " + path)
		t.app.history.Append(p.Style("dim", "    run with a --config path, or create the file"))
	} else {
		t.app.history.Append("  " + ok + " config " + path)
	}

	// Every provider needs a key from somewhere, or a turn fails at the first
	// request with an opaque 401.
	usable := 0
	for _, prov := range t.app.Cfg.Providers {
		if !prov.Enabled {
			continue
		}
		if prov.APIKeyFromEnv() == "" {
			t.app.history.Append("  " + warn + " provider " + prov.Name + " has no API key")
			continue
		}
		usable++
	}
	if usable == 0 {
		t.app.history.Append("  " + bad + " no provider has an API key; no turn can run")
	} else {
		t.app.history.Append("  " + ok + " " + itoa(usable) + " provider(s) ready")
	}

	if _, m, err := t.app.Cfg.ResolveModel(t.app.SelectedModel); err != nil {
		t.app.history.Append("  " + bad + " selected model " + t.app.SelectedModel + " does not resolve")
	} else {
		ctx := ""
		if m.Context > 0 {
			ctx = " (" + humanTokens(m.Context) + " context)"
		}
		t.app.history.Append("  " + ok + " model " + t.app.SelectedModel + ctx)
	}

	ws := t.app.Cwd()
	if info, err := os.Stat(ws); err != nil || !info.IsDir() {
		t.app.history.Append("  " + bad + " workspace " + ws + " is not a directory")
	} else {
		t.app.history.Append("  " + ok + " workspace " + ws)
	}

	t.app.history.Append("  " + ok + " " + itoa(len(t.app.Registry.Names())) + " tools registered")
	t.app.history.Append("  " + ok + " " + itoa(len(t.app.Agent.Messages())) + " messages in context")
	if t.app.session != nil {
		t.app.history.Append("  " + ok + " session " + t.app.session.ID)
	}
	return nil
}

// cmdTokens breaks down context usage, which is the number people need when a
// long session starts behaving strangely.
func cmdTokens(t *TUI, args string) error {
	p := t.app.Theme
	u := t.app.Agent.Usage()

	t.app.history.Append(p.Style("accent_bold", "Tokens"))
	rows := [][2]string{
		{"input", itoa(u.PromptTokens)},
		{"output", itoa(u.CompletionTokens)},
		{"total", itoa(u.TotalTokens)},
	}
	for _, r := range rows {
		t.app.history.Append("  " + pad(r[0], 8) + p.Style("accent2", r[1]))
	}
	t.app.history.Append("  " + pad("messages", 8) + p.Style("dim", itoa(len(t.app.Agent.Messages()))))

	if _, m, err := t.app.Cfg.ResolveModel(t.app.SelectedModel); err == nil && m.Context > 0 {
		used := float64(u.PromptTokens)
		pct := used / float64(m.Context) * 100
		t.app.history.Append("  " + pad("context", 8) +
			p.Style("dim", humanTokens(u.PromptTokens)+" of "+humanTokens(m.Context)+
				fmt.Sprintf(" (%.0f%%)", pct)))
		if pct > 85 {
			t.app.history.Append(p.Style("warning", "  context is nearly full — /compact or /clear"))
		}
	}
	return nil
}

// cmdHistory lists what has been asked this session, so a long conversation
// can be navigated without scrolling.
func cmdHistory(t *TUI, args string) error {
	p := t.app.Theme
	msgs := t.app.Agent.Messages()

	t.app.history.Append(p.Style("accent_bold", "History"))
	n := 0
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		n++
		text := strings.TrimSpace(strings.ReplaceAll(m.Content, "\n", " "))
		t.app.history.Append("  " + p.Style("dim", fmt.Sprintf("%2d", n)) + "  " +
			p.Style("text", truncMiddle(text, 66)))
	}
	if n == 0 {
		t.app.history.Append(p.Style("dim", "  nothing asked yet"))
	}
	return nil
}

// cmdExport writes the conversation to a markdown file. Useful for pasting
// into an issue or handing to someone else.
func cmdExport(t *TUI, args string) error {
	p := t.app.Theme
	path := strings.TrimSpace(args)
	if path == "" {
		path = fmt.Sprintf("nova-%s.md", time.Now().Format("20060102-150405"))
	}
	if !strings.HasSuffix(path, ".md") {
		path += ".md"
	}

	var b strings.Builder
	b.WriteString("# Nova session\n\n")
	fmt.Fprintf(&b, "- workspace: `%s`\n", t.app.Cwd())
	fmt.Fprintf(&b, "- model: `%s`\n", t.app.SelectedModel)
	fmt.Fprintf(&b, "- exported: %s\n\n", time.Now().Format(time.RFC3339))

	for _, m := range t.app.Agent.Messages() {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString("## You\n\n" + strings.TrimSpace(m.Content) + "\n\n")
		case llm.RoleAssistant:
			if c := strings.TrimSpace(m.Content); c != "" {
				b.WriteString("## Nova\n\n" + c + "\n\n")
			}
		case llm.RoleTool:
			if c := strings.TrimSpace(m.Content); c != "" {
				b.WriteString("<details><summary>tool output</summary>\n\n```\n" +
					truncMiddle(c, 2000) + "\n```\n\n</details>\n\n")
			}
		}
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if err := os.WriteFile(abs, []byte(b.String()), 0o644); err != nil {
		t.app.history.Append(p.Style("error", "could not write "+path+": "+err.Error()))
		return err
	}
	t.app.history.Append(p.Style("success", "exported to "+abs))
	return nil
}

// cmdReload re-reads the config from disk, so editing providers in another
// window takes effect without losing the conversation.
func cmdReload(t *TUI, args string) error {
	p := t.app.Theme
	fresh, err := config.Load()
	if err != nil {
		t.app.history.Append(p.Style("error", "could not read config: "+err.Error()))
		return err
	}

	// Keep the live agent if the selected model is unchanged, so the
	// conversation survives; only swap when the target actually moved.
	p2, m2, err := fresh.ResolveModel(t.app.SelectedModel)
	if err != nil {
		t.app.history.Append(p.Style("error", "reloaded config, but "+t.app.SelectedModel+" no longer resolves"))
		t.app.history.Append(p.Style("dim", "  pick a model with /models"))
		t.app.Cfg = fresh
		t.app.Theme = theme.Get(fresh.Theme)
		return err
	}
	t.app.Cfg = fresh
	t.app.Theme = theme.Get(fresh.Theme)
	if err := rebuildAgent(t, p2, m2); err != nil {
		t.app.history.Append(p.Style("error", err.Error()))
		return err
	}
	t.app.history.Append(p.Style("success", "reloaded "+config.Path()))
	return nil
}

// cmdInit asks the agent to write a project briefing file, the usual first
// move in an unfamiliar repository. It is a prompt, not a silent write, so the
// user sees what is about to happen and can refuse.
func cmdInit(t *TUI, args string) error {
	p := t.app.Theme
	target := strings.TrimSpace(args)
	if target == "" {
		target = "AGENTS.md"
	}
	if filepath.IsAbs(target) || strings.Contains(target, "..") {
		t.app.history.Append(p.Style("error", "init writes inside the workspace; "+target+" points outside it"))
		return fmt.Errorf("refusing to write outside the workspace")
	}
	t.app.history.Append(p.Style("dim", "writing "+target+" — say no to the write prompt to stop"))
	return t.Submit(fmt.Sprintf(
		"Read the project and write a %s at the workspace root. Keep it under 60 lines and "+
			"cover only what an agent cannot infer from the code: how to build, how to run the "+
			"tests, the conventions that are not obvious, and anything that is deliberately "+
			"unusual. Do not restate the file tree or the obvious language choice. "+
			"If the file already exists, read it first and improve it rather than replacing it.",
		target))
}

// cmdContext shows what the system prompt currently tells the model, which is
// the first place to look when the agent misunderstands the project.
func cmdContext(t *TUI, args string) error {
	p := t.app.Theme
	text := t.app.Agent.SystemPrompt()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	t.app.history.Append(p.Style("accent_bold", "Context") +
		p.Style("dim", fmt.Sprintf("  %d lines, %d chars", len(lines), len(text))))
	for _, ln := range lines {
		t.app.history.Append(p.Style("dim", "  "+ln))
	}
	return nil
}
