package agent

import "strings"

// Reasoning models emit their scratchpad inline in the content stream, wrapped
// in <think>…</think>. Nova shows content to the user as it arrives, so that
// text would land on screen raw — walls of internal monologue between the
// user and the answer.
//
// The filter has to work on a stream, not on a finished string: a tag can be
// split across two chunks ("<thi" then "nk>"), so anything that might still
// grow into a tag is held back until it is decided.

// The literal delimiters, matched case-insensitively since providers are
// inconsistent about casing.
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// Closing delimiters. Providers disagree: most send </think>, but several
// send the same tag without the slash, so both have to be recognised or the
// filter misses the end of a block and swallows the whole answer.
var thinkCloseTags = []string{thinkClose, "<think>"}

// thinkTag pairs a delimiter with what it means.
type thinkTag struct {
	text string
	open bool
}

// thinkTags is every delimiter stripped from the stream.
var thinkTags = buildThinkTags()

func buildThinkTags() []thinkTag {
	tags := make([]thinkTag, 0, 1+len(thinkCloseTags))
	tags = append(tags, thinkTag{text: thinkOpen, open: true})
	for _, c := range thinkCloseTags {
		tags = append(tags, thinkTag{text: c})
	}
	return tags
}

// maxTagLen is the length of the longest tag; nothing shorter can be a
// complete tag, and nothing longer can be a partial one.
const maxTagLen = 8

// ThinkFilter removes reasoning blocks from a token stream.
type ThinkFilter struct {
	// pending holds text withheld because it may be the start of a tag.
	pending strings.Builder
	// inThink is set between an opening and a closing tag.
	inThink bool
	// hidden counts the characters swallowed, so the UI can report that
	// reasoning was hidden rather than silently losing it.
	hidden int
}

// Feed consumes a chunk of streamed content and returns the part meant for the
// user, which may be empty.
func (f *ThinkFilter) Feed(s string) string {
	f.pending.WriteString(s)
	return f.drain(false)
}

// Flush returns whatever is left once the stream ends. An unterminated
// <think> is treated as running to the end of the reply, since that is what
// the model meant; a stray closing tag is dropped.
func (f *ThinkFilter) Flush() string {
	return f.drain(true)
}

// Hidden reports how many characters of reasoning were suppressed.
func (f *ThinkFilter) Hidden() int { return f.hidden }

// InThink reports whether the stream is currently inside a reasoning block.
func (f *ThinkFilter) InThink() bool { return f.inThink }

// drain moves everything decidable out of the pending buffer. At end of stream
// nothing is held back.
//
// buf[:held] is the undecided region; everything before it has been emitted or
// swallowed. Only the undecided region goes back into pending, so nothing is
// ever emitted twice — which is what a naive "return early and leave pending
// alone" does, duplicating every chunk that arrived after a partial tag.
func (f *ThinkFilter) drain(final bool) string {
	var out strings.Builder
	buf := f.pending.String()
	held := len(buf)

	emit := func(s string) {
		if s == "" {
			return
		}
		if f.inThink {
			f.hidden += len(s)
			return
		}
		out.WriteString(s)
	}

	for held > 0 {
		// A tag can only start at a '<'.
		i := strings.IndexByte(buf[:held], '<')
		if i < 0 {
			// Nothing here can be a tag; withhold only a tail that could still
			// grow into one.
			keep := 0
			if !final {
				keep = tagPrefixLen(buf[:held])
			}
			emit(buf[:held-keep])
			held = keep
			break
		}

		emit(buf[:i])
		buf, held = buf[i:], held-i

		tag, size, decided := matchTag(buf[:held], final)
		if !decided {
			break // wait for more input
		}
		switch tag {
		case thinkOpenTag:
			f.inThink = true
			f.hidden += size
		case thinkCloseTag:
			// A closing tag outside a block is stray; drop it.
			f.inThink = false
			f.hidden += size
		default:
			// Not a tag: the '<' is ordinary text.
			emit("<")
			buf, held = buf[1:], held-1
			continue
		}
		buf, held = buf[size:], held-size
	}

	f.pending.Reset()
	f.pending.WriteString(buf[:held])
	return out.String()
}

// Tag identity, kept as constants so a caller cannot confuse the marker with
// the literal text.
const (
	thinkOpenTag  = 1
	thinkCloseTag = 2
)

// matchTag decides what text starting at '<' is: a complete tag, of which size,
// or a partial one that may still grow. decided is false when more input could
// change the answer.
func matchTag(s string, final bool) (tag, size int, decided bool) {
	partial := false
	for _, t := range thinkTags {
		if len(s) < len(t.text) {
			if strings.EqualFold(s, t.text[:len(s)]) {
				partial = true
			}
			continue
		}
		if strings.EqualFold(s[:len(t.text)], t.text) {
			if t.open {
				return thinkOpenTag, len(t.text), true
			}
			return thinkCloseTag, len(t.text), true
		}
	}
	if partial && !final {
		return 0, 0, false
	}
	return 0, 0, true
}

// tagPrefixLen returns how many trailing bytes of s could still grow into a
// tag, and so must be withheld.
func tagPrefixLen(s string) int {
	max := len(s)
	if max > maxTagLen-1 {
		max = maxTagLen - 1
	}
	for n := max; n > 0; n-- {
		tail := s[len(s)-n:]
		for _, t := range thinkTags {
			if len(tail) < len(t.text) && strings.EqualFold(tail, t.text[:len(tail)]) {
				return n
			}
		}
	}
	return 0
}
