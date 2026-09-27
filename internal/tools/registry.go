// Package tools implements Nova's built-in function-calling tools.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Result is the outcome of running a tool.
type Result struct {
	Output   string // human/model readable output
	IsError  bool
	Meta     map[string]string
	Duration time.Duration
}

// Tool is the runtime interface every tool implements.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	Run(ctx context.Context, args map[string]any) *Result
}

// Registry holds all available tools and dispatches calls.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// ReadOnly rejects any tool that would mutate the workspace (plan mode).
	ReadOnly bool
	// Confirmation gates mutating or risky operations.
	Confirm func(tool string, args map[string]any) bool
}

// NewRegistry returns a registry seeded with an approval callback.
func NewRegistry(confirm func(string, map[string]any) bool) *Registry {
	return &Registry{tools: map[string]Tool{}, Confirm: confirm}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.mu.Unlock()
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// allowed reports whether name may run given the current restrictions.
func (r *Registry) allowed(name string) bool {
	if !r.ReadOnly {
		return true
	}
	return ReadOnlyTools[name]
}

// List returns tools sorted by name, honouring read-only mode.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if !r.allowed(t.Name()) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Schema returns the JSON tool schema list for the LLM request.
func (r *Registry) ToLLMTools() []map[string]any {
	out := make([]map[string]any, 0)
	for _, t := range r.List() {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name(),
				"description": t.Description(),
				"parameters":  t.Schema(),
			},
		})
	}
	return out
}

// Call runs a tool by name, recovering from panics so a single broken tool can
// never take down the whole agent turn.
func (r *Registry) Call(ctx context.Context, name string, rawArgs string) (res *Result) {
	start := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			res = &Result{
				Output:   fmt.Sprintf("tool %s panicked: %v", name, rec),
				IsError:  true,
				Duration: time.Since(start),
			}
		}
	}()

	t, ok := r.Get(name)
	if !ok {
		return &Result{Output: fmt.Sprintf("unknown tool %q; use one of: %s", name, strings.Join(r.Names(), ", ")), IsError: true, Duration: time.Since(start)}
	}

	var args map[string]any
	if strings.TrimSpace(rawArgs) != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			// Some models emit python-ish dicts; try a light normalisation.
			if fixed, ferr := normalizeArgs(rawArgs); ferr == nil {
				args = fixed
			} else {
				return &Result{Output: fmt.Sprintf("invalid JSON arguments for %s: %v", name, err), IsError: true, Duration: time.Since(start)}
			}
		}
	}

	if !r.allowed(name) {
		return &Result{
			Output:   fmt.Sprintf("%s is disabled in plan mode (read-only). Ask the user for permission or switch modes.", name),
			IsError:  true,
			Duration: time.Since(start),
		}
	}

	if r.Confirm != nil && !r.Confirm(name, args) {
		return &Result{Output: "user denied this tool call", IsError: true, Duration: time.Since(start)}
	}

	// Snapshot the files this call is about to touch so the edit watcher can
	// show the change the moment it lands. Read-only tools snapshot nothing,
	// so this costs one map allocation per tool call and nothing else.
	before := beginEdit(touchPaths(name, rawArgs))
	res = t.Run(ctx, args)
	res.Duration = time.Since(start)
	finishEdit(before, name, formatDur(res.Duration), res.Output, res.IsError)
	return res
}

// formatDur renders a duration the way the UI shows it: no fractional noise,
// and nothing at all when the call was effectively instant.
func formatDur(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.Round(time.Millisecond).String()
}

// Names lists registered tool names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for k := range r.tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normalizeArgs(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var args map[string]any
	err := json.Unmarshal([]byte(s), &args)
	return args, err
}

// ---- helpers shared by tools ----

func argString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return fmt.Sprintf("%g", t), true
	case bool:
		return fmt.Sprintf("%v", t), true
	}
	return fmt.Sprintf("%v", v), true
}

func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n := 0
		fmt.Sscanf(t, "%d", &n)
		if n == 0 {
			return def
		}
		return n
	}
	return def
}

func argBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	if s, ok := v.(string); ok {
		return s == "true" || s == "1" || s == "yes"
	}
	return def
}

// errResult builds an error result.
func errResult(format string, a ...any) *Result {
	return &Result{Output: fmt.Sprintf(format, a...), IsError: true}
}

// workspaceRoot is injected by the agent at startup.
var (
	rootMu   sync.RWMutex
	rootPath string
	shell    string
)

// SetWorkspace configures the root path and shell used by path-based tools.
func SetWorkspace(root, sh string) {
	rootMu.Lock()
	rootPath = root
	shell = sh
	rootMu.Unlock()
}

func workspace() string {
	rootMu.RLock()
	defer rootMu.RUnlock()
	return rootPath
}

// GetShell returns the configured login shell.
func GetShell() string {
	rootMu.RLock()
	defer rootMu.RUnlock()
	return shell
}

// resolvePath clamps a path inside the workspace unless it is absolute.
// Absolute paths are honoured so the agent can read system files, but a
// relative path may not climb out with "..".
func resolvePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	root := workspace()
	full := filepath.Join(root, filepath.FromSlash(p))
	if root != "" {
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q escapes the workspace root %q", p, root)
		}
	}
	return full, nil
}

func runInDir(cmd string, dir string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var sh string
	sh = GetShell()
	if sh == "" {
		if runtime.GOOS == "windows" {
			sh = os.Getenv("COMSPEC")
			if sh == "" {
				sh = "cmd.exe"
			}
		} else {
			sh = "/bin/sh"
		}
	}

	var c *exec.Cmd
	if strings.ContainsAny(sh, " ") {
		parts := strings.Fields(sh)
		head := parts[0]
		args := append(parts[1:], "-c", cmd)
		if runtime.GOOS != "windows" {
			c = exec.CommandContext(ctx, head, args...)
		} else {
			c = exec.CommandContext(ctx, head, append(append([]string{}, parts[1:]...), "/c", cmd)...)
		}
	} else {
		if runtime.GOOS == "windows" {
			c = exec.CommandContext(ctx, sh, "/c", cmd)
		} else {
			c = exec.CommandContext(ctx, sh, "-lc", cmd)
		}
	}
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), "", err
}

// hidden reports whether the path component is ignorable.
func hidden(name string) bool {
	switch name {
	case ".git", "node_modules", "__pycache__", ".venv", "venv", "dist", "build", ".next", ".cache", "target":
		return true
	}
	return false
}

// RiskyCommands lists destructive shell patterns that always need user
// confirmation. They are regular expressions matched against the lowercased
// command, with word boundaries so that `mv /tmp/a /tmp/b` does not trip on the
// bare token "mv /".
var RiskyCommands = []string{
	`\brm\s+-[a-zA-Z]*[rf]`,     // rm -rf, rm -fr, rm -Rf
	`\brm\s+(-[a-zA-Z]+\s+)+\S`, // rm -r -f dir
	`\brmdir\s+/(s|q)?`,
	`\bgit\s+reset\s+--hard`,
	`\bgit\s+clean\s+-`,
	`\bgit\s+push\s+(-f\b|--force\b)`,
	`\bgit\s+checkout\s+--\s`,
	`\b(drop|truncate)\s+(table|database|schema)\b`,
	`\bmkfs`,
	`\bdd\s+if=`,
	`:\(\)\s*\{`,
	`\b(shutdown|reboot|poweroff|halt)\b`,
	`\bkill\s+-9\b`,
	`\b(pkill|killall)\b`,
	`\bdocker\s+(system\s+prune|rm\s+-f)`,
	`\bchmod\s+(-[a-zA-Z]+\s+)*0*777`,
	`(curl|wget)\s+[^|]*\|\s*(sh|bash)`,
	`\beval\s`,
	`>\s*/dev/sd`,
	`^\s*mv\s+/\s`,
	`\bformat\s+[a-z]:`,
	`\bsudo\s`,
}

var riskyREs = mustCompileAll(RiskyCommands)

func mustCompileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}

// CommandNeedsApproval inspects a shell command for destructive patterns.
// The TUI and the agent loop both call this so policy stays in one place.
func CommandNeedsApproval(cmd string) bool {
	low := strings.ToLower(cmd)
	for _, re := range riskyREs {
		if re.MatchString(low) {
			return true
		}
	}
	return false
}

// ReadOnlyTools are the tools permitted in plan mode: they observe the
// workspace without changing it.
var ReadOnlyTools = map[string]bool{
	"read":       true,
	"list_dir":   true,
	"glob":       true,
	"grep":       true,
	"web_fetch":  true,
	"web_search": true,
	"git_status": true,
	"git_diff":   true,
	"ask_user":   true,
	"todo":       true,
}

// MutatingTools are the tools that change the workspace and therefore need
// explicit approval unless the user opted into auto-accepting edits.
var MutatingTools = map[string]bool{
	"write":      true,
	"edit":       true,
	"multi_edit": true,
	"patch":      true,
	"bash":       true,
}

// NeedsApproval reports whether a tool invocation must be confirmed first.
// Shell commands only need a prompt when they look destructive.
func NeedsApproval(name string, args map[string]any) bool {
	switch name {
	case "bash":
		cmd, _ := args["command"].(string)
		return CommandNeedsApproval(cmd)
	case "write", "edit", "multi_edit", "patch":
		return true
	}
	return false
}

// ---- undo ----

// undoEntry is the state of a file immediately before a tool overwrote it.
type undoEntry struct {
	Path     string
	Previous []byte
	Mode     os.FileMode
	Existed  bool
}

var (
	undoMu sync.Mutex
	undos  []undoEntry
)

// RecordUndo snapshots path before a write so /undo can restore the old state.
func RecordUndo(path string) {
	undoMu.Lock()
	defer undoMu.Unlock()
	entry := undoEntry{Path: path, Mode: 0o644}
	if b, err := os.ReadFile(path); err == nil {
		entry.Existed = true
		entry.Previous = b
		if info, err := os.Stat(path); err == nil {
			entry.Mode = info.Mode().Perm()
		}
	}
	undos = append(undos, entry)
}

// UndoCount reports how many revertible edits have been recorded.
func UndoCount() int {
	undoMu.Lock()
	defer undoMu.Unlock()
	return len(undos)
}

// ClearUndo drops the recorded edit history (used by /clear and tests).
func ClearUndo() {
	undoMu.Lock()
	undos = nil
	undoMu.Unlock()
}

// Undo reverts the most recently recorded edit. It returns the display path
// as well as the message, so a caller showing a change history can drop the
// entry it just undid rather than keep listing a change that is no longer on
// disk.
func UndoLast() (string, string, error) {
	undoMu.Lock()
	if len(undos) == 0 {
		undoMu.Unlock()
		return "", "", fmt.Errorf("nothing to undo")
	}
	last := undos[len(undos)-1]
	undos = undos[:len(undos)-1]
	undoMu.Unlock()

	rel := last.Path
	if root := workspace(); root != "" {
		if r, err := filepath.Rel(root, last.Path); err == nil {
			rel = r
		}
	}
	if !last.Existed {
		if err := os.Remove(last.Path); err != nil {
			return "", rel, err
		}
		return filepath.ToSlash(rel), fmt.Sprintf("removed %s (it was created by the agent)", rel), nil
	}
	if err := os.WriteFile(last.Path, last.Previous, last.Mode); err != nil {
		return "", rel, err
	}
	return filepath.ToSlash(rel), fmt.Sprintf("restored %s to its previous contents", rel), nil
}

var _ = fs.WalkDir
