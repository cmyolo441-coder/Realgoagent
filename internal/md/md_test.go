package md

import (
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

func render(t *testing.T, src string, width int) []string {
	t.Helper()
	return New(theme.Get("mono"), width).Render(src)
}

func TestHeadings(t *testing.T) {
	out := render(t, "# Title\n\nbody", 40)
	if len(out) == 0 {
		t.Fatal("no output")
	}
	if util.Strip(out[0]) != "Title" {
		t.Errorf("heading = %q", util.Strip(out[0]))
	}
	if !strings.Contains(out[1], "──") {
		t.Errorf("expected rule under h1, got %q", out[1])
	}
}

func TestBulletList(t *testing.T) {
	out := render(t, "- one\n- two", 40)
	if len(out) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(out), out)
	}
	if !strings.Contains(util.Strip(out[0]), "• one") {
		t.Errorf("bullet = %q", util.Strip(out[0]))
	}
}

func TestCheckbox(t *testing.T) {
	out := render(t, "- [x] done\n- [ ] todo", 40)
	if !strings.Contains(util.Strip(out[0]), "☑") {
		t.Errorf("checked = %q", util.Strip(out[0]))
	}
	if !strings.Contains(util.Strip(out[1]), "☐") {
		t.Errorf("unchecked = %q", util.Strip(out[1]))
	}
}

func TestCodeBlock(t *testing.T) {
	out := render(t, "```go\nfunc x() {}\n```", 40)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "1") || !strings.Contains(joined, "func") {
		t.Errorf("code block missing line numbers: %q", joined)
	}
	if !strings.Contains(joined, "╭") || !strings.Contains(joined, "╰") {
		t.Errorf("code block missing rails: %q", joined)
	}
}

func TestInlineCodeNotCorrupted(t *testing.T) {
	// This is the regression that mattered: code spans had to survive
	// emphasis and link rewriting without ANSI bleed.
	out := render(t, "text `code` and **bold** and [link](http://x)", 80)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "[38;2;34;211;238m38;2") {
		t.Fatalf("ANSI bleed detected: %q", joined)
	}
	if !strings.Contains(util.Strip(joined), "code") {
		t.Errorf("code span lost: %q", util.Strip(joined))
	}
	if !strings.Contains(util.Strip(joined), "bold") {
		t.Errorf("bold lost: %q", util.Strip(joined))
	}
}

func TestTable(t *testing.T) {
	out := render(t, "| A | B |\n|---|---|\n| 1 | 2 |", 40)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "┼") {
		t.Errorf("table separator missing: %q", joined)
	}
	if !strings.Contains(util.Strip(joined), "A") {
		t.Errorf("table header missing: %q", joined)
	}
}

func TestQuote(t *testing.T) {
	out := render(t, "> hello", 40)
	if !strings.Contains(util.Strip(out[0]), "hello") {
		t.Errorf("quote = %q", util.Strip(out[0]))
	}
}

func TestFileRefHighlight(t *testing.T) {
	out := render(t, "see `internal/app.go:42`", 80)
	if !strings.Contains(util.Strip(out[0]), "internal/app.go:42") {
		t.Errorf("file ref lost")
	}
}

func TestWidthRespected(t *testing.T) {
	out := render(t, strings.Repeat("word ", 12), 30)
	for _, l := range out {
		if util.VisibleWidth(l) > 30 {
			t.Errorf("line too wide (%d): %q", util.VisibleWidth(l), l)
		}
	}
}

func TestHighlighterPreservesText(t *testing.T) {
	// the colouriser must never drop or reorder characters
	for _, lang := range []string{"go", "py", "js", "rs", "json", "sh", "yaml", "unknown"} {
		for _, line := range []string{
			`func main() { fmt.Println("x") }`,
			`x = 1  # comment`,
			`const a = 'b';`,
			`let x: number = 3;`,
			`{"a": 1, "b": true}`,
			`echo "hi" && ls -la`,
			`key: value`,
		} {
			got := util.Strip(highlight(lang, line))
			if got != line {
				t.Errorf("%s: highlight changed text:\n got %q\nwant %q", lang, got, line)
			}
		}
	}
}
