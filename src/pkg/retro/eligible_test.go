package retro

import (
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/beads"
)

func TestEligible(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	window := 24 * time.Hour

	cases := []struct {
		name string
		lane *Lane
		bead *beads.Bead
		want bool
	}{
		{
			name: "nil bead",
			lane: &Lane{},
			bead: nil,
			want: false,
		},
		{
			name: "advisory beads are never analyzed",
			lane: &Lane{},
			bead: &beads.Bead{Type: beads.TypeAdvisory, Status: beads.StatusClosed},
			want: false,
		},
		{
			name: "open bead not eligible",
			lane: &Lane{},
			bead: &beads.Bead{Status: beads.StatusOpen},
			want: false,
		},
		{
			name: "closed bead eligible",
			lane: &Lane{},
			bead: &beads.Bead{Status: beads.StatusClosed},
			want: true,
		},
		{
			name: "done bead eligible",
			lane: &Lane{},
			bead: &beads.Bead{Status: beads.StatusDone},
			want: true,
		},
		{
			name: "already analyzed bead skipped",
			lane: &Lane{},
			bead: &beads.Bead{
				Status:   beads.StatusClosed,
				Metadata: map[string]interface{}{metadataAnalyzedAt: "2026-08-28T00:00:00Z"},
			},
			want: false,
		},
		{
			name: "closed outside recent window skipped",
			lane: &Lane{recentWindow: window},
			bead: &beads.Bead{
				Status:   beads.StatusClosed,
				Metadata: map[string]interface{}{"closed_at": now.Add(-48 * time.Hour).Format(time.RFC3339)},
			},
			want: false,
		},
		{
			name: "closed inside recent window eligible",
			lane: &Lane{recentWindow: window},
			bead: &beads.Bead{
				Status:   beads.StatusClosed,
				Metadata: map[string]interface{}{"closed_at": now.Add(-1 * time.Hour).Format(time.RFC3339)},
			},
			want: true,
		},
		{
			name: "zero completedAt with window still eligible",
			lane: &Lane{recentWindow: window},
			bead: &beads.Bead{
				Status:   beads.StatusClosed,
				Metadata: map[string]interface{}{"closed_at": "not-a-timestamp"},
			},
			// completedAt falls back to a zero UpdatedAt, and a zero
			// completion time bypasses the window check entirely.
			want: true,
		},
		{
			name: "no window ignores age entirely",
			lane: &Lane{},
			bead: &beads.Bead{
				Status:   beads.StatusClosed,
				Metadata: map[string]interface{}{"closed_at": now.Add(-1000 * time.Hour).Format(time.RFC3339)},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lane.eligible(tc.bead, now); got != tc.want {
				t.Fatalf("eligible() = %v, want %v", got, tc.want)
			}
		})
	}
}
