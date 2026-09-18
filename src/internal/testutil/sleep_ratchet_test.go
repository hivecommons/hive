package testutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sleepBaseline is the number of `time.Sleep(` occurrences in *_test.go files
// under src/pkg and src/cmd at the commit that introduced this ratchet
// (measured with the same substring count TestSleepRatchet uses, so comments
// count too — exactly like `grep -o 'time\.Sleep(' | wc -l`).
//
// It is a ratchet, not a target: the count may go DOWN freely — lower this
// constant when you remove sleeps so the ratchet keeps biting — but it must
// never go UP. A new wait in a test should be testutil.Eventually (or a
// channel / WaitGroup when a goroutine you own can signal directly); a fixed
// sleep is only acceptable for a deliberate "nothing happens during this
// window" negative wait, and that still needs a comment saying so.
const sleepBaseline = 197

// sleepCall is the literal the ratchet counts. Kept as a constant so the
// message and the count cannot drift apart.
const sleepCall = "time.Sleep("

// sleepRatchetRoots are the trees the ratchet walks, relative to this
// package's directory (src/internal/testutil). src/test is the integration
// suite, which is not part of the PR gate and is deliberately excluded.
var sleepRatchetRoots = []string{
	filepath.Join("..", "..", "pkg"),
	filepath.Join("..", "..", "cmd"),
}

// TestSleepRatchet fails when the number of time.Sleep calls in the unit-test
// trees exceeds sleepBaseline. Fixed sleeps are timing margins: they add wall
// clock to every PR-gate run and are still the first thing to flake on a
// loaded runner (9 of 21 flaky-test tickets before this ratchet were exactly
// that). Runs in well under a second: it is a file walk and a substring count.
func TestSleepRatchet(t *testing.T) {
	perFile := map[string]int{}
	total, files := 0, 0
	for _, root := range sleepRatchetRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			files++
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if n := strings.Count(string(data), sleepCall); n > 0 {
				perFile[filepath.ToSlash(path)] = n
				total += n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	// Fail closed: a walk that saw no test files at all means the ratchet is
	// running from the wrong directory and would otherwise pass vacuously.
	if files == 0 {
		t.Fatalf("no _test.go files found under %v — is the test running from src/internal/testutil?", sleepRatchetRoots)
	}

	switch {
	case total > sleepBaseline:
		t.Fatalf("%d %q calls in *_test.go under %v, above the baseline of %d.\n"+
			"Do not add fixed sleeps to tests: wait on the observable condition with "+
			"testutil.Eventually (or close a channel / sync.WaitGroup when the goroutine is yours). "+
			"If the sleep is a deliberate negative wait, keep it, comment why, and remove another "+
			"sleep so the total does not grow.\n%s",
			total, sleepCall, sleepRatchetRoots, sleepBaseline, topSleepFiles(perFile))
	case total < sleepBaseline:
		t.Logf("%d %q calls, below the baseline of %d — lower sleepBaseline in "+
			"internal/testutil/sleep_ratchet_test.go to %d so the ratchet keeps biting",
			total, sleepCall, sleepBaseline, total)
	}
}

// topSleepFiles renders the files with the most sleeps, most first, so the
// failure message points at where to look rather than just at a number.
func topSleepFiles(perFile map[string]int) string {
	const show = 10
	type entry struct {
		path string
		n    int
	}
	entries := make([]entry, 0, len(perFile))
	for p, n := range perFile {
		entries = append(entries, entry{p, n})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].n != entries[j].n {
			return entries[i].n > entries[j].n
		}
		return entries[i].path < entries[j].path
	})
	if len(entries) > show {
		entries = entries[:show]
	}
	var b strings.Builder
	b.WriteString("files with the most sleeps:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  %3d  %s\n", e.n, e.path)
	}
	return b.String()
}
