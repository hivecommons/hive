package github

import (
	"testing"
)

func TestMergeRequestPolicyNilClientIsSafe(t *testing.T) {
	var c *Client
	c.SetMergeRequestAllowUnprotectedBaseRepos(map[string]bool{"o/r": true})
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"o/r": true})
	if c.mergeRequestAllowsUnprotectedBase("o/r") || c.mergeRequestAllowsNoCI("o/r") {
		t.Fatal("nil client must not report policy opt-ins")
	}
}

func TestMergeRequestPolicyRepoSetsNormalizeAndClear(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, nil)

	c.SetMergeRequestAllowUnprotectedBaseRepos(map[string]bool{" r ": true, "ignored": false})
	if !c.mergeRequestAllowsUnprotectedBase("o/r") || !c.mergeRequestAllowsUnprotectedBase("r") {
		t.Fatal("allow_unprotected_base should match bare and qualified repo spellings")
	}
	if c.mergeRequestAllowsUnprotectedBase("o/ignored") {
		t.Fatal("false entries must not opt a repo in")
	}

	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"O/R": true})
	if !c.mergeRequestAllowsNoCI("o/r") {
		t.Fatal("no_ci_ok matching should be case-insensitive")
	}

	c.SetMergeRequestAllowUnprotectedBaseRepos(nil)
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{" ": true})
	if c.mergeRequestAllowsUnprotectedBase("o/r") || c.mergeRequestAllowsNoCI("o/r") {
		t.Fatal("nil/blank repo sets should clear opt-ins")
	}
}
