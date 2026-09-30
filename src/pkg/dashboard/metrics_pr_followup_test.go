package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func servePRFollowUpMetrics(t *testing.T) string {
	t.Helper()
	s := covApiServer(t)
	s.deps.Config.HiveID = "test-hive"
	t.Setenv("HIVE_METRICS_TOKEN", "sk-metrics")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+"sk-metrics")
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestHandleMetricsPRFollowUpCounters(t *testing.T) {
	SetPRFollowUpCountersProvider(func() (PRFollowUpCounters, bool) {
		return PRFollowUpCounters{
			Resumed:           7,
			Fallback:          map[string]int{"session gone": 2, "expired": 1},
			Skipped:           map[string]int{"draft": 3},
			Deferred:          4,
			HandoffsQueued:    5,
			HandoffsDelivered: 6,
			Pruned:            map[string]int{"merged": 8},
		}, true
	})
	t.Cleanup(func() { SetPRFollowUpCountersProvider(nil) })

	body := servePRFollowUpMetrics(t)
	for _, want := range []string{
		"# TYPE hive_pr_followup_resumed_total counter",
		`hive_pr_followup_resumed_total{hive_id="test-hive"} 7`,
		`hive_pr_followup_fallback_total{hive_id="test-hive",reason="expired"} 1` + "\n" +
			`hive_pr_followup_fallback_total{hive_id="test-hive",reason="session gone"} 2`,
		`hive_pr_followup_skipped_total{hive_id="test-hive",reason="draft"} 3`,
		`hive_pr_followup_deferred_total{hive_id="test-hive"} 4`,
		`hive_pr_followup_handoffs_total{hive_id="test-hive",state="queued"} 5`,
		`hive_pr_followup_handoffs_total{hive_id="test-hive",state="delivered"} 6`,
		`hive_pr_followup_pointers_pruned_total{hive_id="test-hive",reason="merged"} 8`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\n---\n%s", want, body)
		}
	}
}

func TestHandleMetricsPRFollowUpCountersAbsent(t *testing.T) {
	for name, fn := range map[string]func() (PRFollowUpCounters, bool){
		"no provider": nil,
		"not ok":      func() (PRFollowUpCounters, bool) { return PRFollowUpCounters{Resumed: 1}, false },
	} {
		t.Run(name, func(t *testing.T) {
			SetPRFollowUpCountersProvider(fn)
			t.Cleanup(func() { SetPRFollowUpCountersProvider(nil) })
			if body := servePRFollowUpMetrics(t); strings.Contains(body, "hive_pr_followup_") {
				t.Errorf("/metrics has pr_followup series without counters:\n%s", body)
			}
		})
	}
}
