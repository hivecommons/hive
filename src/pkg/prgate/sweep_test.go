package prgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// sweepAPI is a fake GitHub for the backstop sweep: open heads, the contexts
// already on each head, and a record of every call.
type sweepAPI struct {
	heads    []OpenHead
	listErr  error
	has      map[string]bool // sha -> a gate context is already there
	posts    []string        // "sha|context"
	postErr  error
	probes   int
	reads    int
	subjects map[int]Subject
}

func (s *sweepAPI) ReadSubject(_ context.Context, _ string, number int) (Subject, error) {
	s.reads++
	return s.subjects[number], nil
}

func (s *sweepAPI) PostStatus(_ context.Context, _ string, _ int, sha, statusContext, _, _ string) error {
	s.posts = append(s.posts, sha+"|"+statusContext)
	return s.postErr
}

func (s *sweepAPI) ListOpenHeads(context.Context, string, int) ([]OpenHead, error) {
	return s.heads, s.listErr
}

func (s *sweepAPI) HeadHasContext(_ context.Context, _, sha, _ string) (bool, error) {
	s.probes++
	return s.has[sha], nil
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func newSweepFixture(mode Mode, heads []OpenHead) (*Gate, *sweepAPI, *bytes.Buffer) {
	api := &sweepAPI{heads: heads, has: map[string]bool{}, subjects: map[int]Subject{}}
	for _, h := range heads {
		api.subjects[h.Number] = Subject{HeadSHA: h.HeadSHA, Title: "✨ add thing", State: "open", Draft: h.Draft}
	}
	var logs bytes.Buffer
	g := New(api, func(string) Mode { return mode }, slog.New(slog.NewTextHandler(&logs, nil)))
	return g, api, &logs
}

func TestSweepPostsOnlyForHeadsWithoutAStatus(t *testing.T) {
	heads := []OpenHead{
		{Number: 1, HeadSHA: sha('a')},
		{Number: 2, HeadSHA: sha('b')},
		{Number: 3, HeadSHA: sha('c'), Draft: true},
		{Number: 4, HeadSHA: "not-a-sha"},
	}
	g, api, _ := newSweepFixture(ModeShadow, heads)
	api.has[sha('b')] = true

	res := g.Sweep(context.Background(), []string{"o/r"})
	if res.Posted != 1 || res.Present != 1 || len(api.posts) != 1 || api.posts[0] != sha('a')+"|"+ContextVerifyShadow {
		t.Fatalf("result %+v posts %v, want one shadow post on head a only", res, api.posts)
	}

	// A second pass answers from memory: no probe, no read, no post.
	probes, reads := api.probes, api.reads
	res = g.Sweep(context.Background(), []string{"o/r"})
	if res.Posted != 0 || api.probes != probes || api.reads != reads || len(api.posts) != 1 {
		t.Fatalf("second pass %+v probes %d->%d reads %d->%d posts %v", res, probes, api.probes, reads, api.reads, api.posts)
	}
}

func TestSweepEnforceChecksAndPostsTheRealName(t *testing.T) {
	g, api, _ := newSweepFixture(ModeEnforce, []OpenHead{{Number: 1, HeadSHA: sha('a')}})
	g.Sweep(context.Background(), []string{"o/r"})
	if len(api.posts) != 3 || api.posts[0] != sha('a')+"|"+ContextVerify {
		t.Fatalf("posts = %v, want the three real names starting with %q", api.posts, ContextVerify)
	}
}

func TestSweepModeOffTouchesNothing(t *testing.T) {
	g, api, _ := newSweepFixture(ModeOff, []OpenHead{{Number: 1, HeadSHA: sha('a')}})
	res := g.Sweep(context.Background(), []string{"o/r"})
	if res.Repos != 0 || api.probes != 0 || api.reads != 0 || len(api.posts) != 0 {
		t.Fatalf("result %+v probes=%d reads=%d posts=%v", res, api.probes, api.reads, api.posts)
	}
}

func TestSweepIsBoundedPerPass(t *testing.T) {
	var heads []OpenHead
	for i := 1; i <= maxSweepPosts+5; i++ {
		heads = append(heads, OpenHead{Number: i, HeadSHA: fmt.Sprintf("%040x", i)})
	}
	g, api, _ := newSweepFixture(ModeShadow, heads)
	g.Sweep(context.Background(), []string{"o/r"})
	if len(api.posts) != maxSweepPosts {
		t.Fatalf("%d posts in one pass, want the bound %d", len(api.posts), maxSweepPosts)
	}
	g.Sweep(context.Background(), []string{"o/r"})
	if len(api.posts) != maxSweepPosts+5 {
		t.Fatalf("%d posts after the next pass, want %d", len(api.posts), maxSweepPosts+5)
	}
}

func TestSweepListFailureIsLoggedNotFatal(t *testing.T) {
	g, api, logs := newSweepFixture(ModeShadow, nil)
	api.listErr = errors.New("boom")
	res := g.Sweep(context.Background(), []string{"o/r"})
	if res.Posted != 0 || !strings.Contains(logs.String(), "could not list open PRs") {
		t.Fatalf("result %+v logs %q", res, logs.String())
	}
}

func TestSweepEscalatesOnlyAfterTheCutoffWhenTheContextIsAbsent(t *testing.T) {
	heads := []OpenHead{{Number: 1, HeadSHA: sha('a')}, {Number: 2, HeadSHA: sha('b')}}
	tests := []struct {
		name          string
		now           time.Time
		contextOnHead bool
		want          bool
	}{
		{"before the cutoff", Cutoff.Add(-time.Hour), false, false},
		{"after the cutoff, context absent", Cutoff.Add(time.Hour), false, true},
		{"after the cutoff, the workflow still supplies it", Cutoff.Add(time.Hour), true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, api, logs := newSweepFixture(ModeEnforce, heads)
			clock := &fakeClock{t: tc.now}
			g.Now = clock.now
			g.Grant = func(context.Context) (GrantInfo, error) { return grantReadFixture, nil }
			api.has[sha('a')], api.has[sha('b')] = tc.contextOnHead, tc.contextOnHead

			res := g.Sweep(context.Background(), []string{"o/r"})
			if got := strings.Count(logs.String(), EscalationMessage); res.Escalated != tc.want || (got == EscalationRepeats) != tc.want || (!tc.want && got != 0) {
				t.Fatalf("escalated=%t, escalation lines=%d, want escalated=%t with %d lines", res.Escalated, strings.Count(logs.String(), EscalationMessage), tc.want, EscalationRepeats)
			}
			if len(api.posts) != 0 {
				t.Errorf("a gate that cannot post must post nothing: %v", api.posts)
			}
		})
	}
}

func TestSweepDoesNotRetryARefusedRepository(t *testing.T) {
	g, api, _ := newSweepFixture(ModeShadow, []OpenHead{{Number: 1, HeadSHA: sha('a')}, {Number: 2, HeadSHA: sha('b')}})
	api.postErr = errors.Join(ErrForbidden, errors.New("403"))
	g.Sweep(context.Background(), []string{"o/r"})
	g.Sweep(context.Background(), []string{"o/r"})
	if len(api.posts) != 1 {
		t.Fatalf("%d write attempts across two passes, want exactly one (cooldown)", len(api.posts))
	}
}

func TestSweepWithoutASweepAPIDoesNothing(t *testing.T) {
	g := newGate(&recordingAPI{}, ModeShadow)
	if res := g.Sweep(context.Background(), []string{"o/r"}); res != (SweepResult{}) {
		t.Fatalf("result %+v", res)
	}
	var nilGate *Gate
	if res := nilGate.Sweep(context.Background(), []string{"o/r"}); res != (SweepResult{}) {
		t.Fatalf("nil gate result %+v", res)
	}
}
