package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func ptrInt(v int) *int { return &v }

func freshHeartbeat() string { return time.Now().UTC().Format(time.RFC3339) }
func staleHeartbeat() string {
	return time.Now().Add(-maxHeartbeatAge - time.Minute).UTC().Format(time.RFC3339)
}

func TestClampFleetCount(t *testing.T) {
	tests := []struct {
		name string
		in   *int
		want *int
	}{
		{"nil stays nil", nil, nil},
		{"negative becomes nil", ptrInt(-5), nil},
		{"zero preserved", ptrInt(0), ptrInt(0)},
		{"normal preserved", ptrInt(42), ptrInt(42)},
		{"over max clamped", ptrInt(maxFleetCount + 1), ptrInt(maxFleetCount)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampFleetCount(tt.in)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("nil mismatch: got %v want %v", got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("got %d want %d", *got, *tt.want)
			}
			// Must not alias the caller's pointer.
			if got != nil && got == tt.in {
				t.Error("clampFleetCount returned aliased pointer")
			}
		})
	}
}

func TestComputeFleetStats(t *testing.T) {
	tests := []struct {
		name  string
		hives []RegistryEntry
		want  FleetStats
	}{
		{
			name: "aggregates public online hives, dedupes repos",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1", "r2"},
					PRsMerged90d: ptrInt(10), PRsRejected90d: ptrInt(2), CVEsClosed: ptrInt(1)},
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2", "a/r3"},
					PRsMerged90d: ptrInt(5), PRsRejected90d: ptrInt(1), CVEsClosed: ptrInt(3)},
			},
			// repos: a/r1, a/r2, a/r3 = 3 distinct; a/r2 reported twice + as "r2".
			want: FleetStats{ReposManaged: 3, PRsMerged: 15, PRsRejected: 3, CVEsClosed: 4, Hives: 2},
		},
		{
			name: "skips private and offline hives",
			hives: []RegistryEntry{
				{IsPublic: false, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(99)},
				{IsPublic: true, Online: false, Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(99)},
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r3"}, PRsMerged90d: ptrInt(7)},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 7, Hives: 1},
		},
		{
			name: "nil counts are not aggregated as zero",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}}, // all nil
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(4)},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 4, Hives: 2},
		},
		{
			name: "empty repo strings are skipped",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"", "r1", ""},
					PRsMerged90d: ptrInt(2)},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 2, Hives: 1},
		},
		{
			name:  "no hives yields empty",
			hives: nil,
			want:  FleetStats{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &HubServer{logger: slog.Default()}
			s.registry.Hives = tt.hives
			got := s.computeFleetStats()
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestHandleFleetStats(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(8), CVEsClosed: ptrInt(2)},
		// Stale (old heartbeat) — markStaleHives flips it offline, so excluded.
		{ID: "h2", IsPublic: true, Online: true, LastHeartbeat: staleHeartbeat(),
			Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(100)},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/fleet-stats", nil)
	s.handleFleetStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var fs FleetStats
	if err := json.Unmarshal(rec.Body.Bytes(), &fs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fs.PRsMerged != 8 {
		t.Errorf("PRsMerged = %d, want 8 (stale hive must be excluded)", fs.PRsMerged)
	}
	if fs.CVEsClosed != 2 {
		t.Errorf("CVEsClosed = %d, want 2", fs.CVEsClosed)
	}
	if fs.Hives != 1 {
		t.Errorf("Hives = %d, want 1", fs.Hives)
	}
	if fs.ReposManaged != 1 {
		t.Errorf("ReposManaged = %d, want 1", fs.ReposManaged)
	}
	if fs.UpdatedAt == "" {
		t.Error("UpdatedAt empty")
	}
}
