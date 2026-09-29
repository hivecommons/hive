package releasesentinel

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func manyRuns(n int) []BlockingRun {
	out := make([]BlockingRun, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, BlockingRun{
			Run: Run{ID: int64(100 + i), Name: fmt.Sprintf("wf-%d", i), Conclusion: "failure", URL: fmt.Sprintf("https://example.test/%d", i)},
			RunDetails: RunDetails{JobCount: 1, FailedJobs: []string{"build / compile"},
				Evidence: []string{"line one\nline two", "e2", "e3", "e4-not-shown"}},
		})
	}
	return out
}

func TestRenderRepairKick(t *testing.T) {
	deadline := time.Date(2026, 9, 29, 14, 0, 0, 0, time.UTC)
	msg := RenderRepairKick(RepairRequest{
		Repo: "acme/widgets", Tag: tag1, SHA: shaA, Agent: "ci-maintainer",
		Round: 2, MaxRounds: 5, Deadline: deadline, Blocking: manyRuns(maxRenderedRuns + 2),
	})
	for _, want := range []string{
		"RELEASE REPAIR", "acme/widgets", tag1, "round 2/5", shaA[:shortSHALen],
		"gh run view <run-id> --repo acme/widgets --log-failed",
		"hive-open-pr", "Do NOT move, delete or re-create the tag",
		"needs-human", "2026-09-29T14:00:00Z",
		`"wf-0" run 100: failure (https://example.test/0)`, "failed: build / compile",
		"evidence: line one line two", "... and 2 more",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "e4-not-shown") {
		t.Error("kick rendered more evidence lines than the cap")
	}
	if strings.Contains(msg, "\u2014") {
		t.Error("kick contains an em-dash")
	}
}

func TestRenderEscalation(t *testing.T) {
	title, body := RenderEscalation(Escalation{Repo: "acme/widgets", Tag: tag1, SHA: shaA, Reason: EscalationPolicy, Detail: "not permitted", Blocking: manyRuns(1)})
	if !strings.Contains(title, "cannot fix") || !strings.Contains(body, "No repair round was dispatched and nothing was pushed") || !strings.Contains(body, "not permitted") {
		t.Fatalf("policy escalation:\n%s\n%s", title, body)
	}
	title, body = RenderEscalation(Escalation{Repo: "acme/widgets", Tag: tag1, SHA: shaA, Reason: EscalationRoundCap, Round: 5, Detail: "cap"})
	if !strings.Contains(title, "after 5 repair round(s)") || strings.Contains(body, "nothing was pushed") || strings.Contains(body, "blocking runs") {
		t.Fatalf("round-cap escalation:\n%s\n%s", title, body)
	}
}
