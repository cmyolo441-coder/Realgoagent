package tui

import (
	"os/exec"
	"strings"

	"github.com/nova-ai/nova/internal/md"
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

// showApproval renders the confirmation block for a risky tool call.
//
// It runs on the event-loop goroutine, driven by an EvToolApproval event. The
// agent's own goroutine must never touch the display buffer: the loop reads
// that buffer on every repaint, and a concurrent append would race with it.
func (t *TUI) showApproval(name string, args map[string]any) {
	p := t.app.Theme
	t.app.history.Append(p.Style("warning", "approve? ") + p.Style("tool", md.FormatBrief(name, args)))
	if cmd, ok := args["command"].(string); ok {
		t.app.history.Append(p.Style("dim", "  $ "+strings.TrimSpace(cmd)))
	} else if path, ok := args["path"].(string); ok {
		t.app.history.Append(p.Style("dim", "  path: "+path))
	}
	t.app.history.Append(p.Style("dim", "  y to run it, anything else to skip"))
}
