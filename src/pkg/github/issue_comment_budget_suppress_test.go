package github

import (
	"testing"
	"time"
)

func TestFirstSuppressionWarnsOncePerWindow(t *testing.T) {
	now := time.Now()
	b := &issueCommentBudget{now: func() time.Time { return now }}
	if !b.firstSuppression("o/r#1") {
		t.Fatal("first suppression should warn")
	}
	if b.firstSuppression("o/r#1") {
		t.Fatal("repeat suppression in window should not warn")
	}
	if !b.firstSuppression("o/r#2") {
		t.Fatal("other key should warn")
	}
	now = now.Add(issueCommentBudgetWindow + time.Minute)
	if !b.firstSuppression("o/r#1") {
		t.Fatal("suppression after window should warn again")
	}
}
