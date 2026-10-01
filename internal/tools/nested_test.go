package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// nestingTool is a tool whose body dispatches another tool call, which is what
// the task tool does: it runs a whole subagent, and that subagent's tools come
// back through the same registry on the same goroutine.
type nestingTool struct {
	reg   *Registry
	depth *int
	// block, when set, holds the tool body open for the given path, so a test
	// can observe that a call on another path is not waiting behind it.
	block map[string]chan struct{}
}

func (nestingTool) Name() string           { return "task" }
func (nestingTool) Description() string    { return "test tool that nests" }
func (nestingTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (n nestingTool) Run(ctx context.Context, a map[string]any) *Result {
	p, _ := a["path"].(string)
	if release, ok := n.block[p]; ok {
		<-release
	}
	if *n.depth > 0 {
		return &Result{Output: "deep enough"}
	}
	*n.depth++
	// The inner call writes the same file the outer one was asked about, which
	// is the shape that deadlocked: a subagent editing the file its parent is
	// already watching.
	res := n.reg.Call(ctx, "write", `{"path":`+quote(p)+`,"content":"inner"}`)
	*n.depth--
	if res.IsError {
		return res
	}
	return &Result{Output: "outer"}
}

// A tool call that runs another tool call must not deadlock.
//
// Capture took one process-wide mutex and held it for the whole tool body, so
// the inner call blocked on the lock its own caller was holding, and the outer
// call could never reach the release. `task` is exactly this shape — it runs a
// subagent, whose tools re-enter here — so a subagent that touched a file hung
// the turn until the session was killed.
func TestNestedToolCallDoesNotDeadlock(t *testing.T) {
	dir := withWorkspace(t)
	depth := 0
	target := filepath.Join(dir, "f.txt")

	reg := NewRegistry()
	real, _ := New("write")
	reg.Register(real)
	reg.Register(nestingTool{reg: reg, depth: &depth})

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		reg.Call(context.Background(), "task", `{"path":`+quote(target)+`}`)
	}()

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a tool call that runs another tool call never returned")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the nested write did not reach disk: %v", err)
	}
}

// The nested call is a real change to a real file and has to be reported:
// dropping it would leave the panel showing less than the agent did.
func TestNestedToolCallStillReportsItsEdit(t *testing.T) {
	dir := withWorkspace(t)
	got := captureEdits(t)
	depth := 0
	target := filepath.Join(dir, "f.txt")

	reg := NewRegistry()
	real, _ := New("write")
	reg.Register(real)
	reg.Register(nestingTool{reg: reg, depth: &depth})
	reg.Call(context.Background(), "task", `{"path":`+quote(target)+`}`)

	if len(*got) == 0 {
		t.Fatal("the nested write was not reported")
	}
	found := false
	for _, rec := range *got {
		for _, f := range rec.Files {
			if f.Path == "f.txt" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the nested write was not reported for f.txt: %+v", *got)
	}
}

// Two calls touching different files must not serialise against each other.
// The guard was global, so a call still inside its body blocked every other
// edit in the session behind it — a slow write to one file held up a write to
// an unrelated one for as long as it took.
func TestAnEditToOneFileDoesNotBlockAnUnrelatedOne(t *testing.T) {
	dir := withWorkspace(t)
	captureEdits(t)
	slow := filepath.Join(dir, "slow.txt")
	fast := filepath.Join(dir, "fast.txt")
	release := make(chan struct{})

	reg := NewRegistry()
	depth := 0
	real, _ := New("write")
	reg.Register(real)
	reg.Register(nestingTool{
		reg: reg, depth: &depth,
		block: map[string]chan struct{}{slow: release},
	})

	slowCall := make(chan struct{})
	go func() {
		defer close(slowCall)
		reg.Call(context.Background(), "task", `{"path":`+quote(slow)+`}`)
	}()
	// Let the slow call take its lock and park inside the body.
	time.Sleep(100 * time.Millisecond)

	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		reg.Call(context.Background(), "task", `{"path":`+quote(fast)+`}`)
	}()
	select {
	case <-fastDone:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("an edit to an unrelated file was blocked behind another one")
	}
	close(release)
	<-slowCall

	if _, err := os.Stat(fast); err != nil {
		t.Fatalf("the unrelated write did not reach disk: %v", err)
	}
}

// A tool that panics between capture and reporting must not strand the lock it
// held, or every later edit to that path blocks for good.
func TestAPanickingToolDoesNotStrandTheLock(t *testing.T) {
	dir := withWorkspace(t)
	captureEdits(t)
	boom := filepath.Join(dir, "boom.txt")

	reg := NewRegistry()
	reg.Register(panickingTool{})
	res := reg.Call(context.Background(), "write", `{"path":`+quote(boom)+`,"content":"x"}`)
	if res == nil || !res.IsError {
		t.Fatalf("a panicking tool must report an error, got %+v", res)
	}

	// The path must be editable again.
	real, _ := New("write")
	reg2 := NewRegistry()
	reg2.Register(real)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg2.Call(context.Background(), "write", `{"path":`+quote(boom)+`,"content":"after"}`)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the path was left locked by a panicking tool")
	}
}

type panickingTool struct{}

func (panickingTool) Name() string           { return "write" }
func (panickingTool) Description() string    { return "test write that panics" }
func (panickingTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (panickingTool) Run(ctx context.Context, a map[string]any) *Result {
	panic("boom")
}
