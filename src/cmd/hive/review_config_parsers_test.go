package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
)

// parseReviseCutoff's contract is that a malformed cutoff is logged and
// ignored — never defaulted to "now", which would re-open every verdict in
// the artifact at once. These tests pin the ignore-and-warn behavior and the
// nil-logger path, neither of which was previously exercised.
func TestParseReviseCutoff(t *testing.T) {
	t.Run("empty and whitespace return zero time", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "\t\n"} {
			if got := parseReviseCutoff(raw, nil); !got.IsZero() {
				t.Errorf("parseReviseCutoff(%q) = %v, want zero time", raw, got)
			}
		}
	})

	t.Run("valid RFC3339 is parsed, surrounding whitespace trimmed", func(t *testing.T) {
		want := time.Date(2026, 9, 18, 12, 30, 0, 0, time.UTC)
		got := parseReviseCutoff("  2026-09-18T12:30:00Z  ", nil)
		if !got.Equal(want) {
			t.Errorf("parseReviseCutoff = %v, want %v", got, want)
		}
	})

	t.Run("malformed value is ignored and warned, not defaulted to now", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		got := parseReviseCutoff("2026-09-18 12:30:00", logger) // space, not RFC3339
		if !got.IsZero() {
			t.Errorf("malformed cutoff = %v, want zero time", got)
		}
		if !strings.Contains(buf.String(), "revise_verdicts_before is not RFC3339") {
			t.Errorf("expected warning about non-RFC3339 value, got log: %q", buf.String())
		}
	})

	t.Run("malformed value with nil logger does not panic", func(t *testing.T) {
		if got := parseReviseCutoff("not-a-time", nil); !got.IsZero() {
			t.Errorf("parseReviseCutoff with nil logger = %v, want zero time", got)
		}
	})
}

// reviewPerspectiveSet must fall back to the built-in set — loudly — when the
// configured perspectives are invalid, rather than disabling review. These
// tests pin the fallback, the warning, and the nil-config/nil-logger paths.
func TestReviewPerspectiveSet(t *testing.T) {
	t.Run("nil config returns built-in default set", func(t *testing.T) {
		set := reviewPerspectiveSet(nil, nil)
		if got, want := set.Names(), (review.PerspectiveSet{}).Names(); got != want {
			t.Errorf("nil config set = %q, want default %q", got, want)
		}
	})

	t.Run("valid selection is honored", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Review.Perspectives = []string{"security", "correctness"}
		set := reviewPerspectiveSet(cfg, nil)
		got := set.List()
		want := []review.Perspective{review.PerspectiveSecurity, review.PerspectiveCorrectness}
		if len(got) != len(want) {
			t.Fatalf("List() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("List()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("invalid selection falls back to built-ins and warns", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		cfg := &config.Config{}
		// A custom name with no focus text is invalid: the reviewer would be
		// asked to judge from a perspective never described to it.
		cfg.Review.Perspectives = []string{"sekurity"}
		set := reviewPerspectiveSet(cfg, logger)
		if got, want := set.Names(), (review.PerspectiveSet{}).Names(); got != want {
			t.Errorf("invalid config set = %q, want default %q", got, want)
		}
		if !strings.Contains(buf.String(), "review.perspectives is invalid") {
			t.Errorf("expected warning about invalid perspectives, got log: %q", buf.String())
		}
	})

	t.Run("invalid selection with nil logger does not panic", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Review.Perspectives = []string{"sekurity"}
		set := reviewPerspectiveSet(cfg, nil)
		if got, want := set.Names(), (review.PerspectiveSet{}).Names(); got != want {
			t.Errorf("invalid config set = %q, want default %q", got, want)
		}
	})
}
