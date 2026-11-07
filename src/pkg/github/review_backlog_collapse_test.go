package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

func TestReviewFindingBelowMinSeverity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		severity    string
		minSeverity string
		want        bool
	}{
		{name: "low below default", severity: "low", want: true},
		{name: "info below default", severity: "info", want: true},
		{name: "medium at default", severity: "medium"},
		{name: "high above default", severity: "high"},
		{name: "P3 files everything", severity: "info", minSeverity: "P3"},
		{name: "P1 holds medium", severity: "medium", minSeverity: "p1", want: true},
		{name: "P0 holds high", severity: "high", minSeverity: "P0", want: true},
		{name: "P0 keeps critical", severity: "critical", minSeverity: "P0"},
		{name: "invalid uses default", severity: "low", minSeverity: "P9", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := outputschema.Finding{Severity: outputschema.Severity(tc.severity)}
			if got := ReviewFindingBelowMinSeverity(f, tc.minSeverity); got != tc.want {
				t.Fatalf("below = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGroupReviewBacklogPending(t *testing.T) {
	p := func(persp review.Perspective, title string, oos bool) reviewBacklogPending {
		return reviewBacklogPending{perspective: persp, finding: outputschema.Finding{Title: title}, outOfScope: oos}
	}
	for _, tc := range []struct {
		name    string
		pending []reviewBacklogPending
		want    [][]string
	}{
		{name: "empty"},
		{name: "one", pending: []reviewBacklogPending{p("docs-currency", "a", false)}, want: [][]string{{"a"}}},
		{
			name: "same perspective collapses in order",
			pending: []reviewBacklogPending{
				p("docs-currency", "a", false), p("correctness", "b", false), p("docs-currency", "c", false),
			},
			want: [][]string{{"a", "c"}, {"b"}},
		},
		{
			name: "scope splits a perspective",
			pending: []reviewBacklogPending{
				p("style", "in", false), p("style", "out", true), p("style", "in2", false),
			},
			want: [][]string{{"in", "in2"}, {"out"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groups := groupReviewBacklogPending(tc.pending)
			var got [][]string
			for _, g := range groups {
				var titles []string
				for _, f := range g {
					titles = append(titles, f.finding.Title)
				}
				got = append(got, titles)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenderReviewFollowUpsComment(t *testing.T) {
	held := func(persp review.Perspective, title, sev, file string, line int, summary string) reviewBacklogHeldFinding {
		return reviewBacklogHeldFinding{Perspective: persp, Finding: outputschema.Finding{Title: title, Severity: outputschema.Severity(sev), File: file, Line: line, Summary: summary}}
	}
	docs := held("docs-currency", "stale example", "low", "docs/a.md", 3, "shows the old flag")
	docsLine := "- [ ] **P3 low** docs-currency — stale example (`docs/a.md:3`): shows the old flag"
	for _, tc := range []struct {
		name     string
		previous string
		held     []reviewBacklogHeldFinding
		want     []string
		wantNot  []string
		empty    bool
	}{
		{name: "nothing", empty: true},
		{
			name: "fresh comment",
			held: []reviewBacklogHeldFinding{docs},
			want: []string{reviewFollowUpsCommentMarker, "min_severity: P2", docsLine},
		},
		{
			name:     "re-report is not listed twice",
			previous: reviewFollowUpsCommentMarker + "\n### x\n\n" + docsLine + "\n",
			held:     []reviewBacklogHeldFinding{docs},
			want:     []string{docsLine},
		},
		{
			name:     "ticked entry stays ticked and new one is appended",
			previous: reviewFollowUpsCommentMarker + "\n\n- [x] **P3 low** docs-currency — stale example (`docs/a.md:3`): shows the old flag\n",
			held:     []reviewBacklogHeldFinding{docs, held("style", "naming", "info", "pkg/c.go", 4, "prefer camelCase")},
			want:     []string{"- [x] **P3 low** docs-currency — stale example (`docs/a.md:3`): shows the old flag", "- [ ] **P3 info** style — naming (`pkg/c.go:4`): prefer camelCase"},
			wantNot:  []string{docsLine},
		},
		{
			name:    "long summary is collapsed to one line and truncated",
			held:    []reviewBacklogHeldFinding{held("style", "long", "low", "pkg/l.go", 1, strings.Repeat("word\n", 100))},
			want:    []string{"…", "(`pkg/l.go:1`): word word"},
			wantNot: []string{"word\nword"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderReviewFollowUpsComment(tc.previous, tc.held, "P2")
			if tc.empty {
				if got != "" {
					t.Fatalf("want empty body, got %q", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("body missing %q:\n%s", w, got)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("body must not contain %q:\n%s", w, got)
				}
			}
			if n := strings.Count(got, docsLine[:len("- [ ] **P3 low** docs-currency — stale example")]); n > 1 {
				t.Errorf("entry listed %d times:\n%s", n, got)
			}
		})
	}
}

// followUpsCommentServer serves the PR's issue comments so the Follow-ups
// marker comment can be created once and then edited in place.
type followUpsCommentServer struct {
	bodies  []string
	patches int
}

func (s *followUpsCommentServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/5/comments":
			var out []map[string]any
			for i, b := range s.bodies {
				out = append(out, map[string]any{"id": i + 1, "body": b, "user": map[string]any{"login": "hive[bot]"}})
			}
			if out == nil {
				out = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/5/comments":
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			s.bodies = append(s.bodies, payload.Body)
			fmt.Fprintf(w, `{"id":%d}`, len(s.bodies))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/comments/"):
			var id int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/comments/%d", &id)
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if id < 1 || id > len(s.bodies) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			s.bodies[id-1] = payload.Body
			s.patches++
			fmt.Fprintf(w, `{"id":%d}`, id)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		}
	}
}

// Findings below review.backlog.min_severity are never items: they land in
// one Follow-ups comment per PR, edited in place on later passes, while
// findings at or above the line still file (hivecommons/hive#17065).
func TestReviewBacklogHoldsFindingsBelowMinSeverityInOneComment(t *testing.T) {
	for _, tc := range []struct {
		name        string
		minSeverity string
		wantItems   int
		wantHeld    bool
	}{
		{name: "default P2 holds the P3 nits", wantItems: 1, wantHeld: true},
		// All three share the correctness perspective, so filing them
		// yields one collapsed item, not three.
		{name: "P3 files everything", minSeverity: "P3", wantItems: 1},
		{name: "P1 holds the medium too", minSeverity: "P1", wantItems: 0, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withReviewBacklogState(t)
			server := &followUpsCommentServer{}
			srv := httptest.NewServer(server.handler(t))
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
			c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
				return ReviewBacklogRouting{
					Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
					Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, MinSeverity: tc.minSeverity},
					Sink:     sink,
				}
			})
			ctx := context.Background()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			report := routingReport(t, 5, "abc",
				nit("unused variable", "medium", "pkg/a.go", 7, "x is assigned and never read"),
				nit("stale example", "low", "docs/a.md", 3, "shows the old flag"),
				nit("naming", "info", "pkg/c.go", 4, "prefer camelCase"),
			)
			req := ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}
			if err := c.fileOutOfScopeReviewBacklog(ctx, req, now); err != nil {
				t.Fatal(err)
			}
			if len(sink.created) != tc.wantItems {
				t.Fatalf("created = %d, want %d: %+v", len(sink.created), tc.wantItems, sink.created)
			}
			if tc.minSeverity == "P3" {
				if got := sink.created[0].Title; !strings.HasSuffix(got, "o/r#5 — 3 follow-ups") {
					t.Fatalf("P3 must file every finding, collapsed: title = %q", got)
				}
			}
			var followUps []string
			for _, b := range server.bodies {
				if strings.Contains(b, reviewFollowUpsCommentMarker) {
					followUps = append(followUps, b)
				}
			}
			if !tc.wantHeld {
				if len(followUps) != 0 {
					t.Fatalf("follow-ups comment posted with nothing held: %v", followUps)
				}
				return
			}
			if len(followUps) != 1 {
				t.Fatalf("follow-ups comments = %d, want 1", len(followUps))
			}
			for _, want := range []string{"stale example (`docs/a.md:3`)", "naming (`pkg/c.go:4`)"} {
				if !strings.Contains(followUps[0], want) {
					t.Errorf("follow-ups comment missing %q:\n%s", want, followUps[0])
				}
			}
			if tc.minSeverity == "P1" && !strings.Contains(followUps[0], "unused variable") {
				t.Errorf("P1 must hold the medium finding too:\n%s", followUps[0])
			}

			// Next head: same nits plus one more. The comment is edited,
			// never posted again, and lists the old and the new entries.
			again := routingReport(t, 5, "abd",
				nit("stale example", "low", "docs/a.md", 3, "shows the old flag"),
				nit("naming", "info", "pkg/c.go", 4, "prefer camelCase"),
				nit("typo", "low", "pkg/b.go", 9, "receive"),
			)
			if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: again}, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			// The PR's once-per-PR backlog summary is also a POST; the
			// Follow-ups comment itself is created once and then edited.
			var followUpPosts int
			for _, b := range server.bodies {
				if strings.Contains(b, reviewFollowUpsCommentMarker) {
					followUpPosts++
				}
			}
			if followUpPosts != 1 || server.patches != 1 {
				t.Fatalf("follow-ups comments=%d patches=%d, want one create then one edit", followUpPosts, server.patches)
			}
			body := followUps[0]
			for _, b := range server.bodies {
				if strings.Contains(b, reviewFollowUpsCommentMarker) {
					body = b
				}
			}
			for _, want := range []string{"stale example", "naming", "typo (`pkg/b.go:9`)"} {
				if !strings.Contains(body, want) {
					t.Errorf("edited follow-ups comment missing %q:\n%s", want, body)
				}
			}
			if strings.Count(body, "stale example") != 1 {
				t.Errorf("re-reported nit listed twice:\n%s", body)
			}
			if len(sink.created) != tc.wantItems {
				t.Fatalf("held findings were filed on the second pass: %+v", sink.created)
			}
		})
	}
}
