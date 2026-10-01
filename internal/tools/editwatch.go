package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// watchLocks holds one mutex per path, so capture for two different files
// cannot block each other and a nested call cannot block its own caller.
var watchLocks struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

// pathLock is a per-path mutex plus a reference count, so a lock can be
// dropped once nothing is watching the path any more. Without the count the
// map would grow one entry per file the agent ever wrote, for the life of the
// process.
type pathLock struct {
	mu   sync.Mutex
	refs int
}

// acquirePathLocks locks every path in full, in sorted order, and returns them
// keyed by path. Sorted order is what keeps two calls touching overlapping
// sets of files from deadlocking against each other.
func acquirePathLocks(paths []string) map[string]*pathLock {
	if len(paths) == 0 {
		return nil
	}
	full := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		res, err := resolvePath(p)
		if err != nil || seen[res] {
			continue
		}
		seen[res] = true
		full = append(full, res)
	}
	sort.Strings(full)

	watchLocks.mu.Lock()
	if watchLocks.locks == nil {
		watchLocks.locks = map[string]*pathLock{}
	}
	out := make(map[string]*pathLock, len(full))
	for _, p := range full {
		l := watchLocks.locks[p]
		if l == nil {
			l = &pathLock{}
			watchLocks.locks[p] = l
		}
		l.refs++
		out[p] = l
	}
	watchLocks.mu.Unlock()

	// Lock outside watchLocks.mu: a lock already held by another goroutine can
	// take a while to free, and the registry must not be blocked on it.
	for _, p := range full {
		out[p].mu.Lock()
	}
	return out
}

// releasePathLocks frees the locks acquired by acquirePathLocks and forgets
// the paths nothing watches any more.
func releasePathLocks(locks map[string]*pathLock) {
	watchLocks.mu.Lock()
	defer watchLocks.mu.Unlock()
	for p, l := range locks {
		l.mu.Unlock()
		if l.refs--; l.refs <= 0 {
			delete(watchLocks.locks, p)
		}
	}
}

// beginEdit captures the pre-state of every path a call is about to touch.
// It returns the snapshots keyed by path, plus the locks now held on them;
// pass both to finishEdit.
//
// The lock is per path rather than one global mutex, and it is keyed to the
// file being edited rather than to the tool call. A single guard held across a
// tool body is not reentrant: `task` runs a whole subagent, whose own tools
// come back through here, so the inner call waits on the lock its own caller
// is holding and the subagent hangs until the session is killed. Per-path
// locks also stop an unrelated file from blocking behind one being edited.
func beginEdit(paths []string) (map[string]snapshot, map[string]*pathLock) {
	locks := acquirePathLocks(paths)
	out := make(map[string]snapshot, len(locks))
	for p := range locks {
		out[p] = snap(p)
	}
	return out, locks
}

// finishEdit reads the post-state, reports the change if there was one, and
// releases the locks beginEdit took. It releases them even when there is
// nothing to report, so a call that changed nothing still cannot strand them.
func finishEdit(before map[string]snapshot, locks map[string]*pathLock, tool, dur, output string, isErr bool) {
	var rec EditRecord
	if len(before) > 0 {
		rec = EditRecord{Tool: tool, Dur: dur, IsErr: isErr, Output: output}
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
		// A patch can touch several files, and the diff reads top-down, so order
		// them by path rather than by map iteration.
		sortFiles(rec.Files)
	}
	// Released before notifying: the watcher ends up on the UI bus, and holding
	// a path lock across that would block every later edit to the same file for
	// as long as the front-end took to drain.
	releasePathLocks(locks)
	if len(rec.Files) > 0 {
		notifyEdit(rec)
	}
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
