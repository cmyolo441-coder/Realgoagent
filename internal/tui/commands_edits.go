package tui

import (
	"fmt"
	"strings"

	"github.com/nova-ai/nova/internal/editdiff"
	"github.com/nova-ai/nova/internal/subagent"
	"github.com/nova-ai/nova/internal/util"
)

// Commands for the two live views: what the agent changed, and what it
// delegated. Both are overlays rather than printed output, because the answer
// to "what did it change" is a list that has to be walked, and printing it
// would bury the conversation under a wall of diff.

// cmdEdits opens the edit viewer. With an argument it opens on the entry for
// that path, so a user who already knows the file does not have to hunt for it.
func cmdEdits(t *TUI, args string) error {
	entries := t.app.edits.list()
	if len(entries) == 0 {
		t.app.history.Append(t.app.Theme.Style("dim", "no edits recorded yet — the agent has not changed a file this session"))
		return nil
	}
	v := newEditViewer()
	if want := strings.TrimSpace(args); want != "" {
		// A suffix match, so "edits agent" finds internal/agent/agent.go
		// without the user typing the full relative path.
		for i := len(entries) - 1; i >= 0; i-- {
			if strings.Contains(entries[i].Path, want) {
				v.sel = i
				break
			}
		}
	}
	t.editView = v
	t.paletteDismissed = true
	t.scheduleDraw()
	return nil
}

// cmdEditsShow prints one edit as a unified diff and nothing else. It is the
// copy-paste path: the overlay is for reading, this is for a bug report.
func cmdEditsShow(t *TUI, args string) error {
	want := strings.TrimSpace(args)
	entries := t.app.edits.list()
	if len(entries) == 0 {
		t.app.history.Append(t.app.Theme.Style("dim", "no edits recorded yet"))
		return nil
	}
	// No argument means the most recent change, which is the one the user is
	// almost always asking about.
	e := entries[len(entries)-1]
	if want != "" {
		found := false
		for i := len(entries) - 1; i >= 0; i-- {
			if strings.Contains(entries[i].Path, want) {
				e = entries[i]
				found = true
				break
			}
		}
		if !found {
			t.app.history.Append(t.app.Theme.Style("error", "no recorded edit matches "+want))
			return nil
		}
	}
	body := e.Diff()
	if body == "" {
		t.app.history.Append(t.app.Theme.Style("dim", "no line-level change recorded for "+e.Path))
		return nil
	}
	t.app.history.Append(t.app.Theme.Style("accent_bold", e.Path) + editStat(t.app.Theme, e.Stat()))
	t.app.history.Append(editdiff.RenderBody(t.app.Theme, body, t.app.Width)...)
	return nil
}

// cmdEditsClear drops the recorded history without touching the workspace.
func cmdEditsClear(t *TUI, args string) error {
	n := t.app.edits.count()
	t.app.edits.clear()
	t.closeEditViewer()
	t.app.history.Append(t.app.Theme.Style("dim", fmt.Sprintf("forgot %d recorded edit(s); the files on disk are unchanged", n)))
	return nil
}

// cmdAgents opens the subagent view.
func cmdAgents(t *TUI, args string) error {
	if t.app.subs.count() == 0 {
		t.app.history.Append(t.app.Theme.Style("dim", "no subagents have run this session"))
		return nil
	}
	t.agents = &subView{}
	t.paletteDismissed = true
	t.scheduleDraw()
	return nil
}

// cmdTask delegates a job to a subagent without going through the model, so
// the user can spend one on demand: "/task explore find every caller of X".
//
// The subagent's answer comes back through the same path the model's would, and
// it is added to the conversation as a user turn so the model can act on it.
func cmdTask(t *TUI, args string) error {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		t.app.history.Append(t.app.Theme.Style("error", "usage: /task <explore|plan|general> <what to do>"))
		t.app.history.Append(t.app.Theme.Style("dim", "roles: "+strings.Join(subagent.Roles(), ", ")))
		return nil
	}
	role := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
	if strings.TrimSpace(rest) == "" {
		t.app.history.Append(t.app.Theme.Style("error", "usage: /task <"+strings.Join(subagent.Roles(), "|")+"> <what to do>"))
		return nil
	}
	if subagent.Describe(role) == "" {
		t.app.history.Append(t.app.Theme.Style("error", "unknown role "+role))
		for _, r := range subagent.Roles() {
			t.app.history.Append("  " + describeRoleLine(r, subagent.Describe(r)))
		}
		return nil
	}
	prompt := fmt.Sprintf("Run a %s subagent on the task below and use its answer to decide what to do next.\n\nTask: %s", role, rest)
	t.app.history.Append(t.app.Theme.Style("accent", "❯ ") + "/task " + role + " " + rest)
	t.app.history.Append("")
	return t.Submit(prompt)
}

// cmdRoles lists the subagent roles, so the choice is discoverable without
// reading the tool description the model sees.
func cmdRoles(t *TUI, args string) error {
	p := t.app.Theme
	t.app.history.Append(p.Style("accent_bold", "Subagent roles"))
	t.app.history.Append("")
	for _, r := range subagent.Roles() {
		t.app.history.Append(describeRoleLine(r, subagent.Describe(r)))
	}
	t.app.history.Append("")
	t.app.history.Append(p.Style("dim", "start one yourself with: /task <role> <what to do>"))
	t.app.history.Append(p.Style("dim", "the model delegates on its own too; /agents shows what is running"))
	return nil
}

// editViewerCommandCompletes is the set of command names this file adds. It
// exists so the registry in commands.go stays a plain list and the tests can
// assert the new commands are present.
func editViewerCommandCompletes() []string {
	return []string{"/edits", "/edits-show", "/edits-clear", "/agents", "/task", "/roles"}
}

var _ = util.Truncate
