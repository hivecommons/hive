package config

import (
	"strings"
	"testing"
)

// Covers ValidateSandboxJobAffinity inside crux/config itself: the same cases
// run from pkg/config through the alias types, but the crux package gates its
// own coverage floor.
func jobAffinityFixture() *SandboxJobAffinity {
	return &SandboxJobAffinity{NodeAffinity: &SandboxJobNodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &SandboxJobNodeSelector{NodeSelectorTerms: []SandboxJobNodeSelectorTerm{{MatchExpressions: []SandboxJobNodeSelectorRequirement{
			{Key: "spyre.node-classification", Operator: "In", Values: []string{"all"}},
			{Key: "kubernetes.io/hostname", Operator: "NotIn", Values: []string{"bad-node"}},
		}}}},
		PreferredDuringSchedulingIgnoredDuringExecution: []SandboxJobPreferredSchedulingTerm{{Weight: 50, Preference: SandboxJobNodeSelectorTerm{MatchExpressions: []SandboxJobNodeSelectorRequirement{{Key: "pool", Operator: "Exists"}}}}},
	}}
}

func TestValidateSandboxJobAffinity(t *testing.T) {
	if err := ValidateSandboxJobAffinity(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSandboxJobAffinity(jobAffinityFixture()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*SandboxJobAffinity)
		field  string
	}{
		{"missing node affinity", func(a *SandboxJobAffinity) { a.NodeAffinity = nil }, "nodeAffinity"},
		{"empty required", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = nil
		}, "nodeSelectorTerms"},
		{"too many terms", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = make([]SandboxJobNodeSelectorTerm, MaxJobAffinityTerms+1)
		}, "nodeSelectorTerms"},
		{"empty expressions", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions = nil
		}, "matchExpressions"},
		{"too many expressions", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions = make([]SandboxJobNodeSelectorRequirement, MaxJobAffinityTerms+1)
		}, "matchExpressions"},
		{"missing key", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key = ""
		}, ".key"},
		{"bad operator", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Equals"
		}, ".operator"},
		{"empty in", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values = nil
		}, ".values"},
		{"exists values", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Exists"
		}, ".values"},
		{"exists explicit empty values", func(a *SandboxJobAffinity) {
			expr := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
			expr.Operator = "Exists"
			expr.Values = []string{}
		}, ".values"},
		{"does not exist explicit empty values", func(a *SandboxJobAffinity) {
			expr := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
			expr.Operator = "DoesNotExist"
			expr.Values = []string{}
		}, ".values"},
		{"too many values", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values = make([]string, MaxJobAffinityValues+1)
		}, ".values"},
		{"bad integer", func(a *SandboxJobAffinity) {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Gt"
		}, ".values"},
		{"bad weight", func(a *SandboxJobAffinity) {
			a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight = 101
		}, ".weight"},
		{"too many preferred", func(a *SandboxJobAffinity) {
			a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = make([]SandboxJobPreferredSchedulingTerm, MaxJobAffinityTerms+1)
		}, "preferredDuringSchedulingIgnoredDuringExecution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := jobAffinityFixture()
			tc.change(a)
			if err := ValidateSandboxJobAffinity(a); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %v, want %s", err, tc.field)
			}
		})
	}
	for _, op := range []string{"In", "NotIn", "Exists", "DoesNotExist", "Gt", "Lt"} {
		a := jobAffinityFixture()
		expr := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
		expr.Operator = op
		switch op {
		case "Exists", "DoesNotExist":
			expr.Values = nil
		case "Gt", "Lt":
			expr.Values = []string{"42"}
		}
		if err := ValidateSandboxJobAffinity(a); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
}
