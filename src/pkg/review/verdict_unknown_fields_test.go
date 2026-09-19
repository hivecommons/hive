package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// The reviewer writes its verdict with a model, and the model routinely decorates
// the schema with extra keys. Production rejected 10 of 17 delivered verdicts with
// `decode review report: json: unknown field "detail"` — the review had been done,
// the comment had been posted, and the routing input was thrown away over a key
// nothing reads.
func TestValidateReportIgnoresUnknownFields(t *testing.T) {
	base := map[string]any{
		"lane":        "review-swarm",
		"kind":        "review",
		"findings":    []any{},
		"prs_opened":  []any{},
		"beads_filed": []any{},
		"summary":     "no blocking findings",
		"perspective": "correctness",
		"verdict":     "approve",
		"repo":        "projectbluefin/utah",
		"number":      117,
		"head_sha":    "f8fda0b909f7d3692bd2bbfc4bf713b14eb1c9af",
	}

	withKey := func(t *testing.T, mutate func(m map[string]any)) []byte {
		t.Helper()
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		mutate(m)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"top-level detail", func(m map[string]any) { m["detail"] = "reviewed the diff and the callers" }},
		{"top-level message", func(m map[string]any) { m["message"] = "LGTM" }},
		{"nested finding detail", func(m map[string]any) {
			m["findings"] = []any{map[string]any{
				"title":    "unchecked error",
				"severity": "low",
				"summary":  "the returned error is discarded",
				"detail":   "the model likes adding this",
			}}
		}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			report, err := ValidateReport(withKey(t, tt.mutate))
			if err != nil {
				t.Fatalf("ValidateReport rejected a usable verdict: %v", err)
			}
			if report.Verdict != VerdictApprove {
				t.Errorf("verdict = %q, want %q", report.Verdict, VerdictApprove)
			}
			if report.Repo != "projectbluefin/utah" || report.Number != 117 {
				t.Errorf("target = %s#%d, want projectbluefin/utah#117", report.Repo, report.Number)
			}
			if report.Kind != outputschema.KindReview {
				t.Errorf("kind = %q, want %q", report.Kind, outputschema.KindReview)
			}
		})
	}

	// Tolerating unknown keys must not let a bad verdict through: the fields the
	// hive actually routes on are still validated.
	t.Run("still rejects an invalid verdict carrying an unknown key", func(t *testing.T) {
		raw := withKey(t, func(m map[string]any) {
			m["detail"] = "decorated"
			m["verdict"] = "looks-fine-to-me"
		})
		if _, err := ValidateReport(raw); err == nil || !strings.Contains(err.Error(), "verdict") {
			t.Fatalf("ValidateReport error = %v, want a verdict complaint", err)
		}
	})
}
