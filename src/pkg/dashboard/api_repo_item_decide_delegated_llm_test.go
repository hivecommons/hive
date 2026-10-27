package dashboard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// fakeDecider stands in for the owner-delegate decider turn; no model runs.
type fakeDecider struct {
	reply string
	err   error
	calls int
	user  string
}

func (f *fakeDecider) RunDeciderTurn(_ context.Context, profile, _, user string) (string, error) {
	f.calls++
	f.user = user
	if profile != delegatedecision.DeciderProfile {
		return "", errors.New("wrong profile " + profile)
	}
	return f.reply, f.err
}

func withDecider(srv *Server, d *fakeDecider, enabled bool) {
	srv.deps.DelegateDecider = d
	srv.deps.Config.Decisions.Delegate.LLM = &config.DelegateDecisionLLMConfig{Enabled: &enabled}
}

func TestDelegatedDecisionLLMStep(t *testing.T) {
	direction := `{"verdict":"direction","text":"use option B","rationale":"B is reversible","actions":[{"kind":"comment-decision"},{"kind":"labels-add","labels":["triage/accepted"]}]}`
	cases := []struct {
		name     string
		reason   string
		decider  *fakeDecider
		enabled  bool
		calls    int
		rule     string
		verdict  delegatedecision.Verdict
		decision string
		note     string
	}{
		{
			name: "default case is decided by the turn", reason: "requires_human", decider: &fakeDecider{reply: direction}, enabled: true,
			calls: 1, rule: delegatedecision.RuleLLM, verdict: delegatedecision.VerdictDirection, decision: "/hive decision use option B", note: delegatedecision.DeciderProfile,
		},
		{
			name: "turn error keeps the reversible default", reason: "requires_human", decider: &fakeDecider{err: errors.New("model down")}, enabled: true,
			calls: 1, rule: "default-approve", verdict: delegatedecision.VerdictApprove, decision: "/hive decision go with the recommendation", note: "kept the rules' reversible default",
		},
		{
			name: "invalid reply keeps the reversible default", reason: "requires_human", decider: &fakeDecider{reply: `{"verdict":"merge-now"}`}, enabled: true,
			calls: 1, rule: "default-approve", verdict: delegatedecision.VerdictApprove, decision: "/hive decision go with the recommendation", note: "unknown verdict",
		},
		{
			name: "LLM step off by default", reason: "requires_human", decider: &fakeDecider{reply: direction}, enabled: false,
			calls: 0, rule: "default-approve", verdict: delegatedecision.VerdictApprove, decision: "/hive decision go with the recommendation",
		},
		{
			name: "rule decision never reaches the turn", reason: "looks like a duplicate of #3", decider: &fakeDecider{reply: direction}, enabled: true,
			calls: 0, rule: "dedupe", verdict: delegatedecision.VerdictCloseDuplicate, decision: "Duplicate of #3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
			srv := newDelegatedServer(t, mock)
			withDecider(srv, tc.decider, tc.enabled)

			w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"`+tc.reason+`"}`, nil)
			job := decodeDelegatedJob(t, w)
			if job.Status != delegatedJobStatusDone || job.Decision == nil || job.Decision.Rule != tc.rule || job.Decision.Verdict != tc.verdict {
				t.Fatalf("code=%d job=%+v", w.Code, job)
			}
			if tc.decider.calls != tc.calls {
				t.Fatalf("decider calls = %d, want %d", tc.decider.calls, tc.calls)
			}
			if len(mock.comments) == 0 || !strings.HasPrefix(mock.comments[0], tc.decision) || !strings.Contains(mock.comments[0], job.Marker) {
				t.Fatalf("comments = %v", mock.comments)
			}
			if tc.note != "" && !strings.Contains(strings.Join(job.Notes, "\n"), tc.note) {
				t.Fatalf("notes = %v, want %q", job.Notes, tc.note)
			}
			if tc.calls > 0 && !strings.Contains(tc.decider.user, "testorg/testrepo") {
				t.Fatalf("decider prompt = %q", tc.decider.user)
			}
		})
	}
}

func TestDelegatedDecisionLLMHeldItemIsRefused(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"hold", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	d := &fakeDecider{reply: `{"verdict":"approve","text":"x","rationale":"y","actions":[{"kind":"comment-decision"}]}`}
	withDecider(srv, d, true)

	job := decodeDelegatedJob(t, postDelegated(t, srv, 7, "writer", `{}`, nil))
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictRefused || d.calls != 0 {
		t.Fatalf("job=%+v calls=%d", job, d.calls)
	}
	if len(mock.comments) != 0 || len(mock.added) != 0 {
		t.Fatalf("held item must see no writes: %+v", mock)
	}
}

func TestDelegatedItemRefs(t *testing.T) {
	cases := []struct {
		text string
		want []int
	}{
		{"see #12 and #3, also #12 again", []int{3, 12}},
		{"self #7 is skipped", nil},
		{"other/repo#5 and url/#6 and a#9 are not same-repo refs", nil},
		{"(#4)", []int{4}},
		{"#1 #2 #3 #4 #5 #6 #8 #9 #10 #11 #12 #13", []int{1, 2, 3, 4, 5, 6, 8, 9, 10, 11}},
	}
	for _, tc := range cases {
		if got := delegatedItemRefs(tc.text, 7); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("delegatedItemRefs(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
