// Package tui implements Nova's terminal user interface.
package tui

import (
	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/session"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/tools"
)

// Mode is the current interaction mode.
type Mode int

const (
	// ModeAgent is the default: the model may use tools.
	ModeAgent Mode = iota
	// ModePlan restricts the agent to read-only tools.
	ModePlan
	// ModeAcceptEdits auto-approves file edits.
	ModeAcceptEdits
)

// App wires the TUI to the agent and config.
type App struct {
	Cfg       *config.Config
	Theme     theme.Palette
	Width     int
	Height    int
	Mode      Mode
	Workspace string
	Agent     *agent.Agent
	Registry  *tools.Registry

	// history is the scrollback buffer of rendered lines.
	history *Buffer
	// status is the token-usage line shown on the prompt box's bottom rail.
	status string
	// PromptText holds the last submitted prompt.
	PromptText string
	// LastReply holds the text of the most recent assistant reply (for /copy).
	LastReply string
	// SelectedModel is the provider/model reference in use.
	SelectedModel string
	// Quiet suppresses decorative output.
	Quiet bool
	// session is the persisted conversation.
	session *session.Session
	// edits is the session's record of every file the agent changed, with the
	// text on both sides of each change. It is what /edits reads.
	edits *editLog
	// subs is the session's record of delegated subagent runs, which is what
	// /agents reads.
	subs *subLog
}

// NewApp builds an App with defaults.
func NewApp(cfg *config.Config) *App {
	a := &App{
		Cfg:     cfg,
		Theme:   theme.Get(cfg.Theme),
		Width:   80,
		Height:  24,
		Mode:    ModeAgent,
		status:  "ready",
		history: NewBuffer(10000),
		edits:   newEditLog(),
		subs:    newSubLog(),
	}
	return a
}

// SetSize updates terminal dimensions.
func (a *App) SetSize(w, h int) {
	a.Width, a.Height = w, h
}

// ThemeName returns the active theme name.
func (a *App) ThemeName() string { return a.Theme.Name }
