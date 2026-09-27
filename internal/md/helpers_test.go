package md

import (
	"strings"
	"testing"
)

// A tool call is printed in the transcript, so a formatter that falls back to
// fmt.Sprint would put Go's map syntax on the user's screen — randomised
// order, brackets and colons. These cover the tools that would otherwise hit
// that path.

func TestFormatBriefForTaskNamesTheRole(t *testing.T) {
	got := FormatBrief("task", map[string]any{
		"role": "explore",
		"task": "find every caller of Registry.Call",
	})
	if !strings.HasPrefix(got, "task explore") {
		t.Fatalf("got %q, want it to start with the role", got)
	}
	if !strings.Contains(got, "find every caller") {
		t.Errorf("the task must be shown: %q", got)
	}
	if strings.Contains(got, "map[") {
		t.Errorf("a Go map leaked into the transcript: %q", got)
	}
}

func TestFormatBriefNeverPrintsAMap(t *testing.T) {
	// Any tool without a dedicated formatter must still render readably.
	got := FormatBrief("some_new_tool", map[string]any{
		"zeta":  1,
		"alpha": "value",
	})
	if strings.Contains(got, "map[") {
		t.Fatalf("map syntax leaked: %q", got)
	}
	if !strings.Contains(got, "alpha=value") || !strings.Contains(got, "zeta=1") {
		t.Errorf("got %q, want key=value pairs", got)
	}
}

func TestFormatBriefIsStableAcrossCalls(t *testing.T) {
	// Go randomises map iteration, so an unsorted fallback would print the
	// same call differently every time — visibly flickering in the transcript.
	args := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5}
	first := FormatBrief("t", args)
	for i := 0; i < 50; i++ {
		if got := FormatBrief("t", args); got != first {
			t.Fatalf("unstable output: %q vs %q", got, first)
		}
	}
}

func TestFormatBriefTruncatesLongTasks(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := FormatBrief("task", map[string]any{"role": "plan", "task": long})
	if len([]rune(got)) > 100 {
		t.Errorf("a 500-char task must be truncated, got %d runes", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncation should be visible: %q", got)
	}
}

func TestFormatArgsDropsTheToolName(t *testing.T) {
	args := map[string]any{"role": "explore", "task": "look"}
	if got := FormatArgs("task", args); strings.HasPrefix(got, "task") {
		t.Errorf("FormatArgs must not repeat the name: %q", got)
	}
}
