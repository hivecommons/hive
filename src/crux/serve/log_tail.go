package serve

// newTail returns the lines of cur that were not already seen in prev,
// treating both as windows onto the same append-only buffer: the longest
// suffix of prev that is a prefix of cur is the overlap. It is the CLI's
// stream rule (crux/cli.newTail) applied server side.
func newTail(prev, cur []string) []string {
	max := len(prev)
	if len(cur) < max {
		max = len(cur)
	}
	for k := max; k > 0; k-- {
		if equalLines(prev[len(prev)-k:], cur[:k]) {
			return cur[k:]
		}
	}
	return cur
}

func equalLines(a, b []string) bool {
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
