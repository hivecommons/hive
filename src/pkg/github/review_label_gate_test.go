package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func labelGateReviewServer(t *testing.T, labels []string, prStatus int, reviews *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			if reviews != nil {
				*reviews++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"html_url":"https://example.test/pr/5#review-1"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			if prStatus != 0 {
				w.WriteHeader(prStatus)
				return
			}
			ls := make([]map[string]string, 0, len(labels))
			for _, l := range labels {
				ls = append(ls, map[string]string{"name": l})
			}
			b, _ := json.Marshal(map[string]any{"number": 5, "user": map[string]string{"login": "hive-bot", "type": "User"}, "labels": ls})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// review.require_labels is enforced by the relay itself: a review aimed at a
// PR carrying none of the labels is refused (.denied, audited) before anything
// reaches GitHub; a labelled PR passes; with the gate off nothing changes.
func TestReviewRelay_RequireLabelsGate(t *testing.T) {
	run := func(t *testing.T, required, prLabels []string, prStatus int) (reviews int, reqPath string, resp ReviewResponse, audits []string) {
		t.Helper()
		srv := labelGateReviewServer(t, prLabels, prStatus, &reviews)
		defer srv.Close()
		c := reviewTestClient(t, srv.URL)
		c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-bot"})
		c.SetReviewRequireLabels(func() []string { return required })
		c.SetAttributionAudit(func(action, detail, agent string) { audits = append(audits, action+":"+detail) })
		dir := withReviewDir(t)
		var err error
		reqPath, err = WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Body: "looks fine", Agent: "reviewer"})
		if err != nil {
			t.Fatal(err)
		}
		c.ProcessReviewRequestsOnce(context.Background())
		resp = readReviewResult(t, reqPath)
		return
	}

	t.Run("unlabelled PR is refused", func(t *testing.T) {
		reviews, reqPath, resp, audits := run(t, []string{"hive-review"}, []string{"bug"}, 0)
		if reviews != 0 {
			t.Fatalf("review must not reach GitHub, got %d", reviews)
		}
		if resp.OK || !strings.Contains(resp.Error, "review.require_labels") || !strings.Contains(resp.Error, "hive-review") {
			t.Fatalf("result must explain the gate: %+v", resp)
		}
		if _, err := os.Stat(reqPath + ".denied"); err != nil {
			t.Fatalf("request must be quarantined as .denied: %v", err)
		}
		found := false
		for _, a := range audits {
			if strings.HasPrefix(a, AuditActionReviewLabelGateRefused+":") {
				found = true
			}
		}
		if !found {
			t.Fatalf("refusal must be audited, got %v", audits)
		}
	})

	t.Run("labelled PR passes case-insensitively", func(t *testing.T) {
		reviews, _, resp, _ := run(t, []string{"hive-review"}, []string{"bug", "Hive-Review"}, 0)
		if reviews != 1 || !resp.OK {
			t.Fatalf("labelled PR must be reviewed: reviews=%d resp=%+v", reviews, resp)
		}
	})

	t.Run("gate off never fetches labels", func(t *testing.T) {
		reviews, _, resp, _ := run(t, nil, nil, http.StatusInternalServerError)
		if reviews != 1 || !resp.OK {
			t.Fatalf("gate off must post without reading the PR: reviews=%d resp=%+v", reviews, resp)
		}
	})

	t.Run("label fetch failure is retried, not posted", func(t *testing.T) {
		reviews, reqPath, resp, _ := run(t, []string{"hive-review"}, nil, http.StatusInternalServerError)
		if reviews != 0 || resp.OK {
			t.Fatalf("fetch failure must not post: reviews=%d resp=%+v", reviews, resp)
		}
		if _, err := os.Stat(reqPath + ".denied"); err == nil {
			t.Fatalf("transient fetch failure must not be a policy denial")
		}
	})
}
