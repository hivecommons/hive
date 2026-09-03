package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestNormalizedAutoMergeLabel(t *testing.T) {
	for _, tt := range []struct {
		name  string
		label string
		want  string
	}{
		{name: "configured", label: "ship-it", want: "ship-it"},
		{name: "trims", label: "  ship-it  ", want: "ship-it"},
		{name: "blank falls back", label: " \t\n ", want: github.AutoMergeQueuedLabel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizedAutoMergeLabel(tt.label); got != tt.want {
				t.Fatalf("normalizedAutoMergeLabel(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}
