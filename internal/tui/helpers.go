package tui

// truncMiddle shortens a string with an ellipsis in the middle, keeping both
// ends readable. Paths and model ids are the usual callers, where the tail
// carries the distinguishing part.
func truncMiddle(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 3 {
		return string(r[:n])
	}
	keep := (n - 1) / 2
	return string(r[:keep]) + "…" + string(r[len(r)-keep-1:])
}
