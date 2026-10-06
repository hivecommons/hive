package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestLegacyReporterTrustNoticeRetainsReporterIdentity(t *testing.T) {
	finding, ok := reporterTrustFinding(ReporterTrustNoticeMarker + "\nThis PR's rationale traces to o/r#581, filed by @stranger (GitHub association: NONE).")
	if !ok || finding.Reporter != "stranger" || finding.Issue != 581 || finding.OwnsNeedsHuman {
		t.Fatalf("legacy finding=%+v ok=%v", finding, ok)
	}
}

func TestReporterTrustSignalReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		held, owns, other, newer bool
		wantRemove               bool
		laterComment             string
	}{
		{name: "held stays visible", held: true, owns: true},
		{name: "human release clears owned signal", owns: true, wantRemove: true},
		{name: "maintainer prose does not block release", owns: true, wantRemove: true, laterComment: "removed hold, needs-human can go too"},
		{name: "bot prose does not block release", owns: true, wantRemove: true, laterComment: "Summary: needs-human was set for reporter trust"},
		{name: "prose does not bypass ledger", owns: true, other: true, laterComment: "needs-human can go too"},
		{name: "prose does not bypass newer label", owns: true, newer: true, laterComment: "needs-human can go too"},
		{name: "preexisting signal retained"},
		{name: "CI escalation retained", owns: true, other: true},
		{name: "newer human signal retained", owns: true, newer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removed := false
			reason := "unset"
			finding := ReporterTrust{Held: true, Issue: 581, Repo: "o/r", Reporter: "stranger", OwnsNeedsHuman: tc.owns}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/comments"):
					comments := []map[string]any{{"body": reporterTrustNotice(finding), "user": userJSON("kubestellar-hive[bot]", "Bot")}}
					if tc.laterComment != "" {
						comments = append(comments, map[string]any{"body": tc.laterComment, "user": userJSON("maintainer", "User")})
						comments = append(comments, map[string]any{"body": tc.laterComment, "user": userJSON("kubestellar-hive[bot]", "Bot")})
					}
					_ = json.NewEncoder(w).Encode(comments)
				case strings.HasSuffix(r.URL.Path, "/events"):
					events := []map[string]any{
						{"event": "unlabeled", "label": map[string]string{"name": "hold"}, "actor": userJSON("maintainer", "User"), "created_at": "2026-10-06T10:00:00Z"},
						{"event": "labeled", "label": map[string]string{"name": "needs-human"}, "actor": userJSON("kubestellar-hive[bot]", "Bot"), "created_at": "2026-10-06T09:00:00Z"},
					}
					if tc.newer {
						events = append(events, map[string]any{"event": "labeled", "label": map[string]string{"name": "needs-human"}, "actor": userJSON("maintainer", "User"), "created_at": "2026-10-06T11:00:00Z"})
					}
					_ = json.NewEncoder(w).Encode(events)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/needs-human"):
					removed = true
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := testClient(t, srv.URL)
			c.SetAppBotLogin("kubestellar-hive[bot]")
			c.SetReporterTrustEscalation(func(repo string, number int, value string) bool { reason = value; return tc.other })
			labels := []string{"needs-human"}
			if tc.held {
				labels = append(labels, "hold")
			}
			pr := &gh.PullRequest{Number: gh.Ptr(583)}
			got, visible := c.reconcileReporterTrustSignal(context.Background(), "o/r", pr, labels)
			if removed != tc.wantRemove || hasExactLabel(got, "needs-human") == tc.wantRemove {
				t.Fatalf("removed=%v labels=%v", removed, got)
			}
			if tc.held {
				if !strings.Contains(reason, "issue #581 filed by @stranger") || visible != reason {
					t.Fatalf("reason=%q visible=%q", reason, visible)
				}
			} else if reason != "" || visible != "" {
				t.Fatalf("released reason=%q visible=%q", reason, visible)
			}
		})
	}
}
