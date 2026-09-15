package agent

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// referenceDeduplicateBlocks is the pre-optimization O(n³) implementation,
// kept here as the oracle for the differential test below.
func referenceDeduplicateBlocks(lines []string) []string {
	if len(lines) < 4 {
		return lines
	}
	maxBlock := len(lines) / 2
	for blockSize := maxBlock; blockSize >= 2; blockSize-- {
		candidate := lines[len(lines)-blockSize:]
		for start := len(lines) - blockSize - 1; start >= 0; start-- {
			if start+blockSize > len(lines)-blockSize {
				continue
			}
			match := true
			for j := 0; j < blockSize; j++ {
				if normalizeLine(lines[start+j]) != normalizeLine(candidate[j]) {
					match = false
					break
				}
			}
			if match {
				result := make([]string, 0, len(lines)-blockSize)
				result = append(result, lines[:start]...)
				result = append(result, lines[start+blockSize:]...)
				return referenceDeduplicateBlocks(result)
			}
		}
	}
	return lines
}

// nearRepeatPane models a Copilot/Claude pane: long runs of identical spinner
// lines punctuated by a unique line every few lines, so no block ever fully
// repeats but every candidate block matches several lines before failing —
// the worst case for the old per-compare normalizeLine scan.
func nearRepeatPane(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		if i%8 == 0 {
			lines[i] = fmt.Sprintf("  ⎿ Read src/pkg/file_%d.go (%d lines)", i, 40+i)
		} else {
			lines[i] = "│ ⠋ Working… (esc to interrupt)"
		}
	}
	return lines
}

func TestDeduplicateBlocksMatchesReference(t *testing.T) {
	alphabet := []string{"a", "b", "c", "d ", "d\t", "⠋ spin", "⠙ spin", "AI Credits: 12", "AI Credits: 99", ""}
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		n := rng.Intn(40)
		k := 1 + rng.Intn(len(alphabet))
		lines := make([]string, n)
		for i := range lines {
			lines[i] = alphabet[rng.Intn(k)]
		}
		got := DeduplicateBlocks(append([]string(nil), lines...))
		want := referenceDeduplicateBlocks(append([]string(nil), lines...))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iter %d: input %q\n got %q\nwant %q", iter, lines, got, want)
		}
	}
	for _, in := range [][]string{nearRepeatPane(64), nearRepeatPane(outputBufferCapacity)} {
		got := DeduplicateBlocks(in)
		want := referenceDeduplicateBlocks(in)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("near-repeat %d: got %d lines, want %d", len(in), len(got), len(want))
		}
	}
}

func TestDeduplicateBlocksReturnsInputWhenUnchanged(t *testing.T) {
	in := []string{"a", "b", "c", "d", "e"}
	if got := DeduplicateBlocks(in); &got[0] != &in[0] {
		t.Fatalf("expected the input slice back when nothing was removed")
	}
}

// A full-size agent buffer must dedupe in well under the status rebuild
// budget. The bound is generous because CI runs with -race on shared
// runners (~0.4s observed); the old implementation took 3.7s unraced on an
// idle M2 Pro and tens of seconds under -race.
func TestDeduplicateBlocksFullBufferIsFast(t *testing.T) {
	lines := nearRepeatPane(outputBufferCapacity)
	start := time.Now()
	DeduplicateBlocks(lines)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("DeduplicateBlocks on %d lines took %s", len(lines), d)
	}
}

func BenchmarkDeduplicateBlocksNearRepeat(b *testing.B) {
	lines := nearRepeatPane(outputBufferCapacity)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DeduplicateBlocks(lines)
	}
}
