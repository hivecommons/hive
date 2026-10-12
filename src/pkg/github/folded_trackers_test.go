package github

import "testing"

func TestAnnotateFoldedTrackers(t *testing.T) {
	c := &Client{}
	c.foldedTrackers.add("o/r", 7)
	issues := []Issue{{Repo: "o/r", Number: 7}, {Repo: "o/r", Number: 8}, {Repo: "o/x", Number: 7}}
	c.annotateFoldedTrackers(issues)
	if !issues[0].FoldedTracker || issues[1].FoldedTracker || issues[2].FoldedTracker {
		t.Fatalf("unexpected flags: %+v", issues)
	}
}
