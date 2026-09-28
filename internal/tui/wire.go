package tui

import (
	"os/exec"
	"strings"
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

