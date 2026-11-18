package prgate

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// grantFixture is a gate over a fake API with a controllable grant provider.
type grantFixture struct {
	g     *Gate
	api   *recordingAPI
	clock *fakeClock
	info  GrantInfo
	err   error
	reads int
}

func newGrantFixture(mode Mode, info GrantInfo) *grantFixture {
	f := &grantFixture{
		api:   &recordingAPI{subject: Subject{HeadSHA: headA, Title: "✨ add thing", State: "open"}},
		clock: &fakeClock{t: time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC)},
		info:  info,
	}
	f.g = newGate(f.api, mode)
	f.g.Now = f.clock.now
	f.g.Grant = func(context.Context) (GrantInfo, error) {
		f.reads++
		return f.info, f.err
	}
	return f
}

// Recorded installation permission answers: statuses write, read only, none,
// and a credential with no installation at all.
var (
	grantWriteFixture = GrantInfo{Applicable: true, Installation: "example-org", Level: "write"}
	grantReadFixture  = GrantInfo{Applicable: true, Installation: "example-org", Level: "read"}
	grantNoneFixture  = GrantInfo{Applicable: true, Installation: "example-org", Level: ""}
	grantPATFixture   = GrantInfo{Applicable: false}
)

func TestGrantInfoMissing(t *testing.T) {
	tests := []struct {
		name string
		info GrantInfo
		want bool
	}{
		{"write", grantWriteFixture, false},
		{"admin", GrantInfo{Applicable: true, Level: "admin"}, false},
		{"read only", grantReadFixture, true},
		{"none", grantNoneFixture, true},
		{"not applicable (token credential)", grantPATFixture, false},
	}
	for _, tc := range tests {
		if got := tc.info.Missing(); got != tc.want {
			t.Errorf("%s: Missing = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestGrantPresentPostsAndMissingForcesOff(t *testing.T) {
	tests := []struct {
		name      string
		info      GrantInfo
		wantSkip  string
		wantPosts int
	}{
		{"grant present", grantWriteFixture, "", 1},
		{"grant missing (read only)", grantReadFixture, SkipGrantMissing, 0},
		{"grant missing (none)", grantNoneFixture, SkipGrantMissing, 0},
		{"no installation grants to check", grantPATFixture, "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGrantFixture(ModeShadow, tc.info)
			out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
			if out.Skipped != tc.wantSkip || len(f.api.posts) != tc.wantPosts {
				t.Fatalf("outcome %+v posts=%d, want skip=%q posts=%d", out, len(f.api.posts), tc.wantSkip, tc.wantPosts)
			}
			if tc.wantSkip != "" && f.api.reads != 0 {
				t.Errorf("a forced-off gate must not even read the PR, reads=%d", f.api.reads)
			}
			h := f.g.HealthFor([]string{"o/r"})
			if h.GrantMissing != (tc.wantSkip != "") || h.CannotPost != (tc.wantSkip != "") {
				t.Errorf("health = %+v", h)
			}
			if tc.wantSkip != "" && h.Effective != ModeOff {
				t.Errorf("effective mode = %s, want off", h.Effective)
			}
		})
	}
}

func TestGrantIsCachedForTTLAndRecoversWhenGranted(t *testing.T) {
	f := newGrantFixture(ModeShadow, grantNoneFixture)
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if f.reads != 1 {
		t.Fatalf("grant read %d times inside the TTL, want 1", f.reads)
	}
	f.info = grantWriteFixture
	f.clock.advance(GrantTTL + time.Second)
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Skipped != "" || len(f.api.posts) != 1 || f.reads != 2 {
		t.Fatalf("after the owner accepts the grant: %+v posts=%d reads=%d", out, len(f.api.posts), f.reads)
	}
}

func TestGrantReadFailureKeepsLastKnownAnswerAndNeverPromotes(t *testing.T) {
	f := newGrantFixture(ModeShadow, grantNoneFixture)
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	f.err = errors.New("github unavailable")
	f.clock.advance(GrantTTL + time.Second)
	out := f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Skipped != SkipGrantMissing || len(f.api.posts) != 0 {
		t.Fatalf("a failed grant read must keep the gate off: %+v", out)
	}
}

func TestForbiddenAtPostTimeIsRecordedNotRetriedAndRereadsGrant(t *testing.T) {
	f := newGrantFixture(ModeShadow, grantWriteFixture)
	f.api.postErr = errors.Join(ErrForbidden, errors.New("403"))
	f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	// The grant looked fine, GitHub refused anyway: recorded, and the repo is
	// left alone for the cooldown.
	out := f.g.HandleEvent(context.Background(), "o/r", 8, headA)
	if out.Skipped != SkipForbiddenCooldown || len(f.api.posts) != 1 {
		t.Fatalf("second event: %+v posts=%d, want a cooldown skip and no retry", out, len(f.api.posts))
	}
	h := f.g.HealthFor([]string{"o/r", "o/other"})
	if len(h.ForbiddenRepos) != 1 || h.ForbiddenRepos[0] != "o/r" || !h.CannotPost {
		t.Fatalf("health = %+v", h)
	}
	if f.g.Snapshot()["o/r"].Forbidden != 1 {
		t.Errorf("forbidden count = %d, want 1", f.g.Snapshot()["o/r"].Forbidden)
	}
	// After the cooldown the repository is tried once more, and the grant is
	// re-read because the refusal invalidated it.
	f.api.postErr = nil
	f.clock.advance(ForbiddenCooldown + time.Second)
	readsBefore := f.reads
	out = f.g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Skipped != "" || len(f.api.posts) != 2 || f.reads != readsBefore+1 {
		t.Fatalf("after the cooldown: %+v posts=%d reads=%d (before %d)", out, len(f.api.posts), f.reads, readsBefore)
	}
	if f.g.HealthFor([]string{"o/r"}).CannotPost {
		t.Error("a recovered repository must clear the cannot-post state")
	}
}

func TestHoldReasonOnlyForEnforceAndGateContexts(t *testing.T) {
	const want = "unauthorized:" + StepPostStatus
	tests := []struct {
		name    string
		mode    Mode
		info    GrantInfo
		forbid  bool
		context string
		want    string
	}{
		{"enforce, grant missing", ModeEnforce, grantReadFixture, false, ContextVerify, want},
		{"enforce, grant missing, PR Type", ModeEnforce, grantNoneFixture, false, ContextPRType, want},
		{"enforce, grant present", ModeEnforce, grantWriteFixture, false, ContextVerify, ""},
		{"enforce, write refused", ModeEnforce, grantWriteFixture, true, ContextVerify, want},
		{"shadow never holds a PR", ModeShadow, grantReadFixture, false, ContextVerify, ""},
		{"off never holds a PR", ModeOff, grantReadFixture, false, ContextVerify, ""},
		{"another required context is not ours", ModeEnforce, grantReadFixture, false, "DCO", ""},
		{"the shadow name is not required", ModeEnforce, grantReadFixture, false, ContextVerifyShadow, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGrantFixture(tc.mode, tc.info)
			f.g.RefreshGrant(context.Background())
			if tc.forbid {
				f.api.postErr = errors.Join(ErrForbidden, errors.New("403"))
				f.g.HandleEvent(context.Background(), "o/r", 7, headA)
			}
			if got := f.g.HoldReason("o/r", tc.context); got != tc.want {
				t.Errorf("HoldReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHoldReasonOnTheNilGateIsEmpty(t *testing.T) {
	var g *Gate
	if g.HoldReason("o/r", ContextVerify) != "" || DefaultHoldReason("o/r", ContextVerify) != "" {
		t.Error("no gate must never produce a hold reason")
	}
	SetDefault(nil)
}

func TestDaysUntilCutoff(t *testing.T) {
	tests := []struct {
		now  time.Time
		want int
	}{
		{time.Date(2026, time.November, 18, 9, 0, 0, 0, time.UTC), 13},
		{time.Date(2026, time.December, 1, 23, 0, 0, 0, time.UTC), 0},
		{Cutoff, 0},
		{Cutoff.Add(48 * time.Hour), 0},
	}
	for _, tc := range tests {
		if got := DaysUntilCutoff(tc.now); got != tc.want {
			t.Errorf("DaysUntilCutoff(%s) = %d, want %d", tc.now, got, tc.want)
		}
	}
}
