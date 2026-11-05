package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hivecommons/hive#16699: a decision park linked "blocked by" another
// decision park ("Answer on #N") takes #N's answer when the owner decides
// #N, instead of coming back as a second ask once #N is unparked or closed.
func TestSweepIssueUnparkCommandsDeferringParkInheritsDecision(t *testing.T) {
	type blocker struct {
		repo   string
		number int
		state  string
	}
	for _, tc := range []struct {
		name     string
		labels   []string
		blockers []blocker
		inherit  bool
	}{
		{"only blocker is the decided park", []string{NeedsDecisionStageLabel}, []blocker{{"org/repo", 511, "open"}}, true},
		{"closed blockers do not count", []string{NeedsDecisionStageLabel}, []blocker{{"org/repo", 511, "open"}, {"other/repo", 7, "closed"}}, true},
		{"another open blocker", []string{NeedsDecisionStageLabel}, []blocker{{"org/repo", 511, "open"}, {"org/repo", 600, "open"}}, false},
		{"blocker in another repo", []string{NeedsDecisionStageLabel}, []blocker{{"other/repo", 511, "open"}}, false},
		{"blocker not decided", []string{NeedsDecisionStageLabel}, []blocker{{"org/repo", 600, "open"}}, false},
		{"also parked on a person", []string{"needs-human", NeedsDecisionStageLabel}, []blocker{{"org/repo", 511, "open"}}, false},
		{"no blockers", []string{NeedsDecisionStageLabel}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decided := parkedIssue(511, humanComment(41, "owner", "/hive decision keep the rc tag"))
			decided.Labels = []wireLabel{{Name: NeedsDecisionStageLabel}}
			deferring := unparkWireIssue{
				Number: 512,
				Title:  "v2.0.0-rc.1 tag",
				Body:   "Answer on #511; the same choice applies here.",
				User:   wireUser{Login: "hive[bot]"},
			}
			for _, l := range tc.labels {
				deferring.Labels = append(deferring.Labels, wireLabel{Name: l})
			}
			inner, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{decided, deferring}, map[string]string{"owner": "admin"})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/org/repo/issues/512/dependencies/blocked_by" {
					inner.Config.Handler.ServeHTTP(w, r)
					return
				}
				payload := []map[string]any{}
				for _, b := range tc.blockers {
					payload = append(payload, map[string]any{
						"number":         b.number,
						"state":          b.state,
						"repository_url": "https://api.github.com/repos/" + b.repo,
					})
				}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			t.Cleanup(server.Close)
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			var inherited *IssueUnparkEvent
			for i, event := range result.Unparked {
				if event.Number == 512 {
					inherited = &result.Unparked[i]
				}
			}
			var note string
			for _, body := range rec.posted[512] {
				if strings.Contains(body, decisionInheritedMarker) {
					note = body
				}
			}
			liftedPark := strings.Contains(strings.Join(rec.removed[512], ","), NeedsDecisionStageLabel)
			if !tc.inherit {
				if inherited != nil || note != "" || liftedPark {
					t.Fatalf("#512 must stay its own ask: event=%+v note=%q removed=%v", inherited, note, rec.removed[512])
				}
				return
			}
			if inherited == nil || inherited.Actor != "owner" || inherited.Decision != "keep the rc tag" {
				t.Fatalf("#512 did not take #511's decision: %+v", result.Unparked)
			}
			if !liftedPark {
				t.Fatalf("#512 kept its decision park: removed=%v", rec.removed[512])
			}
			if !strings.Contains(note, "Answered on #511 by @owner. Decision: keep the rc tag") {
				t.Fatalf("note must say where it was answered, got %q", note)
			}
			for _, body := range rec.posted[512] {
				if strings.Contains(body, fmt.Sprintf("%s41 -->", unparkReplyMarkerPrefix)) {
					t.Fatalf("#511's command must not be answered on #512: %q", body)
				}
			}
		})
	}
}
