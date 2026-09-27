package util

import "testing"

func TestVisibleWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"a─b", 3},
		{"你好", 4},
		{"\x1b[31mabc\x1b[0m", 3},
		{"─" + "\x1b[0m", 1},
	}
	for _, c := range cases {
		if got := VisibleWidth(c.in); got != c.want {
			t.Errorf("VisibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStrip(t *testing.T) {
	if got := Strip("\x1b[1;31mred\x1b[0m"); got != "red" {
		t.Errorf("Strip = %q", got)
	}
	if got := Strip("\x1b]0;title\x07body"); got != "body" {
		t.Errorf("Strip osc = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := VisibleWidth(Truncate("abcdefghij", 5)); got != 5 {
		t.Errorf("Truncate width = %d", got)
	}
	got := Truncate("你好世界", 3)
	if VisibleWidth(got) != 3 {
		t.Errorf("wide truncate = %q width %d", got, VisibleWidth(got))
	}
	if Strip(got) != "你…" {
		t.Errorf("wide truncate text = %q", Strip(got))
	}
}

func TestWrap(t *testing.T) {
	lines := Wrap("the quick brown fox jumps", 10)
	if len(lines) < 2 {
		t.Fatalf("expected wrap, got %v", lines)
	}
	for _, l := range lines {
		if VisibleWidth(l) > 10 {
			t.Errorf("line too wide: %q (%d)", l, VisibleWidth(l))
		}
	}
}

func TestWrapKeepsANSIStyle(t *testing.T) {
	src := "\x1b[31mred\x1b[0m text that must wrap around the width limit"
	for _, l := range Wrap(src, 14) {
		if VisibleWidth(l) > 14 {
			t.Errorf("line too wide: %q (%d)", l, VisibleWidth(l))
		}
		if !containsESC(l) {
			continue
		}
	}
}

func containsESC(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return true
		}
	}
	return false
}

func TestPadRight(t *testing.T) {
	if got := VisibleWidth(PadRight("ab", 5)); got != 5 {
		t.Errorf("PadRight = %q", got)
	}
}
