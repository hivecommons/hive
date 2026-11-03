package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/redmain"
)

// While a repo's main is red past the threshold the kick says new PRs there
// are paused, names the check and the fix PR, and says existing PRs and
// issue filing go on; a green main adds nothing.
func TestAddRedMainNotice(t *testing.T) {
	s := pauseSchedulerConfig(t)
	since := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	fact := &redmain.Fact{Repo: "my-org/console", Branch: "main", Check: "unit", Since: since, TrackingIssue: 42,
		FixPRs: []redmain.FixPR{{Number: 77, Via: redmain.ViaLabel, WaitingOn: "@alice"}}}
	a := &github.ActionableResult{RedMain: map[string]*redmain.Fact{"my-org/console": fact}}

	got := s.addRedMainNotice("", a, "BASE")
	for _, want := range []string{"BASE", "RED MAIN — NEW PRs PAUSED", "my-org/console", "main red since 2026-10-22 08:00 UTC on unit; fix: #77 waiting on @alice", "Fixes #42", "existing PRs and issue filing are not affected"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "my-org/docs") {
		t.Errorf("a green repo must not be named:\n%s", got)
	}

	if got := s.addRedMainNotice("", &github.ActionableResult{}, "BASE"); got != "BASE" {
		t.Errorf("green main changed the kick: %q", got)
	}
	if got := s.addRedMainNotice("", nil, "BASE"); got != "BASE" {
		t.Errorf("nil snapshot changed the kick: %q", got)
	}

	// A paused repo is out of scope already; no red-main notice for it.
	paused := pauseSchedulerConfig(t, "console")
	if got := paused.addRedMainNotice("", a, "BASE"); got != "BASE" {
		t.Errorf("operator-paused repo got a red-main notice: %q", got)
	}
}
