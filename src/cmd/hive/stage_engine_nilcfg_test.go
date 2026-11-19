package main

import (
	"sync"
	"testing"
)

// A nil config must not panic while building the process-wide options
// (hivecommons/hive#17736); the sync.Once is reset so the test is the first caller.
func TestStageEngineOptions_NilConfigDoesNotPanic(t *testing.T) {
	savedOpts := stageEngine.opts
	stageEngine.once = sync.Once{}
	stageEngine.opts = nil
	t.Cleanup(func() {
		stageEngine.once = sync.Once{}
		stageEngine.opts = savedOpts
		if savedOpts != nil {
			stageEngine.once.Do(func() {})
		}
	})

	opts := stageEngineOptions(nil, nil)
	if opts == nil {
		t.Fatal("stageEngineOptions(nil, nil) returned nil Options")
	}
	if opts.WorkflowFor != nil {
		t.Error("WorkflowFor should be nil without a config")
	}
	if opts.WorkflowProvisioned != nil {
		t.Error("WorkflowProvisioned should be nil without a config")
	}
}
