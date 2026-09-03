package dashboard

import (
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// This file backs the spoke dashboard's "Forge App" tab: a read-only inventory
// of every forge App credential the spoke holds, plus the single editable
// active config. It NEVER returns or logs private key material — paths and
// fingerprints only.

// ForgeAppKey is one per-app-id private key held on the spoke's PVC
// (/data/gh-app-key-<appid>.pem). Fingerprint and path only — the key material
// itself must never leave the pod.
type ForgeAppKey struct {
	AppID       string `json:"app_id"`
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
}

// ForgeAppInventory is what only cmd/hive can supply (the key resolution and
// PVC scan live there): the key file the process would actually sign with, and
// every per-app-id key on disk. Injected via SetForgeAppInventoryFn, following
// the SetGitHubAppRecheckFn provider pattern, so pkg/dashboard never imports
// cmd code.
type ForgeAppInventory struct {
	// ActiveKeyFile is the resolved path of the key the active config signs
	// with ("" when none could be resolved).
	ActiveKeyFile string
	// HeldKeys lists every per-app-id key file on the PVC, fingerprints only.
	HeldKeys []ForgeAppKey
}

// SetForgeAppInventoryFn registers the cmd/hive-side provider for the Forge
// App tab. Called once at startup, before the HTTP server serves requests.
func (s *Server) SetForgeAppInventoryFn(fn func() ForgeAppInventory) {
	s.forgeAppInventoryFn = fn
}

// forgeActiveApp is the JSON shape of the active github config row.
type forgeActiveApp struct {
	AppID          int64  `json:"app_id"`
	AppSlug        string `json:"app_slug"`
	APIURL         string `json:"api_url"`
	BaseURL        string `json:"base_url"`
	InstallationID int64  `json:"installation_id"`
	KeyFile        string `json:"key_file"`
	KeyFingerprint string `json:"key_fingerprint"`
	// AuthState is the classified github.AppAuthState wire token, verbatim
	// ("not-installed", "wrong-installation", "key-invalid", ... or "" when
	// no classification has run). This is a debugging surface — do not
	// prettify it here.
	AuthState string `json:"auth_state"`
	// InstallURL is the server-built App install link
	// (config.AppInstallURL(): <resolved base>/github-apps/<slug>/installations/new
	// on GHE, /apps/<slug>/installations/new on github.com — "" only when a
	// GHE host outside the forge table has no configured slug). The tab must
	// use THIS rather than composing its own URL, so the banner and the tab
	// can never drift onto two URL schemes.
	InstallURL string `json:"install_url"`
	Forge      string `json:"forge"`
	Editable   bool   `json:"editable"`
}

type forgeAppsResponse struct {
	Active forgeActiveApp `json:"active"`
	// ReposForge is the forge host the spoke's configured repos live on,
	// derived from the configured base/api URL host (empty = github.com).
	ReposForge string        `json:"repos_forge"`
	HeldKeys   []ForgeAppKey `json:"held_keys"`
}

// normalizeForgeAppHost canonicalizes a forge host for comparison: trimmed,
// lowercased, and empty meaning public github.com — mirroring how the config
// layer treats an absent base/api URL.
func normalizeForgeAppHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "github.com"
	}
	return host
}

// forgeAppEditable reports whether an App config whose forge is appForge may
// be edited from this spoke's dashboard: ONLY when it matches the forge the
// spoke's repos live on. Everything else is displayed read-only — editing a
// credential for a forge this hive's repos do not use is never useful and is
// exactly how the wrong key ends up active.
func forgeAppEditable(appForge, reposForge string) bool {
	return normalizeForgeAppHost(appForge) == normalizeForgeAppHost(reposForge)
}

// handleConfigGitHubForgeApps serves GET /api/config/github/forge-apps.
func (s *Server) handleConfigGitHubForgeApps(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusServiceUnavailable)
		return
	}
	g := s.deps.Config.GitHub

	inv := ForgeAppInventory{}
	if s.forgeAppInventoryFn != nil {
		inv = s.forgeAppInventoryFn()
	}

	keyFile := inv.ActiveKeyFile
	if keyFile == "" {
		keyFile = g.KeyFile
	}
	fingerprint := ""
	if keyFile != "" {
		// A missing/unparseable key simply reports an empty fingerprint — the
		// tab's job is to show what is there, not to fail on what is not.
		if fp, err := config.AppKeyFingerprintFromFile(keyFile); err == nil {
			fingerprint = fp
		}
	}

	// The repos' forge comes from the configured base/api URL host (empty =
	// github.com): HostLabel() is the single authority on that derivation.
	// The App config's own forge honours an explicit `forge:` field first.
	reposForge := normalizeForgeAppHost(g.HostLabel())
	appForge := normalizeForgeAppHost(g.Forge())

	held := append([]ForgeAppKey(nil), inv.HeldKeys...)
	sort.Slice(held, func(i, j int) bool { return held[i].AppID < held[j].AppID })

	resp := forgeAppsResponse{
		Active: forgeActiveApp{
			AppID:          g.AppID,
			AppSlug:        g.ResolvedAppSlug(),
			APIURL:         g.ResolvedAPIURL(),
			BaseURL:        g.ResolvedBaseURL(),
			InstallationID: g.InstallationID,
			KeyFile:        keyFile,
			KeyFingerprint: fingerprint,
			AuthState:      s.GetGitHubAppState(),
			InstallURL:     g.AppInstallURL(),
			Forge:          appForge,
			Editable:       forgeAppEditable(appForge, reposForge),
		},
		ReposForge: reposForge,
		HeldKeys:   held,
	}
	jsonResponse(w, resp)
}
