package agent

// DeduplicateBlocks removes repeated blocks from pane output.
// It finds the longest suffix that also appears earlier (non-overlapping) and
// removes the earlier copy, then repeats until nothing more can be removed.
//
// Lines are normalized ONCE up front and interned to integer ids; block
// matching then runs on a suffix-match table (Z-algorithm over the reversed
// ids) so each pass is O(n²) integer compares at worst instead of the previous
// O(n³) with two allocating normalizeLine calls per compare. The old shape took
// seconds per 500-line agent buffer on near-repeating spinner output and, run
// for every agent on every status rebuild, wedged the dashboard snapshot
// pipeline: rebuilds outlived the next mutation epoch and were dropped, so
// statusSeq stopped advancing.
func DeduplicateBlocks(lines []string) []string {
	if len(lines) < 4 {
		return lines
	}
	ids := internNormalized(lines)
	cur := lines
	changed := false
	for {
		n := len(cur)
		if n < 4 {
			break
		}
		z := suffixMatchLengths(ids)
		start := -1
		// Largest block first; within a block size, the latest earlier
		// occurrence first — the same search order as the original scan.
		for b := n / 2; b >= 2 && start < 0; b-- {
			for e := n - b - 1; e >= b-1; e-- {
				if z[e] >= b {
					start = e - b + 1
					// Remove the earlier duplicate block.
					next := make([]string, 0, n-b)
					next = append(next, cur[:start]...)
					next = append(next, cur[start+b:]...)
					cur = next
					ids = append(ids[:start:start], ids[start+b:]...)
					break
				}
			}
		}
		if start < 0 {
			break
		}
		changed = true
	}
	if !changed {
		return lines
	}
	return cur
}

// internNormalized maps each line to a small integer id such that two lines
// share an id iff their normalizeLine forms are equal.
func internNormalized(lines []string) []int32 {
	ids := make([]int32, len(lines))
	table := make(map[string]int32, len(lines))
	for i, l := range lines {
		key := normalizeLine(l)
		id, ok := table[key]
		if !ok {
			id = int32(len(table))
			table[key] = id
		}
		ids[i] = id
	}
	return ids
}

// suffixMatchLengths returns z where z[e] is the length of the longest block
// ending at index e that equals the block of the same length ending at the
// last index (i.e. the longest common suffix of ids[:e+1] and ids). It is the
// Z-algorithm run over the reversed sequence.
func suffixMatchLengths(ids []int32) []int {
	n := len(ids)
	rev := make([]int32, n)
	for i := range ids {
		rev[i] = ids[n-1-i]
	}
	zr := make([]int, n)
	zr[0] = n
	l, r := 0, 0
	for i := 1; i < n; i++ {
		if i < r {
			zr[i] = min(r-i, zr[i-l])
		}
		for i+zr[i] < n && rev[zr[i]] == rev[i+zr[i]] {
			zr[i]++
		}
		if i+zr[i] > r {
			l, r = i, i+zr[i]
		}
	}
	z := make([]int, n)
	for e := range z {
		z[e] = zr[n-1-e]
	}
	return z
}
