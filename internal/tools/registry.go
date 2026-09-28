// Package tools implements Nova's built-in function-calling tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
//
// Tool calls are not gated. A registered tool runs when the agent calls it, so
// the only restriction left is ReadOnly, which is how plan mode works.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// ReadOnly rejects any tool that would mutate the workspace (plan mode).
	ReadOnly bool
}

// NewRegistry returns a registry ready for tools to be registered.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
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
			Output:   fmt.Sprintf("%s is disabled in plan mode (read-only). Switch to agent mode to run it.", name),
			IsError:  true,
			Duration: time.Since(start),
		}
	}

	// Snapshot the files this call is about to touch so the edit watcher can
	// show the change the moment it lands. Read-only tools snapshot nothing,
	// so this costs one map allocation per tool call and nothing else.
	before := beginEdit(touchPaths(name, rawArgs))
	finished := false
	// beginEdit takes the watcher's lock and finishEdit is what releases it.
	// The recover above catches a panicking tool, so without this the lock
	// would be stranded and every later edit would block in beginEdit for
	// good: one broken tool turning into a hung session. Passing nil releases
	// the lock without reporting a change the tool may never have made.
	defer func() {
		if !finished {
			finishEdit(nil, name, "", "", true)
		}
	}()
	res = t.Run(ctx, args)
	res.Duration = time.Since(start)
	finishEdit(before, name, formatDur(res.Duration), res.Output, res.IsError)
	finished = true
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
	if !ok || v == nil {
		// A JSON null is an absent argument, not the text "<nil>".
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

// resolvePath maps a path the model asked for onto a real path and refuses
// anything that lands outside the workspace.
//
// An absolute path reaches the same place a relative one reaches with "..", so
// both are checked here: `read /etc/shadow` is the same escape as
// `read ../../etc/shadow`, and a tool that honours the first spelling is no
// more confined than one that ignores the second. Symlinks are resolved
// before the check so a link planted inside the workspace is not a second way
// out of it.
//
// What comes back is the cleaned path the caller asked for, so error messages
// and results keep the spelling the model used.
func resolvePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	root := workspace()
	if root == "" {
		// Joining onto an empty root resolves against the process working
		// directory with no confinement at all. Anchor on it explicitly so
		// the containment check below still has something to compare against.
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("no workspace root is set: %v", err)
		}
		root = wd
	}
	full := filepath.FromSlash(p)
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	full = filepath.Clean(full)
	// The root itself may be reached through a symlink, so both sides of the
	// comparison have to be the real paths or every file looks like an escape.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = filepath.Clean(root)
	}
	if !withinRoot(realRoot, evalExisting(full)) {
		return "", fmt.Errorf("path %q escapes the workspace root %q", p, root)
	}
	return full, nil
}

// withinRoot reports whether full is root or lives under it. It compares
// cleaned paths with filepath.Rel rather than as strings, so a sibling
// directory that merely shares a prefix (/ws-backup next to /ws) is not
// mistaken for a child.
func withinRoot(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// evalExisting resolves the symlinks along p, following each one component at
// a time rather than with filepath.EvalSymlinks.
//
// EvalSymlinks fails on a link whose target does not exist, and that is
// exactly the link a model can aim at /etc/cron.d and then create with a
// write, so the path is walked by hand: a link is followed to its target even
// when nothing is there yet, and a component that is missing ends the walk
// (nothing below a missing directory can be a link). A relative target is
// followed from the directory holding the link, an absolute one restarts at
// the filesystem root, and a chain longer than any real path gives up and
// hands back the path as written.
func evalExisting(p string) string {
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = filepath.Join(wd, p)
		}
	}
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	rest := strings.TrimPrefix(filepath.Clean(p), cur)
	links := 0
	for rest != "" {
		part, tail := rest, ""
		if i := strings.Index(part, string(filepath.Separator)); i >= 0 {
			part, tail = rest[:i], rest[i+1:]
		}
		rest = tail
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(cur, part)
		fi, err := os.Lstat(next)
		if err != nil {
			// Missing from here down, so the remainder is plain text.
			return joinTail(cur, rest)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		links++
		if links > 40 {
			return filepath.Clean(p)
		}
		dst, err := os.Readlink(next)
		if err != nil {
			return filepath.Clean(p)
		}
		if rest != "" {
			dst = filepath.Join(dst, rest)
		}
		if filepath.IsAbs(dst) {
			v := filepath.VolumeName(dst)
			cur = v + string(filepath.Separator)
			rest = strings.TrimPrefix(filepath.Clean(dst), cur)
		} else {
			rest = dst
		}
	}
	return cur
}

// joinTail glues the unresolved remainder back onto dir.
func joinTail(dir, rest string) string {
	if rest == "" {
		return dir
	}
	return filepath.Join(append([]string{dir}, strings.Split(rest, string(filepath.Separator))...)...)
}

func runInDir(ctx context.Context, cmd string, dir string, timeout time.Duration) (string, string, error) {
	if ctx == nil {
		// context.WithTimeout panics on a nil parent, and a tool helper is the
		// wrong place to take the process down over it.
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	// Cancelling the context kills the shell, not the commands the shell
	// started, and CombinedOutput does not return until every writer of the
	// output pipe has closed it. A child that outlives the shell therefore
	// holds the pipe open and the tool call never returns, so bound that
	// second wait and let Wait close the pipes.
	c.WaitDelay = 2 * time.Second
	out, err := c.CombinedOutput()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Otherwise the kill surfaces as a bare "signal: killed", which reads
		// as though the command died on its own rather than hitting the limit.
		return string(out), "", fmt.Errorf("timed out after %s", timeout)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command itself is gone; something it started is still holding
		// the output pipe. Say that, rather than passing on a stdlib string
		// that says nothing about what the user should go and look for.
		return string(out), "", fmt.Errorf("the command finished but a process it started is still holding its output open")
	}
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
