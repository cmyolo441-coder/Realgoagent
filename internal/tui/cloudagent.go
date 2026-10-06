package tui

import (
	"context"
	"sync"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/cloud"
	"github.com/nova-ai/nova/internal/llm"
)

// cloudAgent proxies a cloud session: it satisfies the Agent interface the
// TUI drives, but every call goes over HTTP/SSE to the server. Events stream
// back through the sink, exactly like a local agent's EventSink.
type cloudAgent struct {
	client *cloud.Client

	sinkMu sync.Mutex
	sink   func(agent.Event)

	ctx    context.Context
	cancel context.CancelFunc

	// answer routes ask_user answers to the server. It is set when the TUI
	// enters cloud mode so the key handler can reach it.
	onAnswer func(string)
}

// newCloudAgent connects to the session and starts streaming its events to
// sink. The stream reconnects on quiet drops.
func newCloudAgent(client *cloud.Client, sink func(agent.Event)) *cloudAgent {
	ctx, cancel := context.WithCancel(context.Background())
	ca := &cloudAgent{client: client, sink: sink, ctx: ctx, cancel: cancel}
	go ca.readLoop()
	return ca
}

func (ca *cloudAgent) readLoop() {
	_ = ca.client.Stream(ca.ctx, func(ev agent.Event) {
		ca.sinkMu.Lock()
		s := ca.sink
		ca.sinkMu.Unlock()
		if s != nil {
			s(ev)
		}
	})
}

// Close stops the event stream.
func (ca *cloudAgent) Close() { ca.cancel() }

func (ca *cloudAgent) ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func (ca *cloudAgent) Run(prompt string) error {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	return ca.client.Prompt(ctx, prompt)
}

func (ca *cloudAgent) Cancel() {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	_ = ca.client.Cancel(ctx)
}

func (ca *cloudAgent) Reset() {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	_ = ca.client.Reset(ctx)
}

func (ca *cloudAgent) SetPlanMode(on bool) {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	_ = ca.client.SetPlanMode(ctx, on)
}

func (ca *cloudAgent) SetSink(f func(agent.Event)) {
	ca.sinkMu.Lock()
	ca.sink = f
	ca.sinkMu.Unlock()
}

func (ca *cloudAgent) SystemPrompt() string {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	s, _ := ca.client.SystemPrompt(ctx)
	return s
}

func (ca *cloudAgent) Messages() []llm.Message {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	msgs, _ := ca.client.Messages(ctx)
	return msgs
}

func (ca *cloudAgent) Usage() llm.Usage {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	u, _ := ca.client.Usage(ctx)
	return u
}

func (ca *cloudAgent) InjectHistory(msgs []llm.Message) {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	_ = ca.client.Handoff(ctx, msgs)
}

// answerQuestion posts an ask_user answer to the server.
func (ca *cloudAgent) answerQuestion(text string) {
	ctx, cancel := ca.ctxTimeout()
	defer cancel()
	_ = ca.client.Answer(ctx, text)
}

// compile-time check: cloudAgent satisfies the TUI's Agent interface.
var _ Agent = (*cloudAgent)(nil)
