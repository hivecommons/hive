package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// `blocked` is a default waiting label (hivecommons/hive#12376): the band
// shows a blocked issue as parked, whatever it waits on, while the your-turn
// decision (blockedIssueOwnership) says whose move it is.

// The default waiting labels are exactly the four parking buckets' labels
// plus `blocked`, the set the un-park sweep answers (with needs-spec, which
// the band parks but the sweep leaves alone), plus the prior-art gate's park
// stage/017-possible-existing, which the gate's resume answers instead
// (hivecommons/hive#13575).
func TestDefaultWaitingLabelsAreParkingPlusBlocked12376(t *testing.T) {
	var want []string
	for _, p := range defaultOwnershipParking() {
		want = append(want, p.labels...)
	}
	want = append(want, blockedWorkflowLabel, config.PossiblyExistsStageLabel)
	got := append([]string{}, defaultIssueWaitingLabels...)
	slices.Sort(want)
	slices.Sort(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default waiting labels %v, want parking labels plus blocked %v", got, want)
	}
}

func TestIssueBandBlockedIsParked12376(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		issue    github.Issue
		cfg      config.DashboardIssueBandsConfig
		wantBand string
		wantChip string
	}{
		{"blocked, no recorded blocker", github.Issue{Repo: "o/r", Number: 1, Labels: []string{"blocked"}}, config.DashboardIssueBandsConfig{}, "waiting", "blocked"},
		{"blocked on another item", github.Issue{Repo: "o/r", Number: 2, Labels: []string{"Blocked"}, DependsOn: []github.IssueDependency{{Key: "o/other#25"}}}, config.DashboardIssueBandsConfig{}, "waiting", "blocked"},
		{"blocked and needs-human: the human chip wins", github.Issue{Repo: "o/r", Number: 3, Labels: []string{"blocked", "needs-human"}}, config.DashboardIssueBandsConfig{}, "waiting", "human"},
		{"blocked and covered as done", github.Issue{Repo: "o/r", Number: 4, Labels: []string{"blocked", "hive/covered-by-pr"}}, config.DashboardIssueBandsConfig{}, "done", ""},
		{"an operator list without blocked keeps it out", github.Issue{Repo: "o/r", Number: 5, Labels: []string{"blocked"}}, config.DashboardIssueBandsConfig{WaitingLabels: []string{"needs-human"}}, "ready", ""},
		{"an open blocked-by link alone is not parked", github.Issue{Repo: "o/r", Number: 6, DependsOn: []github.IssueDependency{{Key: "o/other#25"}}}, config.DashboardIssueBandsConfig{}, "ready", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := IssueBand(tc.issue, false, tc.cfg, now)
			if info.Band != tc.wantBand || info.Chip != tc.wantChip {
				t.Fatalf("band = %q chip = %q, want %q %q", info.Band, info.Chip, tc.wantBand, tc.wantChip)
			}
		})
	}
	if rule := issueBandSpec("waiting", config.DashboardIssueBandsConfig{}).Rule; !strings.Contains(rule, "blocked") {
		t.Fatalf("Parked band rule %q does not name blocked", rule)
	}
}
