package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxFileBytes bounds one file that a path tool will pull into memory. The
// path comes from the model, so an unbounded os.ReadFile is a way to end the
// process: a multi-gigabyte log answers a one-line tool call with an
// out-of-memory kill.
const maxFileBytes = 16 << 20

// ---- read ----

type readTool struct{}

func (readTool) Name() string { return "read" }
func (readTool) Description() string {
	return "Read a text file with numbered lines. Use offset/limit for large files."
}
func (readTool) Schema() map[string]any {
	return obj(
		req("path", "Path to the file, relative to the workspace. Paths outside the workspace are refused."),
		prop("offset", "integer", "1-based line number to start reading from"),
		prop("limit", "integer", "Maximum number of lines to return"),
	)
}
func (readTool) Run(ctx context.Context, a map[string]any) *Result {
	p, _ := argString(a, "path")
	if p == "" {
		return errResult("read: 'path' is required")
	}
	full, err := resolvePath(p)
	if err != nil {
		return errResult("read: %v", err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return errResult("read: %v", err)
	}
	if info.IsDir() {
		return errResult("read: %s is a directory; use list_dir", p)
	}
	if info.Size() > maxFileBytes {
		return errResult("read: %s is %s, past the %s limit for a single call; use bash (sed -n '1,400p' %s) or grep to work through it",
			p, humanSize(info.Size()), humanSize(maxFileBytes), shellQuote(full))
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return errResult("read: %v", err)
	}
	if !isText(data) {
		return errResult("read: %s looks binary (%d bytes)", p, len(data))
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	total := len(lines)
	off := argInt(a, "offset", 1)
	if off < 1 {
		off = 1
	}
	if off > total {
		off = total
	}
	lim := argInt(a, "limit", 2000)
	if lim <= 0 {
		lim = 2000
	}
	end := off - 1 + lim
	if end > total {
		end = total
	}
	var b strings.Builder
	w := len(fmt.Sprint(max(end, 1)))
	for i := off - 1; i < end; i++ {
		fmt.Fprintf(&b, "%*d│ %s\n", w, i+1, lines[i])
	}
	if end < total {
		fmt.Fprintf(&b, "... (%d more lines; use offset=%d)", total-end, end+1)
	}
	return &Result{Output: b.String(), Meta: map[string]string{"path": p, "lines": fmt.Sprint(total)}}
}

// ---- write ----

type writeTool struct{}

func (writeTool) Name() string { return "write" }
func (writeTool) Description() string {
	return "Create or completely overwrite a file with new content."
}
func (writeTool) Schema() map[string]any {
	return obj(
		req("path"),
		req("content"),
	)
}
func (writeTool) Run(ctx context.Context, a map[string]any) *Result {
	p, _ := argString(a, "path")
	c, hasContent := argString(a, "content")
	if p == "" {
		return errResult("write: 'path' is required")
	}
	if !hasContent {
		// An absent argument is not an empty file. Treating it as one
		// truncates whatever was on disk and then reports a successful write,
		// so a model that simply forgot the content loses the file.
		return errResult("write: 'content' is required (pass an empty string to create an empty file)")
	}
	// Detect transmission corruption: null bytes or other control characters
	// (except \n, \r, \t) indicate the content was mangled in flight.
	// Fail fast with a clear message instead of writing a corrupt file that
	// the agent will then have to delete and rewrite.
	if i := strings.IndexByte(c, 0); i >= 0 {
		return errResult("write: content corrupted in transmission (null byte at offset %d) — please resend the write tool call", i)
	}
	for i := 0; i < len(c); i++ {
		if b := c[i]; b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			return errResult("write: content corrupted in transmission (control byte 0x%02x at offset %d) — please resend the write tool call", b, i)
		}
	}
	full, err := resolvePath(p)
	if err != nil {
		return errResult("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return errResult("write: %v", err)
	}
	old := ""
	if b, err := os.ReadFile(full); err == nil {
		old = string(b)
	}
	RecordUndo(full)
	if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
		return errResult("write: %v", err)
	}
	before, after := countLines(old), countLines(c)
	return &Result{Output: fmt.Sprintf("wrote %s (%+d lines, now %d lines)", p, after-before, after),
		Meta: map[string]string{"path": p}}
}

// ---- edit ----

type editTool struct{}

func (editTool) Name() string { return "edit" }
func (editTool) Description() string {
	return "Replace an exact string in a file. The old_string must be unique unless replace_all is set."
}
func (editTool) Schema() map[string]any {
	return obj(
		req("path"),
		req("old_string"),
		req("new_string"),
		prop("replace_all", "boolean", "Replace every occurrence instead of failing on ambiguity"),
	)
}
func (editTool) Run(ctx context.Context, a map[string]any) *Result {
	p, _ := argString(a, "path")
	old, _ := argString(a, "old_string")
	nw, hasNew := argString(a, "new_string")
	if p == "" || old == "" {
		return errResult("edit: 'path' and 'old_string' are required")
	}
	if !hasNew {
		// An absent new_string means "delete the match", which is a real edit
		// but never an intended one, and it happens whenever the model drops
		// the argument. Say so instead of quietly emptying the text.
		return errResult("edit: 'new_string' is required (pass an empty string to delete the matched text)")
	}
	full, err := resolvePath(p)
	if err != nil {
		return errResult("edit: %v", err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return errResult("edit: %v", err)
	}
	content := string(data)
	n := strings.Count(content, old)
	if n == 0 {
		return errResult("edit: old_string not found in %s — re-read the file and copy the text exactly (including whitespace)", p)
	}
	if n > 1 && !argBool(a, "replace_all", false) {
		return errResult("edit: old_string matches %d times in %s; add more surrounding context or set replace_all", n, p)
	}
	replaced := content
	if argBool(a, "replace_all", false) {
		replaced = strings.ReplaceAll(content, old, nw)
	} else {
		replaced = strings.Replace(content, old, nw, 1)
	}
	RecordUndo(full)
	if err := os.WriteFile(full, []byte(replaced), 0o644); err != nil {
		return errResult("edit: %v", err)
	}
	return &Result{Output: fmt.Sprintf("edited %s (%d replacement%s)", p, n, plural(n)), Meta: map[string]string{"path": p}}
}

// ---- multi_edit ----

type multiEditOp struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type multiEditTool struct{}

func (multiEditTool) Name() string { return "multi_edit" }
func (multiEditTool) Description() string {
	return "Apply several exact-string replacements to one file in a single call."
}
func (multiEditTool) Schema() map[string]any {
	return obj(
		req("path"),
		prop("edits", "array", "List of {old_string,new_string} edits applied in order"),
	)
}
func (multiEditTool) Run(ctx context.Context, a map[string]any) *Result {
	p, _ := argString(a, "path")
	raw, ok := a["edits"]
	if !ok {
		return errResult("multi_edit: 'edits' is required")
	}
	arr, ok := raw.([]any)
	if !ok {
		return errResult("multi_edit: 'edits' must be a list")
	}
	full, err := resolvePath(p)
	if err != nil {
		return errResult("multi_edit: %v", err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return errResult("multi_edit: %v", err)
	}
	content := string(data)
	applied := 0
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return errResult("multi_edit: entry %d is not an object", i)
		}
		old, _ := m["old_string"].(string)
		nw, hasNew := m["new_string"].(string)
		if old == "" {
			return errResult("multi_edit: entry %d has empty old_string", i)
		}
		if !hasNew {
			return errResult("multi_edit: entry %d has no new_string", i)
		}
		if strings.Count(content, old) != 1 {
			return errResult("multi_edit: entry %d old_string not uniquely found in %s", i, p)
		}
		content = strings.Replace(content, old, nw, 1)
		applied++
	}
	RecordUndo(full)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return errResult("multi_edit: %v", err)
	}
	return &Result{Output: fmt.Sprintf("edited %s (%d edits applied)", p, applied), Meta: map[string]string{"path": p}}
}

// ---- bash ----

type bashTool struct {
	Timeout time.Duration
}

func (bashTool) Name() string { return "bash" }
func (bashTool) Description() string {
	return "Run a shell command in the workspace. Prefer this for builds, tests, git and package managers."
}
func (bashTool) Schema() map[string]any {
	return obj(
		req("command"),
		prop("description", "string", "One-line summary of what the command does"),
		prop("timeout_ms", "integer", "Command timeout in milliseconds (default 120000, max 600000)"),
	)
}
func (bashTool) Run(ctx context.Context, a map[string]any) *Result {
	cmd, _ := argString(a, "command")
	if strings.TrimSpace(cmd) == "" {
		return errResult("bash: 'command' is required")
	}
	ms := argInt(a, "timeout_ms", 120000)
	if ms > 600000 {
		ms = 600000
	}
	if ms < 1000 {
		ms = 1000
	}
	out, _, err := runInDir(ctx, cmd, workspace(), time.Duration(ms)*time.Millisecond)
	res := &Result{Output: out}
	if err != nil {
		res.IsError = true
		if ee, ok := err.(interface{ ExitCode() int }); ok {
			res.Output = fmt.Sprintf("%s\n[exit %d]", out, ee.ExitCode())
		} else {
			res.Output = fmt.Sprintf("%s\n[%v]", out, err)
		}
	}
	res.Meta = map[string]string{"desc": mustString(a, "description")}
	return res
}

// ---- list_dir ----

type listDirTool struct{}

func (listDirTool) Name() string { return "list_dir" }
func (listDirTool) Description() string {
	return "List directory contents as a depth-limited tree so the agent can orient itself."
}
func (listDirTool) Schema() map[string]any {
	return obj(
		prop("path", "string", "Directory to list, defaults to the workspace root"),
		prop("depth", "integer", "Maximum recursion depth (default 3)"),
	)
}
func (listDirTool) Run(ctx context.Context, a map[string]any) *Result {
	p, _ := argString(a, "path")
	if p == "" {
		p = "."
	}
	full, err := resolvePath(p)
	if err != nil {
		return errResult("list_dir: %v", err)
	}
	depth := argInt(a, "depth", 3)
	if depth < 1 || depth > 10 {
		depth = 3
	}
	var b strings.Builder
	maxEntries := 500
	entries := 0
	var walk func(dir, prefix string, d int)
	walk = func(dir, prefix string, d int) {
		des, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&b, "%s[error: %v]\n", prefix, err)
			return
		}
		filtered := make([]os.DirEntry, 0, len(des))
		for _, de := range des {
			if hidden(de.Name()) {
				continue
			}
			filtered = append(filtered, de)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name() < filtered[j].Name() })
		for i, de := range filtered {
			if entries >= maxEntries {
				fmt.Fprintf(&b, "%s… (truncated at %d entries)\n", prefix, maxEntries)
				return
			}
			entries++
			last := i == len(filtered)-1
			branch := "├── "
			next := "│   "
			if last {
				branch = "└── "
				next = "    "
			}
			name := de.Name()
			if de.IsDir() {
				fmt.Fprintf(&b, "%s%s%s/\n", prefix, branch, name)
				if d > 1 {
					walk(filepath.Join(dir, name), prefix+next, d-1)
				}
			} else {
				info, _ := de.Info()
				size := ""
				if info != nil {
					size = fmt.Sprintf(" (%s)", humanSize(info.Size()))
				}
				fmt.Fprintf(&b, "%s%s%s%s\n", prefix, branch, name, size)
			}
		}
	}
	fmt.Fprintf(&b, "%s/\n", p)
	walk(full, "", depth)
	return &Result{Output: b.String()}
}

// ---- glob ----

type globTool struct{}

func (globTool) Name() string { return "glob" }
func (globTool) Description() string {
	return "Find files whose paths match a glob pattern such as **/*.go or src/**/*.ts."
}
func (globTool) Schema() map[string]any {
	return obj(
		req("pattern"),
		prop("path", "string", "Directory to search in, defaults to the workspace root"),
		prop("limit", "integer", "Maximum results (default 200)"),
	)
}
func (globTool) Run(ctx context.Context, a map[string]any) *Result {
	pat, _ := argString(a, "pattern")
	if pat == "" {
		return errResult("glob: 'pattern' is required")
	}
	base, _ := argString(a, "path")
	if base == "" {
		base = workspace()
	} else {
		f, err := resolvePath(base)
		if err != nil {
			return errResult("glob: %v", err)
		}
		base = f
	}
	limit := argInt(a, "limit", 200)
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	var matches []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if hidden(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if len(matches) >= limit {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if globMatch(pat, rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return errResult("glob: %v", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return &Result{Output: fmt.Sprintf("no files match %q under %s", pat, base)}
	}
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(m + "\n")
	}
	fmt.Fprintf(&b, "(%d matches)", len(matches))
	return &Result{Output: b.String()}
}

// ---- grep ----

type grepTool struct{}

func (grepTool) Name() string { return "grep" }
func (grepTool) Description() string {
	return "Search file contents with a regular expression. Returns matching lines with line numbers."
}
func (grepTool) Schema() map[string]any {
	return obj(
		req("pattern"),
		prop("path", "string", "File or directory to search, defaults to the workspace root"),
		prop("glob", "string", "Only search files matching this glob, e.g. *.go"),
		prop("limit", "integer", "Maximum matches (default 100)"),
		prop("ignore_case", "boolean", "Case-insensitive search"),
	)
}
func (grepTool) Run(ctx context.Context, a map[string]any) *Result {
	pat, _ := argString(a, "pattern")
	if pat == "" {
		return errResult("grep: 'pattern' is required")
	}
	base, _ := argString(a, "path")
	if base == "" {
		base = workspace()
	} else {
		f, err := resolvePath(base)
		if err != nil {
			return errResult("grep: %v", err)
		}
		base = f
	}
	limit := argInt(a, "limit", 100)
	if limit <= 0 || limit > 2000 {
		limit = 100
	}
	gl, _ := argString(a, "glob")
	m, err := compilePattern(pat, argBool(a, "ignore_case", false))
	if err != nil {
		return errResult("grep: invalid pattern: %v", err)
	}
	type hit struct {
		file string
		line int
		text string
	}
	var hits []hit
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if hidden(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// A link sitting in the workspace can point at anything, and
			// reading through it turns grep into a reader for the whole disk
			// without ever leaving the directory the model named. The walk
			// already refuses to descend through links; this is the same rule
			// for the leaf.
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > maxFileBytes {
			// Same reason read refuses one: the contents of a build artefact
			// or a data dump would be pulled into memory whole, per file.
			return nil
		}
		if gl != "" {
			if ok, _ := filepath.Match(gl, d.Name()); !ok {
				return nil
			}
		}
		data, err := os.ReadFile(p)
		if err != nil || !isText(data) {
			return nil
		}
		rel := p
		if r, rerr := filepath.Rel(workspace(), p); rerr == nil {
			rel = r
		}
		lines := strings.Split(string(data), "\n")
		for i, ln := range lines {
			if m.MatchString(ln) {
				hits = append(hits, hit{filepath.ToSlash(rel), i + 1, strings.TrimRight(ln, "\r")})
				if len(hits) >= limit {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil {
		return errResult("grep: %v", err)
	}
	if len(hits) == 0 {
		return &Result{Output: fmt.Sprintf("no matches for %q", pat)}
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d:%s\n", h.file, h.line, h.text)
	}
	fmt.Fprintf(&b, "(%d matches)", len(hits))
	return &Result{Output: b.String()}
}

// ---- patch ----

type patchTool struct{}

func (patchTool) Name() string { return "patch" }
func (patchTool) Description() string {
	return "Apply a unified diff patch to one or more files. Prefer edit for simple changes."
}
func (patchTool) Schema() map[string]any {
	return obj(req("patch"))
}
func (patchTool) Run(ctx context.Context, a map[string]any) *Result {
	diff, _ := argString(a, "patch")
	if diff == "" {
		return errResult("patch: 'patch' is required")
	}
	tmp, err := os.CreateTemp("", "nova-patch-*.diff")
	if err != nil {
		return errResult("patch: %v", err)
	}
	// A named file in a shared temp directory is a target for anyone on the
	// box: the name was predictable, so a pre-planted symlink turned this
	// write into an overwrite of a file of their choosing, and the mode let
	// the diff (workspace text) be read by every other user. CreateTemp is
	// O_EXCL, random and 0600.
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(diff); err != nil {
		tmp.Close()
		return errResult("patch: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return errResult("patch: %v", err)
	}
	name := tmp.Name()
	out, _, err := runInDir(ctx, "patch -p1 --dry-run < "+shellQuote(name), workspace(), 30*time.Second)
	if err != nil {
		return errResult("patch: dry-run failed:\n%s\n%s", out, err)
	}
	out, _, err = runInDir(ctx, "patch -p1 < "+shellQuote(name), workspace(), 30*time.Second)
	if err != nil {
		return &Result{Output: fmt.Sprintf("patch apply failed:\n%s\n%v", out, err), IsError: true}
	}
	return &Result{Output: "patch applied:\n" + out}
}
