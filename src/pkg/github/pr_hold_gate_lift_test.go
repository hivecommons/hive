package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/holdguard"
)

// holdLiftWireEvent is one timeline entry as GitHub serialises it, with the
// fields LatestHoldGateLift reads: event kind, actor (login + type), label,
// created_at and, for head_ref_force_pushed, the commit_id it produced.
type holdLiftWireEvent struct {
	ID        int64      `json:"id"`
	Event     string     `json:"event"`
	Actor     *wireActor `json:"actor,omitempty"`
	Label     *wireLabel `json:"label,omitempty"`
	CommitID  string     `json:"commit_id,omitempty"`
	CreatedAt string     `json:"created_at"`
}

type wireActor struct {
	Login string `json:"login"`
	Type  string `json:"type,omitempty"`
}

func holdLiftUnlabeled(id int64, actor, actorType, label string, at time.Time) holdLiftWireEvent {
	return holdLiftWireEvent{
		ID:        id,
		Event:     "unlabeled",
		Actor:     &wireActor{Login: actor, Type: actorType},
		Label:     &wireLabel{Name: label},
		CreatedAt: at.UTC().Format(time.RFC3339),
	}
}

func holdLiftForcePush(id int64, commitID string, at time.Time) holdLiftWireEvent {
	return holdLiftWireEvent{
		ID:        id,
		Event:     "head_ref_force_pushed",
		Actor:     &wireActor{Login: "pusher", Type: "User"},
		CommitID:  commitID,
		CreatedAt: at.UTC().Format(time.RFC3339),
	}
}

// holdLiftServer serves GET /repos/{owner}/{repo}/issues/{n}/timeline from
// pages, one page per request, chaining them with rel="next" Link headers so
// the pagination loop in LatestHoldGateLift is exercised. status, when
// non-zero, fails every read.
type holdLiftServer struct {
	pages  [][]holdLiftWireEvent
	status int
	reads  int
}

func newHoldLiftClient(t *testing.T, rec *holdLiftServer) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/timeline") {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		rec.reads++
		if rec.status != 0 {
			w.WriteHeader(rec.status)
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if page < len(rec.pages) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
		}
		events := []holdLiftWireEvent{}
		if page-1 < len(rec.pages) {
			events = rec.pages[page-1]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(events)
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv, "org", []string{"org/repo"})
}

func TestLatestHoldGateLiftNilClient(t *testing.T) {
	var c *Client
	if _, _, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "abc"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: err = %v, want ErrNoGitHubClient", err)
	}
}

func TestLatestHoldGateLiftNoRemovalOnTimeline(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := &holdLiftServer{pages: [][]holdLiftWireEvent{{
		{ID: 1, Event: "labeled", Actor: &wireActor{Login: "maintainer"}, Label: &wireLabel{Name: "hold"}, CreatedAt: base.Format(time.RFC3339)},
		// A removed label that is not a hold-gate label is not a lift.
		holdLiftUnlabeled(2, "maintainer", "User", "needs-rebase", base.Add(time.Minute)),
		// A force-push alone, with nothing lifted, reports nothing.
		holdLiftForcePush(3, "abc123", base.Add(2*time.Minute)),
	}}}
	c := newHoldLiftClient(t, rec)

	lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || lift != (holdguard.GateLift{}) {
		t.Fatalf("no gate removal on the timeline must report ok=false and a zero lift, got ok=%v lift=%+v", ok, lift)
	}
}

func TestLatestHoldGateLiftPicksLatestRemovalAcrossPages(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := &holdLiftServer{pages: [][]holdLiftWireEvent{
		{
			holdLiftUnlabeled(1, "maintainer", "User", "hold", base),
			// Later removal by a bot on page 2 must win over this one.
		},
		{
			// A nil-looking entry (no actor/label) must be skipped, not crash.
			{ID: 2, Event: "unlabeled", CreatedAt: base.Add(30 * time.Second).Format(time.RFC3339)},
			holdLiftUnlabeled(3, "some-app[bot]", "", "on-hold", base.Add(time.Minute)),
			// An earlier removal arriving after the latest one must not
			// replace it.
			holdLiftUnlabeled(4, "maintainer", "User", "hold/review", base.Add(-time.Minute)),
		},
	}}
	c := newHoldLiftClient(t, rec)

	lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected a lift to be found")
	}
	if rec.reads != 2 {
		t.Fatalf("expected both timeline pages to be read, got %d reads", rec.reads)
	}
	if !lift.LiftedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("LiftedAt = %v, want the latest removal at %v", lift.LiftedAt, base.Add(time.Minute))
	}
	if !lift.ActorIsBot {
		t.Fatal("a [bot]-suffixed actor must be reported as a bot")
	}
	if lift.ActorIsHive {
		t.Fatal("an unconfigured bot login is not one of the hive's own accounts")
	}
	if !lift.LatestPushAt.IsZero() || lift.PushEvidence != "" {
		t.Fatalf("an ordinary push leaves no push evidence, got LatestPushAt=%v PushEvidence=%q", lift.LatestPushAt, lift.PushEvidence)
	}
}

func TestLatestHoldGateLiftForcePushDatesOnlyCurrentHead(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := &holdLiftServer{pages: [][]holdLiftWireEvent{{
		holdLiftUnlabeled(1, "maintainer", "User", "hold", base),
		// An earlier force-push that produced a different head says nothing
		// about when the current head landed.
		holdLiftForcePush(2, "0ldhead", base.Add(time.Minute)),
		// Two force-pushes to the same SHA: the later one dates its arrival.
		// SHA comparison is case-insensitive.
		holdLiftForcePush(3, "ABC123", base.Add(2*time.Minute)),
		holdLiftForcePush(4, "abc123", base.Add(3*time.Minute)),
	}}}
	c := newHoldLiftClient(t, rec)

	lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "  abc123 ")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want a lift", ok, err)
	}
	if lift.ActorIsBot || lift.ActorIsHive {
		t.Fatalf("a human maintainer is neither bot nor hive, got %+v", lift)
	}
	if !lift.LatestPushAt.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("LatestPushAt = %v, want the force-push that produced the current head at %v", lift.LatestPushAt, base.Add(3*time.Minute))
	}
	if lift.PushEvidence != holdguard.PushEvidenceForcePush {
		t.Fatalf("PushEvidence = %q, want %q", lift.PushEvidence, holdguard.PushEvidenceForcePush)
	}
	// The force-push post-dates the lift, so the guard must not treat the
	// lift as covering the current head.
	if holdguard.LiftCoversCurrentHead(lift) {
		t.Fatal("a lift older than the force-push that produced the head must not cover it")
	}
}

func TestLatestHoldGateLiftWithoutHeadIgnoresForcePushes(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := &holdLiftServer{pages: [][]holdLiftWireEvent{{
		holdLiftUnlabeled(1, "maintainer", "User", "hold", base),
		holdLiftForcePush(2, "abc123", base.Add(time.Minute)),
	}}}
	c := newHoldLiftClient(t, rec)

	lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want a lift", ok, err)
	}
	if !lift.LatestPushAt.IsZero() || lift.PushEvidence != "" {
		t.Fatalf("with no head SHA to match, force-pushes must not date the head, got %+v", lift)
	}
}

func TestLatestHoldGateLiftRecognisesConfiguredGateLabels(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	hiveBot := "hive-app[bot]"

	t.Run("extra hold label", func(t *testing.T) {
		rec := &holdLiftServer{pages: [][]holdLiftWireEvent{{
			holdLiftUnlabeled(1, hiveBot, "Bot", "do-not-merge", base),
		}}}
		c := newHoldLiftClient(t, rec)
		if _, ok, _ := c.LatestHoldGateLift(context.Background(), "org/repo", 7, ""); ok {
			t.Fatal("a label outside HoldLabels is not a gate label until configured")
		}
		c.SetHoldLabels([]string{"do-not-merge"})
		c.SetHiveIdentity(HiveIdentity{AppLogin: hiveBot})
		lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v, want the configured hold label to count", ok, err)
		}
		if !lift.ActorIsHive || !lift.ActorIsBot {
			t.Fatalf("the hive's own App bot must be reported as hive and bot, got %+v", lift)
		}
	})

	t.Run("sentinel alert label", func(t *testing.T) {
		rec := &holdLiftServer{pages: [][]holdLiftWireEvent{{
			holdLiftUnlabeled(1, "maintainer", "User", "Security/Alert", base),
		}}}
		c := newHoldLiftClient(t, rec)
		if _, ok, _ := c.LatestHoldGateLift(context.Background(), "org/repo", 7, ""); ok {
			t.Fatal("the sentinel label is not a gate label until configured")
		}
		c.SetSentinelAlertLabel(" security/alert ")
		lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v, want the sentinel label to count case-insensitively", ok, err)
		}
		if !lift.LiftedAt.Equal(base) {
			t.Fatalf("LiftedAt = %v, want %v", lift.LiftedAt, base)
		}
	})
}

func TestLatestHoldGateLiftPropagatesTimelineError(t *testing.T) {
	rec := &holdLiftServer{status: http.StatusBadGateway}
	c := newHoldLiftClient(t, rec)

	lift, ok, err := c.LatestHoldGateLift(context.Background(), "org/repo", 7, "abc123")
	if err == nil {
		t.Fatal("a failed timeline read must surface its error")
	}
	if ok || lift != (holdguard.GateLift{}) {
		t.Fatalf("a failed read must not report a lift, got ok=%v lift=%+v", ok, lift)
	}
}

func TestIsBotUser(t *testing.T) {
	cases := []struct {
		name string
		user *gh.User
		want bool
	}{
		{name: "nil", user: nil, want: false},
		{name: "human", user: &gh.User{Login: gh.Ptr("maintainer"), Type: gh.Ptr("User")}, want: false},
		{name: "bot by type", user: &gh.User{Login: gh.Ptr("some-app"), Type: gh.Ptr("bot")}, want: true},
		{name: "bot by suffix", user: &gh.User{Login: gh.Ptr("Some-App[BOT]")}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBotUser(tc.user); got != tc.want {
				t.Fatalf("isBotUser(%+v) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}
