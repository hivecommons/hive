package dashboard

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/governor"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

func coverageConn(login, role string, pong time.Time) *ContributorConnection {
	return &ContributorConnection{
		profile:      &ContributorProfile{GitHubUsername: login, ContributorID: "c-" + login, TrustTier: "contributor"},
		role:         role,
		assignedRole: role,
		lastPong:     pong,
	}
}

func TestRoleCoverage_ConnectedRoleRelaysOnly(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(hub.Close)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	frank := coverageConn("frank", "quality", now)
	hub.mu.Lock()
	hub.connections["alice"] = coverageConn("alice", "quality", now)
	hub.connections["bob"] = coverageConn("bob", "quality", now.Add(-wsHeartbeatTimeout-time.Second)) // stale heartbeat
	hub.connections["carol"] = coverageConn("carol", "", now)                                         // general work, no role
	hub.connections["dave"] = coverageConn("dave", "scanner", now)
	hub.connections["erin"] = coverageConn("erin", "Quality", now) // role is normalised
	hub.connections["frank"] = frank                               // failure-streak paused below
	hub.connections["ghost"] = &ContributorConnection{role: "quality", lastPong: now}
	hub.mu.Unlock()

	hub.failureStreakMu.Lock()
	if hub.contributorFailureStreaks == nil {
		hub.contributorFailureStreaks = make(map[string]contributorFailureStreak)
	}
	hub.contributorFailureStreaks[identityOf(frank)] = contributorFailureStreak{Count: contributorFailureStreakThreshold, LastAt: now}
	hub.failureStreakMu.Unlock()

	got := hub.RoleCoverage(now)
	want := map[string]governor.ContributorCoverage{
		"quality": {Contributors: []string{"alice", "erin"}},
		"scanner": {Contributors: []string{"dave"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoleCoverage = %+v, want %+v", got, want)
	}
}

func TestRoleCoverage_NobodyConnectedIsNil(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(hub.Close)
	if got := hub.RoleCoverage(time.Now()); got != nil {
		t.Fatalf("empty hub: RoleCoverage = %+v, want nil", got)
	}
	var none *ContributeWSHub
	if got := none.RoleCoverage(time.Now()); got != nil {
		t.Fatalf("nil hub: RoleCoverage = %+v, want nil", got)
	}
	var noServer *Server
	if got := noServer.ContributorRoleCoverage(time.Now()); got != nil {
		t.Fatalf("nil server: ContributorRoleCoverage = %+v, want nil", got)
	}
	if got := (&Server{}).ContributorRoleCoverage(time.Now()); got != nil {
		t.Fatalf("server without a hub: ContributorRoleCoverage = %+v, want nil", got)
	}
}

func TestFrontendLaneCoverage(t *testing.T) {
	if got := frontendLaneCoverage(nil); got != nil {
		t.Fatalf("empty input must render nil, got %+v", got)
	}
	since := time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC)
	got := frontendLaneCoverage(map[string]governor.LaneCoverage{
		"quality": {Contributors: []string{"alice"}, Since: since, LastSeen: since.Add(time.Minute)},
		"scanner": {Since: since, LastSeen: since, GraceUntil: since.Add(10 * time.Minute)},
	})
	want := map[string]FrontendLaneCoverage{
		"quality": {Contributors: []string{"alice"}, Since: "2026-11-03T11:00:00Z", LastSeen: "2026-11-03T11:01:00Z"},
		"scanner": {Since: "2026-11-03T11:00:00Z", LastSeen: "2026-11-03T11:00:00Z", GraceUntil: "2026-11-03T11:10:00Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frontendLaneCoverage = %+v, want %+v", got, want)
	}
}

// The operator's "Suspend contributions" switch refuses work to every relay
// while their sockets stay up. A connected relay is then covering nothing, so
// no coverage may be reported — otherwise pausing contributors would also
// pause the resident agent and the lane would have no worker at all.
func TestRoleCoverage_SuspendedContributionsCoverNothing(t *testing.T) {
	hub, s := covK2Hub(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	hub.mu.Lock()
	hub.connections["alice"] = coverageConn("alice", "quality", now)
	hub.mu.Unlock()

	s.deps.Config.Hub.ContributeSuspended = true
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("suspended contributions: RoleCoverage = %+v, want nil", got)
	}
	if got := s.ContributorRoleCoverage(now); got != nil {
		t.Fatalf("suspended contributions: ContributorRoleCoverage = %+v, want nil", got)
	}

	s.deps.Config.Hub.ContributeSuspended = false
	got := hub.RoleCoverage(now)
	if len(got["quality"].Contributors) != 1 || got["quality"].Contributors[0] != "alice" {
		t.Fatalf("contributions resumed: RoleCoverage = %+v, want quality covered by alice", got)
	}
}

// A relay in standby mode stays connected but never asks for ordinary work:
// it takes only what an owner dispatches to it by hand. It must not count as
// contributor-first cover, or the resident agent is withheld for a lane that
// nobody is draining. Driven through the real standby_declare and
// standby_release handlers, so the flag RoleCoverage reads is the one the
// protocol sets.
func TestRoleCoverage_StandbyRelayDoesNotCover(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	bob := coverageConn("bob", "quality", now)
	bob.ws, _ = wsPipe(t)
	hub.mu.Lock()
	hub.connections["alice"] = coverageConn("alice", "quality", now)
	hub.connections["bob"] = bob
	hub.mu.Unlock()
	session := &wsSession{h: hub, contributor: bob}

	session.handleStandbyDeclare(WSMessage{Type: "standby_declare", Standby: &WSStandby{Lanes: []string{"quality"}}})
	want := map[string]governor.ContributorCoverage{"quality": {Contributors: []string{"alice"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("standby relay counted as cover: RoleCoverage = %+v, want %+v", got, want)
	}

	session.handleStandbyRelease(WSMessage{Type: "standby_release"})
	want = map[string]governor.ContributorCoverage{"quality": {Contributors: []string{"alice", "bob"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("after standby_release: RoleCoverage = %+v, want %+v", got, want)
	}
}

// The live case from 2026-11-08 on hive-wild-mole: the only relay acting as
// scanner had its quota guard holding all work (weekly window 15% left,
// reserve 20%), yet the governor withheld scanner's kicks because the hub
// still reported the relay as covering. A held relay covers nothing; once it
// reports the hold released it covers again.
func TestRoleCoverage_QuotaHeldRelayDoesNotCover(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	relay := coverageConn("Danathar", "scanner", now)
	hub.mu.Lock()
	hub.connections["relay"] = relay
	hub.mu.Unlock()
	session := &wsSession{h: hub, contributor: relay}

	want := map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"Danathar"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("precondition: RoleCoverage = %+v, want %+v", got, want)
	}

	session.handleQuotaHold(WSMessage{Type: "quota_hold", State: "held", Reason: "quota_guard"})
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("quota-held relay counted as cover: RoleCoverage = %+v, want nil", got)
	}

	session.handleQuotaHold(WSMessage{Type: "quota_hold", State: "released"})
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the hold was released: RoleCoverage = %+v, want %+v", got, want)
	}
}

// The relay sends quota_hold only to a hub that advertises the token, so the
// hub must advertise it or the fix never reaches the wire.
func TestServerCapabilities_AdvertisesQuotaHold(t *testing.T) {
	if !testContainsString(serverCapabilities(), capQuotaHoldV1) {
		t.Fatalf("serverCapabilities() = %v, missing %q", serverCapabilities(), capQuotaHoldV1)
	}
}

// A suspended standby configuration has every lane of its declaration
// rejected, so nothing is recorded in conn.standby — but the relay is still in
// standby mode and still never asks for ordinary work. It must not cover.
func TestRoleCoverage_RejectedStandbyDeclarationDoesNotCover(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	bob := coverageConn("bob", "quality", now)
	bob.ws, _ = wsPipe(t)
	key := standbyOutcomeKey("bob", standbyConfigFromConnection(bob))
	hub.completedMu.Lock()
	for n := 1; n <= standbypkg.DefaultSuspendThreshold; n++ {
		hub.standbyOutcomes = append(hub.standbyOutcomes, standbyOutcomeRecord{
			Key: key, Lane: "quality", Repo: "org/r", Number: n, Kind: standbypkg.OutcomeClosedUnmerged,
		})
	}
	hub.completedMu.Unlock()
	if suspended, _ := hub.standbySuspended(key); !suspended {
		t.Fatal("setup: bob's standby configuration must be suspended")
	}
	hub.mu.Lock()
	hub.connections["bob"] = bob
	hub.mu.Unlock()

	(&wsSession{h: hub, contributor: bob}).handleStandbyDeclare(WSMessage{Type: "standby_declare"})
	if len(bob.standby) != 0 {
		t.Fatalf("setup: a suspended declaration must record no lane, got %+v", bob.standby)
	}
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("rejected standby relay counted as cover: RoleCoverage = %+v, want nil", got)
	}
}

// rateLimitedCoverageHub builds a hub whose "contributor" tier has the given
// limits and seeds identityOf(relay) with one assignment stamp per age, as
// recordAssignment does when selection hands a task out.
func rateLimitedCoverageHub(t *testing.T, limits config.TierRate, relay *ContributorConnection, now time.Time, ages ...time.Duration) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{"contributor": limits}
	for _, age := range ages {
		hub.recordAssignment(identityOf(relay), now.Add(-age))
	}
	hub.mu.Lock()
	hub.connections[relay.profile.GitHubUsername] = relay
	hub.mu.Unlock()
	return hub, s
}

// selectionRefusal asks real task selection for work on behalf of relay's
// identity and returns the task_unavailable reason. The probe connection has
// no role, so the answer is about the identity's limits, not role policy.
func selectionRefusal(t *testing.T, hub *ContributeWSHub, s *Server, relay *ContributorConnection) string {
	t.Helper()
	oneActionableIssueN(s, 0)
	msg := hub.selectTask(&ContributorConnection{profile: relay.profile, lastPong: time.Now()})
	if msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("setup: selection must refuse this identity, got %+v", msg)
	}
	return msg.Reason
}

// A relay that has been handed its tier's max_per_hour tasks in the last hour
// is refused work with hourly_limit until the window rolls. It is connected
// and acting as the role, but it will not take the lane's next item, so it
// must not keep the resident agent withheld.
func TestRoleCoverage_HourlyLimitReachedDoesNotCover(t *testing.T) {
	now := time.Now()
	relay := coverageConn("alice", "quality", now)
	hub, s := rateLimitedCoverageHub(t, config.TierRate{MaxPerHour: 2, MaxPerDay: 50, MaxConcurrent: 5},
		relay, now, 10*time.Minute, 20*time.Minute)

	if got := selectionRefusal(t, hub, s, relay); got != taskUnavailableHourlyLimit {
		t.Fatalf("setup: selection refused with %q, want %q", got, taskUnavailableHourlyLimit)
	}
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("relay at its hourly limit counted as cover: RoleCoverage = %+v, want nil", got)
	}
}

// The same for the daily cap: no assignment in the last hour, but the tier's
// max_per_day in the last 24 hours, so selection refuses with daily_limit.
func TestRoleCoverage_DailyLimitReachedDoesNotCover(t *testing.T) {
	now := time.Now()
	relay := coverageConn("alice", "scanner", now)
	hub, s := rateLimitedCoverageHub(t, config.TierRate{MaxPerHour: 10, MaxPerDay: 3, MaxConcurrent: 5},
		relay, now, 2*time.Hour, 5*time.Hour, 20*time.Hour)

	if got := selectionRefusal(t, hub, s, relay); got != taskUnavailableDailyLimit {
		t.Fatalf("setup: selection refused with %q, want %q", got, taskUnavailableDailyLimit)
	}
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("relay at its daily limit counted as cover: RoleCoverage = %+v, want nil", got)
	}
}

// A relay one task short of both caps still covers: only a used-up allowance
// takes a relay out. Stamps older than the day window do not count. Bob, in
// the same tier and lane, is at the hourly cap and is the only one dropped.
func TestRoleCoverage_UnderRateLimitsCovers(t *testing.T) {
	now := time.Now()
	alice := coverageConn("alice", "quality", now)
	bob := coverageConn("bob", "quality", now)
	// alice: 1 of 2 this hour, 2 of 3 today (the 25h-old stamp has rolled off).
	hub, _ := rateLimitedCoverageHub(t, config.TierRate{MaxPerHour: 2, MaxPerDay: 3, MaxConcurrent: 5},
		alice, now, 10*time.Minute, 3*time.Hour, 25*time.Hour)
	hub.recordAssignment(identityOf(bob), now.Add(-5*time.Minute))
	hub.recordAssignment(identityOf(bob), now.Add(-15*time.Minute))
	hub.mu.Lock()
	hub.connections["bob"] = bob
	hub.mu.Unlock()

	want := map[string]governor.ContributorCoverage{"quality": {Contributors: []string{"alice"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoleCoverage = %+v, want %+v (alice under both caps, bob at the hourly cap)", got, want)
	}
}

// tierRateLimitReached is the single rule selection and RoleCoverage share.
func TestTierRateLimitReached(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		limits map[string]config.TierRate
		tier   string
		ages   []time.Duration
		want   tierRateLimitHit
	}{
		{name: "under both caps", limits: map[string]config.TierRate{"contributor": {MaxPerHour: 2, MaxPerDay: 3}},
			tier: "contributor", ages: []time.Duration{time.Minute, 2 * time.Hour}},
		{name: "at the hourly cap", limits: map[string]config.TierRate{"contributor": {MaxPerHour: 2, MaxPerDay: 50}},
			tier: "contributor", ages: []time.Duration{time.Minute, 30 * time.Minute},
			want: tierRateLimitHit{Reason: taskUnavailableHourlyLimit, Count: 2, Max: 2}},
		{name: "at the daily cap", limits: map[string]config.TierRate{"contributor": {MaxPerHour: 10, MaxPerDay: 2}},
			tier: "contributor", ages: []time.Duration{2 * time.Hour, 23 * time.Hour},
			want: tierRateLimitHit{Reason: taskUnavailableDailyLimit, Count: 2, Max: 2}},
		{name: "both caps reports hourly first", limits: map[string]config.TierRate{"contributor": {MaxPerHour: 1, MaxPerDay: 1}},
			tier: "contributor", ages: []time.Duration{time.Minute},
			want: tierRateLimitHit{Reason: taskUnavailableHourlyLimit, Count: 1, Max: 1}},
		{name: "stamps past the day window do not count", limits: map[string]config.TierRate{"contributor": {MaxPerHour: 1, MaxPerDay: 1}},
			tier: "contributor", ages: []time.Duration{25 * time.Hour}},
		{name: "zero caps are unlimited", limits: map[string]config.TierRate{"advisor": {}},
			tier: "advisor", ages: []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}},
		{name: "a tier with no row is unlimited", limits: map[string]config.TierRate{"contributor": {MaxPerHour: 1, MaxPerDay: 1}},
			tier: "custom", ages: []time.Duration{time.Minute, 2 * time.Minute}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := covK2Hub(t)
			s.deps.Config.Hub.TierLimits = tc.limits
			for _, age := range tc.ages {
				hub.recordAssignment("c-alice", now.Add(-age))
			}
			if got := hub.tierRateLimitReached("c-alice", tc.tier, now); got != tc.want {
				t.Fatalf("tierRateLimitReached = %+v, want %+v", got, tc.want)
			}
		})
	}

	// No server or config behind the hub: nothing to enforce, never a panic.
	bare := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(bare.Close)
	bare.recordAssignment("c-alice", now)
	if got := bare.tierRateLimitReached("c-alice", "contributor", now); got != (tierRateLimitHit{}) {
		t.Fatalf("hub without a server: tierRateLimitReached = %+v, want zero", got)
	}
	var none *ContributeWSHub
	if got := none.tierRateLimitReached("c-alice", "contributor", now); got != (tierRateLimitHit{}) {
		t.Fatalf("nil hub: tierRateLimitReached = %+v, want zero", got)
	}
}

// A relay whose trust tier the operator listed in hub.disabled_tiers is refused
// every fresh assignment with tier_disabled and every resume too, while its
// socket stays up. It must not keep the resident agent withheld; once the tier
// is re-enabled, the same connection covers again.
func TestRoleCoverage_DisabledTierDoesNotCover(t *testing.T) {
	hub, s := covK2Hub(t)
	now := time.Now()
	relay := coverageConn("alice", "scanner", now)
	hub.mu.Lock()
	hub.connections["alice"] = relay
	hub.mu.Unlock()

	s.deps.Config.Hub.DisabledTiers = []string{"newcomer", "contributor"}
	if got := selectionRefusal(t, hub, s, relay); got != taskUnavailableTierDisabled {
		t.Fatalf("setup: selection refused with %q, want %q", got, taskUnavailableTierDisabled)
	}
	if got := hub.resumeGateReason(relay); got != taskUnavailableTierDisabled {
		t.Fatalf("setup: resumeGateReason = %q, want %q", got, taskUnavailableTierDisabled)
	}
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("relay in a disabled tier counted as cover: RoleCoverage = %+v, want nil", got)
	}

	s.deps.Config.Hub.DisabledTiers = []string{"newcomer"}
	want := map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"alice"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("tier re-enabled: RoleCoverage = %+v, want %+v", got, want)
	}
}

// A revoked profile is refused a resume by the same gate. A connection that
// still carries one covers nothing.
func TestRoleCoverage_RevokedDoesNotCover(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	relay := coverageConn("alice", "scanner", now)
	relay.profile.TrustTier = contributorTierRevoked
	hub.mu.Lock()
	hub.connections["alice"] = relay
	hub.connections["bob"] = coverageConn("bob", "scanner", now)
	hub.mu.Unlock()

	if got := hub.resumeGateReason(relay); got != resumeRefusedAccessRevoked {
		t.Fatalf("setup: resumeGateReason = %q, want %q", got, resumeRefusedAccessRevoked)
	}
	want := map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"bob"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("revoked relay counted as cover: RoleCoverage = %+v, want %+v", got, want)
	}
}

// tierAccessGateReason is the rule resumeGateReason, selection's disabled-tier
// refusal and RoleCoverage share.
func TestTierAccessGateReason(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.Config.Hub.DisabledTiers = []string{"newcomer"}
	for tier, want := range map[string]string{
		"contributor":          "",
		"newcomer":             taskUnavailableTierDisabled,
		contributorTierRevoked: resumeRefusedAccessRevoked,
	} {
		if got := hub.tierAccessGateReason(tier); got != want {
			t.Errorf("tierAccessGateReason(%q) = %q, want %q", tier, got, want)
		}
	}
	var none *ContributeWSHub
	if got := none.tierAccessGateReason("newcomer"); got != "" {
		t.Errorf("nil hub: tierAccessGateReason = %q, want empty", got)
	}
	if got := none.tierAccessGateReason(contributorTierRevoked); got != resumeRefusedAccessRevoked {
		t.Errorf("nil hub: a revoked tier is still refused, got %q", got)
	}
}

// noWorkRelay connects a scanner relay over a real socket to a hub whose queue
// is empty, so every ready it sends is answered by real task selection.
func noWorkRelay(t *testing.T) (*ContributeWSHub, *Server, *ContributorConnection, *wsSession, func() map[string]any) {
	t.Helper()
	hub, s := covK2Hub(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{}
	s.statusMu.Unlock()
	server, client := wsPipe(t)
	relay := coverageConn("alice", "scanner", time.Now())
	relay.ws = server
	hub.mu.Lock()
	hub.connections["alice"] = relay
	hub.mu.Unlock()
	sess := &wsSession{h: hub, conn: server, contributor: relay}
	return hub, s, relay, sess, func() map[string]any { return readWire(t, client) }
}

// sendReady drives one ready through the real handler and returns the frame
// the relay received.
func sendReady(t *testing.T, sess *wsSession, read func() map[string]any) map[string]any {
	t.Helper()
	if stop := sess.handleReady(WSMessage{Type: "ready"}); stop {
		t.Fatal("ready must not stop the session")
	}
	return read()
}

// coverageAt evaluates RoleCoverage at a moment the relay's heartbeat is
// fresh, so only the idle run decides.
func coverageAt(hub *ContributeWSHub, relay *ContributorConnection, at time.Time) map[string]governor.ContributorCoverage {
	relay.mu.Lock()
	relay.lastPong = at
	relay.mu.Unlock()
	return hub.RoleCoverage(at)
}

func idleSince(relay *ContributorConnection) time.Time {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.lastIdleReasonSince
}

var scannerAlice = map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"alice"}}}

// One empty answer is what a relay gets between two tasks when the queue is
// momentarily dry. It must not drop the relay's cover.
func TestRoleCoverage_OneNoMatchingWorkAnswerStillCovers(t *testing.T) {
	hub, _, relay, sess, read := noWorkRelay(t)
	if f := sendReady(t, sess, read); f["type"] != "task_unavailable" || f["reason"] != taskUnavailableNoMatchingWork {
		t.Fatalf("setup: expected task_unavailable/%s, got %v", taskUnavailableNoMatchingWork, f)
	}
	since := idleSince(relay)
	if since.IsZero() {
		t.Fatal("the no_matching_work answer must start an idle run on the connection")
	}
	// The next ready, ~30 s on, is still inside the window.
	if got := coverageAt(hub, relay, since.Add(30*time.Second)); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("one empty answer dropped cover: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
}

// A relay answered no_matching_work on every ready for the whole window is
// connected and idle: it covers nothing. Observed on 2026-11-08 on
// hive-wild-mole, where such a relay kept the scanner lane "covered". A task
// assigned afterwards ends the run, so it covers again — while it works the
// task and after it finishes it.
func TestRoleCoverage_NoMatchingWorkForWindowDoesNotCover(t *testing.T) {
	hub, s, relay, sess, read := noWorkRelay(t)
	sendReady(t, sess, read)
	since := idleSince(relay)
	for i := 0; i < 3; i++ {
		if f := sendReady(t, sess, read); f["reason"] != taskUnavailableNoMatchingWork {
			t.Fatalf("setup: ready %d answered %v, want %s", i+2, f, taskUnavailableNoMatchingWork)
		}
	}
	if got := idleSince(relay); !got.Equal(since) {
		t.Fatalf("consecutive no_matching_work answers must extend one run: since moved %v -> %v", since, got)
	}
	if got := coverageAt(hub, relay, since.Add(coverageNoWorkWindow-time.Second)); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("inside the window: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
	later := since.Add(coverageNoWorkWindow)
	if got := coverageAt(hub, relay, later); got != nil {
		t.Fatalf("relay answered no_matching_work for the whole window counted as cover: RoleCoverage = %+v, want nil", got)
	}

	// Work arrives: the next ready is handed a task.
	oneActionableIssue(s)
	if f := sendReady(t, sess, read); f["type"] != "task_assign" {
		t.Fatalf("setup: expected task_assign once work exists, got %v", f)
	}
	if got := coverageAt(hub, relay, later); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("relay working an assigned task: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
	if got := idleSince(relay); !got.IsZero() {
		t.Fatalf("an assignment must end the idle run, since = %v", got)
	}
	relay.mu.Lock()
	relay.currentTask = nil // the task finished; no new ready yet
	relay.mu.Unlock()
	if got := coverageAt(hub, relay, later.Add(time.Hour)); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("relay between tasks after an assignment: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
}

// repo_unmintable is the name selectTask's re-scan gives no_matching_work when
// it dropped an unmintable repo on the way: nothing in the queue may be handed
// to this relay either way. It drops cover like no_matching_work does, and the
// two continue one run.
func TestRoleCoverage_RepoUnmintableBehavesLikeNoMatchingWork(t *testing.T) {
	hub, s, relay, sess, read := noWorkRelay(t)
	sendReady(t, sess, read) // no_matching_work: the queue is empty
	since := idleSince(relay)

	oneActionableIssue(s)
	hub.markRepoUnmintable("myorg/repo1", time.Now())
	for i := 0; i < 2; i++ {
		if f := sendReady(t, sess, read); f["type"] != "task_unavailable" || f["reason"] != taskUnavailableRepoUnmintable {
			t.Fatalf("setup: expected task_unavailable/%s, got %v", taskUnavailableRepoUnmintable, f)
		}
	}
	if got := idleSince(relay); !got.Equal(since) {
		t.Fatalf("repo_unmintable after no_matching_work must continue the run: since moved %v -> %v", since, got)
	}
	if got := coverageAt(hub, relay, since.Add(30*time.Second)); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("inside the window: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
	if got := coverageAt(hub, relay, since.Add(coverageNoWorkWindow)); got != nil {
		t.Fatalf("relay answered repo_unmintable for the whole window counted as cover: RoleCoverage = %+v, want nil", got)
	}

	// A run of repo_unmintable alone drops cover too.
	relay.mu.Lock()
	relay.clearIdleReasonLocked()
	relay.mu.Unlock()
	sendReady(t, sess, read)
	if got := coverageAt(hub, relay, idleSince(relay).Add(coverageNoWorkWindow)); got != nil {
		t.Fatalf("repo_unmintable-only run counted as cover: RoleCoverage = %+v, want nil", got)
	}
}

// A different answer ends the no-work run: a relay refused for another reason
// is judged by that reason's own rule, not by this window.
func TestRoleCoverage_OtherIdleReasonEndsNoWorkRun(t *testing.T) {
	_, _, relay, _, _ := noWorkRelay(t)
	start := time.Now()
	relay.mu.Lock()
	relay.noteIdleReasonLocked(taskUnavailableNoMatchingWork, start)
	relay.noteIdleReasonLocked(taskUnavailableHubNotReady, start.Add(time.Minute))
	relay.noteIdleReasonLocked(taskUnavailableNoMatchingWork, start.Add(90*time.Second))
	got := relay.lastIdleReasonSince
	relay.mu.Unlock()
	if !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("a run broken by another reason must restart, since = %v, want %v", got, start.Add(90*time.Second))
	}
}

// A dispatched fix-pr task is work too: it ends the idle run, and the relay
// covers while the fix runs and after it settles.
func TestRoleCoverage_FixTaskDispatchEndsNoWorkRun(t *testing.T) {
	hub, _, relay, sess, read := noWorkRelay(t)
	sendReady(t, sess, read)
	later := idleSince(relay).Add(coverageNoWorkWindow)
	if got := coverageAt(hub, relay, later); got != nil {
		t.Fatalf("setup: idle relay must not cover, got %+v", got)
	}

	relay.mu.Lock()
	relay.ws = nil // sendFixTask pushes only to a live socket; the frame is not under test
	relay.mu.Unlock()
	hub.sendFixTask(relay, "alice", &fixqueue.Dispatch{ID: "fix-1", Mode: fixqueue.ModeBoundary, Items: []fixqueue.Item{
		{Repo: "myorg/repo1", Number: 7, URL: "https://github.com/myorg/repo1/pull/7", Title: "red PR"},
	}}, true)
	if got := coverageAt(hub, relay, later); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("relay working a fix task: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
	relay.mu.Lock()
	delete(relay.fixTasks, "fix-1")
	relay.mu.Unlock()
	if got := coverageAt(hub, relay, later.Add(time.Hour)); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("relay after its fix settled: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
}

// A relay subscribed to several hives works one task at a time. Observed on
// 2026-11-09: a relay set "Acting as: scanner" on projectbluefin spent
// 14:25Z-14:32Z on hivecommons tasks and sent projectbluefin no ready. To that
// hub it was a connected, role-holding relay with a fresh heartbeat that had
// never been refused anything, which is exactly what the first assertion
// below counts as cover. A relay that never sends busy_elsewhere (an older
// one) is still read that way; one that reports the other hive's task covers
// nothing until it reports the task finished.
func TestRoleCoverage_BusyElsewhereRelayDoesNotCover(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	relay := coverageConn("Danathar", "scanner", now)
	hub.mu.Lock()
	hub.connections["relay"] = relay
	hub.mu.Unlock()
	session := &wsSession{h: hub, contributor: relay}

	want := map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"Danathar"}}}
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("a role-holding relay that has sent no ready and no report: RoleCoverage = %+v, want %+v (today's reading)", got, want)
	}

	session.handleBusyElsewhere(WSMessage{Type: "busy_elsewhere", State: "busy", Since: now.UTC().Format(time.RFC3339)})
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("relay busy on another hive counted as cover: RoleCoverage = %+v, want nil", got)
	}

	session.handleBusyElsewhere(WSMessage{Type: "busy_elsewhere", State: "clear"})
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the other hive's task ended: RoleCoverage = %+v, want %+v", got, want)
	}
}

// A relay busy on another hive that this hub has ALSO handed work (a primary
// task, or a fix-pr task) is working this lane too, and counts.
func TestRoleCoverage_BusyElsewhereWithThisHubsTaskStillCovers(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	relay := coverageConn("Danathar", "scanner", now)
	hub.mu.Lock()
	hub.connections["relay"] = relay
	hub.mu.Unlock()
	(&wsSession{h: hub, contributor: relay}).handleBusyElsewhere(WSMessage{Type: "busy_elsewhere", State: "busy"})
	want := map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"Danathar"}}}

	relay.mu.Lock()
	relay.currentTask = &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 7}
	relay.mu.Unlock()
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("busy elsewhere but holding this hub's task: RoleCoverage = %+v, want %+v", got, want)
	}

	relay.mu.Lock()
	relay.currentTask = nil
	relay.fixTasks = map[string]*WSTaskAssign{"fix-1": {TaskID: "fix-1", Repo: "myorg/repo1", Number: 8}}
	relay.mu.Unlock()
	if got := hub.RoleCoverage(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("busy elsewhere but holding this hub's fix-pr task: RoleCoverage = %+v, want %+v", got, want)
	}

	relay.mu.Lock()
	relay.fixTasks = nil
	relay.mu.Unlock()
	if got := hub.RoleCoverage(now); got != nil {
		t.Fatalf("busy elsewhere with nothing from this hub: RoleCoverage = %+v, want nil", got)
	}
}

// The hub keeps no wall-clock expiry on busy_elsewhere; instead the relay's
// next ready here clears it, because a relay asks only when it holds no task.
// So a lost "clear" frame cannot keep a relay that is back asking this hub out
// of coverage. Driven through the real ready handler.
func TestRoleCoverage_ReadyClearsBusyElsewhere(t *testing.T) {
	hub, _, relay, sess, read := noWorkRelay(t)
	sess.handleBusyElsewhere(WSMessage{Type: "busy_elsewhere", State: "busy"})
	if got := coverageAt(hub, relay, time.Now()); got != nil {
		t.Fatalf("setup: busy-elsewhere relay must not cover, got %+v", got)
	}

	if f := sendReady(t, sess, read); f["type"] != "task_unavailable" {
		t.Fatalf("setup: expected task_unavailable on an empty queue, got %v", f)
	}
	relay.mu.Lock()
	busy := relay.busyElsewhere
	relay.mu.Unlock()
	if busy {
		t.Fatal("a ready must clear busy_elsewhere")
	}
	if got := coverageAt(hub, relay, idleSince(relay).Add(30*time.Second)); !reflect.DeepEqual(got, scannerAlice) {
		t.Fatalf("relay asking this hub again: RoleCoverage = %+v, want %+v", got, scannerAlice)
	}
}

// The relay sends busy_elsewhere only to a hub that advertises the token, so
// the hub must advertise it or the fix never reaches the wire.
func TestServerCapabilities_AdvertisesBusyElsewhere(t *testing.T) {
	if !testContainsString(serverCapabilities(), capBusyElsewhereV1) {
		t.Fatalf("serverCapabilities() = %v, missing %q", serverCapabilities(), capBusyElsewhereV1)
	}
}
