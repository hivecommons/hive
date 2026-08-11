package hub

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerHeartbeatBearerOKSecurityContract(t *testing.T) {
	srv := &HubServer{hubSecret: "test-master-secret"}
	derived := srv.heartbeatKey()
	if derived == "" || derived == srv.hubSecret {
		t.Fatalf("derived heartbeat key = %q, want non-empty value distinct from raw secret", derived)
	}

	tests := []struct {
		name string
		auth string
		want bool
	}{
		{name: "legacy raw bearer", auth: "Bearer " + srv.hubSecret, want: true},
		{name: "derived heartbeat bearer", auth: "Bearer " + derived, want: true},
		{name: "missing header", auth: "", want: false},
		{name: "missing bearer scheme", auth: srv.hubSecret, want: false},
		{name: "lowercase scheme rejected", auth: "bearer " + srv.hubSecret, want: false},
		{name: "wrong token", auth: "Bearer wrong", want: false},
		{name: "extra leading token space", auth: "Bearer  " + srv.hubSecret, want: false},
		{name: "extra trailing token space", auth: "Bearer " + srv.hubSecret + " ", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := srv.heartbeatBearerOK(tt.auth, ""); got != tt.want {
				t.Errorf("heartbeatBearerOK(%q) = %v, want %v", tt.auth, got, tt.want)
			}
		})
	}
}

func TestServerHeartbeatBearerOKEmptySecretCurrentBehavior(t *testing.T) {
	srv := &HubServer{}
	if !srv.heartbeatBearerOK("Bearer ", "") {
		t.Fatal("heartbeatBearerOK with an empty hubSecret currently accepts an empty bearer; keep the caller-side guard in place")
	}
}

func TestServerIsPrivateURLSSRFGuard(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "loopback localhost", url: "https://localhost/api", want: true},
		{name: "loopback ipv4", url: "http://127.0.0.1:8080", want: true},
		{name: "link local metadata", url: "http://169.254.169.254/latest/meta-data", want: true},
		{name: "rfc1918 ten dot", url: "http://10.20.30.40", want: true},
		{name: "rfc1918172", url: "http://172.31.255.255", want: true},
		{name: "rfc1918 192", url: "http://192.168.1.10", want: true},
		{name: "ipv6 loopback", url: "http://[::1]:8080", want: true},
		{name: "ipv6 ula fails closed", url: "http://[fd00::1]", want: true},
		{name: "dot local fails closed", url: "http://printer.local/status", want: true},
		{name: "hostname resolves to loopback", url: "http://lvh.me/dashboard", want: true},
		{name: "public ipv4", url: "https://8.8.8.8/dns-query", want: false},
		{name: "public hostname", url: "https://example.com", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if got := isPrivateURL(ctx, tt.url); got != tt.want {
				t.Errorf("isPrivateURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestServerHubBannersPersistenceRoundTripAndPrune(t *testing.T) {
	orig := hubBannersPath
	hubBannersPath = filepath.Join(t.TempDir(), "hub-banners.json")
	t.Cleanup(func() { hubBannersPath = orig })

	now := time.Now().UTC()
	saved := &HubServer{logger: slog.Default(), hubBanners: map[string]*HubBannerEntry{
		"fresh": {ID: "fresh-banner", Message: "maintenance tonight", Color: "amber", SentAt: now.Format(time.RFC3339)},
		"stale": {ID: "stale-banner", Message: "old news", Color: "gray", SentAt: now.Add(-hubBannerMaxAge - time.Minute).Format(time.RFC3339)},
	}}
	saved.saveHubBanners()

	loaded := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	loaded.loadHubBanners()
	if len(loaded.hubBanners) != 1 {
		t.Fatalf("loaded %d banners, want only the fresh one: %+v", len(loaded.hubBanners), loaded.hubBanners)
	}
	got := loaded.hubBanners["fresh"]
	if got == nil {
		t.Fatalf("fresh banner was not restored: %+v", loaded.hubBanners)
	}
	if got.ID != "fresh-banner" || got.Message != "maintenance tonight" || got.Color != "amber" {
		t.Errorf("restored banner = %+v, want id/message/color preserved", got)
	}
	if _, ok := loaded.hubBanners["stale"]; ok {
		t.Fatalf("stale banner was not pruned: %+v", loaded.hubBanners)
	}

	reloaded := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	reloaded.loadHubBanners()
	if len(reloaded.hubBanners) != 1 || reloaded.hubBanners["fresh"] == nil {
		t.Errorf("pruned banner file did not persist, reloaded map = %+v", reloaded.hubBanners)
	}
}

func TestServerLoadHubBannersMissingAndInvalidFiles(t *testing.T) {
	orig := hubBannersPath
	hubBannersPath = filepath.Join(t.TempDir(), "hub-banners.json")
	t.Cleanup(func() { hubBannersPath = orig })

	srv := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	srv.loadHubBanners()
	if len(srv.hubBanners) != 0 {
		t.Fatalf("missing banner file loaded %+v, want empty map", srv.hubBanners)
	}

	if err := os.WriteFile(hubBannersPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid banner file: %v", err)
	}
	srv.hubBanners["existing"] = &HubBannerEntry{ID: "keep"}
	srv.loadHubBanners()
	if got := srv.hubBanners["existing"]; got == nil || got.ID != "keep" {
		t.Errorf("invalid persisted JSON should leave existing in-memory banners untouched, got %+v", srv.hubBanners)
	}
}

func TestServerHubBannersPersistenceErrorPaths(t *testing.T) {
	orig := hubBannersPath
	t.Cleanup(func() { hubBannersPath = orig })

	t.Run("save mkdir failure is non-fatal", func(t *testing.T) {
		parentFile := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parentFile, []byte("file"), 0o644); err != nil {
			t.Fatalf("write parent file: %v", err)
		}
		hubBannersPath = filepath.Join(parentFile, "hub-banners.json")
		srv := &HubServer{logger: slog.Default(), hubBanners: map[string]*HubBannerEntry{
			"hive": {ID: "banner", SentAt: time.Now().UTC().Format(time.RFC3339)},
		}}
		srv.saveHubBanners()
	})

	t.Run("save write failure is non-fatal", func(t *testing.T) {
		dir := t.TempDir()
		hubBannersPath = filepath.Join(dir, "hub-banners.json")
		if err := os.Mkdir(hubBannersPath+".tmp", 0o755); err != nil {
			t.Fatalf("create tmp-path directory: %v", err)
		}
		srv := &HubServer{logger: slog.Default(), hubBanners: map[string]*HubBannerEntry{
			"hive": {ID: "banner", SentAt: time.Now().UTC().Format(time.RFC3339)},
		}}
		srv.saveHubBanners()
	})

	t.Run("save rename failure is non-fatal", func(t *testing.T) {
		hubBannersPath = t.TempDir()
		srv := &HubServer{logger: slog.Default(), hubBanners: map[string]*HubBannerEntry{
			"hive": {ID: "banner", SentAt: time.Now().UTC().Format(time.RFC3339)},
		}}
		srv.saveHubBanners()
	})

	t.Run("load read failure is non-fatal", func(t *testing.T) {
		hubBannersPath = t.TempDir()
		srv := &HubServer{logger: slog.Default(), hubBanners: map[string]*HubBannerEntry{
			"existing": {ID: "keep"},
		}}
		srv.loadHubBanners()
		if got := srv.hubBanners["existing"]; got == nil || got.ID != "keep" {
			t.Errorf("read failure should leave in-memory banners untouched, got %+v", srv.hubBanners)
		}
	})
}

func TestServerComputeFleetStatsPriorityScenarios(t *testing.T) {
	count := func(v int) *int { return &v }
	now := time.Now()

	tests := []struct {
		name  string
		hives []RegistryEntry
		want  FleetStats
	}{
		{name: "empty fleet", want: FleetStats{}},
		{
			name: "mixed assigned available offline and private hives",
			hives: []RegistryEntry{
				{ID: "available", Org: placeholderOrgPrefix + "pool", Online: true, ProvStatus: statusAvailable, Repos: []string{"ignored"}, PRsMerged90d: count(99)},
				{ID: "offline", Org: "team", Online: false, Repos: []string{"offline"}, PRsMerged90d: count(99)},
				{ID: "private", Org: "team", Online: true, IsPublic: false, Repos: []string{"repo", "team/repo", "other/repo"}, AgentCount: 2, ContributorCount: 5, ActiveContributors: 1, PRsMerged90d: count(7), PRsRejected90d: count(2), CVEsClosed: count(1), FleetStatsCollectedAt: now},
				{ID: "repoless", Org: "team", Online: true, IsPublic: true, AgentCount: 1, ContributorCount: 3, ActiveContributors: 2},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 7, PRsRejected: 2, CVEsClosed: 1, Hives: 2, TotalHives: 3, AvailableHives: 1, AgentsRunning: 3, Contributors: 3, ContributorsTotal: 8, Reporting: 1, Eligible: 1},
		},
		{
			name: "stale count is excluded but eligible and visible",
			hives: []RegistryEntry{
				{ID: "fresh", Org: "team", Online: true, Repos: []string{"fresh"}, PRsMerged90d: count(4), FleetStatsCollectedAt: now},
				{ID: "stale", Org: "team", Online: true, Repos: []string{"stale"}, PRsMerged90d: count(100), FleetStatsCollectedAt: now.Add(-2 * fleetStatsMaxAge)},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 4, Hives: 2, TotalHives: 2, Reporting: 1, Eligible: 2, Stale: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &HubServer{logger: slog.Default()}
			srv.registry.Hives = tt.hives
			if got := srv.computeFleetStats(); got != tt.want {
				t.Errorf("computeFleetStats() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestServerPureSanitizersAndRepoHelpers(t *testing.T) {
	t.Run("heartbeat field strips unsafe characters", func(t *testing.T) {
		if got := sanitizeHeartbeatField("ok_1/branch.<script>alert(1)</script>\n"); got != "ok_1/branch.scriptalert1/script" {
			t.Errorf("sanitizeHeartbeatField returned %q", got)
		}
	})

	t.Run("prose field preserves readable text while dropping markup", func(t *testing.T) {
		got := sanitizeProseField("The GitHub App <b>failed</b> & needs `repair`.\nTry again.")
		want := "The GitHub App bfailed/b needs repair. Try again."
		if got != want {
			t.Errorf("sanitizeProseField() = %q, want %q", got, want)
		}
	})

	t.Run("image ref keeps OCI separators only", func(t *testing.T) {
		got := sanitizeImageRef(" ghcr.io/kubestellar/hive:v2-latest@sha256:abc<script>")
		want := "ghcr.io/kubestellar/hive:v2-latest@sha256:abcscript"
		if got != want {
			t.Errorf("sanitizeImageRef() = %q, want %q", got, want)
		}
	})

	t.Run("repo entry normalizes pasted URLs", func(t *testing.T) {
		tests := map[string]string{
			"https://github.ibm.com/org/repo.git": "org/repo",
			"github.com/org/team/repo":            "team/repo",
			" /already/owner/repo/ ":              "owner/repo",
			"single-repo":                         "single-repo",
		}
		for in, want := range tests {
			if got := sanitizeRepoEntry(in); got != want {
				t.Errorf("sanitizeRepoEntry(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("enterprise API URL derived only for non-public GitHub hosts", func(t *testing.T) {
		tests := map[string]string{
			"":               "",
			"github.com":     "",
			"api.github.com": "",
			"GitHub.IBM.com": "https://github.ibm.com/api/v3",
		}
		for in, want := range tests {
			if got := gheAPIURLForHost(in); got != want {
				t.Errorf("gheAPIURLForHost(%q) = %q, want %q", in, got, want)
			}
		}
	})
}
