package upstreamwatch

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMarker(t *testing.T) {
	tests := []struct {
		item Item
		want string
	}{
		{Item{Kind: KindPR, Ref: RefPR(123)}, "<!-- upstream-ref: up/widgets#123 -->"},
		{Item{Kind: KindRelease, Ref: RefRelease("v1.2.3")}, "<!-- upstream-ref: up/widgets@v1.2.3 -->"},
	}
	for _, tc := range tests {
		if got := Marker("up/widgets", tc.item); got != tc.want {
			t.Errorf("Marker(%s) = %q, want %q", tc.item.Ref, got, tc.want)
		}
	}
}

func TestRenderIssue(t *testing.T) {
	pr := Item{
		Kind: KindPR, Ref: RefPR(42), Title: "fix(api): handle nil config", Body: "Fixes a nil deref.",
		HTMLURL: "https://github.com/up/widgets/pull/42", Files: []string{"api.go", "gone.go"}, Additions: 10, Deletions: 2,
	}
	many := make([]string, maxRenderedFiles+5)
	for i := range many {
		many[i] = fmt.Sprintf("f%d.go", i)
	}
	tests := []struct {
		name     string
		item     Item
		j        Judgement
		label    string
		title    string
		contains []string
		absent   []string
		labels   []string
	}{
		{
			name:  "pr",
			item:  pr,
			j:     Judgement{Class: ClassBugfix, Difficulty: DifficultyEasy, Applicable: true, Present: []string{"api.go"}},
			label: "upstream/port",
			title: "upstream: fix(api): handle nil config",
			contains: []string{
				"up/widgets#42 (https://github.com/up/widgets/pull/42)",
				"**Class:** bugfix",
				"**Port difficulty:** easy (2 files, +10/-2 lines)",
				"### Summary\n\nFixes a nil deref.",
				"### Affected files (1)",
				"- `api.go`",
				"<!-- upstream-ref: up/widgets#42 -->",
			},
			absent: []string{"gone.go"},
			labels: []string{"upstream/port"},
		},
		{
			name:     "release",
			item:     Item{Kind: KindRelease, Ref: RefRelease("v2.0.0"), Title: "v2.0.0", HTMLURL: "https://github.com/up/widgets/releases/tag/v2.0.0"},
			j:        Judgement{Class: ClassChore, Difficulty: DifficultyModerate, Applicable: true},
			label:    "custom",
			title:    "upstream: v2.0.0",
			contains: []string{"upstream release", "**Port difficulty:** moderate\n", "<!-- upstream-ref: up/widgets@v2.0.0 -->"},
			absent:   []string{"### Affected files", "### Summary"},
			labels:   []string{"custom"},
		},
		{
			name:     "empty title falls back to ref and long file lists are bounded",
			item:     Item{Kind: KindPR, Ref: RefPR(7), Files: many},
			j:        Judgement{Class: ClassChore, Difficulty: DifficultyHard, Applicable: true, Present: many},
			title:    "upstream: upstream#7",
			contains: []string{"- ... and 5 more"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderIssue("up/widgets", tc.item, tc.j, tc.label)
			if got.Title != tc.title {
				t.Errorf("Title = %q, want %q", got.Title, tc.title)
			}
			for _, s := range tc.contains {
				if !strings.Contains(got.Body, s) {
					t.Errorf("body missing %q:\n%s", s, got.Body)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(got.Body, s) {
					t.Errorf("body unexpectedly contains %q", s)
				}
			}
			if strings.Join(got.Labels, ",") != strings.Join(tc.labels, ",") {
				t.Errorf("Labels = %v, want %v", got.Labels, tc.labels)
			}
		})
	}
}

func TestRenderIssue_TruncatesSummary(t *testing.T) {
	item := Item{Kind: KindPR, Ref: RefPR(1), Title: "x", Body: strings.Repeat("a", maxSummaryRunes+100)}
	got := RenderIssue("up/widgets", item, Judgement{}, "")
	if !strings.Contains(got.Body, "_(truncated)_") || strings.Contains(got.Body, strings.Repeat("a", maxSummaryRunes+1)) {
		t.Fatal("summary was not truncated")
	}
	if got.Labels != nil {
		t.Fatalf("Labels = %v, want none for an empty label", got.Labels)
	}
}

func TestGitHubFiler(t *testing.T) {
	marker := "<!-- upstream-ref: up/widgets#42 -->"
	tests := []struct {
		name      string
		search    string
		wantFound bool
		wantNum   int
		wantDism  bool
	}{
		{name: "no hits", search: `{"total_count":0,"items":[]}`},
		{name: "fuzzy hit without the marker is ignored",
			search: `{"items":[{"number":3,"state":"open","body":"upstream-ref: up/widgets#421"}]}`},
		{name: "open issue", wantFound: true, wantNum: 5,
			search: fmt.Sprintf(`{"items":[{"number":5,"state":"open","body":%q}]}`, "x\n"+marker)},
		{name: "closed as not planned", wantFound: true, wantNum: 6, wantDism: true,
			search: fmt.Sprintf(`{"items":[{"number":6,"state":"closed","state_reason":"not_planned","body":%q}]}`, marker)},
		{name: "dismissed label", wantFound: true, wantNum: 8, wantDism: true,
			search: fmt.Sprintf(`{"items":[{"number":8,"state":"open","labels":[{"name":"upstream/dismissed"}],"body":%q}]}`, marker)},
		{name: "closed as completed is not dismissed", wantFound: true, wantNum: 9,
			search: fmt.Sprintf(`{"items":[{"number":9,"state":"closed","state_reason":"completed","body":%q}]}`, marker)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			var query string
			mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query().Get("q")
				writeJSON(w, tc.search)
			})
			f := NewGitHubFiler(newTestClient(t, mux), "fork", "widgets")
			got, found, err := f.FindMarker(t.Context(), marker)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(query, "repo:fork/widgets") || !strings.Contains(query, `"upstream-ref: up/widgets#42"`) {
				t.Errorf("query = %q", query)
			}
			if found != tc.wantFound || got.Number != tc.wantNum || got.Dismissed != tc.wantDism {
				t.Errorf("got %+v found=%v", got, found)
			}
		})
	}
}

func TestGitHubFiler_File(t *testing.T) {
	mux := http.NewServeMux()
	var body string
	mux.HandleFunc("/repos/fork/widgets/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		writeJSON(w, `{"number":77}`)
	})
	f := NewGitHubFiler(newTestClient(t, mux), "fork", "widgets")
	n, err := f.File(t.Context(), Issue{Title: "upstream: x", Body: "b", Labels: []string{"upstream/port"}})
	if err != nil || n != 77 {
		t.Fatalf("File = %d, %v", n, err)
	}
	if !strings.Contains(body, `"labels":["upstream/port"]`) || !strings.Contains(body, `"title":"upstream: x"`) {
		t.Fatalf("request body = %s", body)
	}

	mux.HandleFunc("/repos/fork/broken/issues", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := NewGitHubFiler(newTestClient(t, mux), "fork", "broken").File(t.Context(), Issue{Title: "t"}); err == nil {
		t.Fatal("File succeeded against a failing API")
	}
}
