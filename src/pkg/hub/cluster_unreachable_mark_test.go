package hub

import (
	"testing"
	"time"
)

// Covers the write side of the kubectl suppression breaker
// (markClusterUnreachable / markClusterReachable); the read side
// (clusterRecentlyUnreachable) is exercised elsewhere but the mutations that
// drive it had no coverage.
func TestMarkClusterUnreachableAndReachable(t *testing.T) {
	s := &HubServer{}

	// Empty cluster ID is a no-op and must not allocate the map.
	s.markClusterUnreachable("")
	if s.clusterUnreachableUntil != nil {
		t.Fatal("markClusterUnreachable(\"\") allocated the suppression map")
	}
	s.markClusterReachable("") // no-op on a nil map must not panic

	// Marking lazily allocates the map and starts a suppression window.
	s.markClusterUnreachable("spoke-a")
	if !s.clusterRecentlyUnreachable("spoke-a") {
		t.Fatal("spoke-a should be suppressed after markClusterUnreachable")
	}
	until, ok := s.clusterUnreachableUntil["spoke-a"]
	if !ok {
		t.Fatal("no suppression window recorded for spoke-a")
	}
	remaining := time.Until(until)
	if remaining <= 0 || remaining > clusterUnreachableTTL {
		t.Errorf("suppression window = %v, want within (0, %v]", remaining, clusterUnreachableTTL)
	}

	// Re-marking extends the window rather than erroring.
	s.clusterUnreachableUntil["spoke-a"] = time.Now().Add(time.Second)
	s.markClusterUnreachable("spoke-a")
	if time.Until(s.clusterUnreachableUntil["spoke-a"]) < clusterUnreachableTTL-time.Minute {
		t.Error("re-marking did not extend the suppression window")
	}

	// Recovery clears the suppression immediately — no TTL wait.
	s.markClusterReachable("spoke-a")
	if s.clusterRecentlyUnreachable("spoke-a") {
		t.Error("spoke-a still suppressed after markClusterReachable")
	}
	if _, ok := s.clusterUnreachableUntil["spoke-a"]; ok {
		t.Error("suppression entry not deleted by markClusterReachable")
	}

	// Clearing one cluster leaves others suppressed.
	s.markClusterUnreachable("spoke-b")
	s.markClusterUnreachable("spoke-c")
	s.markClusterReachable("spoke-b")
	if s.clusterRecentlyUnreachable("spoke-b") {
		t.Error("spoke-b should be reachable again")
	}
	if !s.clusterRecentlyUnreachable("spoke-c") {
		t.Error("spoke-c suppression must survive spoke-b's recovery")
	}
}
