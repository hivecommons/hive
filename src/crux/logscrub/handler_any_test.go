package logscrub

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type scrubTestStringer string

func (s scrubTestStringer) String() string { return string(s) }

type scrubTestLogValuer struct {
	value slog.Value
}

func (v scrubTestLogValuer) LogValue() slog.Value { return v.value }

func TestScrubsRenderedAttributes(t *testing.T) {
	const token = "ghs_exampletoken1234567890"
	const jwt = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpYXQiOjE2MDAwMDAwMDAsImV4cCI6MTYwMDAwMDYwMH0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	text := "push https://x-access-token:" + token + "@github.com/org/repo failed: " + jwt
	cases := []struct {
		name string
		attr slog.Attr
	}{
		{"error", slog.Any("value", errors.New(text))},
		{"stringer", slog.Any("value", scrubTestStringer(text))},
		{"logvaluer-string", slog.Any("value", scrubTestLogValuer{slog.StringValue(text)})},
		{"logvaluer-error", slog.Any("value", scrubTestLogValuer{slog.AnyValue(errors.New(text))})},
		{"logvaluer-chain", slog.Any("value", scrubTestLogValuer{slog.AnyValue(scrubTestLogValuer{slog.StringValue(text)})})},
		{"logvaluer-group", slog.Any("value", scrubTestLogValuer{slog.GroupValue(
			slog.Any("error", errors.New(text)),
			slog.Any("stringer", scrubTestStringer(text)),
			slog.Any("lazy", scrubTestLogValuer{slog.StringValue(text)}),
		)})},
		{"nested-group", slog.Group("value", slog.Group("nested", slog.Any("error", errors.New(text))))},
	}
	for _, format := range []string{"text", "json"} {
		for _, bind := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(format+"/"+tc.name+map[bool]string{false: "/record", true: "/with"}[bind], func(t *testing.T) {
					var buf bytes.Buffer
					var inner slog.Handler = slog.NewTextHandler(&buf, nil)
					if format == "json" {
						inner = slog.NewJSONHandler(&buf, nil)
					}
					logger := slog.New(NewHandler(inner)).WithGroup("request")
					if bind {
						logger.With(tc.attr).Info("failed")
					} else {
						logger.LogAttrs(context.Background(), slog.LevelInfo, "failed", tc.attr)
					}
					output := buf.String()
					if strings.Contains(output, token) || strings.Contains(output, jwt) {
						t.Fatalf("credential leaked: %s", output)
					}
					if !strings.Contains(output, "[REDACTED]") || !strings.Contains(output, "push https://x-access-token:") || !strings.Contains(output, "@github.com/org/repo failed:") {
						t.Fatalf("expected scrubbed text with context preserved: %s", output)
					}
				})
			}
		}
	}
}

func TestScrubAttrPreservesNonTextValues(t *testing.T) {
	for _, attr := range []slog.Attr{
		slog.Int("count", 42),
		slog.Bool("ok", true),
		slog.Any("nil", nil),
		slog.Any("struct", struct{ Count int }{42}),
	} {
		if got := scrubAttr(attr); !got.Equal(attr) {
			t.Errorf("scrubAttr(%v) = %v", attr, got)
		}
	}
}
