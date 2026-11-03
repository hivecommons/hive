package logscrub

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestWithGroup covers the WithGroup passthrough, which wraps the inner
// handler's WithGroup and preserves the Handler type.
func TestWithGroup(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewHandler(inner)

	grouped := h.WithGroup("mygroup")
	if _, ok := grouped.(*Handler); !ok {
		t.Fatalf("WithGroup should return a *Handler, got %T", grouped)
	}

	logger := slog.New(grouped)
	logger.Info("grouped message", "secret", "ghs_grouptoken1234567890")
	out := buf.String()
	if !strings.Contains(out, "mygroup") {
		t.Errorf("expected group name in output: %s", out)
	}
	if strings.Contains(out, "ghs_") {
		t.Errorf("token in grouped attr not scrubbed: %s", out)
	}
}

// TestScrubAttrGroupKind covers the slog.KindGroup branch of scrubAttr, where
// a group attribute's nested attrs must each be recursively scrubbed.
func TestScrubAttrGroupKind(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewHandler(inner))

	logger.Info("grouped attrs",
		slog.Group("auth",
			slog.String("token", "ghp_nestedtoken1234567890"),
			slog.Int("count", 3),
		),
	)
	out := buf.String()
	if strings.Contains(out, "ghp_") {
		t.Errorf("token in nested group attr not scrubbed: %s", out)
	}
	if !strings.Contains(out, "count=3") {
		t.Errorf("non-secret nested attr altered: %s", out)
	}
}

// TestScrubAttrOtherKinds covers scrubAttr's passthrough branch for kinds
// that are neither KindString nor KindGroup (e.g. KindInt64, KindBool).
func TestScrubAttrOtherKinds(t *testing.T) {
	a := slog.Int("n", 42)
	got := scrubAttr(a)
	if got.Value.Kind() != slog.KindInt64 || got.Value.Int64() != 42 {
		t.Errorf("non-string/group attr should pass through unchanged, got %+v", got)
	}

	b := slog.Bool("flag", true)
	got2 := scrubAttr(b)
	if got2.Value.Kind() != slog.KindBool || !got2.Value.Bool() {
		t.Errorf("bool attr should pass through unchanged, got %+v", got2)
	}
}

// TestScrubEmptyAndNoMatch covers the no-match / empty-input path of scrub.
func TestScrubEmptyAndNoMatch(t *testing.T) {
	if got := scrub(""); got != "" {
		t.Errorf("scrub(\"\") = %q, want empty string", got)
	}
	plain := "just a normal log line with no secrets"
	if got := scrub(plain); got != plain {
		t.Errorf("scrub should not alter non-matching input, got %q", got)
	}
}

// TestScrubPartialTokenNotMatched ensures a too-short candidate that merely
// looks like a token prefix is left alone (regex requires 10+ trailing chars).
func TestScrubPartialTokenNotMatched(t *testing.T) {
	short := "ghs_short"
	if got := scrub(short); got != short {
		t.Errorf("short non-matching prefix should not be redacted, got %q", got)
	}
}

// TestHandleDirectly exercises Handler.Handle directly (rather than through
// slog.Logger) to cover the record-scrubbing path explicitly, including a
// message with no attrs at all.
func TestHandleDirectly(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewHandler(inner)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "plain message, no attrs", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "plain message, no attrs") {
		t.Errorf("expected message in output: %s", buf.String())
	}
}
