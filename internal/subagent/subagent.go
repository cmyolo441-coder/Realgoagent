// Package subagent runs nested agents: a model that gets its own conversation,
// its own tool set and its own budget, invoked by the main agent as a tool.
//
// A subagent exists because some work does not belong in the main
// conversation. A survey of a large codebase, a self-contained refactor, a
// search that takes forty tool calls — the parent only needs the conclusion,
// and the intermediate steps would otherwise crowd out its context and its
// attention.
package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/md"
	"github.com/nova-ai/nova/internal/tools"
)

// MaxDepth is how many levels of nesting are allowed. The main agent is depth
// zero, so a subagent it spawns is depth one. The cap matters because each
// level can spawn the next: without it, one confused instruction produces a
// tree of agents and the bill is the user's.
const MaxDepth = 1

// Result is what a finished subagent reports back to its parent.
type Result struct {
	// Reply is the subagent's final text: the answer the parent asked for.
	Reply string
	// Tools is how many tool calls it made, which is the number that explains
	// its cost.
	Tools int
	// Usage is what it spent.
	Usage llm.Usage
	// Duration is the wall time it took.
	Duration time.Duration
	// Err is set when the run failed; Reply then explains why.
	Err error
}

// Spawn describes one subagent run.
type Spawn struct {
	// Name identifies the agent in the UI. Empty derives one from the role.
	Name string
	// Role picks the tool set and the iteration budget.
	Role string
	// Task is the instruction. It becomes the subagent's only user message, so
	// it has to be self-contained: a subagent cannot see the parent's
	// conversation, and inventing that it can wastes a whole run.
	Task string
	// Model names the provider/model to run on. Empty means the parent's.
	Model string
	// Depth is the nesting level of the caller. Zero means the main agent.
	Depth int
}

// Runner builds and runs subagents. It carries what a subagent cannot get on
// its own: the session config, the workspace it is confined to, and the
// approval policy that still applies to its tool calls.
type Runner struct {
	// Config resolves models and the generation settings.
	Config *config.Config
	// Workspace is the root the subagent's tools are confined to. It is
	// deliberately the parent's workspace: a subagent works on the same files
	// the user is looking at.
	Workspace string
	// Confirm is the approval policy, so a risky command inside a subagent
	// asks the user exactly as it would in the main agent.
	Confirm func(tool string, args map[string]any) bool
	// OnEvent receives a line whenever a subagent starts, calls a tool or
	// finishes. It is how the parent — and the user — learn what the child is
	// doing, since the child has no UI of its own.
	OnEvent func(ev Event)
	// MaxToolCalls caps one subagent's tool calls regardless of its role.
	// Zero means the role's own cap applies.
	MaxToolCalls int
}

// EventKind classifies a subagent event.
type EventKind string

const (
	// EvSubStart is emitted when a subagent begins.
	EvSubStart EventKind = "subagent_start"
	// EvSubTool is emitted around each of its tool calls.
	EvSubTool EventKind = "subagent_tool"
	// EvSubDone is emitted when it finishes successfully.
	EvSubDone EventKind = "subagent_done"
	// EvSubFail is emitted when it fails or could not be started.
	EvSubFail EventKind = "subagent_fail"
)

// Event is a progress notice from a subagent.
type Event struct {
	// Agent is the subagent's name.
	Agent string
	// Kind is one of the EventKind constants.
	Kind EventKind
	// Text is the detail line, already formatted for display.
	Text string
	// Tool is set for tool events.
	Tool string
}

// Role presets. Each is a tool set rather than a different personality: the
// model already knows how to search and how to edit, so what a role really
// chooses is which tools it may use and how many turns it gets.
const (
	// RoleExplore searches the workspace and reports. It cannot change a file.
	RoleExplore = "explore"
	// RolePlan investigates and writes the plan, without applying it.
	RolePlan = "plan"
	// RoleGeneral works a self-contained task end to end.
	RoleGeneral = "general"
)

// readOnlyTools observe the workspace without changing it.
var readOnlyTools = []string{
	"read", "list_dir", "glob", "grep", "git_status", "git_diff", "todo", "ask_user",
}

// role is one preset.
type role struct {
	desc     string
	tools    []string
	readOnly bool
	budget   int
}

// roles are the presets the task tool exposes. Restricting by role rather
// than by a free-form tool list is what keeps the model's choice cheap and
// honest about cost.
var roles = map[string]role{
	RoleExplore: {
		desc:     "Search the workspace and report findings. Never changes a file.",
		tools:    readOnlyTools,
		readOnly: true,
		budget:   40,
	},
	RolePlan: {
		desc:     "Investigate a change and write the plan, without applying it.",
		tools:    readOnlyTools,
		readOnly: true,
		budget:   30,
	},
	RoleGeneral: {
		desc: "Work a self-contained task end to end, including edits and commands.",
		tools: []string{
			"read", "write", "edit", "multi_edit", "bash",
			"list_dir", "glob", "grep", "git_status", "git_diff", "todo", "ask_user",
		},
		budget: 60,
	},
}

// DefaultRole is used when the model names a role that does not exist.
const DefaultRole = RoleExplore

// Roles lists the preset names, in a stable order so the model's view of the
// choices does not shuffle between turns.
func Roles() []string {
	out := make([]string, 0, len(roles))
	for k := range roles {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Describe returns the one-line description of a role.
func Describe(name string) string {
	if r, ok := roles[name]; ok {
		return r.desc
	}
	return ""
}

// Default is the role used when none is named.
func Default() role { return roles[DefaultRole] }

// lookup resolves a role name, falling back to the default.
func lookup(name string) (string, role) {
	if r, ok := roles[name]; ok {
		return name, r
	}
	return DefaultRole, roles[DefaultRole]
}

// Run executes a subagent to completion and returns its conclusion.
//
// It blocks, because the tool call that started the subagent has to come back
// with an answer: the parent cannot continue without it. Everything the child
// does meanwhile is reported through OnEvent, so the parent is never silent
// while it waits.
func (r *Runner) Run(ctx context.Context, sp Spawn) Result {
	start := time.Now()

	enter()
	defer leave()

	if r == nil || r.Config == nil {
		return Result{Err: fmt.Errorf("subagents are unavailable: no configuration"), Duration: time.Since(start)}
	}
	if sp.Depth >= MaxDepth {
		return Result{
			Err:      fmt.Errorf("a subagent cannot spawn another subagent: do the work yourself"),
			Duration: time.Since(start),
		}
	}
	task := strings.TrimSpace(sp.Task)
	if task == "" {
		return Result{Err: fmt.Errorf("subagent task is required"), Duration: time.Since(start)}
	}

	roleName, profile := lookup(sp.Role)
	name := strings.TrimSpace(sp.Name)
	if name == "" {
		name = roleName
	}
	emit := func(kind EventKind, text, tool string) {
		if r.OnEvent == nil {
			return
		}
		r.OnEvent(Event{Agent: name, Kind: kind, Text: text, Tool: tool})
	}
	fail := func(err error) Result {
		emit(EvSubFail, err.Error(), "")
		return Result{Err: err, Duration: time.Since(start)}
	}

	prov, model, err := r.Config.ResolveModel(sp.Model)
	if err != nil {
		return fail(fmt.Errorf("subagent: %w", err))
	}

	// The subagent gets its own registry holding only the role's tools, so a
	// tool the role excludes is not merely discouraged — it is absent from the
	// request the model sees, and the task tool that started this is not among
	// them, so the child cannot fan out further.
	reg := tools.NewRegistry(r.Confirm)
	for _, toolName := range profile.tools {
		if tool, ok := tools.New(toolName); ok {
			reg.Register(tool)
		}
	}
	if len(reg.Names()) == 0 {
		return fail(fmt.Errorf("subagent: no tools available for role %q", roleName))
	}
	reg.ReadOnly = profile.readOnly

	budget := profile.budget
	if r.MaxToolCalls > 0 && r.MaxToolCalls < budget {
		budget = r.MaxToolCalls
	}

	var (
		mu        sync.Mutex
		toolCalls int
		reply     strings.Builder
	)

	emit(EvSubStart, fmt.Sprintf("%s · %s", name, model.ID), "")

	sub, err := agent.New(agent.Options{
		Config:    r.Config,
		Provider:  prov,
		Model:     model,
		Workspace: r.Workspace,
		Registry:  reg,
		PlanMode:  reg.ReadOnly,
		// One iteration per tool call, plus one for the final answer: the loop
		// spends an iteration on the reply that asks for a tool and another on
		// the reply that does not.
		MaxIterations: budget + 1,
		Confirmation:  r.Confirm,
		EventSink: func(ev agent.Event) {
			switch ev.Kind {
			case agent.EvText:
				mu.Lock()
				reply.WriteString(ev.Text)
				mu.Unlock()
			case agent.EvToolStart:
				mu.Lock()
				toolCalls++
				mu.Unlock()
				emit(EvSubTool, md.FormatBrief(ev.Tool, ev.Args), ev.Tool)
			case agent.EvToolResult:
				if ev.IsErr {
					emit(EvSubTool, ev.Tool+" failed: "+firstLine(ev.Output), ev.Tool)
				}
			}
		},
	})
	if err != nil {
		return fail(err)
	}

	// The run ends in exactly one of three ways, and the parent needs a
	// usable answer in all three: the task done, the task refused, or the
	// depth cap hit mid-way. Cancelling the context is what the main agent
	// does to a running turn, so a stopped parent stops its children too.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runErr := sub.RunTask(runCtx, task)

	mu.Lock()
	res := Result{
		Reply:    strings.TrimSpace(reply.String()),
		Tools:    toolCalls,
		Usage:    sub.Usage(),
		Duration: time.Since(start),
	}
	mu.Unlock()

	if runErr != nil {
		res.Err = runErr
		emit(EvSubFail, runErr.Error(), "")
		return res
	}
	if res.Reply == "" {
		// A subagent that says nothing is not a result the parent can use, and
		// an empty tool result would read as "nothing found", which is a
		// different and wrong claim.
		res.Err = fmt.Errorf("subagent %q finished without answering", name)
		emit(EvSubFail, res.Err.Error(), "")
		return res
	}
	emit(EvSubDone, fmt.Sprintf("%s · %d tool call%s · %s",
		name, res.Tools, plural(res.Tools), res.Duration.Round(time.Millisecond)), "")
	return res
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Inflight counts the subagents currently running, so the UI can show it.
var inflight struct {
	sync.Mutex
	n int
}

// Inflight reports how many subagents are running right now.
func Inflight() int {
	inflight.Lock()
	defer inflight.Unlock()
	return inflight.n
}

func enter() {
	inflight.Lock()
	inflight.n++
	inflight.Unlock()
}

func leave() {
	inflight.Lock()
	if inflight.n > 0 {
		inflight.n--
	}
	inflight.Unlock()
}
