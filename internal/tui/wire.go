package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/session"
	"github.com/nova-ai/nova/internal/tools"
)

// shellExec runs a command in dir, returning combined output.
func shellExec(cmd, dir string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// Cwd returns the working directory configured for the app.
func (a *App) Cwd() string {
	if a.Workspace != "" {
		return a.Workspace
	}
	return "."
}

// InitLocalAgent builds the local agent for non-interactive use (RunOnce).
// Interactive use goes through newTUI/rebuildAgent instead.
func (a *App) InitLocalAgent() error {
	if a.Cfg == nil {
		return fmt.Errorf("no config")
	}
	p, m, err := a.Cfg.ResolveModel(a.SelectedModel)
	if err != nil {
		return fmt.Errorf("no model available: %w", err)
	}
	reg := a.Registry
	if reg == nil {
		reg = tools.NewRegistry()
		tools.RegisterDefaults(reg)
		a.Registry = reg
	}
	tools.SetWorkspace(a.Cwd(), a.Cfg.Shell)
	ag, err := agent.New(agent.Options{
		Config:    a.Cfg,
		Provider:  p,
		Model:     m,
		Workspace: a.Cwd(),
		Registry:  reg,
		EventSink: func(agent.Event) {},
	})
	if err != nil {
		return err
	}
	a.Agent = ag
	return a.resumeSession()
}

// resumeSession loads ResumeSession ("last" = most recent) into the agent.
func (a *App) resumeSession() error {
	if a.ResumeSession == "" || a.Agent == nil {
		return nil
	}
	id := a.ResumeSession
	if id == "last" {
		all, err := session.List()
		if err != nil || len(all) == 0 {
			return fmt.Errorf("no saved sessions to resume")
		}
		id = all[0].ID
	}
	s, err := session.Load(id)
	if err != nil {
		if fixed, ferr := session.ResolvePrefix(id); ferr == nil {
			s = fixed
		} else {
			return err
		}
	}
	a.session = s
	a.Agent.Reset()
	a.Agent.InjectHistory(s.Messages)
	return nil
}

