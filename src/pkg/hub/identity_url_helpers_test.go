package hub

import (
	"log/slog"
	"strconv"
	"testing"
)

// TestAppIDString covers HiveIdentity.AppIDString, which renders the resolved
// app_id for the provisioning template. A zero AppID must render as "" (so an
// unknown App stays unset rather than becoming a literal "0" that fails auth),
// and any non-zero AppID renders as its base-10 string.
func TestAppIDString(t *testing.T) {
	cases := []struct {
		name  string
		appID int64
		want  string
	}{
		{"zero renders empty so the App stays unset", 0, ""},
		{"positive App ID renders as base-10", 123456, "123456"},
		{"single digit", 7, "7"},
		{"large App ID", 9876543210, strconv.FormatInt(9876543210, 10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := HiveIdentity{AppID: tc.appID}
			if got := id.AppIDString(); got != tc.want {
				t.Errorf("AppIDString() with AppID=%d = %q, want %q", tc.appID, got, tc.want)
			}
		})
	}
}

// TestClusterAPIURLForIdentity covers clusterAPIURLForIdentity: it returns the
// cluster's GitHubAPIURL trimmed of surrounding whitespace, and an empty string
// for an unknown cluster or one that declares no API URL. The caller reads an
// empty string as "no forge declared" and skips the hub-side guard entirely.
func TestClusterAPIURLForIdentity(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"pok-prod":   {ID: "pok-prod", GitHubAPIURL: "https://api.github.com"},
			"padded":     {ID: "padded", GitHubAPIURL: "  https://ghe.example/api/v3  "},
			"no-api-url": {ID: "no-api-url"},
		},
	}
	cases := []struct {
		name      string
		clusterID string
		want      string
	}{
		{"known cluster returns its API URL", "pok-prod", "https://api.github.com"},
		{"surrounding whitespace is trimmed", "padded", "https://ghe.example/api/v3"},
		{"declared but empty yields empty", "no-api-url", ""},
		{"unknown cluster yields empty", "does-not-exist", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterAPIURLForIdentity(s, tc.clusterID); got != tc.want {
				t.Errorf("clusterAPIURLForIdentity(%q) = %q, want %q", tc.clusterID, got, tc.want)
			}
		})
	}
}

// TestClusterForgeHost covers clusterForgeHost, which names the forge a cluster
// defaults to. A nil cluster and a cluster that declares neither URL both fall
// back to public github.com; otherwise the base URL wins over the API URL, each
// normalised to a bare host by forgeHostLabel.
func TestClusterForgeHost(t *testing.T) {
	cases := []struct {
		name string
		c    *ClusterConfig
		want string
	}{
		{"nil cluster falls back to public", nil, publicForgeHost},
		{"no URLs declared falls back to public", &ClusterConfig{ID: "bare"}, publicForgeHost},
		{
			"base URL wins and is normalised to a bare host",
			&ClusterConfig{GitHubBaseURL: "https://github.ibm.com", GitHubAPIURL: "https://api.other.example"},
			"github.ibm.com",
		},
		{
			"API URL used when no base URL, path stripped",
			&ClusterConfig{GitHubAPIURL: "https://github.ibm.com/api/v3"},
			"github.ibm.com",
		},
		{
			"public API URL normalises to github.com",
			&ClusterConfig{GitHubAPIURL: "https://api.github.com"},
			publicForgeHost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterForgeHost(tc.c); got != tc.want {
				t.Errorf("clusterForgeHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSanitizeRepoEntry covers sanitizeRepoEntry, which normalises a
// user-entered repo string to bare owner/repo: it trims whitespace, strips an
// http(s):// scheme, surrounding slashes and a trailing .git, drops a leading
// hostname, and keeps only the final two path segments.
func TestSanitizeRepoEntry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace-only stays empty", "   ", ""},
		{"plain owner/repo is unchanged", "hivecommons/hive", "hivecommons/hive"},
		{"https scheme is stripped", "https://github.com/hivecommons/hive", "hivecommons/hive"},
		{"http scheme is stripped", "http://github.com/hivecommons/hive", "hivecommons/hive"},
		{"trailing .git is removed", "hivecommons/hive.git", "hivecommons/hive"},
		{"surrounding slashes are trimmed", "/hivecommons/hive/", "hivecommons/hive"},
		{"leading GHE hostname is dropped", "github.ibm.com/org/repo", "org/repo"},
		{"deep path keeps only last two segments", "github.com/a/b/c/d", "c/d"},
		{"whitespace and scheme together", "  https://github.com/hivecommons/hive.git  ", "hivecommons/hive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRepoEntry(tc.in); got != tc.want {
				t.Errorf("sanitizeRepoEntry(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClusterBaseURLForIdentity covers clusterBaseURLForIdentity: it returns the
// cluster's GitHubBaseURL trimmed of surrounding whitespace, and an empty string
// for an unknown cluster or one that declares no base URL.
func TestClusterBaseURLForIdentity(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		clusters: map[string]ClusterConfig{
			"ghe":         {ID: "ghe", GitHubBaseURL: "https://ghe.example"},
			"padded":      {ID: "padded", GitHubBaseURL: "\thttps://ghe.example \n"},
			"no-base-url": {ID: "no-base-url"},
		},
	}
	cases := []struct {
		name      string
		clusterID string
		want      string
	}{
		{"known cluster returns its base URL", "ghe", "https://ghe.example"},
		{"surrounding whitespace is trimmed", "padded", "https://ghe.example"},
		{"declared but empty yields empty", "no-base-url", ""},
		{"unknown cluster yields empty", "does-not-exist", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterBaseURLForIdentity(s, tc.clusterID); got != tc.want {
				t.Errorf("clusterBaseURLForIdentity(%q) = %q, want %q", tc.clusterID, got, tc.want)
			}
		})
	}
}
