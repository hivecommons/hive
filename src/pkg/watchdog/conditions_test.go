package watchdog

import (
	"testing"
	"time"
)

func TestSetConditionPreservesTransitionTime(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	t2 := t0.Add(10 * time.Minute)

	var conds []Condition
	conds = setCondition(conds, Condition{Type: ConditionReady, Status: ConditionTrue, Reason: "ready"}, t0)
	c, ok := FindCondition(conds, ConditionReady)
	if !ok || c.LastTransitionTime != t0 {
		t.Fatalf("first observation must stamp t0, got %+v ok=%v", c, ok)
	}

	// Same status, new reason: transition time untouched, reason updated.
	conds = setCondition(conds, Condition{Type: ConditionReady, Status: ConditionTrue, Reason: "still ready"}, t1)
	c, _ = FindCondition(conds, ConditionReady)
	if c.LastTransitionTime != t0 {
		t.Fatalf("unchanged status must keep t0, got %v", c.LastTransitionTime)
	}
	if c.Reason != "still ready" {
		t.Fatalf("reason must track the latest observation, got %q", c.Reason)
	}

	// Status flip: transition time moves.
	conds = setCondition(conds, Condition{Type: ConditionReady, Status: ConditionFalse, Reason: "shell-prompt"}, t2)
	c, _ = FindCondition(conds, ConditionReady)
	if c.LastTransitionTime != t2 || c.Status != ConditionFalse {
		t.Fatalf("status flip must stamp t2, got %+v", c)
	}

	// A second type appends without touching the first.
	conds = setCondition(conds, Condition{Type: ConditionProducing, Status: ConditionUnknown}, t2)
	if len(conds) != 2 {
		t.Fatalf("want 2 conditions, got %d", len(conds))
	}
}

func TestFindConditionMissing(t *testing.T) {
	if _, ok := FindCondition(nil, ConditionReady); ok {
		t.Fatal("missing condition must report ok=false")
	}
}
