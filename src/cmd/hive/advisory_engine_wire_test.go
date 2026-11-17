package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/advisoryengine"
)

// TestAdvisoryAuditDetailPrivateIsMetadataOnly: the audit record for a
// private repository never carries advice text, even if a summary reached
// it; a public one keeps the clipped summary.
func TestAdvisoryAuditDetailPrivateIsMetadataOnly(t *testing.T) {
	r := advisoryengine.Result{Mode: "shadow", Repo: "o/r", Number: 7, Private: true, Provider: "gw", Model: "m", Requests: 3,
		Outcome: "task.complete", Verified: 2, Citations: 3, Turns: 5, Summary: "secret root cause in src/a.go:3"}
	d := advisoryAuditDetail(r)
	if strings.Contains(d, "secret") || strings.Contains(d, "summary=") {
		t.Fatalf("private detail carries advice text: %s", d)
	}
	for _, want := range []string{"repo=o/r", "number=7", "outcome=task.complete", "verified=2/3", "turns=5", "provider=gw", "model=m", "requests=3", "elapsed="} {
		if !strings.Contains(d, want) {
			t.Fatalf("private detail missing %q: %s", want, d)
		}
	}
	r.Private = false
	r.Summary = strings.Repeat("x", 700)
	if d := advisoryAuditDetail(r); !strings.Contains(d, "summary=") || strings.Count(d, "x") > 601 {
		t.Fatalf("public detail summary wrong: %s", d)
	}
}
