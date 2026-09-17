package panes_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLastFrameDropsSupersededRepaints pins the normalization that makes
// TestGridGolden immune to the repaint race in #7264.
//
// This is a direct unit test rather than a second end-to-end teatest run
// because the bug was never reproducible on demand: the scanner could not
// reproduce it in 70 local runs, and it only surfaced on a loaded CI shard.
// A test that can only fail when the scheduler cooperates is not a guard.
// Feeding the function a stream that definitely contains a superseded frame
// tests the same property deterministically.
func TestLastFrameDropsSupersededRepaints(t *testing.T) {
	const (
		prologue = "\x1b[?25l\x1b[?2004h"
		epilogue = "\x1b[2K\x1b[?2004l\x1b[?25h"
	)
	stale := "\rthis frame was repainted over"
	fresh := "\rthe frame the user is looking at"

	single := []byte(prologue + fresh + epilogue)

	tests := []struct {
		name   string
		stream []byte
		want   []byte
	}{
		{
			name:   "single frame is untouched",
			stream: single,
			want:   single,
		},
		{
			name:   "one repaint drops the superseded frame",
			stream: []byte(prologue + stale + "\x1b[24A" + fresh + epilogue),
			want:   single,
		},
		{
			// The race can queue more than one repaint; taking the LAST
			// boundary rather than the first is what makes the result
			// independent of how many occur.
			name:   "several repaints keep only the final frame",
			stream: []byte(prologue + stale + "\x1b[24A" + stale + "\x1b[30A" + stale + "\x1b[7A" + fresh + epilogue),
			want:   single,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastFrame(tc.stream)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("lastFrame(%q)\n got %q\nwant %q", tc.stream, got, tc.want)
			}
		})
	}
}

// TestLastFrameIsIdempotentOnTheCommittedGolden guards the invariant that ties
// the normalization to the file on disk: the committed golden is itself a
// normalized single frame, so normalizing it again must change nothing.
//
// If this ever fails, the golden was regenerated from a multi-frame capture
// and the two halves of the scheme have drifted apart -- which would silently
// re-admit exactly the timing dependence #7264 removed.
func TestLastFrameIsIdempotentOnTheCommittedGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "grid.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got := lastFrame(golden); !bytes.Equal(got, golden) {
		t.Errorf("the committed golden is not a normalized single frame: "+
			"lastFrame changed it from %d to %d bytes", len(golden), len(got))
	}
}
