package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// projectbluefin/gutenprint-printer-app#170: Renovate writes `@&#8203;user`
// and `[#&#8203;628]` so GitHub links nothing, and the closing parser read
// "fixed and prefixed label handlers by [@&#8203;" as a claim on #8203. The
// review then filed a backlog issue about the artifact that said itself "No
// action needed on the PR".
func TestClaimParsersSkipHTMLNumericEntities(t *testing.T) {
	renovate := "- feat(plugins): remove, hold, lgtm, fixed and prefixed label handlers by [@&#8203;hivecommons-hive](https://redirect.github.com/hivecommons-hive) in [#&#8203;628](https://redirect.github.com/cncf/prow-github-actions/pull/628)\n" +
		"- fix the documented values by [@&#x200b;jeefy](https://redirect.github.com/jeefy)\n"
	if got := ParseClaimedIssues(renovate, "o/r"); len(got) != 0 {
		t.Fatalf("claimed = %v, want none from entities", got)
	}
	if _, ok := FindClosingKeywordReference(renovate, "o/r", "o/r", 8203); ok {
		t.Fatal("FindClosingKeywordReference matched #8203 inside &#8203;")
	}
	if got := ParseReferencedIssues("Refs &#8203;12 and see &#8203;", "o/r"); len(got) != 0 {
		t.Fatalf("referenced = %v, want none from entities", got)
	}
	// Real references next to entities still count.
	got := ParseClaimedIssues(renovate+"\nFixes #12", "o/r")
	if len(got) != 1 || got[0].Issue != 12 {
		t.Fatalf("claimed = %v, want only #12", got)
	}
	if refs := ParseReferencedIssues("Refs #7 (by [@&#8203;jeefy])", "o/r"); len(refs) != 1 || refs[0].Issue != 7 {
		t.Fatalf("referenced = %v, want only #7", refs)
	}
}

func TestFindingNeedsNoAction(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    outputschema.Finding
		want bool
	}{
		{name: "gutenprint#170", f: outputschema.Finding{Severity: outputschema.SeverityInfo, Title: "Linked issue #8203 is a parsing artifact, not a real issue", Summary: "The diff does exactly that. No action needed on the PR."}, want: true},
		{name: "no action required", f: outputschema.Finding{Severity: outputschema.SeverityInfo, Summary: "No further action is required."}, want: true},
		{name: "low severity says no action: still filed", f: outputschema.Finding{Severity: outputschema.SeverityLow, Summary: "No action needed on the PR, but the base is wrong."}, want: false},
		{name: "info with work in it", f: outputschema.Finding{Severity: outputschema.SeverityInfo, Summary: "The retry loop gives up on the first 502; retry it."}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingNeedsNoAction(tc.f); got != tc.want {
				t.Fatalf("findingNeedsNoAction = %v, want %v", got, tc.want)
			}
		})
	}
}

// The backlog filer skips an info finding that says no action is needed and
// still files the others in the same report.
func TestReviewBacklogSkipsNoActionFinding(t *testing.T) {
	withReviewBacklogState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/issues/5/comments":
			fmt.Fprint(w, `{"id":1}`)
		case "/repos/o/r/pulls/5":
			fmt.Fprint(w, `{"base":{"ref":"v6"}}`)
		case "/repos/o/r/contents/pkg/a.go":
			fmt.Fprint(w, `{"type":"file","path":"pkg/a.go"}`)
		case "/repos/o/r/pulls/5/files":
			fmt.Fprint(w, `[{"filename":"other.go"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
	c := headsBacklogClient(t, srv.URL, sink)
	report := routingReport(t, 5, "abc",
		nit("Linked issue #8203 is a parsing artifact", "info", "pkg/a.go", 7, "The diff does what the title says. No action needed on the PR."),
		nit("retry gives up early", "info", "pkg/a.go", 9, "The retry loop gives up on the first 502; retry it."))
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 1 || !strings.Contains(sink.created[0].Title, "retry gives up early") {
		titles := []string{}
		for _, it := range sink.created {
			titles = append(titles, it.Title)
		}
		t.Fatalf("created = %v, want only the finding with work in it", titles)
	}
}
