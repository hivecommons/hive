package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestWriteIssueReopenSeries(t *testing.T) {
	rate := 0.5
	var b strings.Builder
	writeIssueReopenSeries(&b, "test", []evidence.ReopenRate{
		{Repo: "o/r", ClarityGate: true, Closed: 2, Reopened: 1, Rate: &rate},
		{Repo: "o/r", ClarityGate: false},
	})
	text := b.String()
	for _, want := range []string{
		`# TYPE hive_issue_reopen_rate_30d gauge`,
		`hive_issue_reopen_rate_30d{hive_id="test",repo="o/r",clarity_gate="true"} 0.5`,
		`hive_issue_closures_30d{hive_id="test",repo="o/r",clarity_gate="true"} 2`,
		`hive_issue_reopens_30d{hive_id="test",repo="o/r",clarity_gate="true"} 1`,
		`hive_issue_closures_30d{hive_id="test",repo="o/r",clarity_gate="false"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
	if strings.Contains(text, `hive_issue_reopen_rate_30d{hive_id="test",repo="o/r",clarity_gate="false"}`) {
		t.Fatal("empty cohort must not report zero rate")
	}
}

func TestEvidenceIssueReopenSummaryIgnoresPROpenWindow(t *testing.T) {
	reviewEvidenceTestEnv(t)
	now := time.Now().UTC()
	b := seedReviewEvidenceBundle(t, "abc1234", "", now.Add(-40*24*time.Hour), true)
	b.IssueClosures = []evidence.IssueClosure{{Repo: "acme/widget", Issue: 1, At: now.Add(-48 * time.Hour), ClarityGate: true}}
	b.IssueReopens = []evidence.IssueReopen{{Issue: 1, At: now.Add(-24 * time.Hour), Source: evidence.IssueReopenSourceClaimLedger}}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ghpkg.ReviewEvidencePath("", b.Repo, b.Number, b.HeadSHA), data, 0o644); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	(&Server{}).handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary?hours=24&repo=acme/widget&author_kind=agent", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var dto EvidenceSummary
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.N != 0 || len(dto.IssueReopenRates) != 2 || dto.IssueReopenRates[1].Closed != 1 || dto.IssueReopenRates[1].Reopened != 1 {
		t.Fatalf("summary = %+v", dto)
	}
}

func TestEvidenceIssueReopenPanel(t *testing.T) {
	data, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dto.issue_reopen_rates_30d", "evIssueReopenRatesHtml(dto)", "escapeHtml(String(r.repo", "rolling 30 days", "Missing historical clarity status is excluded"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing panel contract %q", want)
		}
	}
}
