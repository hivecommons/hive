package prgate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// enforceOrder is the order enforce settles the real names, as pr-verifier.yml
// does: the two verdicts, then the wrapper.
var enforceOrder = []string{ContextPRType, ContextPRIssue, ContextVerify}

// newEnforceFixture is an enforce gate whose grant check has confirmed
// Commit statuses write.
func newEnforceFixture(t *testing.T, title string) *grantFixture {
	t.Helper()
	f := newGrantFixture(ModeEnforce, grantWriteFixture)
	f.api.subject.Title = title
	f.g.RefreshGrant(context.Background())
	return f
}

func contextsOf(posts []Status) []string {
	out := make([]string, 0, len(posts))
	for _, p := range posts {
		out = append(out, p.Context)
	}
	return out
}

func statesByContext(posts []Status) map[string]string {
	out := map[string]string{}
	for _, p := range posts {
		out[p.Context] = p.State
	}
	return out
}

func TestEnforceIsAvailable(t *testing.T) {
	if !EnforceAvailable || Effective(ModeEnforce) != ModeEnforce || Effective(ModeShadow) != ModeShadow || Effective(ModeOff) != ModeOff {
		t.Error("enforce must be available and Effective must leave every mode alone")
	}
}

func TestEnforceWithConfirmedGrantPostsTheRealNamesOnly(t *testing.T) {
	f := newEnforceFixture(t, "✨ add thing")
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Err != nil || strings.Join(out.Posted, ",") != strings.Join(enforceOrder, ",") {
		t.Fatalf("outcome %+v, want posts %v", out, enforceOrder)
	}
	for i, p := range f.api.posts {
		if err := PostableContext(p.Context); err != nil || p.Context == ContextVerifyShadow {
			t.Errorf("enforce posted %q", p.Context)
		}
		if f.api.postHead[i] != headA {
			t.Errorf("%s went on %s, not the event head", p.Context, f.api.postHead[i])
		}
	}
	if h := f.g.HealthFor([]string{"o/r"}); h.Effective != ModeEnforce || h.CannotPost {
		t.Errorf("health = %+v, want effective enforce", h)
	}
	if got := f.g.HoldReason("o/r", ContextVerify); got != "" {
		t.Errorf("a confirmed grant must not hold a PR: %q", got)
	}
}

// Enforce posts the verdicts the workflow records, for every title in the
// golden corpus the JS verifier replays: PR Type and PR Issue carry the
// verdict and the wrapper is green whatever it is.
func TestEnforceStatusesMatchTheWorkflowOnTheGoldenCorpus(t *testing.T) {
	for _, c := range loadCorpus(t) {
		got := statesByContext(Plan(ModeEnforce, c.Title))
		want := map[string]string{ContextPRType: c.Type.Conclusion, ContextPRIssue: c.Issue.Conclusion, ContextVerify: StateSuccess}
		for name, state := range want {
			if got[name] != state {
				t.Errorf("Plan(enforce, %q)[%s] = %q, the workflow records %q", c.Title, name, got[name], state)
			}
		}
		if len(got) != len(want) {
			t.Errorf("Plan(enforce, %q) planned %v", c.Title, got)
		}
	}
}

// Recorded PR shapes the workflow sees on pull_request_target. The gate keys
// on the event head and a fresh read of the title only, so a fork PR and a
// Dependabot PR get exactly what a same-repo PR gets.
func TestEnforceOnRecordedPRShapes(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  map[string]string
	}{
		{"same-repo feature", "✨ add thing", map[string]string{ContextPRType: StateSuccess, ContextPRIssue: StateSuccess, ContextVerify: StateSuccess}},
		{"fork PR with an issue number in the title", "🐛 fix crash #12", map[string]string{ContextPRType: StateSuccess, ContextPRIssue: StateFailure, ContextVerify: StateSuccess}},
		{"fork PR with no type prefix", "fix crash", map[string]string{ContextPRType: StateFailure, ContextPRIssue: StateSuccess, ContextVerify: StateSuccess}},
		{"Dependabot with the configured prefix", "🌱 build(deps): bump golang.org/x/net from 0.30.0 to 0.31.0", map[string]string{ContextPRType: StateSuccess, ContextPRIssue: StateSuccess, ContextVerify: StateSuccess}},
		{"Dependabot without a prefix", "Bump golang.org/x/net from 0.30.0 to 0.31.0", map[string]string{ContextPRType: StateFailure, ContextPRIssue: StateSuccess, ContextVerify: StateSuccess}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnforceFixture(t, tc.title)
			f.g.HandleEvent(context.Background(), "o/r", 7, headA)
			got := statesByContext(f.api.posts)
			for name, state := range tc.want {
				if got[name] != state {
					t.Errorf("%s = %q, want %q", name, got[name], state)
				}
			}
		})
	}
}

// A label flip re-runs the workflow on the same head with the same verdict;
// the gate posts nothing new for it, and after a restart (empty memory) the
// same flip posts the same verdict again.
func TestEnforceLabelFlipPostsTheSameVerdict(t *testing.T) {
	f := newEnforceFixture(t, "✨ add thing")
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	before := len(f.api.posts)
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Skipped != SkipUnchanged || len(f.api.posts) != before {
		t.Fatalf("a label flip on a settled head reposted: %+v", out)
	}
	restarted := newEnforceFixture(t, "✨ add thing")
	restarted.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if strings.Join(contextsOf(restarted.api.posts), ",") != strings.Join(enforceOrder, ",") {
		t.Fatalf("after a restart the flip must post again: %v", contextsOf(restarted.api.posts))
	}
	for i := range restarted.api.posts {
		if restarted.api.posts[i] != f.api.posts[i] {
			t.Errorf("restart posted %+v, first run %+v", restarted.api.posts[i], f.api.posts[i])
		}
	}
}

// A github.token autofix push fires no pull_request_target run (#16567), but
// the App's pull_request webhook still arrives: the gate posts on the new head,
// and a late event for the superseded head posts nothing.
func TestEnforceGithubTokenAutofixPushGetsTheGateOnTheNewHead(t *testing.T) {
	f := newEnforceFixture(t, "✨ add thing")
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	f.api.subject.HeadSHA = headB
	n := len(f.api.posts)
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headB)
	if strings.Join(out.Posted, ",") != strings.Join(enforceOrder, ",") {
		t.Fatalf("synchronize to the autofix head: %+v", out)
	}
	for _, h := range f.api.postHead[n:] {
		if h != headB {
			t.Errorf("a status went on %s, not the autofix head", h)
		}
	}
	if late := f.g.HandleEvent(context.Background(), "o/r", 7, headA); late.Skipped != SkipHeadMoved {
		t.Errorf("a late event for the old head: %+v", late)
	}
}

// A verdict that cannot be recorded turns the wrapper red with the reason, as
// the workflow job fails only then; the next event records it and the wrapper
// goes green.
func TestEnforceUnrecordedVerdictTurnsTheWrapperRedUntilRetried(t *testing.T) {
	f := newEnforceFixture(t, "✨ add thing")
	api := &flakyAPI{recordingAPI: f.api, failContext: ContextPRType}
	f.g.API = api
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Err == nil {
		t.Fatal("the failed write must be reported")
	}
	got := statesByContext(f.api.posts)
	if got[ContextPRIssue] != StateSuccess || got[ContextVerify] != StateFailure {
		t.Fatalf("posts = %+v", f.api.posts)
	}
	if last := f.api.posts[len(f.api.posts)-1]; !strings.Contains(last.Description, "could not record PR Type") || len([]rune(last.Description)) > StatusDescriptionMax {
		t.Errorf("wrapper description = %q", last.Description)
	}
	api.failContext = ""
	f.api.posts = nil
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	got = statesByContext(f.api.posts)
	if got[ContextPRType] != StateSuccess || got[ContextVerify] != StateSuccess || len(f.api.posts) != 2 {
		t.Fatalf("retry posts = %+v, want PR Type and a green wrapper", f.api.posts)
	}
}

func TestEnforceForbiddenStopsAtTheFirstWrite(t *testing.T) {
	f := newEnforceFixture(t, "✨ add thing")
	f.api.postErr = errors.Join(ErrForbidden, errors.New("403"))
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if !errors.Is(out.Err, ErrForbidden) || len(f.api.posts) != 1 {
		t.Fatalf("outcome %+v posts=%d, want one refused write and no more", out, len(f.api.posts))
	}
	if got := f.g.HoldReason("o/r", ContextVerify); got != "unauthorized:"+StepPostStatus {
		t.Errorf("hold reason = %q", got)
	}
}

// Enforce posts the real names only on a positively confirmed grant. A grant
// that is missing posts nothing and raises the cannot-post state the banner
// and the hub read; one that is unknown (no App credential, or no successful
// read yet) runs as shadow. Either way a PR missing the required context shows
// the held reason. When the owner accepts the grant, enforce takes over.
func TestEnforceFallsBackWithoutAConfirmedGrant(t *testing.T) {
	tests := []struct {
		name          string
		info          GrantInfo
		readErr       error
		wantPosts     []string
		wantEffective Mode
		wantCannot    bool
	}{
		{"grant missing (none)", grantNoneFixture, nil, nil, ModeOff, true},
		{"grant missing (read only)", grantReadFixture, nil, nil, ModeOff, true},
		{"no App installation to check", grantPATFixture, nil, []string{ContextVerifyShadow}, ModeShadow, false},
		{"grant read failed", GrantInfo{}, errors.New("github unavailable"), []string{ContextVerifyShadow}, ModeShadow, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGrantFixture(ModeEnforce, tc.info)
			f.err = tc.readErr
			f.g.HandleEvent(context.Background(), "o/r", 7, headA)
			if strings.Join(contextsOf(f.api.posts), ",") != strings.Join(tc.wantPosts, ",") {
				t.Fatalf("posts = %v, want %v", contextsOf(f.api.posts), tc.wantPosts)
			}
			h := f.g.HealthFor([]string{"o/r"})
			if h.Configured != ModeEnforce || h.Effective != tc.wantEffective || h.CannotPost != tc.wantCannot {
				t.Errorf("health = %+v", h)
			}
			if got := f.g.HoldReason("o/r", ContextVerify); got != "unauthorized:"+StepPostStatus {
				t.Errorf("hold reason = %q, want the missing-grant step", got)
			}

			// The owner accepts the grant: the next read after the TTL
			// confirms it and enforce posts the real names.
			f.info, f.err = grantWriteFixture, nil
			f.clock.advance(GrantTTL + time.Second)
			f.api.posts = nil
			f.g.HandleEvent(context.Background(), "o/r", 7, headA)
			if strings.Join(contextsOf(f.api.posts), ",") != strings.Join(enforceOrder, ",") {
				t.Fatalf("after the grant: posts = %v", contextsOf(f.api.posts))
			}
		})
	}
}

// A 403 invalidates the confirmed grant: until it is read again, enforce does
// not claim the real names.
func TestEnforceForbiddenInvalidatesTheConfirmedGrant(t *testing.T) {
	f := newEnforceFixture(t, "✨ add thing")
	f.api.postErr = errors.Join(ErrForbidden, errors.New("403"))
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if f.g.mode("o/r") != ModeShadow {
		t.Errorf("mode after a 403 = %s, want shadow until the grant is re-read", f.g.mode("o/r"))
	}
}

// flakyAPI fails the writes of one context with a transient error.
type flakyAPI struct {
	*recordingAPI
	failContext string
}

func (f *flakyAPI) PostStatus(ctx context.Context, repo string, number int, sha, statusContext, state, description string) error {
	if statusContext == f.failContext {
		return errors.New("502 bad gateway")
	}
	return f.recordingAPI.PostStatus(ctx, repo, number, sha, statusContext, state, description)
}
