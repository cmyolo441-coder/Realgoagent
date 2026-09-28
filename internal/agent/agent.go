// Package agent implements Nova's tool-calling agent loop.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/prompt"
	"github.com/nova-ai/nova/internal/tools"
)

// EventKind classifies events emitted by the Agent.
type EventKind string

const (
	EvText       EventKind = "text"
	EvThinking   EventKind = "thinking"
	EvToolStart  EventKind = "tool_start"
	EvToolResult EventKind = "tool_result"
	EvToolDenied EventKind = "tool_denied"
	// EvReasoningHidden reports how much of the model's internal monologue was
	// filtered out of a reply, so the UI can say so rather than silently
	// losing text.
	EvReasoningHidden EventKind = "reasoning_hidden"
	EvAskUser         EventKind = "ask_user"
	EvIteration       EventKind = "iteration"
	EvError           EventKind = "error"
	EvDone            EventKind = "done"
	EvUsage           EventKind = "usage"
	EvModelWait       EventKind = "model_wait"
	// EvToolProgress streams partial tool-call arguments while the model is
	// still producing them, so the front-end can preview a write line by
	// line instead of showing the diff only after the tool has run.
	EvToolProgress EventKind = "tool_progress"
	// EvEdit reports a file change the agent just made, with the text on both
	// sides of it, so the front-end can show the change as it lands rather
	// than leaving the user to reconstruct it from a final diff.
	EvEdit EventKind = "edit"
)

// Event is a single update from the agent.
type Event struct {
	Kind   EventKind
	Text   string
	Tool   string
	Args   map[string]any
	Output string
	IsErr  bool
	Dur    time.Duration
	Usage  *llm.Usage
	Task   string // for todo tool
	// Edit carries the before/after text of a file change, set on EvEdit.
	Edit *tools.EditRecord
}

// Options configures an Agent instance.
type Options struct {
	Config    *config.Config
	Provider  *config.Provider
	Model     *config.Model
	Workspace string
	Registry  *tools.Registry
	// PlanMode restricts the loop to read-only tools. It is the only gate:
	// a tool outside read-only mode runs as soon as the model asks for it.
	PlanMode bool
	// OnAskUser is invoked when the model calls ask_user.
	OnAskUser func(question string) string
	// EventSink receives all events; may be nil.
	EventSink func(Event)
	// OnDone fires once a turn finishes (used for session persistence).
	OnDone func()
	// MaxIterations caps the loop.
	MaxIterations int
	Temperature   float64
	MaxTokens     int
}

// SetPlanMode toggles read-only operation at runtime.
func (a *Agent) SetPlanMode(on bool) {
	a.mu.Lock()
	a.opt.PlanMode = on
	a.opt.Registry.ReadOnly = on
	a.toolNames = a.opt.Registry.Names()
	a.mu.Unlock()
}

// readOnly reports whether the registry is currently in read-only mode.
func (a *Agent) readOnly() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opt.Registry.ReadOnly
}

// onAskUser returns the ask_user callback, if the front-end installed one.
func (a *Agent) onAskUser() func(question string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opt.OnAskUser
}

// PlanMode reports whether the agent is read-only.
func (a *Agent) PlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opt.PlanMode
}

// Agent runs the conversation loop.
type Agent struct {
	opt      Options
	client   *llm.Client
	mu       sync.Mutex
	messages []llm.Message
	// totalUsage accumulates across the whole conversation, not one turn.
	totalUsage llm.Usage
	toolNames  []string
	cwd        string
	ctx        context.Context
	cancel     context.CancelFunc
	// sinkMu guards opt.EventSink on its own, separate from mu. A sink may
	// block — on the UI bus, or on the user answering a prompt — so it must
	// never be invoked with the agent state locked.
	sinkMu sync.Mutex
}

// New constructs an Agent.
func New(o Options) (*Agent, error) {
	if o.Config == nil {
		return nil, fmt.Errorf("agent: nil config")
	}
	if o.Provider == nil || o.Model == nil {
		return nil, fmt.Errorf("agent: provider and model are required")
	}
	if o.Registry == nil {
		o.Registry = tools.NewRegistry()
		tools.RegisterDefaults(o.Registry)
	}
	if o.EventSink == nil {
		o.EventSink = func(Event) {}
	}
	if o.MaxIterations <= 0 {
		o.MaxIterations = o.Config.MaxIterations
	}
	if o.MaxIterations <= 0 {
		o.MaxIterations = 60
	}

	o.Registry.ReadOnly = o.PlanMode
	a := &Agent{
		opt:       o,
		client:    llm.NewClient(o.Provider.BaseURL, o.Provider.APIKeyFromEnv(), o.Model.ID),
		messages:  []llm.Message{},
		toolNames: o.Registry.Names(),
		cwd:       o.Workspace,
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	return a, nil
}

// SystemPrompt renders the current system prompt.
func (a *Agent) SystemPrompt() string {
	a.mu.Lock()
	plan := a.opt.PlanMode
	names := append([]string{}, a.toolNames...)
	ws := a.opt.Workspace
	model := a.opt.Provider.Name + "/" + a.opt.Model.ID
	a.mu.Unlock()

	var extra []string
	if plan {
		extra = append(extra, "## Plan mode\n\n"+
			"You are in read-only plan mode. The tools `write`, `edit`, `multi_edit`, `patch` and `bash` are disabled.\n"+
			"Investigate and propose a plan instead of applying it. Describe each change with exact file paths and the commands\n"+
			"the user should run to apply it. Do not attempt to write files.")
	}
	extra = append(extra, prompt.LoadInstructionFiles(ws, 8192)...)

	return prompt.Build(prompt.Options{
		Workspace: ws,
		Shell:     a.cfgShell(),
		OS:        prompt.RuntimeOS(),
		Model:     model,
		Tools:     names,
		DateTime:  prompt.Now(),
		Extra:     extra,
		Git: prompt.GitContext(func(cmd string) string {
			out, _ := runGit(cmd, ws)
			return out
		}),
	})
}

func (a *Agent) cfgShell() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.opt.Config.Shell; s != "" {
		return s
	}
	return "/bin/sh"
}

// SetOnDone sets the per-turn completion callback.
func (a *Agent) SetOnDone(f func()) {
	a.mu.Lock()
	a.opt.OnDone = f
	a.mu.Unlock()
}

// onDoneFn returns the per-turn completion callback. It is called with the
// lock released: the hook persists the session, and persisting reads the
// conversation back through Messages and Usage.
func (a *Agent) onDoneFn() func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opt.OnDone
}

// SetSink replaces the event sink after construction.
func (a *Agent) SetSink(f func(Event)) {
	a.sinkMu.Lock()
	a.opt.EventSink = f
	a.sinkMu.Unlock()
}

// Cancel stops a running turn. The loop notices at its next checkpoint and
// reports the turn as stopped rather than as a failure.
func (a *Agent) Cancel() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Reset clears conversation history.
func (a *Agent) Reset() {
	a.mu.Lock()
	a.messages = []llm.Message{}
	a.totalUsage = llm.Usage{}
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Messages returns a copy of the conversation history.
func (a *Agent) Messages() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]llm.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// Usage returns cumulative token usage.
func (a *Agent) Usage() llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totalUsage
}

// InjectHistory seeds the conversation with prior messages (session resume).
func (a *Agent) InjectHistory(msgs []llm.Message) {
	a.mu.Lock()
	a.messages = append([]llm.Message{}, msgs...)
	a.totalUsage = llm.Usage{}
	a.mu.Unlock()
}

// Run processes one user turn to completion.
//
// Every exit path reports EvDone and fires OnDone exactly once. A front-end
// that watches those signals cannot otherwise tell a finished turn from one
// that died, and would sit in its "working" state forever.
func (a *Agent) Run(userText string) error {
	return a.RunTask(context.Background(), userText)
}

// RunTask is Run with a caller-supplied context, so a parent can stop a child
// by cancelling the context it passed in. The turn's own Cancel still works on
// top of it: the agent derives a cancellable child of the given context, so
// whichever fires first stops the turn.
func (a *Agent) RunTask(parent context.Context, userText string) (err error) {
	if parent == nil {
		parent = context.Background()
	}
	// A fresh context per turn: the previous turn's cancel must not poison the
	// next one, or every message after a Ctrl-C would fail instantly.
	a.mu.Lock()
	prev := a.cancel
	a.mu.Unlock()
	if prev != nil {
		prev()
	}
	// Render the system prompt before taking the lock: SystemPrompt snapshots
	// agent state itself and would otherwise deadlock on a.mu.
	system := a.SystemPrompt()

	turnCtx, turnCancel := context.WithCancel(parent)
	a.mu.Lock()
	a.ctx, a.cancel = turnCtx, turnCancel
	// Replace the system message to prevent unbounded growth
	if len(a.messages) > 0 && a.messages[0].Role == llm.RoleSystem {
		a.messages[0] = llm.Message{Role: llm.RoleSystem, Content: system}
	} else {
		a.messages = append([]llm.Message{{Role: llm.RoleSystem, Content: system}}, a.messages...)
	}
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: userText})
	a.mu.Unlock()
	// Release the turn's context on every exit path. A stream reader or a tool
	// still winding down watches this context, so cancelling it here is what
	// stops it from holding a connection open for a turn that is already over.
	defer turnCancel()

	// completed is set by the success path. Every other exit — a stream error,
	// a cancelled context, the iteration cap — still has to announce the end of
	// the turn and persist the session, or the front-end is left spinning.
	start := time.Now()
	completed := false
	iterations := 0
	defer func() {
		summary := "stopped"
		if completed {
			summary = fmt.Sprintf("completed in %s (%d iterations)",
				time.Since(start).Round(time.Millisecond), iterations)
		}
		a.emit(Event{Kind: EvUsage, Usage: a.usageCopy()})
		a.emit(Event{Kind: EvDone, Text: summary})
		if onDone := a.onDoneFn(); onDone != nil {
			onDone()
		}
	}()

	maxIter := a.opt.MaxIterations
	for iter := 1; iter <= maxIter; iter++ {
		iterations = iter
		select {
		case <-turnCtx.Done():
			return turnCtx.Err()
		default:
		}
		a.emit(Event{Kind: EvIteration, Text: fmt.Sprintf("%d", iter)})

		req := a.buildRequest()
		chunks := a.client.Stream(turnCtx, req)

		var streamed strings.Builder
		var toolCalls []llm.ToolCall
		var usage *llm.Usage
		// Reasoning arrives inline in the content stream; it is filtered out
		// here so it never reaches the screen or the saved conversation.
		think := &ThinkFilter{}

		for c := range chunks {
			if c.Err != nil {
				// A cancelled context means the user pressed Esc, not that
				// something broke. The deferred handler reports the turn as
				// stopped; printing an error here would read as a failure.
				if !errors.Is(c.Err, context.Canceled) {
					a.emit(Event{Kind: EvError, Text: c.Err.Error(), IsErr: true})
				}
				return c.Err
			}
			if c.Delta.Content != "" {
				text := think.Feed(c.Delta.Content)
				if text != "" {
					streamed.WriteString(text)
					a.emit(Event{Kind: EvText, Text: text})
				}
			}
			for _, tc := range c.Delta.Tools {
				streamMerge(&toolCalls, tc)
				// Emit the merged arguments so far: the UI keeps only the
				// latest frame per tool call and diffs it against the file
				// on disk, which is what makes a 10-line write appear line
				// by line while the model is still producing it.
				if merged := mergedToolCall(toolCalls, tc); merged != nil {
					a.emit(Event{Kind: EvToolProgress, Tool: merged.Function.Name, Output: merged.Function.Arguments, Text: merged.ID})
				}
			}
			if c.Usage != nil {
				u := *c.Usage
				usage = &u
			}
		}
		// A stream that closed because the turn was cancelled must not be
		// mistaken for a complete reply. The HTTP body can reach EOF at the
		// same moment as the cancel, in which case no error chunk arrives and
		// the tool calls from the aborted reply would run after the user
		// pressed Esc.
		if cerr := turnCtx.Err(); cerr != nil {
			if !errors.Is(cerr, context.Canceled) {
				a.emit(Event{Kind: EvError, Text: cerr.Error(), IsErr: true})
			}
			return cerr
		}
		// A reply that ended mid-reasoning still has to deliver whatever came
		// before the opening tag.
		if text := think.Flush(); text != "" {
			streamed.WriteString(text)
			a.emit(Event{Kind: EvText, Text: text})
		}
		if think.Hidden() > 0 {
			a.emit(Event{Kind: EvReasoningHidden, Text: fmt.Sprint(think.Hidden())})
		}

		msg := llm.Message{Role: llm.RoleAssistant, Content: streamed.String(), ToolCalls: toolCalls}
		a.mu.Lock()
		a.messages = append(a.messages, msg)
		if usage != nil {
			a.totalUsage.PromptTokens += usage.PromptTokens
			a.totalUsage.CompletionTokens += usage.CompletionTokens
			a.totalUsage.TotalTokens += usage.TotalTokens
		}
		a.mu.Unlock()

		if len(toolCalls) == 0 {
			completed = true
			return nil
		}

		// Execute tool calls; they run sequentially to keep ordering readable.
		// They run with no lock held: a tool can block on an ask_user answer,
		// and holding a.mu across that would deadlock every other method on the
		// agent, including Cancel, so Esc could not stop the turn.
		toolMsgs := a.runToolCalls(turnCtx, toolCalls)
		a.appendMessages(toolMsgs)
	}
	a.emit(Event{Kind: EvError, Text: fmt.Sprintf("reached max iterations (%d)", maxIter), IsErr: true})
	return fmt.Errorf("max iterations reached (%d)", maxIter)
}

// appendMessages extends the conversation history.
func (a *Agent) appendMessages(msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	a.mu.Lock()
	a.messages = append(a.messages, msgs...)
	a.mu.Unlock()
}

func (a *Agent) buildRequest() llm.Request {
	// Render the system prompt before taking the lock: SystemPrompt snapshots
	// agent state itself and would otherwise deadlock on a.mu.
	system := a.SystemPrompt()

	a.mu.Lock()
	msgs := make([]llm.Message, 0, len(a.messages)+1)
	// send only the newest system prompt
	for _, m := range a.messages {
		if m.Role == llm.RoleSystem {
			continue
		}
		msgs = append(msgs, m)
	}
	temp := a.opt.Config.Temperature
	registry := a.opt.Registry
	a.mu.Unlock()
	msgs = append([]llm.Message{{Role: llm.RoleSystem, Content: system}}, msgs...)

	llmTools := make([]llm.Tool, 0)
	for _, t := range registry.List() {
		llmTools = append(llmTools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return llm.Request{
		Messages:    msgs,
		Tools:       llmTools,
		Temperature: temp,
		MaxTokens:   a.maxOutputTokens(msgs, llmTools),
	}
}

// maxOutputTokens sizes max_tokens for the active model — the single place the
// output budget is decided. The model's own limit is used, then clamped to the
// room left in its context window once the prompt and the tool schemas are
// counted, so a long conversation narrows the reply instead of asking the
// provider for a request that cannot fit.
func (a *Agent) maxOutputTokens(msgs []llm.Message, tools []llm.Tool) int {
	est := 0
	for _, m := range msgs {
		est += config.EstimateTokens(m.Content) + config.EstimateTokens(m.Name) + 4
		for _, tc := range m.ToolCalls {
			est += config.EstimateTokens(tc.Function.Name) +
				config.EstimateTokens(tc.Function.Arguments) + 8
		}
	}
	for _, t := range tools {
		est += config.EstimateTokens(t.Function.Name) +
			config.EstimateTokens(t.Function.Description) + 8
		for _, v := range t.Function.Parameters {
			est += config.EstimateTokens(fmt.Sprint(v))
		}
	}
	return a.opt.Config.MaxOutputTokens(a.opt.Model, est)
}

// runToolCalls executes one batch of tool calls in order. ctx is the running
// turn's context, passed in rather than read from the field so a concurrent
// turn cannot swap it out from under the call in flight.
func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall) []llm.Message {
	out := make([]llm.Message, 0, len(calls))
	for _, tc := range calls {
		args := parseArgs(tc.Function.Arguments)

		// Read-only mode is the one gate left: a tool that would change the
		// workspace is refused in plan mode, and the model is told to describe
		// the change instead. Outside it, the call just runs.
		if a.readOnly() && !tools.ReadOnlyTools[tc.Function.Name] {
			a.emit(Event{Kind: EvToolDenied, Tool: tc.Function.Name})
			out = append(out, llm.Message{
				Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Function.Name,
				Content: fmt.Sprintf("%s is blocked in plan mode. Describe the change and let the user run it.", tc.Function.Name),
			})
			continue
		}

		a.emit(Event{
			Kind: EvToolStart,
			Tool: tc.Function.Name,
			Args: args,
		})

		if tc.Function.Name == "ask_user" {
			q := ""
			if args := parseArgs(tc.Function.Arguments); args != nil {
				q, _ = args["question"].(string)
			}
			// Emit before blocking so the UI renders the question while the
			// agent waits for the typed answer.
			a.emit(Event{Kind: EvAskUser, Text: q})
			answer := ""
			if ask := a.onAskUser(); ask != nil {
				answer = ask(q)
			}
			if strings.TrimSpace(answer) == "" {
				answer = "(no answer given)"
			}
			out = append(out, llm.Message{
				Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Function.Name,
				Content: fmt.Sprintf("user answered: %s", answer),
			})
			continue
		}

		a.mu.Lock()
		registry := a.opt.Registry
		a.mu.Unlock()
		res := registry.Call(ctx, tc.Function.Name, tc.Function.Arguments)
		ev := Event{
			Kind:   EvToolResult,
			Tool:   tc.Function.Name,
			Output: res.Output,
			IsErr:  res.IsError,
			Dur:    res.Duration,
		}
		if res.Meta != nil {
			if task, ok := res.Meta["task"]; ok && task != "" {
				ev.Task = task
			}
		}
		a.emit(ev)
		out = append(out, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Function.Name, Content: res.Output})
	}
	return out
}

func (a *Agent) emit(e Event) {
	// The sink is read under its own lock and called with every lock released:
	// a sink may block on the UI bus, so it must not be able to hold up the
	// state the rest of the agent needs.
	a.sinkMu.Lock()
	sink := a.opt.EventSink
	a.sinkMu.Unlock()
	if sink != nil {
		sink(e)
	}
}

func (a *Agent) usageCopy() *llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.totalUsage
	return &u
}

func streamMerge(list *[]llm.ToolCall, tc llm.ToolCall) {
	if tc.ID != "" {
		for i := range *list {
			if (*list)[i].ID == tc.ID {
				appendFrag(&(*list)[i], tc)
				return
			}
		}
		cp := tc
		cp.Type = "function"
		*list = append(*list, cp)
		return
	}
	if len(*list) == 0 {
		cp := tc
		cp.Type = "function"
		if cp.ID == "" {
			cp.ID = fmt.Sprintf("agent_call_%d", len(*list))
		}
		*list = append(*list, cp)
		return
	}
	appendFrag(&(*list)[len(*list)-1], tc)
}

// mergedToolCall returns the accumulated call streamMerge just updated, so
// the progress event carries the arguments merged so far rather than the
func mergedToolCall(list []llm.ToolCall, frag llm.ToolCall) *llm.ToolCall {
	if frag.ID != "" {
		for i := range list {
			if list[i].ID == frag.ID {
				return &list[i]
			}
		}
		return nil
	}
	if len(list) == 0 {
		return nil
	}
	return &list[len(list)-1]
}
func appendFrag(dst *llm.ToolCall, src llm.ToolCall) {
	if src.Function.Name != "" {
		dst.Function.Name = src.Function.Name
	}
	dst.Function.Arguments += src.Function.Arguments
	if dst.Type == "" {
		dst.Type = "function"
	}
}

func parseArgs(s string) map[string]any {
	m := make(map[string]any)
	if err := jsonUnmarshal(s, &m); err != nil {
		return m
	}
	return m
}

func runGit(cmd, dir string) (string, error) {
	return runShell(cmd, dir)
}
