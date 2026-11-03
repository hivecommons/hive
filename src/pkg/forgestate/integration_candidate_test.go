package forgestate

import "testing"

func TestIntegrationCandidateCoreExcludesLeaseNotTestedIdentity(t *testing.T) {
	first := IntegrationCandidateSubject{
		Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: "owner/repo",
		Base: "v5", BaseSHA: "base", Policy: "policy", WorkflowTree: "workflow", ID: "subject", Generation: 1,
		Members:  []IntegrationCandidateMember{{PR: 1, Head: "A"}, {PR: 2, Head: "B"}},
		Prefixes: []IntegrationCandidatePrefix{{Commit: "base+A", Tree: "treeA"}, {Commit: "base+A+B", Tree: "treeAB"}},
	}
	copySubject := func() IntegrationCandidateSubject {
		out := first
		out.Members = append([]IntegrationCandidateMember(nil), first.Members...)
		out.Prefixes = append([]IntegrationCandidatePrefix(nil), first.Prefixes...)
		return out
	}
	reobserved := copySubject()
	reobserved.Generation++
	reobserved.PreparedBy = IntegrationPreparationSource{SourceSHA: "new-observer"}
	reobserved.Claim = &IntegrationDispatchClaim{State: "observer"}
	if !first.SameCore(reobserved) {
		t.Fatal("an unchanged documented prefix lost identity after a lease/provenance observation")
	}
	for _, change := range []struct {
		name   string
		mutate func(*IntegrationCandidateSubject)
	}{
		{"schema", func(s *IntegrationCandidateSubject) { s.Schema++ }},
		{"protocol", func(s *IntegrationCandidateSubject) { s.Protocol += "-changed" }},
		{"engine", func(s *IntegrationCandidateSubject) { s.Engine = "native" }},
		{"repository", func(s *IntegrationCandidateSubject) { s.Repo = "other/repo" }},
		{"base-name", func(s *IntegrationCandidateSubject) { s.Base = "other" }},
		{"base-object", func(s *IntegrationCandidateSubject) { s.BaseSHA = "other-base" }},
		{"policy", func(s *IntegrationCandidateSubject) { s.Policy = "other-policy" }},
		{"producer-closure", func(s *IntegrationCandidateSubject) { s.WorkflowTree = "other-harness" }},
		{"subject", func(s *IntegrationCandidateSubject) { s.ID = "other-subject" }},
		{"member-head", func(s *IntegrationCandidateSubject) { s.Members[0].Head = "new-A" }},
		{"member-order", func(s *IntegrationCandidateSubject) { s.Members[0], s.Members[1] = s.Members[1], s.Members[0] }},
		{"tested-commit", func(s *IntegrationCandidateSubject) { s.Prefixes[1].Commit = "other-commit" }},
		{"tested-tree", func(s *IntegrationCandidateSubject) { s.Prefixes[1].Tree = "other-tree" }},
		{"remove-predecessor", func(s *IntegrationCandidateSubject) { s.Members = s.Members[1:]; s.Prefixes = s.Prefixes[1:] }},
		{"drop-prefix", func(s *IntegrationCandidateSubject) { s.Prefixes = s.Prefixes[:1] }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := copySubject()
			change.mutate(&changed)
			if first.SameCore(changed) {
				t.Fatal("different ordered tested identity reused evidence")
			}
		})
	}
}
