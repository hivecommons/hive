package dashboard

import (
	"testing"
	"time"
)

func TestNoModelsWarnTracker(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &noModelsWarnTracker{last: map[string]time.Time{}, now: func() time.Time { return now }}

	if !tr.shouldWarn("llm-d") {
		t.Fatal("first occurrence must warn")
	}
	now = now.Add(10 * time.Minute)
	if tr.shouldWarn("llm-d") {
		t.Fatal("repeat inside the window must not warn")
	}
	if !tr.shouldWarn("vllm") {
		t.Fatal("other backend has its own window")
	}
	tr.clear("llm-d")
	if !tr.shouldWarn("llm-d") {
		t.Fatal("warn again after a successful fetch cleared the key")
	}
	now = now.Add(noModelsWarnInterval)
	if !tr.shouldWarn("llm-d") {
		t.Fatal("warn again after the window elapsed")
	}
}
