package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withWorkspace points the package at a temp dir for the duration of a test.
// The workspace is process-global for the same reason the undo stack is, so a
// test that changed it must put it back or it will corrupt its neighbours.
func withWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetWorkspace(dir, "/bin/sh")
	t.Cleanup(func() { SetWorkspace("", "") })
	return dir
}

// captureEdits installs a watcher and returns the records it collects.
func captureEdits(t *testing.T) *[]EditRecord {
	t.Helper()
	var got []EditRecord
	SetEditWatcher(func(r EditRecord) { got = append(got, r) })
	t.Cleanup(func() { SetEditWatcher(nil) })
	return &got
}

func TestWriteIsReportedWithBeforeAndAfter(t *testing.T) {
	_ = withWorkspace(t)
	// The file does not exist yet, which is the case worth covering: a new
	// file has no before-text at all.
	got := captureEdits(t)
	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	if res := reg.Call(context.Background(), "write", `{"path":"new.txt","content":"one\ntwo\n"}`); res.IsError {
		t.Fatalf("write failed: %s", res.Output)
	}

	if len(*got) != 1 {
		t.Fatalf("want 1 edit record, got %d", len(*got))
	}
	rec := (*got)[0]
	if len(rec.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(rec.Files))
	}
	f := rec.Files[0]
	if f.Before != "" {
		t.Errorf("a new file must have no before-text, got %q", f.Before)
	}
	if !strings.Contains(f.After, "two") {
		t.Errorf("after-text missing the content: %q", f.After)
	}
	if f.Path != "new.txt" {
		t.Errorf("path = %q, want new.txt", f.Path)
	}
}

func TestEditIsReportedWithTheOldTextIntact(t *testing.T) {
	dir := withWorkspace(t)
	got := captureEdits(t)
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	res := reg.Call(context.Background(), "edit", `{"path":"f.txt","old_string":"beta","new_string":"BETA"}`)
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Output)
	}

	if len(*got) != 1 || len((*got)[0].Files) != 1 {
		t.Fatalf("want 1 record with 1 file, got %#v", *got)
	}
	f := (*got)[0].Files[0]
	if !strings.Contains(f.Before, "beta") {
		t.Errorf("before-text lost the original line: %q", f.Before)
	}
	if !strings.Contains(f.After, "BETA") || strings.Contains(f.After, "beta\n") {
		t.Errorf("after-text wrong: %q", f.After)
	}
}

func TestUnchangedFileIsNotReported(t *testing.T) {
	withWorkspace(t)
	got := captureEdits(t)

	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	// A failed edit leaves the file exactly as it was. Reporting that as a
	// change would show the user a diff for work that did not happen.
	res := reg.Call(context.Background(), "edit", `{"path":"missing.txt","old_string":"a","new_string":"b"}`)
	if !res.IsError {
		t.Fatalf("expected the edit to fail on a missing file")
	}
	if len(*got) != 0 {
		t.Fatalf("a failed, no-op edit must not be reported, got %#v", *got)
	}
}

func TestReadOnlyToolsReportNoEdits(t *testing.T) {
	withWorkspace(t)
	got := captureEdits(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	SetWorkspace(dir, "/bin/sh")

	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	if res := reg.Call(context.Background(), "read", `{"path":"f.txt"}`); res.IsError {
		t.Fatalf("read failed: %s", res.Output)
	}
	if len(*got) != 0 {
		t.Fatalf("read must not report an edit, got %#v", *got)
	}
}

func TestPatchReportsEveryFileItTouches(t *testing.T) {
	dir := withWorkspace(t)
	got := captureEdits(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+ONE\n" +
		"--- a/b.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-two\n+TWO\n"
	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	if res := reg.Call(context.Background(), "patch", `{"patch":`+quote(patch)+`}`); res.IsError {
		t.Skipf("patch(1) unavailable: %s", res.Output)
	}

	if len(*got) != 1 {
		t.Fatalf("want 1 record, got %d", len(*got))
	}
	files := (*got)[0].Files
	if len(files) != 2 {
		t.Fatalf("patch touched 2 files, got %d: %#v", len(files), files)
	}
	// The diff reads top-down, so the order must not depend on map iteration.
	if files[0].Path != "a.txt" || files[1].Path != "b.txt" {
		t.Errorf("files out of order: %q, %q", files[0].Path, files[1].Path)
	}
}

func TestPatchPathsReadsBothHeaders(t *testing.T) {
	// A pure deletion has no added line, so relying on "+++" alone would miss
	// the file entirely and its change would go unreported.
	got := PatchPaths("--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-hi\n")
	if len(got) != 1 || got[0] != "gone.txt" {
		t.Fatalf("got %v, want [gone.txt]", got)
	}
}

func TestPatchPathsIgnoresDevNull(t *testing.T) {
	if got := PatchPaths("--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+hi\n"); len(got) != 1 || got[0] != "new.txt" {
		t.Fatalf("got %v, want [new.txt]", got)
	}
}

func TestTaskToolRefusesWithoutARunner(t *testing.T) {
	SetRunner(nil)
	task := &Result{}
	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	res := reg.Call(context.Background(), "task", `{"task":"do a thing","role":"explore"}`)
	if res == nil {
		t.Fatal("no result")
	}
	task = res
	if !task.IsError {
		t.Fatalf("without a runner the task must fail loudly, got %q", task.Output)
	}
	if !strings.Contains(task.Output, "not available") {
		t.Errorf("the refusal must say subagents are unavailable: %q", task.Output)
	}
}

func TestTaskToolRejectsUnknownRole(t *testing.T) {
	SetRunner(stubRunner{})
	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	res := reg.Call(context.Background(), "task", `{"task":"x","role":"wizard"}`)
	if !res.IsError || !strings.Contains(res.Output, "unknown role") {
		t.Fatalf("got %+v, want an unknown-role error", res)
	}
}

func TestTaskToolReportsTheSubagentAnswer(t *testing.T) {
	SetRunner(stubRunner{reply: "the caller is main.go:42"})
	defer SetRunner(nil)
	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	res := reg.Call(context.Background(), "task", `{"task":"find it","role":"explore"}`)
	if res.IsError {
		t.Fatalf("got error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "main.go:42") {
		t.Errorf("the answer must reach the parent, got %q", res.Output)
	}
}

func TestTaskToolIsDepthCapped(t *testing.T) {
	SetRunner(stubRunner{reply: "should not be reachable"})
	defer SetRunner(nil)
	// A subagent asking for a subagent is the case that has to be refused.
	WithDepth(1, func() {
		reg := NewRegistry(nil)
		RegisterDefaults(reg)
		res := reg.Call(context.Background(), "task", `{"task":"go deeper","role":"explore"}`)
		if !res.IsError {
			t.Fatalf("nesting past the cap must fail, got %q", res.Output)
		}
		if !strings.Contains(res.Output, "already a subagent") {
			t.Errorf("the refusal must explain why, got %q", res.Output)
		}
	})
}

// TestDepthIsRestoredAfterTheRun guards the process-global depth counter: if
// WithDepth failed to restore it, every later task call would be refused.
func TestDepthIsRestoredAfterTheRun(t *testing.T) {
	before := CurrentDepth()
	WithDepth(3, func() {})
	if CurrentDepth() != before {
		t.Fatalf("depth leaked: %d, want %d", CurrentDepth(), before)
	}
}

func TestNewReturnsEveryBuiltinTool(t *testing.T) {
	reg := NewRegistry(nil)
	RegisterDefaults(reg)
	for _, name := range reg.Names() {
		if _, ok := New(name); !ok {
			t.Errorf("New(%q) failed for a registered tool", name)
		}
	}
	if _, ok := New("no_such_tool"); ok {
		t.Error("New must not invent tools")
	}
}

// stubRunner answers every task immediately.
type stubRunner struct{ reply string }

func (s stubRunner) RunTask(_ context.Context, role, task, model string) TaskResult {
	return TaskResult{Reply: s.reply, Tools: 1, Report: "subagent (" + role + "): 1 tool call(s)"}
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
