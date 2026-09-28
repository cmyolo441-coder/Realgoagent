package tui

import (
	"fmt"
	"strings"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/md"
	"github.com/nova-ai/nova/internal/session"
	"github.com/nova-ai/nova/internal/subagent"
	"github.com/nova-ai/nova/internal/tools"
)

// Options configures App creation.
type Options struct {
	Workspace string
	PlanMode  bool
	Quiet     bool
	Continue  bool
}

// New builds an App with an agent attached.
func New(cfg *config.Config, opts *Options) (*App, error) {
	if opts == nil {
		opts = &Options{}
	}
	app := NewApp(cfg)
	app.Workspace = opts.Workspace
	app.Quiet = opts.Quiet

	p, m, err := cfg.ResolveModel(cfg.DefaultModel)
	if err != nil {
		return nil, fmt.Errorf("no model available: %w", err)
	}

	reg := tools.NewRegistry()
	tools.RegisterDefaults(reg)
	tools.SetWorkspace(opts.Workspace, cfg.Shell)
	app.Registry = reg

	if opts.PlanMode {
		app.Mode = ModePlan
	}

	// Subagents have to be installed here, not only in the interactive path:
	// `nova -p` runs a full agent turn, and a task tool with no runner behind
	// it would refuse every call for no reason the user could see.
	subagent.Install(&subagent.Runner{
		Config:    cfg,
		Workspace: opts.Workspace,
	})

	ag, err := agent.New(agent.Options{
		Config:    cfg,
		Provider:  p,
		Model:     m,
		Workspace: opts.Workspace,
		Registry:  reg,
		PlanMode:  opts.PlanMode,
		EventSink: func(agent.Event) {},
	})
	if err != nil {
		return nil, err
	}
	app.Agent = ag
	app.SelectedModel = p.Name + "/" + m.ID

	// Resume the most recent session when asked, so `nova -c` continues work.
	if opts.Continue {
		if s, err := session.Latest(); err == nil {
			ag.InjectHistory(s.Messages)
			app.history.Append(app.Theme.Style("dim", "resumed session "+s.ID))
			app.session = s
		}
	}
	if app.session == nil {
		app.session = session.New(opts.Workspace, app.SelectedModel)
	}
	app.session.Model = app.SelectedModel

	// Persist the conversation after every finished turn.
	ag.SetOnDone(func() {
		app.session.Messages = ag.Messages()
		app.session.Usage = ag.Usage()
		_ = app.session.Save()
	})
	return app, nil
}

// RunOnce executes a single prompt and returns the final text. Used for -p.
func (a *App) RunOnce(prompt string) (string, error) {
	if a.Agent == nil {
		return "", fmt.Errorf("agent not initialised")
	}
	var sb strings.Builder
	sink := func(ev agent.Event) {
		switch ev.Kind {
		case agent.EvText:
			sb.WriteString(ev.Text)
		case agent.EvToolStart:
			if !a.Quiet {
				sb.WriteString("\n[" + ev.Tool + "] " + md.FormatBrief(ev.Tool, ev.Args) + "\n")
			}
		case agent.EvToolResult:
			if !a.Quiet && ev.Output != "" {
				out := strings.TrimRight(ev.Output, "\n")
				if len(out) > 4000 {
					out = out[:4000] + "\n…"
				}
				sb.WriteString(out + "\n")
			}
		}
	}
	a.Agent.SetSink(sink)
	if err := a.Agent.Run(prompt); err != nil {
		return sb.String(), err
	}
	return sb.String(), nil
}

// Banner renders the startup text for non-interactive use.
func (a *App) Banner() string {
	p := a.Theme
	return p.Style("dim", "nova ") + p.Style("accent2", a.SelectedModel) + "\n" + p.Style("dim", a.Cwd())
}
