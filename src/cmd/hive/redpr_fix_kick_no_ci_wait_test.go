package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
)

// The targeted FIX-BEFORE-NEW re-engagement kick carries no policy template,
// so it must state the no-CI-wait rule itself (#9673): a scanner spent a full
// turn in `gh run watch` after pushing the repair.
func TestRedPRFixKickForbidsCIPolling(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "hivecommons/hive", Number: 9688, HeadSHA: "69773dc"}, false)
	for _, want := range []string{
		"Do NOT open a replacement PR.",
		"do not watch, poll, or sleep on CI",
		"automerge sweep merges the PR once it is green",
		"head: 69773dc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("redPRFixKick missing %q in:\n%s", want, got)
		}
	}
}
