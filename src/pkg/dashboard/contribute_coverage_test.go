package dashboard

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/governor"
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
