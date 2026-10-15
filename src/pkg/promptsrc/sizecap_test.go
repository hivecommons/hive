package promptsrc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recLogger captures Warn/Info calls so a test can assert the rejection was
// logged with the source and size (hivecommons/hive#12130).
type recLogger struct {
	warns []string
	infos []string
}

func (l *recLogger) Warn(msg string, args ...any) { l.warns = append(l.warns, render(msg, args)) }
func (l *recLogger) Info(msg string, args ...any) { l.infos = append(l.infos, render(msg, args)) }

func render(msg string, args []any) string {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		b.WriteString(" ")
		b.WriteString(args[i].(string))
		b.WriteString("=")
		switch v := args[i+1].(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteString(itoa(v))
		default:
			b.WriteString("?")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// shippedTemplateDirs are the two trees of kick templates the hive ships, in
// which every file must fit under the cap with margin. Relative to this
// package's directory.
var shippedTemplateDirs = []string{
	filepath.Join("..", "..", "policies"),
	filepath.Join("..", "policies", "defaults"),
}

// The cap exists to refuse a mis-pointed source, not a real template. Every
// shipped template must fit with at least 2x headroom, and the test says which
// one got closest so a growing policy is noticed before it starts being
// rejected in the field.
func TestShippedTemplatesFitUnderCap(t *testing.T) {
	const headroom = 2
	largest, largestName, seen := 0, "", 0
	for _, dir := range shippedTemplateDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			seen++
			if int(info.Size()) > largest {
				largest, largestName = int(info.Size()), filepath.Join(dir, e.Name())
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no shipped templates to measure")
	}
	if largest*headroom > DefaultMaxPromptBytes {
		t.Fatalf("largest shipped template %s is %d bytes; DefaultMaxPromptBytes=%d leaves less than %dx headroom — raise the cap or shrink the template",
			largestName, largest, DefaultMaxPromptBytes, headroom)
	}
	t.Logf("%d shipped templates; largest %s = %d bytes under cap %d (%.1fx headroom)", seen, largestName, largest, DefaultMaxPromptBytes, float64(DefaultMaxPromptBytes)/float64(largest))
}

// An oversize body is rejected whole — never truncated — and the resolver
// falls back to the cached copy that did fit. The log line names the source
// and the size.
func TestResolve_OversizeRejectedFallsBackToCache(t *testing.T) {
	good := "# policy that fits\n"
	f := &stubFetcher{body: good}
	lg := &recLogger{}
	r := NewResolver(f, allowAll, lg)
	if res := r.Resolve(context.Background(), fullSource()); !res.Ok || res.Body != good {
		t.Fatalf("priming fetch failed: %+v", res)
	}

	f.body = strings.Repeat("a", r.MaxBytes()+1)
	res := r.Resolve(context.Background(), fullSource())
	if !res.Ok || res.Body != good {
		t.Fatalf("want cached good body on oversize, got %+v", res)
	}
	if !strings.HasPrefix(res.Source, "github-cache:") {
		t.Errorf("provenance = %q, want github-cache", res.Source)
	}
	if len(lg.warns) != 1 {
		t.Fatalf("want exactly one warning, got %v", lg.warns)
	}
	for _, want := range []string{"REJECTED", "hivecommons/hive", "prompts/scanner.md", "size_bytes=" + itoa(r.MaxBytes()+1), "cap_bytes=" + itoa(r.MaxBytes()), MaxPromptBytesEnv} {
		if !strings.Contains(lg.warns[0], want) {
			t.Errorf("warning lacks %q: %s", want, lg.warns[0])
		}
	}
	if strings.Contains(strings.ToLower(lg.warns[0]), "truncat") && !strings.Contains(lg.warns[0], "not truncated") {
		t.Errorf("warning still speaks of truncation: %s", lg.warns[0])
	}
}

// With nothing cached, an oversize body yields a miss so the caller takes the
// next resolution step (inline template). No partial body ever escapes.
func TestResolve_OversizeNoCacheIsMiss(t *testing.T) {
	f := &stubFetcher{body: strings.Repeat("a", DefaultMaxPromptBytes+100)}
	r := NewResolver(f, allowAll, &recLogger{})
	res := r.Resolve(context.Background(), fullSource())
	if res.Ok || res.Body != "" {
		t.Fatalf("oversize with no cache must be a miss, got Ok=%v len=%d", res.Ok, len(res.Body))
	}
	if res.Source != "oversize" {
		t.Errorf("provenance = %q, want oversize", res.Source)
	}
	// And the rejected body was not stored: a later failure must not serve it.
	f.err = errors.New("boom")
	if res := r.Resolve(context.Background(), fullSource()); res.Ok {
		t.Fatalf("rejected body leaked into cache: %+v", res)
	}
}

// A body exactly at the cap is fine; the bound is "over", not "at".
func TestResolve_BodyAtCapIsAccepted(t *testing.T) {
	r := NewResolver(&stubFetcher{body: strings.Repeat("x", DefaultMaxPromptBytes)}, allowAll, nil)
	if res := r.Resolve(context.Background(), fullSource()); !res.Ok || len(res.Body) != DefaultMaxPromptBytes {
		t.Fatalf("body at cap should resolve whole, got Ok=%v len=%d", res.Ok, len(res.Body))
	}
}

// FetchOnce surfaces the rejection as an error the dashboard can show, naming
// the size and the cap, instead of baking a cut-off policy to disk.
func TestFetchOnce_OversizeIsError(t *testing.T) {
	f := &stubFetcher{body: strings.Repeat("b", DefaultMaxPromptBytes+50)}
	body, err := FetchOnce(context.Background(), f, allowAll, fullSource())
	if err == nil {
		t.Fatalf("want error, got %d-byte body", len(body))
	}
	if body != "" {
		t.Errorf("partial body returned alongside error (%d bytes)", len(body))
	}
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Errorf("err = %v, want ErrPromptTooLarge", err)
	}
	for _, want := range []string{"hivecommons/hive:prompts/scanner.md", itoa(DefaultMaxPromptBytes + 50), itoa(DefaultMaxPromptBytes), MaxPromptBytesEnv, "not truncated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// The env override raises or lowers the cap for new resolvers and FetchOnce;
// garbage leaves the default in force; SetMaxBytes ignores non-positive values.
func TestMaxPromptBytesOverride(t *testing.T) {
	cases := map[string]int{
		"":        DefaultMaxPromptBytes,
		"garbage": DefaultMaxPromptBytes,
		"0":       DefaultMaxPromptBytes,
		"-5":      DefaultMaxPromptBytes,
		"4096":    4096,
		"128KiB":  128 * 1024,
		"128kb":   128 * 1024,
		"1MiB":    1024 * 1024,
		" 2 MB ":  2 * 1024 * 1024,
	}
	for in, want := range cases {
		t.Setenv(MaxPromptBytesEnv, in)
		if got := MaxPromptBytes(); got != want {
			t.Errorf("%s=%q: MaxPromptBytes()=%d, want %d", MaxPromptBytesEnv, in, got, want)
		}
	}

	t.Setenv(MaxPromptBytesEnv, "4096")
	r := NewResolver(&stubFetcher{body: strings.Repeat("c", 5000)}, allowAll, nil)
	if r.MaxBytes() != 4096 {
		t.Fatalf("resolver cap = %d, want env override 4096", r.MaxBytes())
	}
	if res := r.Resolve(context.Background(), fullSource()); res.Ok {
		t.Fatal("5000-byte body accepted under a 4096 cap")
	}
	if _, err := FetchOnce(context.Background(), &stubFetcher{body: strings.Repeat("c", 5000)}, allowAll, fullSource()); !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("FetchOnce ignored env cap: %v", err)
	}
	r.SetMaxBytes(0)
	r.SetMaxBytes(-1)
	if r.MaxBytes() != 4096 {
		t.Errorf("SetMaxBytes accepted a non-positive cap: %d", r.MaxBytes())
	}
	r.SetMaxBytes(8192)
	if res := r.Resolve(context.Background(), fullSource()); !res.Ok {
		t.Fatalf("5000-byte body rejected under an 8192 cap: %+v", res)
	}
}
