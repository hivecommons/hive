package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// dedupe.advisory_autoclose reaches the close-on-merge backfill
// (hivecommons/hive#15754): unset is on, false turns the item pass off.
func TestCloseOnMergeOptionsAdvisoryAutoclose(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name        string
		cfg         config.Config
		wantDisable bool
	}{
		{"unset", config.Config{}, false},
		{"off", config.Config{Dedupe: config.DedupeAdvisoryConfig{AdvisoryAutoclose: &off}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closeOnMergeOptionsFor(&tc.cfg, nil, nil).DisableAdvisoryAutoclose; got != tc.wantDisable {
				t.Fatalf("DisableAdvisoryAutoclose = %v, want %v", got, tc.wantDisable)
			}
		})
	}
}
