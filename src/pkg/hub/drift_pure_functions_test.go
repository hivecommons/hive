package hub

import (
	"testing"
	"time"
)

// TestDriftEligibleForNorm verifies that only online non-placeholder hives
// contribute to the fleet norm.
func TestDriftEligibleForNorm(t *testing.T) {
	tests := []struct {
		name string
		h    MyHiveEntry
		want bool
	}{
		{
			name: "online claimed hive is eligible",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Online: true, Org: "myorg"}},
			want: true,
		},
		{
			name: "offline hive is not eligible",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Online: false, Org: "myorg"}},
			want: false,
		},
		{
			name: "online placeholder by provStatus is not eligible",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Online: true}, ProvStatus: statusAvailable},
			want: false,
		},
		{
			name: "online placeholder by org prefix is not eligible",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Online: true, Org: placeholderOrgPrefix + "slot1"}},
			want: false,
		},
		{
			name: "offline placeholder is not eligible",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Online: false}, ProvStatus: statusAvailable},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := driftEligibleForNorm(tt.h)
			if got != tt.want {
				t.Errorf("driftEligibleForNorm() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsPlaceholderEntryEdgeCases checks the two signals (provStatus, org prefix).
func TestIsPlaceholderEntryEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		h    MyHiveEntry
		want bool
	}{
		{
			name: "statusAvailable marks placeholder",
			h:    MyHiveEntry{ProvStatus: statusAvailable},
			want: true,
		},
		{
			name: "available- org prefix marks placeholder",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Org: "available-slot5"}},
			want: true,
		},
		{
			name: "claimed hive is not placeholder",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Org: "realorg", ProvStatus: "claimed"}},
			want: false,
		},
		{
			name: "empty provStatus and non-placeholder org is not placeholder",
			h:    MyHiveEntry{RegistryEntry: RegistryEntry{Org: "acme-corp"}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPlaceholderEntry(tt.h)
			if got != tt.want {
				t.Errorf("isPlaceholderEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestImageRefIsPinned exercises the pinning logic edge cases.
func TestImageRefIsPinned(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{name: "empty is not pinned", ref: "", want: false},
		{name: "digest pin is pinned", ref: "ghcr.io/hivecommons/hive@sha256:abc123", want: true},
		{name: "mutable tag is not pinned", ref: "ghcr.io/hivecommons/hive:v2-latest", want: false},
		{name: "immutable sha tag is pinned", ref: "ghcr.io/hivecommons/hive:abc1234", want: true},
		{name: "registry port is not confused as tag", ref: "registry:5000/org/repo", want: false},
		{name: "registry port with tag", ref: "registry:5000/org/repo:v1.0", want: true},
		{name: "whitespace trimmed", ref: "  ghcr.io/hivecommons/hive:abc123  ", want: true},
		{name: "branch-latest is mutable", ref: "ghcr.io/hivecommons/hive:main-latest", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := imageRefIsPinned(tt.ref)
			if got != tt.want {
				t.Errorf("imageRefIsPinned(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

// TestHealthStatusOf checks nil, missing, and normal status extraction.
func TestHealthStatusOf(t *testing.T) {
	tests := []struct {
		name   string
		health map[string]any
		want   string
	}{
		{name: "nil map returns unknown", health: nil, want: "unknown"},
		{name: "empty map returns unknown", health: map[string]any{}, want: "unknown"},
		{name: "non-string status returns unknown", health: map[string]any{"status": 42}, want: "unknown"},
		{name: "empty string status returns unknown", health: map[string]any{"status": ""}, want: "unknown"},
		{name: "healthy status", health: map[string]any{"status": "healthy"}, want: "healthy"},
		{name: "degraded status", health: map[string]any{"status": "degraded"}, want: "degraded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := healthStatusOf(tt.health)
			if got != tt.want {
				t.Errorf("healthStatusOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseRFC3339 checks valid and invalid timestamps.
func TestParseRFC3339(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOk  bool
		wantStr string // only checked if wantOk
	}{
		{name: "valid RFC3339", input: "2026-01-15T10:30:00Z", wantOk: true, wantStr: "2026-01-15T10:30:00Z"},
		{name: "valid with offset", input: "2026-01-15T10:30:00-05:00", wantOk: true},
		{name: "empty string", input: "", wantOk: false},
		{name: "garbage", input: "not-a-date", wantOk: false},
		{name: "date only", input: "2026-01-15", wantOk: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRFC3339(tt.input)
			if ok != tt.wantOk {
				t.Errorf("parseRFC3339(%q) ok = %v, want %v", tt.input, ok, tt.wantOk)
			}
			if tt.wantOk && tt.wantStr != "" {
				expected, _ := time.Parse(time.RFC3339, tt.wantStr)
				if !got.Equal(expected) {
					t.Errorf("parseRFC3339(%q) = %v, want %v", tt.input, got, expected)
				}
			}
		})
	}
}

// TestPluralize covers the simple pluralization helper.
func TestPluralize(t *testing.T) {
	tests := []struct {
		n    int
		one  string
		many string
		want string
	}{
		{1, "item", "items", "1 item"},
		{0, "item", "items", "0 items"},
		{5, "hive", "hives", "5 hives"},
		{-1, "item", "items", "-1 items"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := pluralize(tt.n, tt.one, tt.many)
			if got != tt.want {
				t.Errorf("pluralize(%d, %q, %q) = %q, want %q", tt.n, tt.one, tt.many, got, tt.want)
			}
		})
	}
}

// TestDriftFirstSeenKey verifies the composite key format.
func TestDriftFirstSeenKey(t *testing.T) {
	got := driftFirstSeenKey("hive-abc", "version-behind")
	if got == "" {
		t.Fatal("driftFirstSeenKey returned empty string")
	}
	// Key must contain both hive ID and kind to be unique.
	if got == driftFirstSeenKey("hive-abc", "image-pinned") {
		t.Error("different kinds must produce different keys")
	}
	if got == driftFirstSeenKey("hive-xyz", "version-behind") {
		t.Error("different hive IDs must produce different keys")
	}
}
