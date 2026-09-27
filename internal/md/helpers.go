package md

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// min/max helpers for Go 1.21 compatibility (1.22 has builtins but be explicit).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Render is a convenience wrapper.
func Render(src string, p theme.Palette, width int) string {
	return strings.Join(New(p, width).Render(src), "\n")
}

// RenderPlain strips style and returns plain text (useful for tests/copy).
func RenderPlain(src string, width int) string {
	return util.Strip(Render(src, theme.Get(""), width))
}

// SpinnerFrames are braille dot frames for an indeterminate spinner.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// DotsFrames are a simpler ASCII fallback.
var DotsFrames = []string{".  ", ".. ", "...", " ..", "  .", "   "}

// Frames returns spinner frames for the requested style.
func Frames(name string) []string {
	switch strings.ToLower(name) {
	case "dots", "ascii":
		return DotsFrames
	case "line", "pipe":
		return []string{"|", "/", "-", "\\"}
	case "arrow":
		return []string{"←", "↖", "↑", "↗", "→", "↘", "↓", "↙"}
	default:
		return SpinnerFrames
	}
}

// FormatArgs renders the interesting arguments of a tool call, without the
// tool name. Callers that already show the name use this; FormatBrief is for
// the ones that do not.
func FormatArgs(name string, args map[string]any) string {
	brief := FormatBrief(name, args)
	// FormatBrief always starts with the tool name; drop it.
	return strings.TrimSpace(strings.TrimPrefix(brief, name))
}

// FormatBrief renders a compact one-line description for tool invocation logs.
func FormatBrief(name string, args map[string]any) string {
	switch name {
	case "bash":
		c, _ := args["command"].(string)
		if d, _ := args["description"].(string); d != "" {
			return fmt.Sprintf("%s %s", name, truncate(c, 60))
		}
		return name + " " + truncate(c, 60)
	case "read", "write", "edit", "multi_edit":
		p, _ := args["path"].(string)
		return name + " " + p
	case "grep":
		pat, _ := args["pattern"].(string)
		return name + " " + pat
	case "glob":
		pat, _ := args["pattern"].(string)
		return name + " " + pat
	case "list_dir":
		p, _ := args["path"].(string)
		if p == "" {
			p = "."
		}
		return name + " " + p
	case "task":
		role, _ := args["role"].(string)
		task, _ := args["task"].(string)
		if role == "" {
			role = "subagent"
		}
		return name + " " + role + "  " + truncate(task, 60)
	case "patch":
		d, _ := args["patch"].(string)
		return name + " " + truncate(strings.TrimSpace(d), 60)
	case "todo":
		t, _ := args["task"].(string)
		return name + " " + t
	case "ask_user":
		q, _ := args["question"].(string)
		return name + " " + truncate(q, 60)
	}
	if len(args) == 0 {
		return name
	}
	// No formatter for this tool: render the arguments as a stable, readable
	// "key=value" list rather than fmt.Sprint's map output, which depends on
	// Go's randomised map order and reads as a debug dump on screen.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(fmt.Sprint(args[k]), 32)))
	}
	return name + " " + strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if util.VisibleWidth(s) <= n {
		return s
	}
	return util.Truncate(s, n)
}
