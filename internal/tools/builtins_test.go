package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetWorkspace(dir, "/bin/sh")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n")
	write("readme.md", "# Title\n\nTODO: write docs\n")
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("a/b.txt", "nested value")
	return dir
}

func run(t *testing.T, tool Tool, args map[string]any) *Result {
	t.Helper()
	return tool.Run(context.Background(), args)
}

func mustNoErr(t *testing.T, r *Result) string {
	t.Helper()
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Output)
	}
	return r.Output
}

func TestRead(t *testing.T) {
	setupWorkspace(t)
	out := mustNoErr(t, run(t, readTool{}, map[string]any{"path": "main.go"}))
	if !strings.Contains(out, "package main") {
		t.Errorf("content missing: %q", out)
	}
	if !strings.Contains(out, "1") || !strings.Contains(out, "5") {
		t.Errorf("line numbers missing: %q", out)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	setupWorkspace(t)
	out := mustNoErr(t, run(t, readTool{}, map[string]any{"path": "main.go", "offset": 3, "limit": 2}))
	// drop the trailing "… (N more lines)" hint before counting
	if i := strings.Index(out, "... ("); i >= 0 {
		out = out[:i]
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "3") || !strings.Contains(lines[1], "4") {
		t.Errorf("wrong line range: %q", out)
	}
}
func TestReadBinaryRejected(t *testing.T) {
	dir := setupWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0, 1, 2, 0, 255}, 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, readTool{}, map[string]any{"path": "bin.dat"})
	if !r.IsError || !strings.Contains(r.Output, "binary") {
		t.Errorf("expected binary rejection, got %q", r.Output)
	}
}

func TestReadDirectoryRejected(t *testing.T) {
	setupWorkspace(t)
	r := run(t, readTool{}, map[string]any{"path": "."})
	if !r.IsError || !strings.Contains(r.Output, "directory") {
		t.Errorf("expected directory rejection, got %q", r.Output)
	}
}

func TestWriteAndEdit(t *testing.T) {
	dir := setupWorkspace(t)
	// write a new file
	mustNoErr(t, run(t, writeTool{}, map[string]any{"path": "new.txt", "content": "hello world"}))
	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(data) != "hello world" {
		t.Fatalf("write failed: %v %q", err, data)
	}
	// edit it
	mustNoErr(t, run(t, editTool{}, map[string]any{
		"path": "new.txt", "old_string": "hello", "new_string": "goodbye",
	}))
	data, _ = os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(data) != "goodbye world" {
		t.Errorf("edit result = %q", data)
	}
}

func TestEditNotFound(t *testing.T) {
	setupWorkspace(t)
	// point at a file that does exist but with an absent target string
	os.WriteFile(filepath.Join(mustWorkspace(t), "target.txt"), []byte("stable content"), 0o644)
	r := run(t, editTool{}, map[string]any{"path": "target.txt", "old_string": "zzz", "new_string": "x"})
	if !r.IsError || !strings.Contains(r.Output, "not found") {
		t.Errorf("expected not-found, got %q", r.Output)
	}
}

func mustWorkspace(t *testing.T) string {
	t.Helper()
	rootMu.RLock()
	defer rootMu.RUnlock()
	if rootPath == "" {
		t.Fatal("workspace not set")
	}
	return rootPath
}

func TestEditAmbiguous(t *testing.T) {
	dir := setupWorkspace(t)
	os.WriteFile(filepath.Join(dir, "dup.txt"), []byte("aa aa aa"), 0o644)
	r := run(t, editTool{}, map[string]any{"path": "dup.txt", "old_string": "aa", "new_string": "b"})
	if !r.IsError || !strings.Contains(r.Output, "3 times") {
		t.Errorf("expected ambiguity error, got %q", r.Output)
	}
	// replace_all resolves it
	run(t, editTool{}, map[string]any{
		"path": "dup.txt", "old_string": "aa", "new_string": "b", "replace_all": true,
	})
	data, _ := os.ReadFile(filepath.Join(dir, "dup.txt"))
	if string(data) != "b b b" {
		t.Errorf("replace_all = %q", data)
	}
}

func TestMultiEdit(t *testing.T) {
	dir := setupWorkspace(t)
	mustNoErr(t, run(t, writeTool{}, map[string]any{"path": "m.txt", "content": "one two three"}))
	args := map[string]any{"path": "m.txt", "edits": []any{
		map[string]any{"old_string": "one", "new_string": "1"},
		map[string]any{"old_string": "three", "new_string": "3"},
	}}
	mustNoErr(t, run(t, multiEditTool{}, args))
	data, _ := os.ReadFile(filepath.Join(dir, "m.txt"))
	if string(data) != "1 two 3" {
		t.Errorf("multi_edit = %q", data)
	}
}

func TestGlob(t *testing.T) {
	setupWorkspace(t)
	out := mustNoErr(t, run(t, globTool{}, map[string]any{"pattern": "*.go"}))
	if !strings.Contains(out, "main.go") {
		t.Errorf("glob missed main.go: %q", out)
	}
	// ** path component
	out = mustNoErr(t, run(t, globTool{}, map[string]any{"pattern": "**/b.txt"}))
	if !strings.Contains(out, "b.txt") {
		t.Errorf("nested glob missed b.txt: %q", out)
	}
}

func TestGrep(t *testing.T) {
	setupWorkspace(t)
	out := mustNoErr(t, run(t, grepTool{}, map[string]any{"pattern": "TODO"}))
	if !strings.Contains(out, "readme.md") || !strings.Contains(out, ":3") {
		t.Errorf("grep result = %q", out)
	}
	// case-insensitive
	out = mustNoErr(t, run(t, grepTool{}, map[string]any{"pattern": "todo", "ignore_case": true}))
	if !strings.Contains(out, "readme.md") {
		t.Errorf("ignore_case failed: %q", out)
	}
	// glob filter
	out = mustNoErr(t, run(t, grepTool{}, map[string]any{"pattern": "package", "glob": "*.go"}))
	if !strings.Contains(out, "main.go") {
		t.Errorf("glob filter failed: %q", out)
	}
}

func TestGrepNoMatch(t *testing.T) {
	setupWorkspace(t)
	r := run(t, grepTool{}, map[string]any{"pattern": "zzzznotthere"})
	if r.IsError || !strings.Contains(r.Output, "no matches") {
		t.Errorf("got %q", r.Output)
	}
}

func TestBash(t *testing.T) {
	setupWorkspace(t)
	out := mustNoErr(t, run(t, bashTool{}, map[string]any{"command": "echo hello"}))
	if !strings.Contains(out, "hello") {
		t.Errorf("bash out = %q", out)
	}
	r := run(t, bashTool{}, map[string]any{"command": "exit 3"})
	if !r.IsError || !strings.Contains(r.Output, "exit 3") {
		t.Errorf("expected exit code 3 reported, got %q", r.Output)
	}
}

func TestListDirSkipsNoise(t *testing.T) {
	dir := setupWorkspace(t)
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules/x.js"), []byte("x"), 0o644)
	out := mustNoErr(t, run(t, listDirTool{}, map[string]any{}))
	if strings.Contains(out, "node_modules") {
		t.Errorf("node_modules should be hidden: %q", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("main.go missing: %q", out)
	}
}

func TestSchemaRequired(t *testing.T) {
	r := readTool{}.Schema()
	req, _ := r["required"].([]string)
	if len(req) == 0 || req[0] != "path" {
		t.Errorf("schema required = %v", req)
	}
	props, _ := r["properties"].(map[string]any)
	if props["path"] == nil {
		t.Error("path property missing")
	}
}

func TestRegistryDispatch(t *testing.T) {
	setupWorkspace(t)
	reg := NewRegistry()
	RegisterDefaults(reg)
	if len(reg.Names()) < 10 {
		t.Fatalf("expected at least 10 tools, got %d", len(reg.Names()))
	}
	res := reg.Call(context.Background(), "grep", `{"pattern":"TODO"}`)
	if res.IsError || !strings.Contains(res.Output, "readme.md") {
		t.Errorf("registry call failed: %q", res.Output)
	}
	// unknown tool
	if res := reg.Call(context.Background(), "nope", "{}"); !res.IsError {
		t.Error("unknown tool should error")
	}
	// bad JSON args are recovered, not fatal
	if res := reg.Call(context.Background(), "grep", `{"pattern":`); res.IsError == false {
		t.Error("bad JSON should error")
	}
}

// Tool calls run as soon as the agent asks for them. Read-only mode is the
// only thing that still stops one.
func TestRegistryRunsTools(t *testing.T) {
	setupWorkspace(t)
	reg := NewRegistry()
	RegisterDefaults(reg)
	if res := reg.Call(context.Background(), "bash", `{"command":"echo hi"}`); res.IsError {
		t.Errorf("bash should run unattended, got %q", res.Output)
	}
}

func TestReadOnlyBlocksWrites(t *testing.T) {
	dir := setupWorkspace(t)
	reg := NewRegistry()
	RegisterDefaults(reg)
	reg.ReadOnly = true
	res := reg.Call(context.Background(), "write", `{"path":"blocked.txt","content":"x"}`)
	if !res.IsError || !strings.Contains(res.Output, "plan mode") {
		t.Errorf("write should be blocked in read-only mode, got %q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "blocked.txt")); !os.IsNotExist(err) {
		t.Error("blocked write left a file behind")
	}
}

func TestLLMToolsSchema(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaults(reg)
	specs := reg.ToLLMTools()
	if len(specs) < 10 {
		t.Fatalf("expected specs, got %d", len(specs))
	}
	for _, s := range specs {
		if s["type"] != "function" {
			t.Errorf("type = %v", s["type"])
		}
		fn, _ := s["function"].(map[string]any)
		for _, key := range []string{"name", "description", "parameters"} {
			if fn[key] == nil {
				t.Errorf("function missing %q: %+v", key, fn)
			}
		}
	}
}
