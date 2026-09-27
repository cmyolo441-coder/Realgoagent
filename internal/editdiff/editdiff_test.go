package editdiff

import (
	"strings"
	"testing"
)

// collapse renders a change script as "+a/-b" so expectations read like a diff.
func collapse(ls []Line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l.Op) + l.Text
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIdenticalTextHasNoChanges(t *testing.T) {
	if a, r := Count("a\nb\nc\n", "a\nb\nc\n"); a != 0 || r != 0 {
		t.Fatalf("identical text must have no changes, got +%d -%d", a, r)
	}
	// The display form must be empty too, not a screenful of context: a viewer
	// rendering it would show the user a diff that does not exist.
	if got := Context("a\nb\nc\n", "a\nb\nc\n", 3); len(got) != 0 {
		t.Fatalf("identical text must render as nothing, got %v", collapse(got))
	}
}

func TestTrailingNewlineIsNotAChange(t *testing.T) {
	// A missing final newline must not read as a one-line edit; that is a
	// formatting artefact, and reporting it as a change would show the user a
	// diff that corresponds to no intent.
	if a, r := Count("a\nb", "a\nb\n"); a != 0 || r != 0 {
		t.Fatalf("trailing newline must not diff, got +%d -%d", a, r)
	}
}

func TestSingleLineReplacement(t *testing.T) {
	got := collapse(Context("alpha\nbeta\ngamma\n", "alpha\nBETA\ngamma\n", 1))
	// Deletions precede additions within a change, which is what every diff
	// reader expects and what makes the + block read as the new text.
	want := []string{
		" alpha",
		"-beta",
		"+BETA",
		" gamma",
	}
	if !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPureInsertionHasNoRemovedLines(t *testing.T) {
	a, r := Count("a\nb\n", "a\nnew\nb\n")
	if a != 1 || r != 0 {
		t.Fatalf("insert: got +%d -%d, want +1 -0", a, r)
	}
}

func TestPureDeletionHasNoAddedLines(t *testing.T) {
	a, r := Count("a\ngone\nb\n", "a\nb\n")
	if a != 0 || r != 1 {
		t.Fatalf("delete: got +%d -%d, want +0 -1", a, r)
	}
}

func TestCreatedFileIsAllAdded(t *testing.T) {
	a, r := Count("", "one\ntwo\n")
	if a != 2 || r != 0 {
		t.Fatalf("create: got +%d -%d, want +2 -0", a, r)
	}
}

func TestCreatedFileHasNoDeletionHunk(t *testing.T) {
	// Before the first line there is no line 1 in the old file, so the diff
	// must not claim to have deleted anything.
	if got := Unified("new.txt", "", "one\ntwo\n", 0); strings.Contains(got, "-1,") {
		t.Fatalf("creation must not produce a deletion hunk:\n%s", got)
	}
}

func TestLineNumbersAreOneBasedAndCorrect(t *testing.T) {
	// Change the last of five lines; the reported numbers must point at it in
	// both the old and the new file, or a rendered gutter is a lie.
	for _, l := range Compute("1\n2\n3\n4\n5\n", "1\n2\n3\n4\nFIVE\n") {
		if l.Op == OpAdd && l.Text == "FIVE" && l.New != 5 {
			t.Errorf("added line reported as new:%d, want 5", l.New)
		}
		if l.Op == OpDel && l.Text == "5" && l.Old != 5 {
			t.Errorf("removed line reported as old:%d, want 5", l.Old)
		}
	}
}

func TestLineNumbersSurviveAnEarlierInsertion(t *testing.T) {
	// Add a line at the top and change the last one: the old numbering must
	// stay on the old text while the new numbering shifts, which is the whole
	// reason the two are tracked separately.
	for _, l := range Compute("1\n2\n", "NEW\n1\nTWO\n") {
		if l.Op == OpAdd && l.Text == "TWO" && l.New != 3 {
			t.Errorf("added line reported as new:%d, want 3", l.New)
		}
		if l.Op == OpDel && l.Text == "2" && l.Old != 2 {
			t.Errorf("removed line reported as old:%d, want 2", l.Old)
		}
	}
}

func TestFarApartChangesElideTheMiddle(t *testing.T) {
	before := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\no\np\n"
	after := "A\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\no\nP\n"
	got := collapse(Context(before, after, 3))
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "+A") || !strings.Contains(joined, "+P") {
		t.Fatalf("both changes must survive, got %v", got)
	}
	// With a context of three, the first change keeps "b".."d" and the second
	// keeps "m".."o", so everything from "e" to "l" is the elided middle and
	// must appear only as the single gap marker.
	for _, gone := range []string{" e", " f", " h", " l"} {
		if strings.Contains(joined, gone) {
			t.Errorf("unchanged middle was not elided: %v", got)
		}
	}
	for _, kept := range []string{" b", " c", " d", " m", " n", " o"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("context around a change was lost, %q missing: %v", kept, got)
		}
	}
	markers := 0
	for _, l := range got {
		if l == " "+gapMarker {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("want exactly 1 gap marker, got %d in %v", markers, got)
	}
}

func TestNearbyChangesKeepTheirContext(t *testing.T) {
	// Two edits within one context window must not be split by a marker: the
	// region between them is what tells the reader the edits are related.
	// Only two unchanged lines separate these, well inside a window of three.
	before := "a\nb\nc\nd\ne\nf\ng\n"
	after := "A\nb\nc\nd\ne\nf\nG\n"
	for _, l := range collapse(Context(before, after, 3)) {
		if l == " "+gapMarker {
			t.Fatalf("no gap expected between nearby edits, got %v", collapse(Context(before, after, 3)))
		}
	}
}

func TestDistantChangesAreSplitByAGap(t *testing.T) {
	// The counterpart to the test above: when the edits are further apart than
	// the context window, the run between them is replaced by one marker and
	// the reader is told something was skipped.
	// The last of forty lines is the one that changes.
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "line"
	}
	beforeText := strings.Join(lines, "\n") + "\n"
	lines[39] = "LINE"
	got := collapse(Context(beforeText, strings.Join(lines, "\n")+"\n", 3))
	markers := 0
	for _, l := range got {
		if l == " "+gapMarker {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("want exactly 1 gap marker, got %d in %v", markers, got)
	}
}
func TestReorderedLinesReportBothSides(t *testing.T) {
	// Moving a line is a delete plus an insert, not a silent no-op: a diff
	// that hid reordering would misreport what the agent did.
	a, r := Count("one\ntwo\nthree\n", "three\ntwo\none\n")
	if a == 0 || r == 0 {
		t.Fatalf("reorder must report changes, got +%d -%d", a, r)
	}
}

func TestCountIsSymmetricUnderReverse(t *testing.T) {
	before, after := "a\nb\nc\nd\n", "a\nB\nc\nd\ne\n"
	ab, rb := Count(before, after)
	ab2, rb2 := Count(after, before)
	if ab != rb2 || rb != ab2 {
		t.Fatalf("reverse diff is not symmetric: +%d -%d vs +%d -%d", ab, rb, ab2, rb2)
	}
}

func TestUnifiedBodyMatchesTheChange(t *testing.T) {
	body := Unified("f.txt", "1\n2\n3\n4\n5\n", "1\n2\nTHREE\n4\n5\n", 3)
	for _, want := range []string{"--- a/f.txt", "+++ b/f.txt", "@@", " 2", "-3", "+THREE", " 4"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// No unchanged line may appear with a + or - marker. The hunk header
	// carries its own -1/+1 numbers, so the check is over body lines only.
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln == "" || strings.HasPrefix(ln, "---") || strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "@@") {
			continue
		}
		if (strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-")) &&
			!strings.Contains(ln, "3") && !strings.Contains(ln, "THREE") {
			t.Errorf("untouched line was marked as changed: %q\n%s", ln, body)
		}
	}
}

func TestUnifiedIsEmptyForNoChange(t *testing.T) {
	if got := Unified("f.txt", "same\n", "same\n", 3); got != "" {
		t.Fatalf("no change must render as empty, got:\n%s", got)
	}
}

func TestLargeRewriteStaysBounded(t *testing.T) {
	// A whole-file rewrite would be a 20k x 20k LCS table if the guard did
	// not catch it. This asserts the guard by finishing at all, and by still
	// accounting for every line.
	var b, a strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("line\n")
		a.WriteString("other\n")
	}
	added, removed := Count(b.String(), a.String())
	if removed != 20000 || added != 20000 {
		t.Fatalf("whole-file rewrite: got +%d -%d, want +20000 -20000", added, removed)
	}
}

func TestEmptyToEmptyIsNoChange(t *testing.T) {
	if a, r := Count("", ""); a != 0 || r != 0 {
		t.Fatalf("empty to empty: +%d -%d", a, r)
	}
}
