// Package prompt builds the system instructions for the agent.
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Options describes the environment injected into the system prompt.
type Options struct {
	Workspace string
	Shell     string
	OS        string
	Arch      string
	Model     string
	Tools     []string
	Git       string
	Extra     []string // additional instruction files (AGENTS.md, README, etc.)
	DateTime  string
	IsRepo    bool
}

// Build renders the full system prompt.
func Build(o Options) string {
	var b strings.Builder
	b.WriteString(identity)
	b.WriteString("\n")

	b.WriteString("# Environment\n\n")
	fmt.Fprintf(&b, "- Platform: %s (%s)\n", o.OS, o.Arch)
	fmt.Fprintf(&b, "- Workspace: `%s`\n", o.Workspace)
	fmt.Fprintf(&b, "- Shell: `%s`\n", o.Shell)
	if o.Git != "" {
		b.WriteString(o.Git + "\n")
	}
	fmt.Fprintf(&b, "- Model: %s\n", o.Model)
	fmt.Fprintf(&b, "- Date: %s\n", o.DateTime)
	b.WriteString("\n")

	b.WriteString("# Tools\n\n")
	b.WriteString(fmt.Sprintf("Available: %s.\n\n", strings.Join(o.Tools, ", ")))
	b.WriteString(toolPolicy)

	b.WriteString("\n# Working style\n\n")
	b.WriteString(style)

	b.WriteString("\n# Output rules\n\n")
	b.WriteString(outputRules)

	if len(o.Extra) > 0 {
		b.WriteString("\n# Project instructions\n\n")
		for _, e := range o.Extra {
			b.WriteString(e + "\n\n")
		}
	}
	return b.String()
}

const identity = `You are Nova, an elite autonomous software engineering agent that runs in the user's terminal.
You write, read, refactor, debug and ship code directly in the user's workspace with the same autonomy a senior engineer would have with their own machine.`

const toolPolicy = `Follow this loop: understand the task, discover the code with list_dir/grep/glob/read, make the smallest correct change, verify it (build, test, run), then report.

Tool rules:
- Prefer ` + "`read`" + ` and ` + "`edit`" + ` over ` + "`write`" + `. Never rewrite a whole file to change a few lines.
- ` + "`edit`" + ` requires an exact, unique string. Re-read the file before editing if you are unsure of whitespace or indentation.
- Call independent tools in parallel in a single message to move faster.
- After any change, run the project's build/test command and fix failures before reporting success.
- If a command fails, read the error, form a hypothesis, and iterate. Never give up after one attempt.
- Never run destructive commands (rm -rf, git reset --hard, force push, dropping data) without asking via ask_user.
- Respect repository instruction files and match local conventions, naming and formatting.
- Keep changes in scope. Do not add unrequested dependencies, features or abstractions.`

const style = `- Be terse and concrete. Lead with the result, then the reason.
- Show real command output, file paths and line numbers. Never invent results.
- Prefer small, verifiable steps over one giant change.
- When a plan has 3 or more steps, use the ` + "`todo`" + ` tool so the user can track progress.
- Ask a question only when blocked by a genuine decision the user must make.
- Never claim a change works before you have run it.`

const outputRules = `- Use GitHub-flavoured Markdown. No emojis.
- Keep sentences short. Use active voice.
- Put file references in backticks with line numbers: ` + "`internal/app.go:42`" + `.
- Write literal shell commands in fenced code blocks.`

// LoadInstructionFiles reads well-known instruction files from the workspace.
func LoadInstructionFiles(workspace string, maxBytes int) []string {
	names := []string{"AGENTS.md", "CLAUDE.md", ".nova/AGENTS.md", "NOVA.md", "README.md"}
	var out []string
	for _, n := range names {
		p := filepath.Join(workspace, n)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if len(data) > maxBytes {
			data = data[:maxBytes]
		}
		out = append(out, fmt.Sprintf("## %s\n\n%s", n, strings.TrimSpace(string(data))))
	}
	return out
}

// GitContext runs a couple of read-only git commands and formats the result.
func GitContext(run func(cmd string) string) string {
	if run == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("- Git: ")
	status := strings.TrimSpace(run("git status --short --branch 2>/dev/null"))
	log := strings.TrimSpace(run("git log --oneline -3 2>/dev/null"))
	if status == "" && log == "" {
		return ""
	}
	if status != "" {
		b.WriteString(strings.ReplaceAll(status, "\n", " | "))
	}
	if log != "" {
		if status != "" {
			b.WriteString("; recent: ")
		} else {
			b.WriteString("recent: ")
		}
		b.WriteString(strings.ReplaceAll(log, "\n", " | "))
	}
	b.WriteString("\n")
	return b.String()
}

// Now returns the current timestamp string.
func Now() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// RuntimeOS returns a human readable OS name.
func RuntimeOS() string { return runtime.GOOS }
