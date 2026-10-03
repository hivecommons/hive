package dashboard

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

func discardLoggerForIdentityTest() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestRefFromIssueMapIdentity pins the #4245 contract: a GitHub-numbered item
// keys as "repo#number", a string-keyed item (Linear/Jira) keys as
// "repo!externalID", and an item with neither yields an empty Key() so callers
// skip it rather than fabricating a "repo#0" identity.
func TestRefFromIssueMapIdentity(t *testing.T) {
	cases := []struct {
		name     string
		repoFull string
		issue    map[string]any
		wantKey  string
	}{
		{
			name:     "github numbered issue",
			repoFull: "acme/widgets",
			issue:    map[string]any{"number": float64(42), "url": "https://example/42"},
			wantKey:  "acme/widgets#42",
		},
		{
			name:     "github numbered issue as real int (never marshalled)",
			repoFull: "acme/widgets",
			issue:    map[string]any{"number": 7},
			wantKey:  "acme/widgets#7",
		},
		{
			name:     "external linear item",
			repoFull: "acme/widgets",
			issue:    map[string]any{"source_type": "linear", "external_id": "ENG-123"},
			wantKey:  "acme/widgets!ENG-123",
		},
		{
			name:     "no usable identity",
			repoFull: "acme/widgets",
			issue:    map[string]any{"number": float64(0)},
			wantKey:  "",
		},
		{
			name:     "no repo at all",
			repoFull: "",
			issue:    map[string]any{"external_id": "ENG-9"},
			wantKey:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := refFromIssueMap(tc.repoFull, tc.issue)
			if got := ref.Key(); got != tc.wantKey {
				t.Fatalf("refFromIssueMap(%q, %v).Key() = %q, want %q", tc.repoFull, tc.issue, got, tc.wantKey)
			}
		})
	}
}

// TestDependenciesFromIssueMap covers the well-formed, malformed, and absent
// "depends_on" shapes a status payload can round-trip through JSON with.
func TestDependenciesFromIssueMap(t *testing.T) {
	t.Run("well-formed dependencies", func(t *testing.T) {
		issue := map[string]any{
			"depends_on": []any{
				map[string]any{"key": "acme/widgets#1", "resolved": true},
				map[string]any{"key": "acme/widgets#2", "resolved": false},
			},
		}
		got := dependenciesFromIssueMap(issue)
		if len(got) != 2 {
			t.Fatalf("len(deps) = %d, want 2", len(got))
		}
		if got[0].Key != "acme/widgets#1" || !got[0].Resolved {
			t.Fatalf("deps[0] = %+v", got[0])
		}
		if got[1].Key != "acme/widgets#2" || got[1].Resolved {
			t.Fatalf("deps[1] = %+v", got[1])
		}
	})

	t.Run("non-map entries are skipped, not panicked on", func(t *testing.T) {
		issue := map[string]any{"depends_on": []any{"not-a-map", 5, map[string]any{"key": "acme/widgets#3"}}}
		got := dependenciesFromIssueMap(issue)
		if len(got) != 1 || got[0].Key != "acme/widgets#3" {
			t.Fatalf("deps = %+v, want one entry for acme/widgets#3", got)
		}
	})

	t.Run("missing depends_on yields empty, non-nil slice", func(t *testing.T) {
		got := dependenciesFromIssueMap(map[string]any{})
		if got == nil {
			t.Fatal("dependenciesFromIssueMap returned nil, want empty slice")
		}
		if len(got) != 0 {
			t.Fatalf("len(deps) = %d, want 0", len(got))
		}
	})
}

// TestIntFromAny covers every shape a JSON-round-tripped (float64) or
// never-marshalled (int/int64) number can arrive as, plus the zero-value
// fallback for anything else.
func TestIntFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64 from JSON", float64(42), 42},
		{"real int", 7, 7},
		{"int64", int64(99), 99},
		{"nil", nil, 0},
		{"string is not a number", "42", 0},
		{"bool is not a number", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intFromAny(tc.in); got != tc.want {
				t.Fatalf("intFromAny(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestStringFromAny covers the string and wrongly-typed/missing shapes
// callers in this file rely on instead of a panicking type assertion.
func TestStringFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"real string", "ENG-123", "ENG-123"},
		{"nil", nil, ""},
		{"wrong type", 42, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stringFromAny(tc.in); got != tc.want {
				t.Fatalf("stringFromAny(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestForEachActionableIssue pins the #4245 skip contract end to end: a
// well-formed github.Issue-shaped entry and a map both invoke fn with their
// canonical Ref, an un-marshalable entry is skipped (logged, not panicked),
// and a no-identity entry is skipped without ever reaching fn.
func TestForEachActionableIssue(t *testing.T) {
	t.Run("invokes fn for identifiable entries only", func(t *testing.T) {
		raw := []any{
			map[string]any{"number": float64(11)},
			map[string]any{"source_type": "linear", "external_id": "ENG-5"},
			map[string]any{"number": float64(0)}, // no usable identity: must be skipped
			func() {},                            // fails json.Marshal: must be skipped
		}
		var gotKeys []string
		forEachActionableIssue(discardLoggerForIdentityTest(), "[test]", "acme/widgets", raw, func(issue map[string]any, ref worksource.Ref) {
			gotKeys = append(gotKeys, ref.Key())
		})
		want := []string{"acme/widgets#11", "acme/widgets!ENG-5"}
		if len(gotKeys) != len(want) {
			t.Fatalf("gotKeys = %v, want %v", gotKeys, want)
		}
		for i, w := range want {
			if gotKeys[i] != w {
				t.Fatalf("gotKeys[%d] = %q, want %q", i, gotKeys[i], w)
			}
		}
	})

	t.Run("nil logger does not panic on skip paths", func(t *testing.T) {
		raw := []any{map[string]any{"number": float64(0)}, func() {}}
		called := false
		forEachActionableIssue(nil, "[test]", "acme/widgets", raw, func(issue map[string]any, ref worksource.Ref) {
			called = true
		})
		if called {
			t.Fatal("fn should not have been invoked for no-identity/unmarshalable entries")
		}
	})

	t.Run("empty input invokes fn zero times", func(t *testing.T) {
		called := 0
		forEachActionableIssue(discardLoggerForIdentityTest(), "[test]", "acme/widgets", nil, func(issue map[string]any, ref worksource.Ref) {
			called++
		})
		if called != 0 {
			t.Fatalf("called = %d, want 0", called)
		}
	})
}
