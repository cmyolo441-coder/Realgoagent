package agent

import (
	"strings"
	"testing"
)

// feed runs a whole reply through the filter one chunk at a time.
func feedReply(t *testing.T, chunks []string) string {
	t.Helper()
	f := &ThinkFilter{}
	var out strings.Builder
	for _, c := range chunks {
		out.WriteString(f.Feed(c))
	}
	out.WriteString(f.Flush())
	return out.String()
}

// A reasoning block must not reach the user.
func TestThinkFilterHidesReasoning(t *testing.T) {
	got := feedReply(t, []string{"<think>let me work this out, hmm</think>Here is the answer."})
	if got != "Here is the answer." {
		t.Errorf("output = %q, want only the answer", got)
	}
	if strings.Contains(got, "let me work") {
		t.Error("reasoning leaked into the output")
	}
}

// The real hazard: a tag split across two chunks. "<thi" + "nk>" must still be
// recognised, or the reasoning pours onto the screen mid-word.
func TestThinkFilterHandlesTagSplitAcrossChunks(t *testing.T) {
	for _, split := range []struct {
		chunks []string
		want   string
	}{
		{[]string{"<thi", "nk>hidden</think>visible"}, "visible"},
		{[]string{"<think", ">hidden</think>visible"}, "visible"},
		{[]string{"visible<thin", "k>hidden</think>more"}, "visiblemore"},
		{[]string{"visible<", "think>hidden</think>more"}, "visiblemore"},
		{[]string{"<think>hi", "d</think>visible"}, "visible"},
		// Closing tag split across chunks, with a block actually open.
		{[]string{"<think>hidden</thi", "nk>tail"}, "tail"},
		{[]string{"<think>hidden</think", ">tail"}, "tail"},
	} {
		got := feedReply(t, split.chunks)
		if got != split.want {
			t.Errorf("chunks %q: output = %q, want %q", split.chunks, got, split.want)
		}
	}
}

// </think> closes a block, it never opens one. A reply that never opens a block
// has nothing to hide, so its text — tags aside — stays visible.
func TestThinkFilterCloseTagDoesNotOpenABlock(t *testing.T) {
	got := feedReply(t, []string{"visible</think>hidden</think>tail"})
	if got != "visiblehiddentail" {
		t.Errorf("output = %q, want the text kept and only the tags dropped", got)
	}
}

// Providers that close with </think> instead of </think> must still end the
// block, or the whole answer after it is swallowed.
func TestThinkFilterAcceptsCloseTagWithoutSlash(t *testing.T) {
	got := feedReply(t, []string{"<think>hidden</think>the answer"})
	if got != "the answer" {
		t.Errorf("output = %q, want %q", got, "the answer")
	}
	if strings.Contains(got, "hidden") {
		t.Error("reasoning leaked")
	}
}

// A reply that never closes its reasoning block is reasoning all the way to
// the end, so nothing after the tag may be shown.
func TestThinkFilterHidesUnterminatedBlock(t *testing.T) {
	got := feedReply(t, []string{"answer first<think>still going", "and going"})
	if got != "answer first" {
		t.Errorf("output = %q, want only the text before the tag", got)
	}
}

// A closing tag with no opening is stray and must be dropped, not shown.
func TestThinkFilterDropsStrayClosingTag(t *testing.T) {
	got := feedReply(t, []string{"visible</think>also visible"})
	if got != "visiblealso visible" {
		t.Errorf("output = %q, want the stray tag removed", got)
	}
}

// A '<' that is not a tag is ordinary text and must survive untouched.
func TestThinkFilterKeepsLiteralAngleBrackets(t *testing.T) {
	for _, in := range []string{
		"if a < b and b > c",
		"generic List<T> is fine",
		"<div>html</div>",
		"a < b",
	} {
		if got := feedReply(t, []string{in}); got != in {
			t.Errorf("input %q: output %q, want it unchanged", in, got)
		}
	}
}

// Content that merely ends in something tag-like must not be swallowed.
func TestThinkFilterKeepsPartialTagLikeText(t *testing.T) {
	got := feedReply(t, []string{"compare with <thi", "s value"})
	if got != "compare with <this value" {
		t.Errorf("output = %q, want the text kept", got)
	}
}

// Providers are inconsistent about casing.
func TestThinkFilterIsCaseInsensitive(t *testing.T) {
	got := feedReply(t, []string{"<THINK>hidden</THINK>shown"})
	if got != "shown" {
		t.Errorf("output = %q, want %q", got, "shown")
	}
}

// Hidden counts the reasoning, so the UI can report it rather than silently
// dropping text.
func TestThinkFilterCountsHidden(t *testing.T) {
	f := &ThinkFilter{}
	f.Feed("<think>12345</think>done")
	f.Flush()
	if f.Hidden() != len("<think>12345</think>") {
		t.Errorf("Hidden() = %d, want %d", f.Hidden(), len("<think>12345</think>"))
	}
}

// A reply with no reasoning reports nothing hidden, so the UI stays quiet.
func TestThinkFilterNoReasoningReportsZero(t *testing.T) {
	f := &ThinkFilter{}
	f.Feed("just an answer")
	f.Flush()
	if f.Hidden() != 0 {
		t.Errorf("Hidden() = %d, want 0", f.Hidden())
	}
}

// Feeding a reply in one piece must match feeding it byte by byte, or the
// output depends on how the provider chunked its stream.
func TestThinkFilterIsChunkingIndependent(t *testing.T) {
	reply := "before<think>hidden reasoning</think>after <not a tag> done"
	want := feedReply(t, []string{reply})

	var pieces []string
	for i := 0; i < len(reply); i++ {
		pieces = append(pieces, reply[i:i+1])
	}
	if got := feedReply(t, pieces); got != want {
		t.Errorf("byte-by-byte output = %q, want %q", got, want)
	}

	// And in uneven slices.
	for _, size := range []int{2, 3, 5, 7, 11} {
		var slices []string
		for i := 0; i < len(reply); i += size {
			end := i + size
			if end > len(reply) {
				end = len(reply)
			}
			slices = append(slices, reply[i:end])
		}
		if got := feedReply(t, slices); got != want {
			t.Errorf("slices of %d: output = %q, want %q", size, got, want)
		}
	}
}
