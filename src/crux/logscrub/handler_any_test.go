package logscrub

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

type scrubTestStringer string

func (s scrubTestStringer) String() string { return string(s) }

type scrubTestTextMarshaler struct {
	text string
	err  error
}

func (v scrubTestTextMarshaler) MarshalText() ([]byte, error) {
	return []byte(v.text), v.err
}

// scrubTestTextAppender implements only encoding.TextAppender, which
// slog.TextHandler renders before any other interface.
type scrubTestTextAppender struct {
	text string
	err  error
}

func (v scrubTestTextAppender) AppendText(b []byte) ([]byte, error) {
	return append(b, v.text...), v.err
}

// scrubTestTextAppenderStringer renders different text per interface so the
// test can assert that TextAppender wins, matching slog.TextHandler on Go 1.27+.
type scrubTestTextAppenderStringer struct{ appended, stringed string }

func (v scrubTestTextAppenderStringer) AppendText(b []byte) ([]byte, error) {
	return append(b, v.appended...), nil
}

func (v scrubTestTextAppenderStringer) MarshalText() ([]byte, error) {
	return []byte(v.stringed), nil
}

func (v scrubTestTextAppenderStringer) String() string { return v.stringed }

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
		{"text-marshaler", slog.Any("value", scrubTestTextMarshaler{text: text})},
		{"text-marshaler-error", slog.Any("value", scrubTestTextMarshaler{text: token, err: errors.New(text)})},
		{"text-appender", slog.Any("value", scrubTestTextAppender{text: text})},
		{"text-appender-error", slog.Any("value", scrubTestTextAppender{text: token, err: errors.New(text)})},
		{"logvaluer-text-appender", slog.Any("value", scrubTestLogValuer{slog.AnyValue(scrubTestTextAppender{text: text})})},
		{"logvaluer-text-marshaler", slog.Any("value", scrubTestLogValuer{slog.AnyValue(scrubTestTextMarshaler{text: text})})},
		{"logvaluer-string", slog.Any("value", scrubTestLogValuer{slog.StringValue(text)})},
		{"logvaluer-error", slog.Any("value", scrubTestLogValuer{slog.AnyValue(errors.New(text))})},
		{"logvaluer-chain", slog.Any("value", scrubTestLogValuer{slog.AnyValue(scrubTestLogValuer{slog.StringValue(text)})})},
		{"logvaluer-group", slog.Any("value", scrubTestLogValuer{slog.GroupValue(
			slog.Any("error", errors.New(text)),
			slog.Any("stringer", scrubTestStringer(text)),
			slog.Any("text-marshaler", scrubTestTextMarshaler{text: text}),
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

func TestScrubAttrPrefersTextAppenderLikeSlog(t *testing.T) {
	// slog.TextHandler consults encoding.TextAppender ahead of TextMarshaler
	// from Go 1.27; earlier toolchains ignore the interface. scrubAttr fixes
	// the 1.27 order so the rendered text is stable across toolchains and
	// every text form is scrubbed either way.
	v := scrubTestTextAppenderStringer{appended: "from-append-text", stringed: "from-string"}
	got := scrubAttr(slog.Any("value", v))
	if got.Value.Kind() != slog.KindString || got.Value.String() != v.appended {
		t.Fatalf("scrubAttr rendered %v, want %q", got.Value, v.appended)
	}
}

type scrubTestCount int

// Composite payloads (structs, maps, slices) are rendered by the inner handler
// after the scrubbing boundary, so they are now rendered and scrubbed here.
func TestScrubsCompositeAnyPayloads(t *testing.T) {
	const token = "ghs_exampletoken1234567890"
	type payload struct{ Out string }
	cases := []struct {
		name string
		val  any
	}{
		{"bytes", []byte("stderr: " + token)},
		{"strings", []string{"ok", "push " + token}},
		{"map", map[string]string{"auth": token}},
		{"struct", payload{Out: token}},
		{"struct-ptr", &payload{Out: token}},
		{"text-marshaler", scrubTestTextMarshaler{text: token}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubAttr(slog.Any("value", tc.val))
			if got.Value.Kind() != slog.KindString || strings.Contains(got.Value.String(), token) || !strings.Contains(got.Value.String(), "[REDACTED]") {
				t.Fatalf("scrubAttr = %v", got.Value)
			}
			for _, inner := range []func(*bytes.Buffer) slog.Handler{
				func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
				func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			} {
				var buf bytes.Buffer
				slog.New(NewHandler(inner(&buf))).Info("x", "value", tc.val)
				if strings.Contains(buf.String(), token) {
					t.Fatalf("credential leaked: %s", buf.String())
				}
			}
		})
	}
}

func TestScrubAttrPreservesNonTextValues(t *testing.T) {
	for _, attr := range []slog.Attr{
		slog.Int("count", 42),
		slog.Bool("ok", true),
		slog.Any("nil", nil),
		slog.Any("named-int", scrubTestCount(7)),
		slog.Duration("elapsed", 3),
	} {
		if got := scrubAttr(attr); !got.Equal(attr) {
			t.Errorf("scrubAttr(%v) = %v", attr, got)
		}
	}
}

// Pointer-receiver implementations that dereference without a nil guard, as
// many real types do; slog renders a nil pointer of these as <nil> rather
// than panicking, and the scrubber must not regress that.
type scrubTestPtrTextAppender struct{ text string }

func (v *scrubTestPtrTextAppender) AppendText(b []byte) ([]byte, error) {
	return append(b, v.text...), nil
}

type scrubTestPtrTextMarshaler struct{ text string }

func (v *scrubTestPtrTextMarshaler) MarshalText() ([]byte, error) {
	return []byte(v.text), nil
}

type scrubTestPtrStringer struct{ text string }

func (v *scrubTestPtrStringer) String() string { return v.text }

type scrubTestPanickingMarshaler struct{}

func (scrubTestPanickingMarshaler) MarshalText() ([]byte, error) { panic("boom") }

func TestScrubAttrRecoversLikeSlog(t *testing.T) {
	cases := []struct {
		name string
		attr slog.Attr
		want string
	}{
		{"nil-text-appender", slog.Any("value", (*scrubTestPtrTextAppender)(nil)), "<nil>"},
		{"nil-text-marshaler", slog.Any("value", (*scrubTestPtrTextMarshaler)(nil)), "<nil>"},
		{"nil-stringer", slog.Any("value", (*scrubTestPtrStringer)(nil)), "<nil>"},
		{"panicking-marshaler", slog.Any("value", scrubTestPanickingMarshaler{}), "!PANIC: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubAttr(tc.attr)
			if got.Value.Kind() != slog.KindString || got.Value.String() != tc.want {
				t.Fatalf("scrubAttr(%v) = %v, want %q", tc.attr, got.Value, tc.want)
			}
			// The logging path (record and With) must not panic either, and
			// slog.TextHandler must agree on the rendered text.
			for _, bind := range []bool{false, true} {
				var buf bytes.Buffer
				logger := slog.New(NewHandler(slog.NewTextHandler(&buf, nil)))
				if bind {
					logger.With(tc.attr).Info("x")
				} else {
					logger.LogAttrs(context.Background(), slog.LevelInfo, "x", tc.attr)
				}
				if !strings.Contains(buf.String(), "value="+strconv.Quote(tc.want)) && !strings.Contains(buf.String(), "value="+tc.want) {
					t.Fatalf("bind=%v: rendered %q, want value %q", bind, buf.String(), tc.want)
				}
				buf.Reset()
				slog.New(slog.NewTextHandler(&buf, nil)).Info("x", tc.attr)
				if !strings.Contains(buf.String(), tc.want) {
					t.Fatalf("slog.TextHandler rendered %q, expected %q", buf.String(), tc.want)
				}
			}
		})
	}
}
