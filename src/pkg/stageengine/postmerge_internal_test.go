package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// pipeline.status reports the observed post-merge status (#18645) and
// stays unknown without an observation or for an unrecognised value.
func TestPipelineStatusFact(t *testing.T) {
	merged := func(pm *PostMergeFacts) Facts {
		return Facts{Ref: Ref{Kind: stagelabels.PRItem}, Merged: true, PR: &PRFacts{PostMerge: pm}}
	}
	cases := []struct {
		name string
		f    Facts
		want string
	}{
		{"no PR facts", Facts{Ref: Ref{Kind: stagelabels.IssueItem}}, "unknown"},
		{"not observed", merged(nil), "unknown"},
		{"green", merged(&PostMergeFacts{Status: "green"}), "green"},
		{"red", merged(&PostMergeFacts{Status: "red", Column: "release", Reason: "release: run 7 failed"}), "red"},
		{"running", merged(&PostMergeFacts{Status: "running"}), "running"},
		{"unknown with reason", merged(&PostMergeFacts{Status: "unknown", Reason: "main-ci: run 3: revision could not be determined"}), "unknown"},
		{"unrecognised", merged(&PostMergeFacts{Status: "purple"}), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scalarFact(tc.f, "pipeline.status")
			if !ok || got != tc.want {
				t.Fatalf("pipeline.status = %q (%v), want %q", got, ok, tc.want)
			}
			if !holds(tc.f, workflowmodel.Predicate{Fact: "pipeline.status", Op: workflowmodel.OpIs, Value: tc.want}) {
				t.Fatalf("pipeline.status is %s does not hold", tc.want)
			}
		})
	}
}
