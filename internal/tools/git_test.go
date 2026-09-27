package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitAvailable skips the git tests when git is not installed, rather than
// failing for a reason that has nothing to do with the code.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// nonRepoDir returns an empty directory that is not inside a git repository.
func nonRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// t.TempDir can land inside a repository, so check rather than assume.
	if _, ok := checkGitRepo(dir); ok {
		t.Skip("temp dir is inside a git repository")
	}
	return dir
}

// Outside a repository, the git tools must explain the situation instead of
// returning git's fatal message. The old behaviour produced a wall of stderr
// that read like a tool failure and cost the model a turn.
func TestGitStatusExplainsNonRepository(t *testing.T) {
	gitAvailable(t)
	dir := nonRepoDir(t)
	SetWorkspace(dir, "/bin/sh")
	defer SetWorkspace("", "/bin/sh")

	res := gitStatusTool{}.Run(nil, map[string]any{})
	if res.IsError {
		t.Errorf("a non-repository is a fact, not a tool failure; got IsError with %q", res.Output)
	}
	if !strings.Contains(res.Output, "not a git repository") {
		t.Errorf("output = %q, want it to say the directory is not a repository", res.Output)
	}
	if strings.Contains(res.Output, "fatal:") {
		t.Errorf("output = %q, want no raw git stderr", res.Output)
	}
	// The message has to say what to do instead, or the model is stuck.
	if !strings.Contains(res.Output, "list_dir") {
		t.Errorf("output = %q, want it to suggest an alternative", res.Output)
	}
}

// git_diff must behave the same way.
func TestGitDiffExplainsNonRepository(t *testing.T) {
	gitAvailable(t)
	dir := nonRepoDir(t)
	SetWorkspace(dir, "/bin/sh")
	defer SetWorkspace("", "/bin/sh")

	res := gitDiffTool{}.Run(nil, map[string]any{})
	if res.IsError {
		t.Errorf("a non-repository is a fact, not a tool failure; got IsError with %q", res.Output)
	}
	if !strings.Contains(res.Output, "not a git repository") {
		t.Errorf("output = %q, want it to say the directory is not a repository", res.Output)
	}
	if strings.Contains(res.Output, "fatal:") {
		t.Errorf("output = %q, want no raw git stderr", res.Output)
	}
}

// A real repository must report the branch and a working-tree summary, with no
// separator lines or raw porcelain codes.
func TestGitStatusInRepository(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")

	SetWorkspace(dir, "/bin/sh")
	defer SetWorkspace("", "/bin/sh")

	res := gitStatusTool{}.Run(nil, map[string]any{})
	if res.IsError {
		t.Fatalf("Run failed: %q", res.Output)
	}
	for _, want := range []string{"branch:", "working tree:", "recent commits:", "first"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output = %q, want it to contain %q", res.Output, want)
		}
	}
	if strings.Contains(res.Output, "---") {
		t.Errorf("output = %q, want no separator line", res.Output)
	}
}

// An uncommitted change must be counted, not dumped as porcelain codes.
func TestGitStatusCountsUncommitted(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	SetWorkspace(dir, "/bin/sh")
	defer SetWorkspace("", "/bin/sh")

	res := gitStatusTool{}.Run(nil, map[string]any{})
	if !strings.Contains(res.Output, "working tree: 1 changed") {
		t.Errorf("output = %q, want the change counted", res.Output)
	}
}

// A clean tree says so plainly.
func TestGitStatusCleanTree(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")

	SetWorkspace(dir, "/bin/sh")
	defer SetWorkspace("", "/bin/sh")

	res := gitStatusTool{}.Run(nil, map[string]any{})
	if !strings.Contains(res.Output, "working tree: clean") {
		t.Errorf("output = %q, want it to say the tree is clean", res.Output)
	}
}

// An empty diff is a fact about the repository, not an empty result the model
// has to interpret.
func TestGitDiffEmptyIsExplained(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")

	SetWorkspace(dir, "/bin/sh")
	defer SetWorkspace("", "/bin/sh")

	res := gitDiffTool{}.Run(nil, map[string]any{})
	if res.IsError {
		t.Errorf("an empty diff is not an error; got %q", res.Output)
	}
	if !strings.Contains(res.Output, "no uncommitted changes") {
		t.Errorf("output = %q, want it to say there is nothing to show", res.Output)
	}
}

func TestSummarisePorcelain(t *testing.T) {
	in := " M a.txt\n?? new.txt\nM  staged.txt\n"
	changed, staged := summarisePorcelain(in)
	if changed != 3 {
		t.Errorf("changed = %d, want 3", changed)
	}
	// " M" is unstaged, "??" is untracked, "M " is staged.
	if staged != 1 {
		t.Errorf("staged = %d, want 1", staged)
	}
	if c, s := summarisePorcelain(""); c != 0 || s != 0 {
		t.Errorf("empty porcelain gave (%d, %d), want (0, 0)", c, s)
	}
}
