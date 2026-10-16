package gfi

import (
	"testing"

	"github.com/hivecommons/hive/pkg/originlabel"
)

// Hive's own standing reports are never good first issues; their origin/*
// provenance labels sit in the default exclusion list beside the hive/*
// signal labels they accompany. Agent-filed and review-backlog items stay
// eligible: they are real work, provenance aside.
func TestDefaultExcludeLabelsCoverHiveReportOrigins(t *testing.T) {
	has := func(name string) bool {
		for _, l := range DefaultExcludeLabels {
			if l == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{originlabel.Advisory, originlabel.SelfReport, originlabel.Recommendations} {
		if !has(want) {
			t.Errorf("DefaultExcludeLabels missing %s", want)
		}
	}
	for _, keep := range []string{originlabel.Agent, originlabel.Review} {
		if has(keep) {
			t.Errorf("DefaultExcludeLabels must not exclude %s", keep)
		}
	}
}
