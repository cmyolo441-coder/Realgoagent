package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// globMatch matches a path against a glob with doublestar (**) semantics.
// filepath.Match treats "**" exactly like "*" (it never crosses "/"), which
// breaks patterns like "internal/**/tools/*.go". This splits the pattern and
// the path into slash-separated components and matches them recursively, with
// "**" consuming any number of components.
func globMatch(pattern, name string) bool {
	return matchComponents(
		strings.Split(filepath.ToSlash(strings.Trim(pattern, "/")), "/"),
		strings.Split(filepath.ToSlash(strings.Trim(name, "/")), "/"),
	)
}

// matchComponents aligns pattern components against path components.
func matchComponents(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		// "**" matches zero or more components.
		for i := 0; i <= len(name); i++ {
			if matchComponents(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, _ := filepath.Match(pat[0], name[0]); !ok {
		return false
	}
	return matchComponents(pat[1:], name[1:])
}

// ---- todo ----

type todoTool struct{}

type todoItem struct {
	ID     string `json:"id"`
	Task   string `json:"task"`
	Status string `json:"status"` // pending|in_progress|completed
}

func (todoTool) Name() string { return "todo" }
func (todoTool) Description() string {
	return "Create or update the visible task checklist. Use for multi-step work."
}
func (todoTool) Schema() map[string]any {
	return obj(prop("items", "array", "List of {id, task, status} entries"))
}
func (todoTool) Run(ctx context.Context, a map[string]any) *Result {
	raw, ok := a["items"].([]any)
	if !ok {
		return errResult("todo: 'items' must be a list")
	}
	var out strings.Builder
	out.WriteString("task list updated\n")
	var active string
	for _, it := range raw {
		m, _ := it.(map[string]any)
		task, _ := m["task"].(string)
		status, _ := m["status"].(string)
		mark := "[ ]"
		switch status {
		case "completed":
			mark = "[x]"
		case "in_progress":
			mark = "[~]"
			if task != "" {
				active = task
			}
		case "cancelled":
			mark = "[-]"
		}
		fmt.Fprintf(&out, "%s %s\n", mark, task)
	}
	meta := map[string]string{}
	if active != "" {
		// The agent loop mirrors Meta["task"] onto the event so the UI can
		// show the live checklist in the prompt rail.
		meta["task"] = active
	}
	return &Result{Output: out.String(), Meta: meta}
}

// ---- web_fetch ----

type webFetchTool struct{}

func (webFetchTool) Name() string { return "web_fetch" }
func (webFetchTool) Description() string {
	return "Fetch a URL and return its readable text content (HTML tags stripped)."
}
func (webFetchTool) Schema() map[string]any {
	return obj(
		req("url"),
		prop("max_chars", "integer", "Truncate the response to this many characters (default 20000)"),
	)
}
func (webFetchTool) Run(ctx context.Context, a map[string]any) *Result {
	u, _ := argString(a, "url")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return errResult("web_fetch: url must start with http")
	}
	maxChars := argInt(a, "max_chars", 20000)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errResult("web_fetch: %v", err)
	}
	req.Header.Set("User-Agent", "nova-agent/1.0")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return errResult("web_fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errResult("web_fetch: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if rerr != nil {
		return errResult("web_fetch: %v", rerr)
	}
	text := htmlToText(string(body))
	if maxChars > 0 && len(text) > maxChars {
		text = text[:maxChars] + fmt.Sprintf("\n... truncated at %d chars", maxChars)
	}
	return &Result{Output: fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), text)}
}

func htmlToText(s string) string {
	s = cutScripts(s)
	s = strings.ReplaceAll(s, "<script", "<x")
	rx, _ := regexp.Compile(`<[^>]*>`)
	s = rx.ReplaceAllString(s, " ")
	ent := map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": "\"", "&#39;": "'", "&nbsp;": " "}
	for k, v := range ent {
		s = strings.ReplaceAll(s, k, v)
	}
	lines := strings.Split(s, "\n")
	var out []string
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

func cutScripts(s string) string {
	open := strings.Index(strings.ToLower(s), "<script")
	closeIdx := strings.Index(strings.ToLower(s), "</script")
	if open < 0 || closeIdx < 0 {
		return s
	}
	return s[:open] + s[closeIdx+8:]
}

// ---- web_search ----

type webSearchTool struct{}

func (webSearchTool) Name() string { return "web_search" }
func (webSearchTool) Description() string {
	return "Search the web using DuckDuckGo Lite and return the top results."
}
func (webSearchTool) Schema() map[string]any {
	return obj(
		req("query"),
		prop("limit", "integer", "Maximum results (default 8)"),
	)
}
func (webSearchTool) Run(ctx context.Context, a map[string]any) *Result {
	q, _ := argString(a, "query")
	if q == "" {
		return errResult("web_search: 'query' is required")
	}
	limit := argInt(a, "limit", 8)
	if limit < 1 || limit > 20 {
		limit = 8
	}
	url := "https://lite.duckduckgo.com/lite/?q=" + urlQuery(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errResult("web_search: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return errResult("web_search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errResult("web_search: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rerr != nil {
		return errResult("web_search: %v", rerr)
	}
	text := htmlToText(string(body))
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		if len(out) >= limit {
			break
		}
		if len(ln) > 12 && !strings.Contains(ln, "DuckDuckGo") {
			out = append(out, ln)
		}
	}
	if len(out) == 0 {
		return &Result{Output: "no results parsed"}
	}
	return &Result{Output: strings.Join(out, "\n")}
}

func urlQuery(s string) string {
	r := strings.NewReplacer(" ", "+", "\"", "%22", "&", "%26")
	return r.Replace(s)
}

// ---- git helpers ----

type gitStatusTool struct{}

func (gitStatusTool) Name() string { return "git_status" }
func (gitStatusTool) Description() string {
	return "Show branch, working-tree summary and recent commits. Only works inside a git " +
		"repository; in a plain directory use list_dir or read instead."
}
func (gitStatusTool) Schema() map[string]any { return obj() }
func (gitStatusTool) Run(ctx context.Context, a map[string]any) *Result {
	ws := workspace()
	// Check for a repository first. Running git and echoing its stderr produced
	// a wall of "fatal: not a git repository" that looked like a tool failure
	// rather than a fact about the workspace, and cost the model a turn.
	if reason, ok := checkGitRepo(ws); !ok {
		return &Result{Output: reason}
	}

	var b strings.Builder
	branch, _, _ := runInDir("git rev-parse --abbrev-ref HEAD 2>/dev/null", ws, 10*time.Second)
	if strings.TrimSpace(branch) == "" {
		branch = "(detached)"
	}
	b.WriteString("branch: " + strings.TrimSpace(branch) + "\n")

	// Ahead/behind is worth a line of its own; it is what people look for.
	if ab, _, _ := runInDir("git rev-list --left-right --count @{upstream}...HEAD 2>/dev/null", ws, 10*time.Second); strings.TrimSpace(ab) != "" {
		parts := strings.Fields(ab)
		if len(parts) == 2 {
			b.WriteString(fmt.Sprintf("upstream: %s behind, %s ahead\n", parts[0], parts[1]))
		}
	}

	porcelain, _, _ := runInDir("git status --porcelain 2>/dev/null", ws, 15*time.Second)
	changed, staged := summarisePorcelain(porcelain)
	switch {
	case changed == 0:
		b.WriteString("working tree: clean\n")
	default:
		b.WriteString(fmt.Sprintf("working tree: %d changed, %d staged\n", changed, staged))
	}

	if log, _, _ := runInDir("git log --oneline -5 2>/dev/null", ws, 10*time.Second); strings.TrimSpace(log) != "" {
		b.WriteString("\nrecent commits:\n" + strings.TrimRight(log, "\n"))
	}
	return &Result{Output: b.String()}
}

// checkGitRepo reports whether dir is inside a git work tree. When it is not,
// the returned string explains it in terms the model can act on.
func checkGitRepo(dir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return "", true
	}
	// A worktree or a bare repo has no .git directory, so ask git itself.
	if out, _, _ := runInDir("git rev-parse --is-inside-work-tree 2>/dev/null", dir, 10*time.Second); strings.TrimSpace(out) == "true" {
		return "", true
	}
	return "not a git repository: " + dir + "\n" +
		"git_status and git_diff do not apply here. Use list_dir to see what is in the " +
		"directory, or read to inspect a file.", false
}

// summarisePorcelain counts changed and staged entries in git status output.
func summarisePorcelain(porcelain string) (changed, staged int) {
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 2 {
			continue
		}
		changed++
		if line[0] != ' ' && line[0] != '?' {
			staged++
		}
	}
	return changed, staged
}

type gitDiffTool struct{}

func (gitDiffTool) Name() string { return "git_diff" }
func (gitDiffTool) Description() string {
	return "Show the current uncommitted diff, optionally for a single file or staged " +
		"changes. Only works inside a git repository."
}
func (gitDiffTool) Schema() map[string]any {
	return obj(
		prop("path", "string", "Limit the diff to this file"),
		prop("staged", "boolean", "Show staged changes instead of the working tree"),
		prop("stat", "boolean", "Show only the --stat summary"),
	)
}
func (gitDiffTool) Run(ctx context.Context, a map[string]any) *Result {
	ws := workspace()
	// Same reasoning as git_status: say the workspace is not a repository
	// rather than returning git's fatal message as if the tool had failed.
	if reason, ok := checkGitRepo(ws); !ok {
		return &Result{Output: reason}
	}

	cmd := "git diff"
	if argBool(a, "staged", false) {
		cmd = "git diff --cached"
	}
	if argBool(a, "stat", false) {
		cmd += " --stat"
	}
	if p, _ := argString(a, "path"); p != "" {
		cmd += " -- " + shellQuote(p)
	}
	out, _, err := runInDir(cmd, ws, 60*time.Second)
	out = strings.TrimRight(out, "\n")
	if out == "" {
		if argBool(a, "staged", false) {
			return &Result{Output: "no staged changes"}
		}
		return &Result{Output: "no uncommitted changes"}
	}
	return &Result{Output: out, IsError: err != nil}
}

// ---- lsp-ish: go build/test shortcuts are just bash ----

// ---- ask_user (interactive confirmation is handled in the agent, this is the fallback) ----

type askUserTool struct{}

func (askUserTool) Name() string { return "ask_user" }
func (askUserTool) Description() string {
	return "Ask the human a question and wait for a typed answer. Use sparingly."
}
func (askUserTool) Schema() map[string]any {
	return obj(req("question"))
}
func (askUserTool) Run(ctx context.Context, a map[string]any) *Result {
	return &Result{Output: "user_turn_required", Meta: map[string]string{"q": mustString(a, "question")}}
}

// ---- plan mode note ----

// RegisterDefaults installs every built-in tool into r.
func RegisterDefaults(r *Registry) {
	for _, t := range builtinTools() {
		r.Register(t)
	}
}

// builtinTools is the single source of truth for which tools exist. The task
// tool is installed with the rest: without a subagent host it refuses every
// call with a clear message, which is better than a model inventing a tool
// that does not exist.
func builtinTools() []Tool {
	return []Tool{
		readTool{},
		writeTool{},
		editTool{},
		multiEditTool{},
		bashTool{},
		listDirTool{},
		globTool{},
		grepTool{},
		patchTool{},
		todoTool{},
		webFetchTool{},
		webSearchTool{},
		gitStatusTool{},
		gitDiffTool{},
		askUserTool{},
		taskTool{},
	}
}

// New returns a single built-in tool by name. It is how a subagent assembles
// a restricted registry: naming a tool is enough to get the real
// implementation, so a subagent gets the same code paths as the main agent
// and cannot drift from them.
func New(name string) (Tool, bool) {
	for _, t := range builtinTools() {
		if t.Name() == name {
			return t, true
		}
	}
	return nil, false
}

var _ = jsonSchemaHelpers
var _ = os.Stat
var _ = filepath.Join
var _ = os.Getenv
var _ = runtime.GOOS
var _ = utf8.ValidString

func mustString(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func isText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	sample := b
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	if utf8.Valid(sample) {
		// a NUL byte means binary almost always
		for _, c := range sample {
			if c == 0 {
				return false
			}
		}
		return true
	}
	return false
}

func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var _ = os.Getenv
var _ = filepath.Join
