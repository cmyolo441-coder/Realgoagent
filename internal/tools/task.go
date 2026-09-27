package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Runner is what the task tool needs from a subagent host. It is an interface
// so `tools` does not import `subagent`, which imports `tools` — the
// dependency would be a cycle, and the alternative is a build failure rather
// than an inconvenience.
type Runner interface {
	// RunTask executes one subagent synchronously and returns its report.
	RunTask(ctx context.Context, role, task, model string) TaskResult
}

// TaskResult is a subagent's report, as the tool sees it.
type TaskResult struct {
	// Reply is the subagent's answer.
	Reply string
	// Tools is how many tool calls it made.
	Tools int
	// Err is set when the subagent failed.
	Err error
	// Report is the one-line summary shown in the transcript.
	Report string
}

// SetRunner installs the subagent host used by the task tool. Passing nil
// removes it, and with no runner the tool refuses every call rather than
// pretending to work.
func SetRunner(r Runner) {
	runnerMu.Lock()
	taskRunner = r
	runnerMu.Unlock()
}

var (
	runnerMu    sync.RWMutex
	taskRunner  Runner
	runnerDepth int
)

// RoleDescription returns the published description of a role, or "" when the
// role is unknown. It lets a caller check the hand-off between the subagent
// host and this package without reaching into private state.
func RoleDescription(name string) string { return roleDesc(name) }

// Roles lists the published role names, sorted.
func Roles() []string {
	rolesMu.RLock()
	out := make([]string, 0, len(roleCatalog))
	for k := range roleCatalog {
		out = append(out, k)
	}
	rolesMu.RUnlock()
	sortStrings(out)
	return out
}

// CurrentDepth is the nesting level of the agent using this process's tools.
// The task tool reads it to refuse a subagent that tries to spawn another,
// which is what keeps a confused instruction from turning into a tree of
// agents billed to the user.
func CurrentDepth() int {
	runnerMu.RLock()
	defer runnerMu.RUnlock()
	return runnerDepth
}

// WithDepth runs fn with the nesting level set to depth. The subagent host
// calls it around a subagent's run, so the rule holds without threading a
// depth argument through every tool call.
func WithDepth(depth int, fn func()) {
	runnerMu.Lock()
	prev := runnerDepth
	runnerDepth = depth
	runnerMu.Unlock()
	defer func() {
		runnerMu.Lock()
		runnerDepth = prev
		runnerMu.Unlock()
	}()
	fn()
}

// ---- the task tool ----

// subagentRoles is documented in the tool description. It is injected rather
// than hardcoded so the description and the subagent package cannot drift
// apart: the model must be offered exactly the roles that exist.
var (
	rolesMu     sync.RWMutex
	roleCatalog = map[string]string{
		"explore": "Search the workspace and report findings. Never changes a file.",
		"plan":    "Investigate a change and write the plan, without applying it.",
		"general": "Work a self-contained task end to end, including edits and commands.",
	}
)

// SetRoleCatalog tells the task tool which roles exist and what each is for.
func SetRoleCatalog(catalog map[string]string) {
	rolesMu.Lock()
	roleCatalog = catalog
	rolesMu.Unlock()
}

func roleDesc(name string) string {
	rolesMu.RLock()
	defer rolesMu.RUnlock()
	return roleCatalog[name]
}

type taskTool struct{}

func (taskTool) Name() string { return "task" }

func (taskTool) Description() string {
	rolesMu.RLock()
	var b strings.Builder
	b.WriteString("Delegate a self-contained job to a subagent: a second model with its own conversation, " +
		"its own tools and its own turn budget. Use it when work needs many steps " +
		"(surveying a subsystem, chasing down a bug across files) and you only need the conclusion. " +
		"The task must stand alone — the subagent cannot see this conversation. " +
		"The subagent reports back its final answer; its intermediate steps do not enter your context. " +
		"Available roles: ")
	names := make([]string, 0, len(roleCatalog))
	for k := range roleCatalog {
		names = append(names, k)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	for i, n := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s (%s)", n, roleCatalog[n])
	}
	rolesMu.RUnlock()
	return b.String()
}

func (taskTool) Schema() map[string]any {
	rolesMu.RLock()
	enum := make([]string, 0, len(roleCatalog))
	for k := range roleCatalog {
		enum = append(enum, k)
	}
	rolesMu.RUnlock()
	for i := 1; i < len(enum); i++ {
		for j := i; j > 0 && enum[j] < enum[j-1]; j-- {
			enum[j], enum[j-1] = enum[j-1], enum[j]
		}
	}
	roleSchema := prop("role", "string", "Which subagent to run: "+strings.Join(enum, " | "))
	roleSchema["enum"] = enum
	return obj(
		req("task", "The complete, self-contained instruction for the subagent. It cannot see this conversation, so include every path, symbol and constraint it needs."),
		req("role", "The subagent role: "+strings.Join(enum, " | ")),
		prop("model", "string", "Provider/model to run the subagent on, e.g. kiosai/grok. Omit to use your own model."),
	)
}

func (taskTool) Run(ctx context.Context, a map[string]any) *Result {
	task, _ := argString(a, "task")
	role, _ := argString(a, "role")
	model, _ := argString(a, "model")

	if d := CurrentDepth(); d >= maxSubagentDepth {
		return &Result{
			Output:  fmt.Sprintf("You are already a subagent and cannot delegate further. Do the work yourself with your own tools (you have: %s).", strings.Join(roleToolsFor(role), ", ")),
			IsError: true,
		}
	}

	runnerMu.RLock()
	r := taskRunner
	runnerMu.RUnlock()
	if r == nil {
		return &Result{
			Output:  "subagents are not available in this session. Do the work yourself with your own tools.",
			IsError: true,
		}
	}
	if strings.TrimSpace(task) == "" {
		return &Result{Output: "task: 'task' is required", IsError: true}
	}
	if strings.TrimSpace(role) == "" {
		role = DefaultSubagentRole
	} else if roleDesc(role) == "" {
		known := make([]string, 0, 4)
		rolesMu.RLock()
		for k := range roleCatalog {
			known = append(known, k)
		}
		rolesMu.RUnlock()
		sortStrings(known)
		return &Result{
			Output:  fmt.Sprintf("task: unknown role %q; use one of: %s", role, strings.Join(known, ", ")),
			IsError: true,
		}
	}

	// The call blocks until the subagent answers: the parent cannot continue
	// without its conclusion. Progress arrives through the Runner's event hook
	// and is shown to the user meanwhile.
	res := r.RunTask(ctx, role, task, model)
	if res.Err != nil {
		return &Result{
			Output:  fmt.Sprintf("subagent (%s) failed: %v", role, res.Err),
			IsError: true,
		}
	}
	out := res.Reply
	if res.Report != "" {
		out = res.Report + "\n\n" + out
	}
	return &Result{Output: out, Meta: map[string]string{"role": role, "tools": fmt.Sprint(res.Tools)}}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// maxSubagentDepth and DefaultSubagentRole are injected by the subagent host,
// so the two packages agree without one importing the other.
var (
	maxSubagentDepth    = 1
	DefaultSubagentRole = "explore"
)

// ConfigureTaskTool tells the task tool the nesting cap, the default role and
// the role catalogue. The subagent host calls it at startup.
func ConfigureTaskTool(depth int, defaultRole string, catalog map[string]string) {
	if depth > 0 {
		maxSubagentDepth = depth
	}
	if defaultRole != "" {
		DefaultSubagentRole = defaultRole
	}
	if len(catalog) > 0 {
		SetRoleCatalog(catalog)
	}
}

// roleToolsFor lists what a subagent of the given role could use, for the
// refusal message. It is advisory: the point is to tell the child what to do
// instead of delegating, not to enforce anything.
func roleToolsFor(role string) []string {
	rolesMu.RLock()
	defer rolesMu.RUnlock()
	if t, ok := roleTools[role]; ok {
		return t
	}
	return roleTools[DefaultSubagentRole]
}

var roleTools = map[string][]string{
	"explore": {"read", "list_dir", "glob", "grep", "git_status", "git_diff", "todo", "ask_user"},
	"plan":    {"read", "list_dir", "glob", "grep", "git_status", "git_diff", "todo", "ask_user"},
	"general": {"read", "write", "edit", "multi_edit", "bash", "list_dir", "glob", "grep", "git_status", "git_diff", "todo", "ask_user"},
}
