package dashboard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerOpenDependencies(t *testing.T) {
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		labels []string
		deps   []github.IssueDependency
		want   []string
	}{
		{
			name: "open blockers without label",
			deps: []github.IssueDependency{
				{Key: "acme/console#7"}, {Key: "acme/console#3", Resolved: true},
				{Key: "acme/console#6"}, {Key: "acme/console#7"}, {},
				{Key: "acme/console-api#8"},
			},
			want: []string{"acme/console#6", "acme/console#7", "acme/console-api#8"},
		},
		{
			name: "open blocker and label", labels: []string{"blocked"},
			deps: []github.IssueDependency{{Key: "acme/console#7"}}, want: []string{"acme/console#7"},
		},
		{name: "resolved blockers", deps: []github.IssueDependency{{Key: "acme/console#7", Resolved: true}, {}}},
		{name: "no dependencies"},
		{name: "label only", labels: []string{"blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			store := stagelabels.NewStore("", func() time.Time { return now })
			store.Observe(stagelabels.Key("acme/console", 21), stagelabels.Actionable, "actionable")
			status := minimalPayload()
			status.Repos = []FrontendRepo{{Name: "console", Full: "acme/console", ActionableIssues: []any{
				github.Issue{Number: 21, Title: "Waiting work", Labels: tc.labels, DependsOn: tc.deps},
			}}}
			s.UpdateStatus(status)
			resp := s.buildStageTracker(s.status, store, now)
			it := trackerItem(t, resp, "issue", 21)
			if it.Stage != trackerNumberActionable || it.Reason != "actionable" {
				t.Fatalf("dependency display must preserve the stage and reason: %+v", it)
			}
			if !slices.Equal(it.BlockedBy, tc.want) {
				t.Fatalf("blocked_by = %v, want %v", it.BlockedBy, tc.want)
			}
			wantBlocked := len(tc.want) > 0 || slices.Contains(tc.labels, "blocked")
			count := 0
			for _, mod := range it.Modifiers {
				if mod == modBlocked {
					count++
				}
			}
			if (count == 1) != wantBlocked || count > 1 {
				t.Fatalf("blocked modifier count = %d, want blocked=%v", count, wantBlocked)
			}
			if len(tc.want) > 0 {
				if !strings.Contains(it.Caption, "waiting on #") || strings.Contains(it.Caption, "waiting for the scanner") || it.Actor != "its blockers" {
					t.Fatalf("blocked work must explain the wait: %+v", it)
				}
				for _, key := range tc.want {
					ref := key
					if number, ok := strings.CutPrefix(key, "acme/console#"); ok {
						ref = "#" + number
					}
					if !strings.Contains(it.Caption, ref) {
						t.Errorf("caption %q omits %s", it.Caption, ref)
					}
				}
			} else if it.Actor != "scanner" || !strings.Contains(it.Caption, "waiting for the scanner") {
				t.Fatalf("no unresolved dependencies should preserve the normal caption: %+v", it)
			}
			body, err := json.Marshal(it)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), `"blocked_by"`) != (len(tc.want) > 0) {
				t.Fatalf("blocked_by must be present only for unresolved dependencies: %s", body)
			}
		})
	}
}
