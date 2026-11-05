package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

// hivecommons/hive#16699: a decision park linked "blocked by" another
// decision park ("Answer on #N") takes #N's answer when the owner decides
// #N, instead of coming back as a second ask once #N is unparked or closed.
func TestSweepIssueUnparkCommandsDeferringParkInheritsDecision(t *testing.T) {
	type blocker struct {
		repo   string
		number int
		state  string
	}
	const deferringBody = "Answer on #511; the same choice applies here."
	for _, tc := range []struct {
		name     string
		labels   []string
		blockers []blocker
		inherit  bool
		// body is #512's text; "" means deferringBody.
		body string
		// blockerLabels are #511's park labels before the sweep; nil means
		// the needs-decision stage label.
		blockerLabels []string
		// command is the owner's reply on #511; "" means a decision.
		command string
		// maxActions caps the pass; 0 means the default.
		maxActions int
	}{
		{name: "only blocker is the decided park", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, inherit: true},
		{name: "closed blockers do not count", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}, {"other/repo", 7, "closed"}}, inherit: true},
		{name: "another open blocker", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}, {"org/repo", 600, "open"}}},
		{name: "blocker in another repo", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"other/repo", 511, "open"}}},
		{name: "blocker not decided", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 600, "open"}}},
		{name: "also parked on a person", labels: []string{"needs-human", NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}},
		{name: "no blockers", labels: []string{NeedsDecisionStageLabel}, blockers: nil},
		// The body must defer to the blocker: a decision park blocked by
		// ordinary work is still the owner's to make.
		{name: "body does not defer to the blocker", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, body: "Needs the rc tag from #511 to land first. Which installer should ship?"},
		{name: "body defers to a different issue", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, body: "Answer on #600; the same choice applies here."},
		// The blocker must have been a decision park: a `/hive approve` on a
		// blocked or needs-human park answers nothing the deferring park asked.
		{name: "blocker was parked blocked, not on a decision", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, blockerLabels: []string{"blocked"}, command: "/hive approve"},
		{name: "blocker was parked on a person only", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, blockerLabels: []string{"needs-human"}, command: "/hive approve"},
		// `/hive done` reports an owner-only step, not a decision.
		{name: "blocker answered with /hive done", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, command: "/hive done"},
		// The pass's action budget counts the inherited un-park too.
		{name: "action budget spent on the blocker", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, maxActions: 1},
		{name: "action budget leaves room", labels: []string{NeedsDecisionStageLabel}, blockers: []blocker{{"org/repo", 511, "open"}}, maxActions: 2, inherit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.command
			if command == "" {
				command = "/hive decision keep the rc tag"
			}
			decided := parkedIssue(511, humanComment(41, "owner", command))
			decided.Labels = nil
			blockerLabels := tc.blockerLabels
			if blockerLabels == nil {
				blockerLabels = []string{NeedsDecisionStageLabel}
			}
			for _, l := range blockerLabels {
				decided.Labels = append(decided.Labels, wireLabel{Name: l})
			}
			body := tc.body
			if body == "" {
				body = deferringBody
			}
			deferring := unparkWireIssue{
				Number: 512,
				Title:  "v2.0.0-rc.1 tag",
				Body:   body,
				User:   wireUser{Login: "hive[bot]"},
			}
			for _, l := range tc.labels {
				deferring.Labels = append(deferring.Labels, wireLabel{Name: l})
			}
			inner, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{decided, deferring}, map[string]string{"owner": "admin"})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/org/repo/issues/512/dependencies/blocked_by" {
					inner.Config.Handler.ServeHTTP(w, r)
					return
				}
				payload := []map[string]any{}
				for _, b := range tc.blockers {
					payload = append(payload, map[string]any{
						"number":         b.number,
						"state":          b.state,
						"repository_url": "https://api.github.com/repos/" + b.repo,
					})
				}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			t.Cleanup(server.Close)
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{MaxActions: tc.maxActions})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if tc.maxActions > 0 && result.actions() > tc.maxActions {
				t.Fatalf("pass exceeded MaxActions=%d: %d actions, unparked=%+v", tc.maxActions, result.actions(), result.Unparked)
			}
			var blockerUnparked bool
			for _, event := range result.Unparked {
				if event.Number == 511 {
					blockerUnparked = true
				}
			}
			if !blockerUnparked {
				t.Fatalf("#511 was not un-parked by the owner's reply: %+v", result.Unparked)
			}
			var inherited *IssueUnparkEvent
			for i, event := range result.Unparked {
				if event.Number == 512 {
					inherited = &result.Unparked[i]
				}
			}
			var note string
			for _, body := range rec.posted[512] {
				if strings.Contains(body, decisionInheritedMarker) {
					note = body
				}
			}
			liftedPark := strings.Contains(strings.Join(rec.removed[512], ","), NeedsDecisionStageLabel)
			if !tc.inherit {
				if inherited != nil || note != "" || liftedPark {
					t.Fatalf("#512 must stay its own ask: event=%+v note=%q removed=%v", inherited, note, rec.removed[512])
				}
				return
			}
			if inherited == nil || inherited.Actor != "owner" || inherited.Decision != "keep the rc tag" {
				t.Fatalf("#512 did not take #511's decision: %+v", result.Unparked)
			}
			if !liftedPark {
				t.Fatalf("#512 kept its decision park: removed=%v", rec.removed[512])
			}
			if !strings.Contains(note, "Answered on #511 by @owner. Decision: keep the rc tag") {
				t.Fatalf("note must say where it was answered, got %q", note)
			}
			for _, body := range rec.posted[512] {
				if strings.Contains(body, fmt.Sprintf("%s41 -->", unparkReplyMarkerPrefix)) {
					t.Fatalf("#511's command must not be answered on #512: %q", body)
				}
			}
		})
	}
}

// The inherited-decision note is an unsolicited comment, so it goes through
// the write-back gate like every other decision-park comment: a repo whose
// mode is off still gets its deferring park lifted, but no note is posted.
func TestSweepIssueUnparkCommandsInheritedNoteConsultsWritebackGate(t *testing.T) {
	decided := parkedIssue(511, humanComment(41, "owner", "/hive decision keep the rc tag"))
	decided.Labels = []wireLabel{{Name: NeedsDecisionStageLabel}}
	deferring := unparkWireIssue{
		Number: 512,
		Title:  "v2.0.0-rc.1 tag",
		Body:   "Answer on #511; the same choice applies here.",
		User:   wireUser{Login: "hive[bot]"},
		Labels: []wireLabel{{Name: NeedsDecisionStageLabel}},
	}
	inner, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{decided, deferring}, map[string]string{"owner": "admin"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/issues/512/dependencies/blocked_by" {
			inner.Config.Handler.ServeHTTP(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number":         511,
			"state":          "open",
			"repository_url": "https://api.github.com/repos/org/repo",
		}})
	}))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"org/repo": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var inherited bool
	for _, event := range result.Unparked {
		if event.Number == 512 {
			inherited = true
		}
	}
	if !inherited {
		t.Fatalf("#512 did not take #511's decision: %+v", result.Unparked)
	}
	if !strings.Contains(strings.Join(rec.removed[512], ","), NeedsDecisionStageLabel) {
		t.Fatalf("#512 kept its decision park: removed=%v", rec.removed[512])
	}
	for _, body := range rec.posted[512] {
		if strings.Contains(body, decisionInheritedMarker) {
			t.Fatalf("gate off must withhold the inherited-decision note, got %q", body)
		}
	}
}
