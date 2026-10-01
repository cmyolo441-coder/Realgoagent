// Package agent implements Nova's tool-calling agent loop.
package agent

import (
	"context"
	"encoding/json"
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

// gitTTL is how long the git context is reused before the branch and the
// recent log are re-read. Long enough that every iteration of a turn shares
// one read, short enough that a commit made during a turn is visible on the
// next one.
const gitTTL = 30 * time.Second

// sysCacheTTL bounds how long a rendered system prompt is reused. The prompt
// states the current date and time, so it cannot be cached for the whole
// session; it also has to pick up an AGENTS.md the user edits while Nova is
// open. Thirty seconds is far below the notice threshold for either, and long
// enough that every iteration of a turn shares one rendering.
const sysCacheTTL = 30 * time.Second

// sysCacheEntry is a memoised system prompt plus the state it was built from.
// The zero value is never a valid entry.
type sysCacheEntry struct {
	valid  bool
	key    string
	prompt string
	stamp  time.Time
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
	// sysCache memoises the rendered system prompt. Rendering it shells out
	// to git twice and re-reads every instruction file in the workspace;
	// doing that per request put roughly 150ms of process startup in front of
	// every model call, and again for every tool call inside the same turn.
	// That is what made a reply feel slow to start and slow to continue.
	sysCache sysCacheEntry
	// gitStamp/gitContext cache the git status and log for gitTTL, so a turn's
	// iterations share one read while an agent that commits mid-turn still
	// sees the new head on the next turn.
	gitStamp   time.Time
	gitContext string

	// sinkMu guards opt.EventSink on its own, separate from mu. A sink may
	// block — on the UI bus, or on the user answering a prompt — so it must
	// never be invoked with the agent state locked.
	sinkMu sync.Mutex
	// ctx and cancel are the running turn's handles.
	ctx    context.Context
	cancel context.CancelFunc
	// onRender is called whenever the system prompt is rendered rather than
	// served from the cache. It is a test hook: nothing in the product reads
	// it, and a test uses it to pin how often the expensive path runs.
	onRender func()
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
	a.setClient(llm.NewClient(o.Provider.BaseURL, o.Provider.APIKeyFromEnv(), o.Model.ID))
	return a, nil
}

// setClient installs the transport the agent streams through and subscribes
// it to this agent's events.
//
// The subscription lives here rather than in New because swapping the client
// is a normal thing to do — a model switch, a test pointing at a stub — and a
// hook wired only at construction is silently dropped by every one of them.
func (a *Agent) setClient(c *llm.Client) {
	// A rate-limited request waits twenty seconds before it is tried again,
	// and a transient failure up to thirty. The turn produces nothing at all
	// during that wait, and a front-end with no way to tell a deliberate pause
	// from a hang has nothing to show but a frozen spinner.
	c.OnWait = func(d time.Duration, err error) {
		reason := "connection problem"
		if isRateLimitErr(err) {
			reason = "rate limit"
		}
		a.emit(Event{Kind: EvModelWait, Text: fmt.Sprintf("%s — retrying in %s", reason, d.Round(time.Second))})
	}
	a.mu.Lock()
	a.client = c
	a.mu.Unlock()
}

// streamClient returns the transport the turn should stream through.
func (a *Agent) streamClient() *llm.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// isRateLimitErr reports whether the client stalled on a provider-side rate
// limit rather than an ordinary network fault, so the message names the
// reason instead of guessing.
func isRateLimitErr(err error) bool { return llm.IsRateLimit(err) }

// SystemPrompt renders the current system prompt.
//
// The result is memoised. Rendering it reads the workspace's instruction
// files and shells out to git twice, and both happen on the request path:
// buildRequest renders it for every model call, and RunTask renders it again
// per turn. That put well over a hundred milliseconds of process startup in
// front of the first token, and again in front of every tool call the model
// asked for, which is what a user experiences as "slow to start, then slow
// again after every step".
//
// The entry is dropped on a state change rather than invalidated eagerly, so
// a mode switch or a tool-set change still shows up on the very next request.
func (a *Agent) SystemPrompt() string {
	key := a.systemKey()
	now := time.Now()

	a.mu.Lock()
	if c := a.sysCache; c.valid && c.key == key && now.Sub(c.stamp) < sysCacheTTL {
		p := c.prompt
		a.mu.Unlock()
		return p
	}
	a.mu.Unlock()

	rendered := a.renderSystemPrompt()

	a.mu.Lock()
	// Another goroutine may have rendered the same prompt meanwhile. The two
	// renderings differ only in the timestamp, so last writer wins and both
	// are correct.
	a.sysCache = sysCacheEntry{valid: true, key: key, prompt: rendered, stamp: time.Now()}
	a.mu.Unlock()
	return rendered
}

// systemKey fingerprints the state the rendered prompt depends on. Two calls
// that produce the same key must produce the same text, so anything that
// reaches the prompt belongs here.
func (a *Agent) systemKey() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join([]string{
		a.opt.Provider.Name,
		a.opt.Model.ID,
		a.opt.Workspace,
		a.cfgShellLocked(),
		fmt.Sprint(a.opt.PlanMode),
		strings.Join(a.toolNames, ","),
	}, "\x00")
}

// renderSystemPrompt builds the prompt from scratch. Everything slow happens
// here and nowhere else.
func (a *Agent) renderSystemPrompt() string {
	a.mu.Lock()
	hook := a.onRender
	a.mu.Unlock()
	if hook != nil {
		hook()
	}

	a.mu.Lock()
	plan := a.opt.PlanMode
	names := append([]string{}, a.toolNames...)
	ws := a.opt.Workspace
	model := a.opt.Provider.Name + "/" + a.opt.Model.ID
	shell := a.cfgShellLocked()
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
		Shell:     shell,
		OS:        prompt.RuntimeOS(),
		Model:     model,
		Tools:     names,
		DateTime:  prompt.Now(),
		Extra:     extra,
		Git:       a.gitContextCached(ws),
	})
}

// gitContextCached returns the git status and recent log, spawning git at
// most once per gitTTL.
//
// git status is not free: it walks the worktree, so on a large repository it
// costs far more than the model call it is attached to. The state it reports
// is slow-moving compared to a turn, so a short cache keeps the prompt honest
// while collapsing the per-iteration cost to a single read.
func (a *Agent) gitContextCached(ws string) string {
	a.mu.Lock()
	if !a.gitStamp.IsZero() && time.Since(a.gitStamp) < gitTTL {
		c := a.gitContext
		a.mu.Unlock()
		return c
	}
	a.mu.Unlock()

	// Built outside the lock: this runs two subprocesses and must not be
	// holding a.mu, or Cancel and Messages would block behind it.
	ctx := prompt.GitContext(func(cmd string) string {
		out, _ := runGit(cmd, ws)
		return out
	})

	a.mu.Lock()
	a.gitStamp = time.Now()
	a.gitContext = ctx
	a.mu.Unlock()
	return ctx
}

func (a *Agent) cfgShell() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfgShellLocked()
}

// cfgShellLocked is cfgShell with a.mu already held. systemKey and
// renderSystemPrompt both call it while they own the lock, and a second Lock
// on a sync.Mutex deadlocks rather than reading through.
func (a *Agent) cfgShellLocked() string {
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

// historyBudgetTokens is how much of a model's context window the
// conversation is allowed to occupy. A turn is cut back to fit before it is
// sent rather than after the provider rejects it, so the failure mode is a
// shorter context instead of a dead session.
const historyBudgetTokens = 120000

// trimHistory drops the oldest turns until the conversation fits the budget.
//
// It exists because a session's history only ever grows, and a long one used
// to end with every request refused: each turn appends the model reply and
// every tool result, and the model call that would have trimmed it cannot
// happen because the request carrying the too-long history is the one being
// rejected. The tool schemas and the reply budget take their share of the
// window first, so only what is left is available to the conversation.
//
// Messages are dropped in whole units — a user turn with the reply and tool
// results that followed it — never one message at a time. An assistant
// message carrying tool_calls whose results were dropped is a protocol
// violation the provider rejects, so a lone orphan is worse than a long
// history. The newest turn is always kept: it is the one being answered, and
// a turn that cannot fit is reported rather than silently halved.
func trimHistory(msgs []llm.Message, budget int) ([]llm.Message, bool) {
	if budget <= 0 || estimateTokens(msgs) <= budget {
		return msgs, false
	}
	// The first message is the system prompt, which the caller re-adds, so the
	// search for the first user turn starts after it.
	start := 0
	for start < len(msgs) && msgs[start].Role == llm.RoleSystem {
		start++
	}
	head := append([]llm.Message{}, msgs[:start]...)
	body := msgs[start:]

	// Units run from one user message to the next, so each cut removes a
	// complete exchange.
	bounds := []int{0}
	for i, m := range body {
		if m.Role == llm.RoleUser {
			bounds = append(bounds, i)
		}
	}
	if len(bounds) < 2 {
		// A single turn is over budget on its own. There is nothing older to
		// drop, so the turn goes out as it is and the caller says so.
		return msgs, true
	}
	bounds = append(bounds, len(body))

	keep := len(bounds) - 1
	for keep > 1 {
		keep--
		candidate := append(head, body[bounds[keep]:]...)
		if estimateTokens(candidate) <= budget {
			return candidate, true
		}
	}
	last := append(head, body[bounds[len(bounds)-2]:]...)
	return last, true
}

// estimateTokens approximates the size of a message list.
func estimateTokens(msgs []llm.Message) int {
	est := 0
	for _, m := range msgs {
		est += config.EstimateTokens(m.Content) + config.EstimateTokens(m.Name) + 4
		for _, tc := range m.ToolCalls {
			est += config.EstimateTokens(tc.Function.Name) +
				config.EstimateTokens(tc.Function.Arguments) + 8
		}
	}
	return est
}

// callFingerprint identifies a tool call for the repeat guard: the same tool
// asked for with the same arguments.
//
// The arguments are re-encoded rather than compared as text, because a model
// re-emitting the same object with different spacing or key order is making
// the same call, not a new one — comparing raw strings would let a loop that
// merely reformats its arguments run forever.
func callFingerprint(name, args string) string {
	canonical := ""
	var m map[string]any
	if err := jsonUnmarshal(args, &m); err == nil && m != nil {
		if b, err := json.Marshal(m); err == nil {
			canonical = string(b)
		}
	}
	if canonical == "" {
		canonical = strings.Join(strings.Fields(args), " ")
	}
	return name + "\x00" + canonical
}

// repeatLimit is how many times the same call may run within one turn. Three
// covers a genuine retry — a tool that failed, a path that had not been
// created yet — while stopping a model stuck re-reading the same file from
// spending the turn's whole budget, and the user's tokens, on it.
const repeatLimit = 3

// repeatedToolCall counts one call and returns an error naming the loop once
// the same call has already run repeatLimit times this turn. It returns nil
// while the count is still within budget, so the tool runs exactly
// repeatLimit times and the next identical call is the one refused.
func repeatedToolCall(counts map[string]int, name, args string) error {
	key := callFingerprint(name, args)
	counts[key]++
	if counts[key] <= repeatLimit {
		return nil
	}
	return fmt.Errorf("%s has already run %d times in this turn with identical arguments and is not changing anything; stop repeating it and either do something different or report what you found", name, repeatLimit)
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
	// One counter per distinct tool call, held for the whole turn rather than
	// for one iteration: a model stuck re-reading the same file repeats across
	// iterations, so a counter reset each time would never reach its limit.
	repeats := make(map[string]int)
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
		chunks := a.streamClient().Stream(turnCtx, req)

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
		toolMsgs := a.runToolCalls(turnCtx, toolCalls, repeats)
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

	// The conversation is cut to fit the window before the request is built, so
	// a long session degrades to a shorter context instead of being refused
	// outright. The trim is written back to the history so the dropped turns
	// stop being re-sent, and re-counted by the session that persists it.
	if trimmed, cut := trimHistory(msgs, a.historyBudget(llmTools)); cut {
		msgs = trimmed
		a.replaceHistory(trimmed)
	}
	return llm.Request{
		Messages:    msgs,
		Tools:       llmTools,
		Temperature: temp,
		MaxTokens:   a.maxOutputTokens(msgs, llmTools),
	}
}

// historyBudget is how many tokens the conversation may occupy on the current
// model. The window minus the tool schemas, the system prompt and a reserve
// for the reply, floored at a small floor so a tiny configured window still
// sends something rather than an empty request.
func (a *Agent) historyBudget(llmTools []llm.Tool) int {
	a.mu.Lock()
	window := a.opt.Model.Context
	a.mu.Unlock()
	if window <= 0 {
		return historyBudgetTokens
	}
	reserved := 0
	for _, t := range llmTools {
		reserved += config.EstimateTokens(t.Function.Name) +
			config.EstimateTokens(t.Function.Description) + 8
		for _, v := range t.Function.Parameters {
			reserved += config.EstimateTokens(fmt.Sprint(v))
		}
	}
	budget := window - reserved - a.opt.Model.OutputLimit()
	if budget < 4096 {
		budget = 4096
	}
	return budget
}

// replaceHistory swaps in a trimmed conversation, keeping the system message
// the agent holds so the next turn still renders and re-attaches it.
func (a *Agent) replaceHistory(msgs []llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := make([]llm.Message, 0, len(msgs))
	system := ""
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			if system == "" {
				system = m.Content
			}
			continue
		}
		kept = append(kept, m)
	}
	if system != "" {
		kept = append([]llm.Message{{Role: llm.RoleSystem, Content: system}}, kept...)
	}
	a.messages = kept
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
// turn cannot swap it out from under the call in flight. repeats is the turn's
// per-call counter map, passed in so the guard spans iterations rather than
// resetting at each one.
func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall, repeats map[string]int) []llm.Message {
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

		// A call identical to one already made this turn is refused with the
		// reason, so the model gets the explanation in its own context and can
		// act on it. The tool is not run: the point is that running it again
		// produces the result it has already been given.
		if err := repeatedToolCall(repeats, tc.Function.Name, tc.Function.Arguments); err != nil {
			a.emit(Event{Kind: EvToolDenied, Tool: tc.Function.Name, Text: err.Error()})
			out = append(out, llm.Message{
				Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Function.Name,
				Content: err.Error(),
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
