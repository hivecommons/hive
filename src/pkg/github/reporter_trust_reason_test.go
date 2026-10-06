package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestReporterTrustReasonFromComments(t *testing.T) {
	finding := ReporterTrust{Held: true, Issue: 42, Reporter: "outside-user", Repo: "acme/widget"}
	for _, tt := range []struct {
		name, body, author, want string
	}{
		{"current", reporterTrustNotice(finding), "hive[bot]", finding.NeedsHumanReason()},
		{"legacy", ReporterTrustNoticeMarker + "\nThis PR's rationale traces to acme/widget#42, filed by @outside-user (GitHub association: NONE).", "hive[bot]", finding.NeedsHumanReason()},
		{"untrusted marker", reporterTrustNotice(finding), "outside-user", ""},
		{"unmarked reason", finding.NeedsHumanReason(), "hive[bot]", ""},
		{"unknown notice format", ReporterTrustNoticeMarker, "hive[bot]", "reporter-trust hold — maintainer sign-off required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			comments := []*gh.IssueComment{nil, {Body: gh.Ptr(tt.body), User: &gh.User{Login: gh.Ptr(tt.author)}}}
			if got := reporterTrustReasonFromComments(comments, "hive[bot]"); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetchPRsReporterTrustReason(t *testing.T) {
	finding := ReporterTrust{Held: true, Issue: 42, Reporter: "outside-user", Repo: "acme/widget"}
	for _, failComments := range []bool{false, true} {
		t.Run(map[bool]string{false: "notice", true: "lookup failure"}[failComments], func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{{Number: 188, Labels: []wireLabel{{Name: "hold"}, {Name: "needs-human"}}, CreatedAt: hoursAgo(2)}}))
			mux.HandleFunc("/repos/acme/widget/issues/188/comments", func(w http.ResponseWriter, r *http.Request) {
				if failComments {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode([]*gh.IssueComment{{Body: gh.Ptr(reporterTrustNotice(finding)), User: &gh.User{Login: gh.Ptr("hive[bot]")}}})
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			c := newTestClient(t, server, "acme", []string{"widget"})
			c.appBotLogin = "hive[bot]"
			// No live trust gate: the original notice still describes the hold.
			actionable, _, held, _, _, _, _, err := c.fetchPRs(t.Context(), "widget", nil)
			if err != nil || len(actionable) != 0 || len(held) != 1 {
				t.Fatalf("enumeration changed: actionable=%v held=%v err=%v", actionable, held, err)
			}
			want := finding.NeedsHumanReason()
			if failComments {
				want = ""
			}
			if held[0].NeedsHumanReason != want {
				t.Fatalf("reason = %q, want %q", held[0].NeedsHumanReason, want)
			}
		})
	}
}
