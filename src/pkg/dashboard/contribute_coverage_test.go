package dashboard

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

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
