package subagent

import (
	"context"
	"fmt"

	"github.com/nova-ai/nova/internal/tools"
)

// Install wires this package into the tool layer: it publishes the role
// catalogue the task tool advertises, publishes the depth cap, and hands the
// task tool a way to actually run a subagent.
//
// It must be called once per session, before the agent is built. The tool layer
// keeps these process-global because a tool has no reference back to the agent
// that dispatched it; threading a runner through every call to avoid that
// would move the same state around without removing it.
func Install(r *Runner) {
	catalog := make(map[string]string, len(roles))
	for name, profile := range roles {
		catalog[name] = profile.desc
	}
	tools.ConfigureTaskTool(MaxDepth, DefaultRole, catalog)
	tools.SetRunner(&adapter{runner: r})
}

// adapter satisfies tools.Runner. The conversion is deliberately one-way:
// tools cannot see this package, so all it learns about subagents is the
// reply, the call count and the failure — which is exactly what the model is
// entitled to know about a job it delegated.
type adapter struct {
	runner *Runner
}

func (a *adapter) RunTask(ctx context.Context, role, task, model string) tools.TaskResult {
	if a == nil || a.runner == nil {
		return tools.TaskResult{Err: fmt.Errorf("subagents are unavailable")}
	}
	// Depth is the level of the *caller*: the main agent delegates at 0, so a
	// subagent runs at 1. It is captured before the increment, because
	// WithDepth's callback already sees the deeper level and passing that in
	// would compare the child against its own depth and refuse every call.
	depth := tools.CurrentDepth()
	var res Result
	tools.WithDepth(depth+1, func() {
		res = a.runner.Run(ctx, Spawn{Role: role, Task: task, Model: model, Depth: depth})
	})
	if res.Err != nil {
		return tools.TaskResult{Err: res.Err}
	}
	return tools.TaskResult{
		Reply: res.Reply,
		Tools: res.Tools,
		Report: fmt.Sprintf("subagent (%s): %d tool call(s), %s, %d tokens",
			role, res.Tools, res.Duration.Round(1e6), res.Usage.TotalTokens),
	}
}
