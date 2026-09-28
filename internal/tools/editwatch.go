package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nova-ai/nova/internal/editdiff"
)

// EditRecord is one completed change to a file, with both sides of it
// captured. The TUI renders these as they happen so the user can watch the
// agent's edits instead of inferring them from a final git diff.
type EditRecord struct {
	// Tool is the tool that made the change ("write", "edit", "patch", …).
	Tool string
	// Files holds the before/after text of every file the call touched.
	Files []editdiff.FileDiff
	// Dur is how long the tool call took, formatted for display.
	Dur string
	// IsErr is set when the call failed; the text is then whatever is on disk.
	IsErr bool
	// Output is the tool's own result text, shown alongside the diff.
	Output string
}

// EditWatcher receives every file change a tool makes. It is called on the
// tool's own goroutine, so an implementation must not touch the display.
type EditWatcher func(EditRecord)

// watchMu guards the installed watcher. The watcher is process-global for the
// same reason the undo stack is: a tool is a value with no reference back to
// the agent that dispatched it, and threading a callback through every one of
// them would only move the same state around.
var (
	watchMu   sync.RWMutex
	watcher   EditWatcher
	watchBusy sync.Mutex
)

// SetEditWatcher installs the callback that observes file changes. Passing nil
// removes it, which is what the one-shot and test paths do.
func SetEditWatcher(fn EditWatcher) {
	watchMu.Lock()
	watcher = fn
	watchMu.Unlock()
}

func notifyEdit(rec EditRecord) {
	watchMu.RLock()
	fn := watcher
	watchMu.RUnlock()
	if fn != nil {
		fn(rec)
	}
}

// snapshot is one file's contents at a point in time.
type snapshot struct {
	existed bool
	text    string
}

func snap(path string) snapshot {
	b, err := os.ReadFile(path)
	if err != nil {
		return snapshot{}
	}
	return snapshot{existed: true, text: string(b)}
}

// watchGuard serialises capture around a mutating tool call so two concurrent
// edits to one file cannot interleave their before/after reads.
var watchGuard sync.Mutex

// beginEdit captures the pre-state of every path a call is about to touch.
// It returns the snapshots keyed by path; pass them to finishEdit.
func beginEdit(paths []string) map[string]snapshot {
	watchGuard.Lock()
	out := make(map[string]snapshot, len(paths))
	for _, p := range paths {
		full, err := resolvePath(p)
		if err != nil {
			continue
		}
		out[full] = snap(full)
	}
	return out
}

// finishEdit reads the post-state and reports the change, if any.
func finishEdit(before map[string]snapshot, tool, dur, output string, isErr bool) {
	defer watchGuard.Unlock()
	if len(before) == 0 {
		return
	}
	rec := EditRecord{Tool: tool, Dur: dur, IsErr: isErr, Output: output}
	for full, pre := range before {
		post := snap(full)
		if pre.existed == post.existed && pre.text == post.text {
			// The call reported success but nothing moved; showing a diff for
			// an unchanged file would claim work that did not happen.
			continue
		}
		rec.Files = append(rec.Files, editdiff.FileDiff{
			Path:   relPath(full),
			Before: pre.text,
			After:  post.text,
		})
	}
	if len(rec.Files) == 0 {
		return
	}
	// A patch can touch several files, and the diff reads top-down, so order
	// them by path rather than by map iteration.
	sortFiles(rec.Files)
	notifyEdit(rec)
}

func sortFiles(fs []editdiff.FileDiff) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j].Path < fs[j-1].Path; j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
}

// relPath renders a path for display: relative to the workspace when it is
// inside it, and absolute otherwise.
func relPath(full string) string {
	if root := workspace(); root != "" {
		if r, err := filepath.Rel(root, full); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return full
}

// ---- patch path extraction ----

// patchPathRe matches the "+++ b/<path>" and "--- a/<path>" headers of a
// unified diff. Both are captured so a pure-deletion patch, which has no added
// line at all, is still attributed to the right file. The path capture stops
// at the first tab so that trailing timestamps (e.g. "file\t2024-01-01 ...")
// are excluded from the path.
var patchPathRe = regexp.MustCompile(`^(?:\+\+\+|---)\s+(?:[ab]/)?([^\t]+?)\s*$`)

// PatchPaths returns the files a unified diff would touch.
//
// It is a best-effort read of the patch text, used only to decide what to
// snapshot: a path that is not in the patch is never watched, and a path in
// the patch that does not exist is simply skipped when the post-state is
// read.
func PatchPaths(diff string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, ln := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(ln, "+++") && !strings.HasPrefix(ln, "---") {
			continue
		}
		m := patchPathRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		p := strings.TrimSpace(m[1])
		if p == "" || p == "/dev/null" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// touchPaths reports the files a tool call is about to modify, so the watcher
// can snapshot them. An empty result means "nothing to watch".
func touchPaths(tool, rawArgs string) []string {
	args := parseArgsFor(rawArgs)
	switch tool {
	case "write", "edit", "multi_edit":
		if p, ok := args["path"].(string); ok && p != "" {
			return []string{p}
		}
	case "patch":
		if d, ok := args["patch"].(string); ok {
			return PatchPaths(d)
		}
	}
	return nil
}

// parseArgsFor decodes raw tool arguments for the watcher. A parse failure
// yields an empty map: the watcher only needs to know which paths to
// snapshot, and a tool that rejects the arguments has nothing to report.
func parseArgsFor(rawArgs string) map[string]any {
	if strings.TrimSpace(rawArgs) == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		if fixed, ferr := normalizeArgs(rawArgs); ferr == nil {
			return fixed
		}
		return nil
	}
	return args
}
