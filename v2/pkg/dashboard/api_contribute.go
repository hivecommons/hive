package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const (
	defaultContributorsDir    = "/data/contributors"
	contributorAutoPromoteAt  = 5
	contributorTrustedAt      = 20
	defaultFederationRegistry = "/data/federation/registry.json"
)

func getContributorsDir() string {
	if v := os.Getenv("HIVE_CONTRIBUTORS_DIR"); v != "" {
		return v
	}
	return defaultContributorsDir
}

func getFederationRegistryPath() string {
	if v := os.Getenv("HIVE_FEDERATION_REGISTRY_PATH"); v != "" {
		return v
	}
	return defaultFederationRegistry
}

type ContributorProfile struct {
	GitHubUsername    string `json:"github_username"`
	ContributorID     string `json:"contributor_id"`
	RegistrationToken string `json:"registration_token"`
	TokenPlain        string `json:"registration_token_plain,omitempty"`
	TrustTier         string `json:"trust_tier"`
	PreferredRole     string `json:"preferred_role,omitempty"`
	CLIBackend        string `json:"cli_backend,omitempty"`
	Model             string `json:"model,omitempty"`
	AvatarURL         string `json:"avatar_url,omitempty"`
	RegisteredAt      string `json:"registered_at"`
	TasksCompleted    int    `json:"total_tasks_completed"`
	// TasksWithPR counts only completions that reported a pull request.
	// Auto-promotion reads this rather than TasksCompleted, so write access is
	// never granted for completions where nothing was shown to have shipped.
	TasksWithPR       int                   `json:"total_tasks_completed_with_pr"`
	TasksFailed       int                   `json:"total_tasks_failed"`
	LastActive        string                `json:"last_active,omitempty"`
	LastCompletedTask *WSTaskAssign         `json:"last_completed_task,omitempty"`
	RateLimits        ContributorRateLimits `json:"rate_limits"`
	Active            bool                  `json:"active,omitempty"`
	CurrentTask       *WSTaskAssign         `json:"current_task,omitempty"`
	ActiveTasks       []WSTaskAssign        `json:"active_tasks,omitempty"`
	Sessions          int                   `json:"sessions,omitempty"`
}

type ContributorRateLimits struct {
	MaxConcurrent int `json:"max_concurrent_tasks"`
	MaxPerHour    int `json:"max_tasks_per_hour"`
	MaxPerDay     int `json:"max_tasks_per_day"`
}

type ContributorPool struct {
	Active     int `json:"active"`
	Registered int `json:"registered"`
	mu         sync.RWMutex
}

var contributorPool = &ContributorPool{}

type ContributorPoolStatus struct {
	Active     int            `json:"active"`
	Registered int            `json:"registered"`
	ByRole     map[string]int `json:"by_role,omitempty"`
}

func (s *Server) BuildContributorPoolStatus() *ContributorPoolStatus {
	profiles := listContributorProfiles()
	active := 0
	var byRole map[string]int
	if s.contributeHub != nil {
		active = s.contributeHub.ActiveCount()
		byRole = s.contributeHub.RoleBreakdown()
	}
	return &ContributorPoolStatus{
		Active:     active,
		Registered: len(profiles),
		ByRole:     byRole,
	}
}

func (s *Server) registerContributeRoutes() {
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.mux.HandleFunc("GET /contribute", s.handleContributeLanding)
	s.mux.HandleFunc("GET /api/contribute/ws", s.contributeHub.HandleWS)
	s.mux.HandleFunc("POST /api/contribute/register", s.handleContributeRegister)
	s.mux.HandleFunc("POST /api/contribute/reissue-token", s.handleContributeReissueToken)
	s.mux.HandleFunc("GET /api/contribute/status", s.handleContributeStatus)
	s.mux.HandleFunc("GET /api/contribute/activity", s.handleContributeActivity)
	s.mux.HandleFunc("GET /api/contribute/fleet", s.handleContributeFleet)
	s.mux.HandleFunc("GET /api/contributors", s.handleContributorsList)
	s.mux.HandleFunc("GET /api/contributors/{id}", s.handleContributorGet)
	s.mux.HandleFunc("PUT /api/contributors/{id}/trust", s.handleContributorTrust)
	s.mux.HandleFunc("POST /api/contributors/{id}/revoke", s.handleContributorRevoke)
	s.mux.HandleFunc("DELETE /api/contributors/{id}", s.handleContributorDelete)

	s.mux.HandleFunc("GET /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("POST /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("GET /api/docs", s.handleAPIDocs)

	s.mux.HandleFunc("GET /leaderboard", s.handleLeaderboardPage)
	s.mux.HandleFunc("GET /api/leaderboard", s.handleLeaderboardAPI)

	s.mux.HandleFunc("GET /api/hives", s.handleHivesList)
	s.mux.HandleFunc("POST /api/hives/register", s.handleHivesRegister)
	s.mux.HandleFunc("POST /api/hives/{id}/heartbeat", s.handleHivesHeartbeat)
	s.mux.HandleFunc("DELETE /api/hives/{id}", s.handleHivesDelete)
	s.mux.HandleFunc("POST /api/hives/onboard", s.handleHivesOnboard)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ensureDir(dir string) {
	_ = os.MkdirAll(dir, 0o755)
}

func loadContributorProfile(username string) (*ContributorProfile, error) {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil, fmt.Errorf("invalid username")
	}
	data, err := os.ReadFile(filepath.Join(getContributorsDir(), username+".json"))
	if err != nil {
		return nil, err
	}
	var p ContributorProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func saveContributorProfile(p *ContributorProfile) error {
	if strings.Contains(p.GitHubUsername, "..") || strings.Contains(p.GitHubUsername, "/") || strings.Contains(p.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save")
	}
	ensureDir(getContributorsDir())
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(getContributorsDir(), p.GitHubUsername+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func listContributorProfiles() []ContributorProfile {
	ensureDir(getContributorsDir())
	entries, err := os.ReadDir(getContributorsDir())
	if err != nil {
		return nil
	}
	var profiles []ContributorProfile
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(getContributorsDir(), e.Name()))
		if err != nil {
			continue
		}
		var p ContributorProfile
		if json.Unmarshal(data, &p) == nil && p.GitHubUsername != "" && p.ContributorID != "" {
			profiles = append(profiles, p)
		}
	}
	return profiles
}

func createContributorProfile(username string) (*ContributorProfile, string) {
	cid := "c-" + randomHex(6)
	token := randomHex(32)
	p := &ContributorProfile{
		GitHubUsername:    username,
		ContributorID:     cid,
		RegistrationToken: sha256Hex(token),
		TokenPlain:        token,
		TrustTier:         "newcomer",
		RegisteredAt:      time.Now().UTC().Format(time.RFC3339),
		RateLimits: ContributorRateLimits{
			MaxConcurrent: 1,
			MaxPerHour:    3,
			MaxPerDay:     10,
		},
	}
	_ = saveContributorProfile(p)
	return p, token
}

func findContributor(id string) *ContributorProfile {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return nil
	}
	// Fast path: try direct file lookup by username (O(1) disk read)
	if p, err := loadContributorProfile(id); err == nil {
		return p
	}
	// Slow path: scan all profiles to match by contributor_id
	profiles := listContributorProfiles()
	for i := range profiles {
		if profiles[i].ContributorID == id {
			return &profiles[i]
		}
	}
	return nil
}

// ── Landing page ───────────────────────────────────────────────────────────

// handleContributeLanding renders the public sign-up page for ClankeR, the
// contributor relay: it explains the deal, offers per-CLI copy-paste setup
// commands, and shows a live feed of contributor activity.
func (s *Server) handleContributeLanding(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	projectName := ""
	if s.deps != nil && s.deps.Config != nil {
		projectName = s.deps.Config.Project.Name
	}
	projectName = html.EscapeString(projectName)
	if projectName == "" {
		projectName = "Hive"
	}

	// Count profiles by trust tier and active status
	activeCount := 0
	if s.contributeHub != nil {
		activeCount = s.contributeHub.ActiveCount()
	}
	tierCounts := map[string]int{
		"newcomer":    0,
		"contributor": 0,
		"trusted":     0,
		"advisor":     0,
		"revoked":     0,
	}
	for _, p := range profiles {
		tierCounts[p.TrustTier]++
	}

	// Build tier stat boxes HTML
	type tierStat struct {
		label string
		color string
		count int
	}
	tierStats := []tierStat{
		{"Active", "#3fb950", activeCount},
		{"Newcomer", "#d29922", tierCounts["newcomer"]},
		{"Contributor", "#58a6ff", tierCounts["contributor"]},
		{"Trusted", "#3fb950", tierCounts["trusted"]},
		{"Advisor", "#bc8cff", tierCounts["advisor"]},
		{"Revoked", "#f85149", tierCounts["revoked"]},
	}
	var tierBoxes strings.Builder
	for _, ts := range tierStats {
		fmt.Fprintf(&tierBoxes,
			`<div class="stat"><div class="stat-num" style="color:%s">%d</div><div class="stat-label">%s</div></div>`,
			ts.color, ts.count, ts.label)
	}

	wsProto := "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		wsProto = "wss"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	host = strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == ':' || c == '-' {
			return c
		}
		return -1
	}, host)
	hubURL := fmt.Sprintf("%s://%s/contribute", wsProto, host)

	// #2544: render the Contributor/Trusted trust-tier rows from the SAME constants
	// the promotion code uses (contributorAutoPromoteAt / contributorTrustedAt) so
	// the on-page numbers cannot drift from the code again, and word them to match
	// what the code actually does:
	//   - Auto-promotion (newcomer -> contributor) counts TasksWithPR — completions
	//     that REPORTED A PR — not bare completed tasks (see contribute_ws.go
	//     TasksWithPR >= contributorAutoPromoteAt). The old "5 completed tasks"
	//     over-promised.
	//   - "Trusted" is NOT auto-granted at 20: there is no code path that promotes
	//     to trusted on a task count. It is set by an operator via
	//     PUT /api/contributors/{id}/trust — the "maintainer voucher" in practice.
	//     contributorTrustedAt is the documented guideline threshold, so we phrase
	//     it as "~20 PR tasks, then granted by a maintainer" rather than implying an
	//     automatic unlock. Trusted's scoped token adds checks:read on top of the
	//     contributor scopes; the merge decision itself is still gated by the
	//     project's /approve + lgtm automation, so we do not claim "Merge PRs".
	tierTableRows := fmt.Sprintf(
		`<tr><td>Contributor</td><td>%d tasks that produced a PR</td><td>Create PRs, push code</td></tr>`+
			`<tr><td>Trusted</td><td>~%d PR tasks, then granted by a maintainer</td><td>Extra review scope (checks:read)</td></tr>`,
		contributorAutoPromoteAt, contributorTrustedAt,
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Contribute to %s</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;margin:0;min-height:100vh}
.page{display:flex;min-height:100vh;width:100%%}
.main{flex:3;padding:40px 48px;overflow-y:auto}
.sidebar{flex:1;background:#161b22;border-left:1px solid #30363d;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow-y:auto}
h1{font-size:2rem;margin-bottom:8px}
.subtitle{color:#8b949e;font-size:1.1rem;margin-bottom:32px}
.stat-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(80px,1fr));gap:10px;margin-bottom:24px}
.stat{background:#161b22;border:1px solid #30363d;border-radius:10px;padding:14px 8px;text-align:center}
.stat-num{font-size:1.5rem;font-weight:700;color:#58a6ff}
.stat-label{font-size:.7rem;color:#8b949e;margin-top:4px}
.steps{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:24px;margin-top:24px}
.steps h3{margin-top:0;color:#58a6ff}
.steps ol{padding-left:20px;line-height:2}
code{background:#0d1117;padding:2px 8px;border-radius:4px;font-size:.9rem}
.how{margin-top:32px}
.how h3{color:#e6edf3}
.how p{color:#8b949e;line-height:1.6}
.tier-table{width:100%%;border-collapse:collapse;margin-top:16px}
.tier-table th,.tier-table td{padding:8px 12px;text-align:left;border-bottom:1px solid #30363d;font-size:.85rem}
.tier-table th{color:#8b949e;font-weight:600}
.feed-header{padding:20px 20px 12px;border-bottom:1px solid #30363d;display:flex;align-items:center;gap:8px}
.feed-header h3{font-size:.95rem;color:#e6edf3}
.feed-dot{width:8px;height:8px;border-radius:50%%;background:#3fb950;animation:pulse 2s infinite}
@keyframes pulse{0%%,100%%{opacity:1}50%%{opacity:.4}}
.feed-count{font-size:.75rem;color:#8b949e;margin-left:auto}
.feed-scroll{flex:1;overflow-y:auto;padding:0}
.feed-entry{padding:10px 20px;border-bottom:1px solid #21262d;font-size:.85rem;animation:fadeIn .3s ease;display:flex;align-items:flex-start;gap:12px}
@keyframes fadeIn{from{opacity:0;transform:translateY(-4px)}to{opacity:1;transform:translateY(0)}}
.feed-entry:hover{background:rgba(88,166,255,.04)}
.feed-text{flex:1;min-width:0}
.feed-time{color:#8b949e;font-size:.75rem;white-space:nowrap;flex-shrink:0}
.feed-role{color:#58a6ff;font-weight:500}
.feed-cli{color:#8b949e;font-size:.8rem}
.feed-empty{padding:40px 20px;text-align:center;color:#8b949e;font-size:.85rem}
@media(max-width:768px){.page{flex-direction:column}.sidebar{border-left:none;border-top:1px solid #30363d;max-width:none;max-height:300px}}
/* Management & Operations tab chrome — additive, does not touch onboarding content */
.page-tabs{display:flex;gap:2px;background:#161b22;border-bottom:1px solid #30363d;padding:0 48px}
.page-tab{background:none;border:none;color:#8b949e;font-size:.95rem;font-weight:500;padding:14px 20px;cursor:pointer;border-bottom:2px solid transparent;font-family:inherit}
.page-tab:hover{color:#e6edf3}
.page-tab.active{color:#e6edf3;border-bottom-color:#58a6ff}
.tab-panel{display:none}
.tab-panel.active{display:block}
.ops{padding:40px 48px;overflow-y:auto}
.ops h1{font-size:1.7rem;margin-bottom:6px}
.ops-grid{display:grid;grid-template-columns:340px 1fr;gap:20px;margin-top:24px}
@media(max-width:900px){.ops-grid{grid-template-columns:1fr}}
.ops-card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:0;overflow:hidden}
.ops-card-head{padding:16px 20px;border-bottom:1px solid #30363d;display:flex;align-items:center;gap:10px}
.ops-card-head h3{font-size:.95rem;color:#e6edf3;margin:0}
.ops-card-count{font-size:.75rem;color:#8b949e;margin-left:auto}
.ops-filters{display:flex;gap:4px;padding:12px 20px;border-bottom:1px solid #21262d;flex-wrap:wrap}
.ops-filter{background:#0d1117;border:1px solid #30363d;color:#8b949e;font-size:.78rem;padding:4px 12px;border-radius:999px;cursor:pointer;font-family:inherit}
.ops-filter.active{background:#1f6feb;border-color:#1f6feb;color:#fff}
.work-list{max-height:520px;overflow-y:auto}
.work-item{padding:14px 20px;border-bottom:1px solid #21262d;cursor:pointer}
.work-item:hover{background:rgba(88,166,255,.04)}
.work-item.selected{background:rgba(88,166,255,.08)}
.work-repo{font-size:.75rem;color:#8b949e;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.work-title{font-size:.9rem;color:#e6edf3;margin:2px 0 6px}
.work-meta{display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:.75rem;color:#8b949e}
.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:.7rem;font-weight:600;border:1px solid transparent}
.pill-progress{background:rgba(88,166,255,.12);color:#58a6ff;border-color:rgba(88,166,255,.3)}
.pill-review{background:rgba(210,153,34,.12);color:#d29922;border-color:rgba(210,153,34,.3)}
.pill-passed{background:rgba(63,185,80,.12);color:#3fb950;border-color:rgba(63,185,80,.3)}
.pill-blocked{background:rgba(248,81,73,.12);color:#f85149;border-color:rgba(248,81,73,.3)}
.pill-idle{background:rgba(139,148,158,.12);color:#8b949e;border-color:rgba(139,148,158,.3)}
.clanker-row{display:flex;align-items:center;gap:10px;padding:12px 20px;border-bottom:1px solid #21262d}
.clanker-av{width:28px;height:28px;border-radius:50%%;flex-shrink:0;background:#30363d}
.clanker-main{flex:1;min-width:0}
.clanker-user{font-size:.88rem;color:#e6edf3;font-weight:500}
.clanker-sub{font-size:.74rem;color:#8b949e;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.clanker-dot{width:8px;height:8px;border-radius:50%%;background:#3fb950;flex-shrink:0}
.clanker-dot.stale{background:#8b949e}
.pipeline{display:flex;align-items:center;gap:6px;flex-wrap:wrap;margin:14px 0}
.pipe-node{background:#0d1117;border:1px solid #30363d;border-radius:8px;padding:8px 14px;font-size:.82rem;color:#e6edf3}
.pipe-node .lgtm{color:#3fb950;font-size:.72rem}
.pipe-arrow{color:#8b949e}
.policy-row{display:flex;justify-content:space-between;gap:12px;padding:8px 0;border-bottom:1px solid #21262d;font-size:.85rem}
.policy-row:last-child{border-bottom:none}
.policy-key{color:#8b949e}
.policy-val{color:#e6edf3;text-align:right;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-word}
.ops-empty{padding:32px 20px;text-align:center;color:#8b949e;font-size:.85rem}
.ops-note{color:#6e7681;font-size:.78rem;margin-top:12px;line-height:1.5}
.ops-note code{background:#0d1117;padding:1px 6px;border-radius:4px}
.prompt-preview{margin-top:10px;border-top:1px solid #21262d;padding-top:8px}
.prompt-preview summary{cursor:pointer;color:#58a6ff;font-size:.78rem;list-style:none}
.prompt-preview summary::-webkit-details-marker{display:none}
.prompt-preview summary::before{content:'\25B8 ';color:#8b949e}
.prompt-preview[open] summary::before{content:'\25BE '}
.prompt-labels{margin:8px 0 4px;display:flex;flex-wrap:wrap;gap:4px}
.prompt-text{margin-top:8px;background:#0d1117;border:1px solid #30363d;border-radius:8px;padding:12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.75rem;color:#c9d1d9;white-space:pre-wrap;word-break:break-word;max-height:220px;overflow-y:auto}
.prompt-preview .ops-note{margin-top:8px}
/* #2534 Operator admin controls — mirror the Governor Hub config controls into the
   Management & Operations tab. Owner/read-write only; a read viewer never sees them. */
.ops-admin{display:none}
.ops-admin.enabled{display:block}
.admin-badge{font-size:.68rem;font-weight:600;padding:2px 8px;border-radius:999px;background:rgba(210,153,34,.12);color:#d29922;border:1px solid rgba(210,153,34,.3);margin-left:auto}
.admin-body{padding:16px 20px}
.admin-toggle{display:flex;align-items:center;gap:10px;padding:8px 0}
.admin-switch{width:38px;height:20px;border-radius:999px;background:#30363d;position:relative;cursor:pointer;flex-shrink:0;transition:background .15s}
.admin-switch::after{content:'';position:absolute;top:2px;left:2px;width:16px;height:16px;border-radius:50%%;background:#e6edf3;transition:left .15s}
.admin-switch.on{background:#1f6feb}
.admin-switch.on.danger{background:#f85149}
.admin-switch.on::after{left:20px}
.admin-toggle-label{font-size:.85rem;color:#e6edf3}
.admin-toggle-sub{font-size:.74rem;color:#8b949e}
.admin-field{margin:14px 0}
.admin-field>label{display:block;font-size:.78rem;color:#8b949e;margin-bottom:6px}
.admin-modeseg{display:inline-flex;border:1px solid #30363d;border-radius:6px;overflow:hidden;margin-bottom:6px}
.admin-modeseg button{background:#0d1117;border:none;color:#8b949e;font-size:.72rem;padding:3px 10px;cursor:pointer;font-family:inherit}
.admin-modeseg button.on{background:#1f6feb;color:#fff}
.admin-chips{display:flex;flex-wrap:wrap;gap:4px;margin-bottom:6px}
.admin-chip{display:inline-flex;align-items:center;gap:4px;padding:2px 8px;border-radius:999px;font-size:.72rem;background:rgba(139,148,158,.12);color:#c9d1d9;border:1px solid #30363d}
.admin-chip .x{cursor:pointer;opacity:.7}
.admin-chip .x:hover{opacity:1;color:#f85149}
.admin-addrow{display:flex;gap:4px}
.admin-addrow input{flex:1;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e6edf3;font-size:.78rem;padding:5px 8px;font-family:inherit}
.admin-addrow button,.admin-save{background:#238636;border:1px solid #2ea043;color:#fff;font-size:.75rem;padding:5px 12px;border-radius:6px;cursor:pointer;font-family:inherit}
.admin-addrow button{background:#21262d;border-color:#30363d;color:#c9d1d9}
.admin-save{margin-top:8px}
.admin-save:disabled{opacity:.5;cursor:default}
.admin-hr{border:none;border-top:1px solid #21262d;margin:16px 0}
.admin-actions{display:flex;gap:6px;flex-wrap:wrap;margin-left:auto}
.admin-act{background:#21262d;border:1px solid #30363d;color:#c9d1d9;font-size:.7rem;padding:3px 9px;border-radius:6px;cursor:pointer;font-family:inherit}
.admin-act:hover{border-color:#8b949e}
.admin-act.danger:hover{border-color:#f85149;color:#f85149}
.admin-act select{background:#0d1117;border:1px solid #30363d;color:#c9d1d9;font-size:.7rem;border-radius:6px;padding:2px 4px;font-family:inherit}
.admin-modal-back{display:none;position:fixed;inset:0;background:rgba(1,4,9,.7);z-index:1000;align-items:center;justify-content:center}
.admin-modal-back.show{display:flex}
.admin-modal{background:#161b22;border:1px solid #30363d;border-radius:12px;max-width:420px;width:90%%;padding:22px}
.admin-modal h4{margin:0 0 8px;font-size:1rem;color:#e6edf3}
.admin-modal p{font-size:.85rem;color:#8b949e;line-height:1.5;margin:0 0 18px}
.admin-modal-btns{display:flex;gap:8px;justify-content:flex-end}
.admin-modal-btns button{font-size:.8rem;padding:6px 14px;border-radius:6px;cursor:pointer;font-family:inherit;border:1px solid #30363d;background:#21262d;color:#c9d1d9}
.admin-modal-btns button.confirm{background:#da3633;border-color:#f85149;color:#fff}
.admin-note{color:#6e7681;font-size:.76rem;margin-top:10px;line-height:1.5}
</style></head><body>
<div class="page-tabs" role="tablist">
<button class="page-tab active" role="tab" id="ptab-onboarding" aria-selected="true" data-panel="tab-onboarding">Onboarding</button>
<button class="page-tab" role="tab" id="ptab-manage" aria-selected="false" data-panel="tab-manage">Management</button>
<button class="page-tab" role="tab" id="ptab-ops" aria-selected="false" data-panel="tab-ops">Operations</button>
</div>
<div class="tab-panel active" id="tab-onboarding" role="tabpanel" aria-labelledby="ptab-onboarding">
<div class="page">
<div class="main">
<h1>🐝 Contribute to %s</h1>
<p class="subtitle">Donate your CLI + API tokens to help this project's AI agent swarm.</p>
<p class="subtitle" style="font-size:.95rem;margin-top:-24px;margin-bottom:32px">Powered by <strong style="color:#e6edf3">ClankeR</strong>, the contributor relay &mdash; it hands tasks from this hive's backlog to the agent running on your machine. Your compute, their backlog.</p>
<div class="stat-row">
<div class="stat"><div class="stat-num" style="color:#58a6ff">%d</div><div class="stat-label">Total</div></div>
%s
</div>
<div class="steps">
<h3>How it works</h3>
<div style="margin-bottom:16px;display:flex;align-items:center;gap:16px;flex-wrap:wrap">
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">OS:</label>
<select id="os-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="macos" selected>macOS</option>
<option value="linux">Linux</option>
<option value="windows">Windows</option>
</select>
</span>
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">Choose your CLI:</label>
<select id="cli-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="claude" data-install="npm i -g @anthropic-ai/claude-code" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="">Claude Code</option>
<option value="copilot" data-install="" data-host-install="npm install -g @github/copilot # uses your existing gh auth" data-model-flag="--model" data-default-model="">GitHub Copilot</option>
<option value="pi" data-install="" data-host-install="curl -fsSL https://pi.dev/install.sh | sh" data-model-flag="--model" data-default-model="">Pi</option>
<option value="goose" data-install="" data-host-install="# Install Goose: https://github.com/block/goose/releases\n# Install Ollama: https://ollama.com/download\nollama pull llama3.2:3b\nexport GOOSE_PROVIDER=ollama GOOSE_MODEL=llama3.2:3b" data-model-flag="" data-default-model="">Goose</option>
<option value="litellm" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# Your own LiteLLM proxy — exported locally, never sent to the hive\nexport HIVE_LITELLM_ENDPOINT=https://your-litellm-host:4000\nexport HIVE_LITELLM_API_KEY=sk-your-litellm-key  # only if your proxy needs one">LiteLLM (Claude Code + your proxy)</option>
<option value="openrouter" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# OpenRouter — Claude Code routed through OpenRouter\nexport HIVE_LITELLM_ENDPOINT=https://openrouter.ai/api/v1\nexport HIVE_LITELLM_API_KEY=sk-or-...  # your OpenRouter key">OpenRouter (Claude Code + your key)</option>
<option value="vllm" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# vLLM — self-hosted OpenAI-compatible server\nexport HIVE_LITELLM_ENDPOINT=http://your-vllm-host:8000/v1\nexport HIVE_LITELLM_API_KEY=sk-your-vllm-key  # only if your server needs one">vLLM (self-hosted)</option>
<option value="llm-d" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# llm-d — self-hosted OpenAI-compatible endpoint\nexport HIVE_LITELLM_ENDPOINT=http://your-llm-d-host:8000/v1\nexport HIVE_LITELLM_API_KEY=sk-your-llm-d-key  # only if your endpoint needs one">llm-d (self-hosted)</option>
<option value="bob" data-install="" data-host-install="curl -fsSL https://bob.ibm.com/download/bobshell.sh | bash" data-model-flag="" data-default-model="" data-env="# Bob (IBM bobshell) — get a key at https://bob.ibm.com (Scope: Inference).\n# Exported locally, never sent to the hive.\nexport BOBSHELL_API_KEY=your-bob-api-key">Bob</option>
<option value="other" data-install="" data-host-install="# Install your CLI tool" data-model-flag="" data-default-model="">Other (host only)</option>
</select>
</span>
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">Mode:</label>
<select id="mode-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="containerized">Containerized (recommended)</option>
<option value="host">Host (non-containerized)</option>
</select>
</span>
<span id="runtime-group" style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">Runtime:</label>
<select id="runtime-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="">Auto-detect</option>
<option value="docker">Docker</option>
<option value="podman">Podman</option>
</select>
</span>
</div>
<div id="model-row" style="margin-bottom:12px;display:none;align-items:center;gap:8px">
<label style="font-size:.9rem;color:#8b949e">Model (optional):</label>
<input id="model-input" type="text" placeholder="e.g. claude-sonnet-4-6, gpt-4o" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.85rem;flex:1;max-width:300px" oninput="updateCmds()">
</div>
<p style="color:#8b949e;margin-bottom:8px">Copy and paste these commands to get started:</p>
<div style="margin-top:16px;background:#0d1117;border:1px solid #30363d;border-radius:8px;padding:16px;position:relative">
<button id="copy-btn" style="position:absolute;top:8px;right:8px;background:#238636;color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:.75rem">Copy</button>
<pre id="copy-cmds" style="color:#e6edf3;font-size:.85rem;margin:0;overflow-x:auto;white-space:pre"># Default shown: macOS + Claude Code + containerized mode.
# Use the OS / CLI / Mode / Runtime selectors above to customize.
brew install just gh
git clone -b v2 https://github.com/kubestellar/hive && cd hive
export HIVE_HUB=%s
just contribute-setup claude
just contribute-hive</pre>
</div>
<script>
(function(){
var osSel=document.getElementById('os-select');
var sel=document.getElementById('cli-select');
var modeSel=document.getElementById('mode-select');
var runtimeSel=document.getElementById('runtime-select');
var runtimeGroup=document.getElementById('runtime-group');
var cmds=document.getElementById('copy-cmds');
var hubURL='%s';
// Prerequisite line (just + gh) per OS, using each project's own documented
// install method. macOS stays brew install just gh — the historical default.
var prereqByOS={
macos:'brew install just gh',
linux:'curl --proto \'=https\' --tlsv1.2 -sSf https://just.systems/install.sh | bash -s -- --to ~/.local/bin\n(type -p wget >/dev/null || (sudo apt update && sudo apt install wget -y)) && sudo mkdir -p -m 755 /etc/apt/keyrings && out=$(mktemp) && wget -nv -O$out https://cli.github.com/packages/githubcli-archive-keyring.gpg && cat $out | sudo tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null && sudo chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg && sudo mkdir -p -m 755 /etc/apt/sources.list.d && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null && sudo apt update && sudo apt install gh -y',
windows:'winget install --id Casey.Just --exact\nwinget install --id GitHub.cli'
};
var containerTpl='PREREQ\ngit clone -b v2 https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\njust contribute-setup CLI\njust contribute-hive';
var hostTpl='PREREQ\nINSTALL\ngit clone -b v2 https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\njust contribute-setup CLI\njust contribute-hive CLI local';
var modelRow=document.getElementById('model-row');
var modelInput=document.getElementById('model-input');
function updateCmds(){update();}
function update(){
var os=osSel.value;
var prereq=prereqByOS[os]||prereqByOS.macos;
var cli=sel.value;
var opt=sel.options[sel.selectedIndex];
var mode=modeSel.value;
var modelFlag=opt.getAttribute('data-model-flag')||'';
var model=(modelInput.value||'').trim();
if(cli==='other')mode='host';
if(mode==='containerized'&&cli==='other'){modeSel.value='host';mode='host';}
modelRow.style.display=(modelFlag||cli==='goose')?'flex':'none';
var modelLine='';
if(model){
if(cli==='goose'){modelLine='export GOOSE_MODEL='+model+'\n';}
else if(modelFlag){modelLine='export AGENT_MODEL='+model+'\n';}
}
var envLines=(opt.getAttribute('data-env')||'').replace(/\\n/g,'\n');
if(envLines)envLines+='\n';
// Runtime selector only applies to containerized mode; the export line is
// injected into the copy-paste commands so the choice is explicit.
var showRuntime=(mode==='containerized');
runtimeGroup.style.display=showRuntime?'inline-flex':'none';
var runtimeLine=(showRuntime&&runtimeSel.value)?'export HIVE_CONTAINER_RUNTIME='+runtimeSel.value+'\n':'';
var preLines=envLines+modelLine+runtimeLine;
var tpl,install;
if(mode==='host'){
tpl=hostTpl;
install=opt.getAttribute('data-host-install');
if(!install)install='# '+cli+' uses your existing gh auth';
cmds.textContent=tpl.replace('PREREQ',prereq).replace('INSTALL',install.replace(/\\n/g,'\n')).replace(/CLI/g,cli).replace('just contribute-setup',preLines+'just contribute-setup');
}else{
cmds.textContent=containerTpl.replace('PREREQ',prereq).replace(/CLI/g,cli).replace('just contribute-setup',preLines+'just contribute-setup');
}
}
osSel.addEventListener('change',update);
sel.addEventListener('change',function(){modelInput.value='';update();});
modeSel.addEventListener('change',update);
runtimeSel.addEventListener('change',update);
document.getElementById('copy-btn').addEventListener('click',function(){
var el=document.getElementById('copy-cmds');
var btn=document.getElementById('copy-btn');
var range=document.createRange();
range.selectNodeContents(el);
var sel=window.getSelection();
sel.removeAllRanges();
sel.addRange(range);
var ok=false;
try{ok=document.execCommand('copy')}catch(e){}
if(!ok&&navigator.clipboard){navigator.clipboard.writeText(el.textContent.trim()).catch(function(){});ok=true}
btn.textContent=ok?'Copied!':'Select + Cmd+C';
btn.style.background='#16a34a';
setTimeout(function(){btn.textContent='Copy';btn.style.background='#238636'},2000);
});
})();
</script>
</div>
<p style="color:#6e7681;font-size:.78rem;margin-top:8px">Containerized mode auto-detects docker, then podman &mdash; force one with <code>export HIVE_CONTAINER_RUNTIME=podman</code>. Rootless podman (keep-id, SELinux labels) is handled automatically.</p>
<p style="color:#6e7681;font-size:.78rem;margin-top:8px">Don't see your CLI? <a href="https://github.com/kubestellar/hive/issues/new?title=CLI+request:+&labels=enhancement" target="_blank" style="color:#58a6ff">Open an issue</a> and we'll add support for it.</p>
<div style="margin-top:20px;display:flex;gap:12px;flex-wrap:wrap">
<a href="/leaderboard" style="display:inline-block;padding:8px 20px;background:#161b22;border:1px solid #30363d;border-radius:8px;color:#58a6ff;text-decoration:none;font-size:.9rem">🏆 View Leaderboard</a>
</div>
<div class="how">
<h3>What you bring vs. what the hive provides</h3>
<p><strong>You bring:</strong> Your GitHub account + CLI API tokens. With LiteLLM you bring your own proxy instead — Claude Code on your machine talks directly to your endpoint, and the hive never sees your endpoint or key. Issues and PRs are created under YOUR name.</p>
<p><strong>The hive provides:</strong> Work queue, task assignment, and coordination &mdash; ClankeR carries each task to your agent over a secure WebSocket. Your credentials never leave your machine.</p>
</div>
<div class="how">
<h3>Trust tiers</h3>
<table class="tier-table">
<tr><th>Tier</th><th>Unlocked at</th><th>Can do</th></tr>
<tr><td>Newcomer</td><td>Registration</td><td>Comment on issues</td></tr>
%s
<tr><td>Advisor</td><td>Registration</td><td>Review agent PRs</td></tr>
</table>
</div>
</div>
<div class="sidebar">
<div class="feed-header">
<span class="feed-dot"></span>
<h3>Live Activity</h3>
<span class="feed-count" id="feed-count"></span>
</div>
<div class="feed-scroll" id="activity-feed">
<div class="feed-empty">Watching for contributors...</div>
</div>
</div>
</div>
</div>
<!-- Management tab — operator admin CONTROLS only. Split out of the former
     single "Management & Operations" tab so controls (this panel) and monitoring
     (the Operations panel below) live apart. The admin block below is moved here
     verbatim; nothing about its gating, IDs, or endpoints changed. -->
<div class="tab-panel" id="tab-manage" role="tabpanel" aria-labelledby="ptab-manage">
<div class="ops">
<h1>Management</h1>
<p class="subtitle" style="font-size:.95rem">Operator admin controls for the contributor (&ldquo;clanker&rdquo;) fleet, mirrored from the Governor Hub configuration. Owner &amp; read-write only &mdash; a read viewer sees no controls here. Live monitoring of the fleet lives under the <strong style="color:#e6edf3">Operations</strong> tab.</p>

<!-- #2534 Operator admin controls. Hidden by default; shown only after /api/role
     reports owner or read-write. These mirror the Governor Hub config section
     (Suspend Contributions + the admission filters) and write through the SAME
     endpoint the Governor dialog uses (PUT /api/config/governor/hub), plus the
     existing per-contributor endpoints. The Governor Hub tab stays the canonical
     editor — this is a mirror for the clanker-ops context. -->
<div class="ops-card ops-admin" id="ops-admin">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Operator admin controls</h3><span class="admin-badge" id="admin-role-badge"></span></div>
<div class="admin-body">
<p class="ops-note" style="margin-top:0">Mirrored from the Governor Hub configuration. Changes here write the same <code>Config.Hub.*</code> fields the Governor config dialog edits. Owner &amp; read-write only.</p>

<div class="admin-toggle">
<div class="admin-switch" id="admin-suspend-switch" data-key="contribute_suspended"></div>
<div><div class="admin-toggle-label">Suspend contributions</div><div class="admin-toggle-sub">Stop assigning tasks. Connected clankers stay online but idle.</div></div>
</div>
<div class="admin-toggle">
<div class="admin-switch" id="admin-skip-switch" data-key="contribute_skip_assigned_to_others"></div>
<div><div class="admin-toggle-label">Skip issues assigned to others</div><div class="admin-toggle-sub">Never serve an issue already assigned to a different GitHub user.</div></div>
</div>

<hr class="admin-hr">
<h3 style="font-size:.9rem;color:#e6edf3;margin:0 0 4px">Admission filters</h3>
<p class="ops-note" style="margin-top:0">The queue-shaping levers. Deny (default) skips matches; Allow serves only matches.</p>

<div class="admin-field" id="admin-filter-titles"></div>
<div class="admin-field" id="admin-filter-authors"></div>
<div class="admin-field" id="admin-filter-labels"></div>

<div class="admin-field">
<label>Allowed models <span style="color:#6e7681">— wildcards (*) and /regex/. Empty = allow all.</span></label>
<div class="admin-chips" id="admin-allow-models"></div>
<div class="admin-addrow"><input type="text" id="admin-allow-model-input" placeholder="e.g. claude-opus*, /gemini-\d/"><button type="button" id="admin-add-model">Add</button></div>
<div class="admin-toggle" style="padding-top:8px"><div class="admin-switch" id="admin-reject-switch" data-key="contribute_reject_unknown_models"></div><div class="admin-toggle-sub">Reject unknown models at connect time (only when the allowlist is non-empty).</div></div>
</div>

<button type="button" class="admin-save" id="admin-save-btn" disabled>Save filters</button>
<p class="admin-note" id="admin-save-hint">Suspend / skip toggles apply immediately. Filter edits apply on Save. Both persist through <code>PUT /api/config/governor/hub</code>.</p>
</div>
</div>
</div>
</div>
<!-- Operations tab — MONITORING. The Connected-clankers list (with its per-row
     trust / Revoke / Remove controls, still owner/read-write gated), My work
     queue, and the read-only Pipeline & policy panel. Split out of the former
     "Management & Operations" tab; the admin CONTROLS moved to the Management
     panel above, everything here stayed put. -->
<div class="tab-panel" id="tab-ops" role="tabpanel" aria-labelledby="ptab-ops">
<div class="ops">
<h1>Operations</h1>
<p class="subtitle" style="font-size:.95rem">A live view over the contributor (&ldquo;clanker&rdquo;) fleet and its in-flight work. The panels below surface what this hive already knows; the per-clanker trust / revoke / remove controls are owner &amp; read-write only. Admin controls (suspend, admission filters) live under the <strong style="color:#e6edf3">Management</strong> tab.</p>

<div class="ops-grid">
<div>
<div class="ops-card">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Connected clankers</h3><span class="ops-card-count" id="clanker-count"></span></div>
<div id="clanker-list"><div class="ops-empty">Loading fleet&hellip;</div></div>
</div>
<div class="ops-card" style="margin-top:20px">
<div class="ops-card-head"><h3>Pipeline &amp; policy</h3></div>
<div style="padding:16px 20px">
<div class="pipeline">
<span class="pipe-node">opened</span><span class="pipe-arrow">&rarr;</span>
<span class="pipe-node">review <span class="lgtm">[lgtm]</span></span><span class="pipe-arrow">&rarr;</span>
<span class="pipe-node">approved</span><span class="pipe-arrow">&rarr;</span>
<span class="pipe-node">merged</span>
</div>
<div id="policy-body"><div class="ops-empty">Loading policy&hellip;</div></div>
<p class="ops-note">Merge automation advances a PR when CI is green and a maintainer signals <code>/approve</code> or <code>lgtm</code>; a <code>do-not-merge</code> label blocks it. This panel displays the configured admission posture &mdash; it does not change it.</p>
</div>
</div>
</div>
<div>
<div class="ops-card">
<div class="ops-card-head"><h3>My work</h3><span class="ops-card-count" id="work-count"></span></div>
<div class="ops-filters" role="tablist">
<button class="ops-filter active" data-filter="all">All</button>
<button class="ops-filter" data-filter="active">Active</button>
<button class="ops-filter" data-filter="review">Review requests</button>
<button class="ops-filter" data-filter="done">Done</button>
</div>
<div class="work-list" id="work-list"><div class="ops-empty">Loading work&hellip;</div></div>
</div>
</div>
</div>
</div>
</div>
<script>
(function(){
// Tab switching for the /contribute page. Additive: leaves onboarding intact.
var tabs=document.querySelectorAll('.page-tab');
var panels=document.querySelectorAll('.tab-panel');
var opsStarted=false;   // Operations fleet polling started
var adminStarted=false; // /api/role gate resolved (adminEnabled set)
// The role gate now backs controls in BOTH tabs: the admin block under Management
// AND the per-clanker trust/revoke/remove buttons under Operations. So initAdmin()
// must run when EITHER tab is first opened — otherwise a viewer who lands straight
// on Operations would never resolve their role and would lose the per-row controls.
// It is idempotent (fetches /api/role once) and independent of opsPoll().
tabs.forEach(function(t){t.addEventListener('click',function(){
  tabs.forEach(function(x){x.classList.remove('active');x.setAttribute('aria-selected','false');});
  panels.forEach(function(p){p.classList.remove('active');});
  t.classList.add('active');t.setAttribute('aria-selected','true');
  var panel=document.getElementById(t.getAttribute('data-panel'));
  if(panel)panel.classList.add('active');
  var dp=t.getAttribute('data-panel');
  if((dp==='tab-ops'||dp==='tab-manage')&&!adminStarted){adminStarted=true;initAdmin();}
  if(dp==='tab-ops'&&!opsStarted){opsStarted=true;opsPoll();}
});});

var currentFilter='all';
var lastWork=[];
document.querySelectorAll('.ops-filter').forEach(function(f){f.addEventListener('click',function(){
  document.querySelectorAll('.ops-filter').forEach(function(x){x.classList.remove('active');});
  f.classList.add('active');
  currentFilter=f.getAttribute('data-filter');
  renderWork(lastWork);
});});

function esc(s){var d=document.createElement('div');d.textContent=(s==null?'':String(s));return d.innerHTML;}
function rel(ts){if(!ts)return '';var d=new Date(ts);if(isNaN(d))return '';var s=Math.floor((Date.now()-d.getTime())/1000);if(s<60)return s+'s ago';var m=Math.floor(s/60);if(m<60)return m+'m ago';var h=Math.floor(m/60);if(h<24)return h+'h ago';return Math.floor(h/24)+'d ago';}

// ── #2534 Operator admin controls (mirror of the Governor Hub config) ──────────
// adminEnabled gates everything: it is only ever set true after /api/role reports
// owner or read-write. A read viewer never sees a control, and the server enforces
// the same boundary independently (roleEnforcement blocks non-GET on
// /api/config/governor/hub for read; requireContributorWrite blocks the
// contributor endpoints), so hiding is UX, not the security boundary.
var adminEnabled=false;
var adminHub=null;      // last-loaded Config.Hub.* snapshot (contribute_* fields)
var adminDirty=false;   // filter edits pending Save

function toast(msg,ok){
  var t=document.createElement('div');
  t.textContent=msg;
  t.style.cssText='position:fixed;bottom:24px;left:50%%;transform:translateX(-50%%);z-index:1100;padding:10px 18px;border-radius:8px;font-size:.85rem;color:#fff;background:'+(ok===false?'#da3633':'#238636')+';box-shadow:0 4px 16px rgba(1,4,9,.5)';
  document.body.appendChild(t);
  setTimeout(function(){t.style.opacity='0';t.style.transition='opacity .4s';setTimeout(function(){t.remove();},400);},2600);
}

// Themed confirm — never native window.confirm (dashboard house rule).
var _confirmCb=null;
function adminConfirm(title,msg,okLabel,cb){
  document.getElementById('admin-confirm-title').textContent=title;
  document.getElementById('admin-confirm-msg').textContent=msg;
  var ok=document.getElementById('admin-confirm-ok');
  ok.textContent=okLabel||'Confirm';
  _confirmCb=cb;
  document.getElementById('admin-confirm-back').classList.add('show');
}
document.getElementById('admin-confirm-cancel').addEventListener('click',function(){document.getElementById('admin-confirm-back').classList.remove('show');_confirmCb=null;});
document.getElementById('admin-confirm-ok').addEventListener('click',function(){var cb=_confirmCb;document.getElementById('admin-confirm-back').classList.remove('show');_confirmCb=null;if(cb)cb();});

// Persist a subset of Config.Hub.* through the SAME endpoint the Governor Hub
// dialog uses. Only the passed keys are sent; the handler ignores omitted fields.
async function adminSaveHub(patch,okMsg){
  try{
    var res=await fetch('/api/config/governor/hub',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(patch)});
    if(!res.ok){
      var msg='Save failed ('+res.status+')';
      try{var d=await res.json();if(d&&d.error)msg=d.error;}catch(e){}
      toast(msg,false);return false;
    }
    toast(okMsg||'Saved',true);
    return true;
  }catch(e){toast('Save failed: '+(e&&e.message||'network error'),false);return false;}
}

function renderAdminFilter(fieldId,label,noun,modeKey,listKey){
  var el=document.getElementById(fieldId);
  if(!el||!adminHub)return;
  var mode=(adminHub[modeKey]==='allow')?'allow':'deny';
  var list=adminHub[listKey]||[];
  var chips=list.map(function(v){
    return '<span class="admin-chip">'+esc(v)+'<span class="x" data-list="'+listKey+'" data-val="'+esc(v)+'">&times;</span></span>';
  }).join('');
  el.innerHTML='<label>'+esc(label)+' filter</label>'+
    '<div class="admin-modeseg" data-mode-key="'+modeKey+'">'+
      '<button type="button" data-mode="deny"'+(mode==='deny'?' class="on"':'')+'>Deny</button>'+
      '<button type="button" data-mode="allow"'+(mode==='allow'?' class="on"':'')+'>Allow</button>'+
    '</div>'+
    '<div class="admin-chips">'+(chips||'<span class="admin-toggle-sub">none</span>')+'</div>'+
    '<div class="admin-addrow"><input type="text" data-add-list="'+listKey+'" placeholder="add '+esc(noun)+'&hellip;"><button type="button" data-add-list-btn="'+listKey+'">Add</button></div>';
}

function renderAdminModels(){
  var el=document.getElementById('admin-allow-models');
  if(!el||!adminHub)return;
  var list=adminHub.contribute_allow_models||[];
  el.innerHTML=list.length?list.map(function(v){
    return '<span class="admin-chip">'+esc(v)+'<span class="x" data-list="contribute_allow_models" data-val="'+esc(v)+'">&times;</span></span>';
  }).join(''):'<span class="admin-toggle-sub">all models accepted</span>';
}

function renderAdminControls(){
  if(!adminEnabled||!adminHub)return;
  // Immediate toggles.
  document.getElementById('admin-suspend-switch').classList.toggle('on',!!adminHub.contribute_suspended);
  document.getElementById('admin-suspend-switch').classList.toggle('danger',!!adminHub.contribute_suspended);
  document.getElementById('admin-skip-switch').classList.toggle('on',!!adminHub.contribute_skip_assigned_to_others);
  document.getElementById('admin-reject-switch').classList.toggle('on',!!adminHub.contribute_reject_unknown_models);
  // Filters (mirror Governor Hub: titles/authors/labels + modes, allow-models).
  renderAdminFilter('admin-filter-titles','Titles','title','contribute_titles_mode','contribute_deny_titles');
  renderAdminFilter('admin-filter-authors','Authors','author','contribute_authors_mode','contribute_deny_authors');
  renderAdminFilter('admin-filter-labels','Labels','label','contribute_labels_mode','contribute_deny_labels');
  renderAdminModels();
  var save=document.getElementById('admin-save-btn');
  if(save)save.disabled=!adminDirty;
}

// Immediate-apply toggle (suspend / skip): flips config and persists at once,
// like the Governor Hub toggle switches. Filters are the deferred-Save path.
function bindImmediateToggle(id){
  var sw=document.getElementById(id);
  if(!sw)return;
  sw.addEventListener('click',function(){
    var key=sw.getAttribute('data-key');
    var next=!(adminHub&&adminHub[key]);
    var patch={};patch[key]=next;
    adminSaveHub(patch,next?'Enabled '+key.replace(/_/g,' '):'Disabled '+key.replace(/_/g,' ')).then(function(ok){
      if(ok){adminHub[key]=next;renderAdminControls();}
    });
  });
}

// Delegated handlers for the filter editors (mode switch, add, remove) — mark
// dirty so nothing is sent until the operator clicks Save.
document.getElementById('ops-admin').addEventListener('click',function(e){
  var t=e.target;
  var seg=t.closest?t.closest('.admin-modeseg button'):null;
  if(seg){var mk=seg.parentNode.getAttribute('data-mode-key');adminHub[mk]=seg.getAttribute('data-mode');adminDirty=true;renderAdminControls();return;}
  if(t.classList&&t.classList.contains('x')&&t.getAttribute('data-list')){
    var lk=t.getAttribute('data-list'),val=t.getAttribute('data-val');
    adminHub[lk]=(adminHub[lk]||[]).filter(function(v){return v!==val;});
    adminDirty=true;renderAdminControls();return;
  }
  if(t.getAttribute&&t.getAttribute('data-add-list-btn')){
    var lk2=t.getAttribute('data-add-list-btn');
    var inp=document.querySelector('[data-add-list="'+lk2+'"]');
    if(inp&&inp.value.trim()){adminHub[lk2]=(adminHub[lk2]||[]).concat([inp.value.trim()]);inp.value='';adminDirty=true;renderAdminControls();}
    return;
  }
});

document.getElementById('admin-add-model').addEventListener('click',function(){
  var inp=document.getElementById('admin-allow-model-input');
  if(inp&&inp.value.trim()){adminHub.contribute_allow_models=(adminHub.contribute_allow_models||[]).concat([inp.value.trim()]);inp.value='';adminDirty=true;renderAdminControls();}
});

document.getElementById('admin-save-btn').addEventListener('click',function(){
  if(!adminDirty||!adminHub)return;
  var patch={
    contribute_titles_mode:adminHub.contribute_titles_mode||'deny',
    contribute_authors_mode:adminHub.contribute_authors_mode||'deny',
    contribute_labels_mode:adminHub.contribute_labels_mode||'deny',
    contribute_deny_titles:adminHub.contribute_deny_titles||[],
    contribute_deny_authors:adminHub.contribute_deny_authors||[],
    contribute_deny_labels:adminHub.contribute_deny_labels||[],
    contribute_allow_models:adminHub.contribute_allow_models||[]
  };
  adminSaveHub(patch,'Admission filters saved').then(function(ok){if(ok){adminDirty=false;renderAdminControls();}});
});

// Per-contributor actions (delegated on the clanker list). Each calls an EXISTING
// endpoint; destructive ones go through the themed confirm.
document.getElementById('clanker-list').addEventListener('change',function(e){
  var sel=e.target;
  if(!adminEnabled||sel.getAttribute('data-role')!=='tier')return;
  var cid=sel.getAttribute('data-cid'),tier=sel.value;
  fetch('/api/contributors/'+encodeURIComponent(cid)+'/trust',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({tier:tier})})
    .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
    .then(function(x){if(x.ok){toast('Trust tier set to '+tier,true);opsPoll();}else{toast((x.d&&x.d.error)||'Failed to set tier',false);}})
    .catch(function(){toast('Failed to set tier',false);});
});
document.getElementById('clanker-list').addEventListener('click',function(e){
  var b=e.target;
  if(!adminEnabled||b.tagName!=='BUTTON')return;
  var role=b.getAttribute('data-role');
  if(role!=='revoke'&&role!=='remove')return;
  var cid=b.getAttribute('data-cid'),user=b.getAttribute('data-user')||'this contributor';
  if(role==='revoke'){
    adminConfirm('Revoke '+user,'Set '+user+' to the revoked tier. Their agent stops receiving scoped tokens for new work. This uses the existing POST /api/contributors/{id}/revoke endpoint.','Revoke',function(){
      fetch('/api/contributors/'+encodeURIComponent(cid)+'/revoke',{method:'POST'})
        .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
        .then(function(x){if(x.ok){toast(user+' revoked',true);opsPoll();}else{toast((x.d&&x.d.error)||'Revoke failed',false);}})
        .catch(function(){toast('Revoke failed',false);});
    });
  }else{
    adminConfirm('Remove '+user,'Permanently delete '+user+'&rsquo;s contributor profile from this hive. This uses the existing DELETE /api/contributors/{id} endpoint and cannot be undone.','Remove',function(){
      fetch('/api/contributors/'+encodeURIComponent(cid),{method:'DELETE'})
        .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
        .then(function(x){if(x.ok){toast(user+' removed',true);opsPoll();}else{toast((x.d&&x.d.error)||'Remove failed',false);}})
        .catch(function(){toast('Remove failed',false);});
    });
  }
});

// Gate: only owner / read-write get the controls. Mirrors the main dashboard,
// which reads the viewer role from /api/role.
async function initAdmin(){
  var role='owner';
  try{var r=await fetch('/api/role');var d=await r.json();if(d&&d.role)role=d.role;}catch(e){}
  if(role!=='owner'&&role!=='read-write')return; // read viewer: controls stay hidden
  adminEnabled=true;
  var badge=document.getElementById('admin-role-badge');if(badge)badge.textContent=role;
  document.getElementById('ops-admin').classList.add('enabled');
  bindImmediateToggle('admin-suspend-switch');
  bindImmediateToggle('admin-skip-switch');
  bindImmediateToggle('admin-reject-switch');
  try{
    // The Governor config GET is what carries the hub.contribute_* fields the
    // Governor Hub dialog edits (GET /api/config, by contrast, is a thin summary
    // without them). Reading the same source keeps this mirror in lockstep.
    var cr=await fetch('/api/config/governor');var cd=await cr.json();
    adminHub=(cd&&cd.hub)?cd.hub:{};
  }catch(e){adminHub={};}
  renderAdminControls();
}

// #2546: human-readable label for the machine reason a clanker is idle. Keeps the
// raw reason as a fallback so a new server-side reason still renders legibly.
function idleReasonLabel(r){
  var m={contribution_suspended:'contribution suspended',hub_not_ready:'hub not ready',
    no_matching_work:'no matching work',token_mint_failed:'token mint failed',
    tier_disabled:'tier disabled',concurrency_limit:'concurrency limit'};
  return m[r]||String(r).replace(/_/g,' ');
}
function renderClankers(list){
  var el=document.getElementById('clanker-list');
  document.getElementById('clanker-count').textContent=(list.length)+(list.length===1?' connected':' connected');
  if(!list.length){el.innerHTML='<div class="ops-empty">No clankers connected right now.</div>';return;}
  el.innerHTML=list.map(function(c){
    var user=c.github_username||c.contributor_id||'clanker';
    var av=c.github_username?'<img class="clanker-av" src="https://github.com/'+esc(c.github_username)+'.png" alt="">':'<span class="clanker-av"></span>';
    var sub=[c.cli_backend,c.model,c.role,c.trust_tier].filter(Boolean).map(esc).join(' &middot; ');
    // #2546: when idle with a known reason, show "idle: no matching work" etc.
    var task=c.current_task
      ?('<div class="clanker-sub">on '+esc(c.current_task.repo)+'#'+esc(c.current_task.number)+'</div>')
      :(c.idle_reason?('<div class="clanker-sub">idle: '+esc(idleReasonLabel(c.idle_reason))+'</div>'):'');
    // #2534: owner/read-write get per-contributor admin actions wired to the
    // EXISTING endpoints — set trust tier / promote (PUT /api/contributors/{id}/trust),
    // revoke (POST .../revoke), remove (DELETE .../{id}). Hidden for read viewers.
    // The contributor id (not the username) keys those endpoints.
    var actions='';
    if(adminEnabled&&c.contributor_id){
      var cid=esc(c.contributor_id);
      var tier=c.trust_tier||'newcomer';
      var opts=['newcomer','contributor','trusted','advisor'].map(function(t){
        return '<option value="'+t+'"'+(t===tier?' selected':'')+'>'+t+'</option>';
      }).join('');
      actions='<div class="admin-actions">'+
        '<select class="admin-act" title="Set trust tier (maintainer voucher)" data-cid="'+cid+'" data-role="tier">'+opts+'</select>'+
        '<button type="button" class="admin-act danger" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="revoke">Revoke</button>'+
        '<button type="button" class="admin-act danger" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="remove">Remove</button>'+
        '</div>';
    }
    return '<div class="clanker-row"><span class="clanker-dot'+(c.stale?' stale':'')+'"></span>'+av+
      '<div class="clanker-main"><div class="clanker-user">'+esc(user)+'</div>'+
      '<div class="clanker-sub">'+(sub||'&mdash;')+'</div>'+task+'</div>'+
      (actions||('<span class="feed-time">'+esc(rel(c.connected_at))+'</span>'))+'</div>';
  }).join('');
}

function workMatchesFilter(w){
  if(currentFilter==='all')return true;
  if(currentFilter==='active')return w.status==='in-progress';
  if(currentFilter==='review')return w.status==='review';
  if(currentFilter==='done')return w.status==='done';
  return true;
}
function statusPill(s){
  if(s==='in-progress')return '<span class="pill pill-progress">in-progress</span>';
  if(s==='review')return '<span class="pill pill-review">review</span>';
  if(s==='done')return '<span class="pill pill-passed">done</span>';
  if(s==='blocked')return '<span class="pill pill-blocked">blocked</span>';
  return '<span class="pill pill-idle">'+esc(s)+'</span>';
}
function renderWork(list){
  lastWork=list;
  var shown=list.filter(workMatchesFilter);
  document.getElementById('work-count').textContent=shown.length+(shown.length===1?' item':' items');
  var el=document.getElementById('work-list');
  if(!shown.length){el.innerHTML='<div class="ops-empty">No work items in flight'+(currentFilter!=='all'?' for this filter.':'.')+'</div>';return;}
  el.innerHTML=shown.map(function(w){
    var who=w.github_username?('<span class="feed-role">'+esc(w.github_username)+'</span>'):'';
    var cli=w.cli_backend?(' &middot; '+esc(w.cli_backend)):'';
    // #2539: read-only prompt preview. Show the exact prompt the agent runs plus
    // task metadata (repo/number/title). The server never puts the github_token in
    // prompt_preview, so this can never leak the credential.
    var labels=(w.labels&&w.labels.length)?('<div class="prompt-labels">'+w.labels.map(function(l){return '<span class="pill pill-idle">'+esc(l)+'</span>';}).join(' ')+'</div>'):'';
    var preview=w.prompt_preview
      ?('<details class="prompt-preview"><summary>Prompt preview</summary>'+labels+
        '<pre class="prompt-text">'+esc(w.prompt_preview)+'</pre>'+
        '<p class="ops-note">Read-only. This is the instruction the agent receives; the scoped GitHub token is delivered separately and is never shown here.</p></details>')
      :'';
    return '<div class="work-item"><div class="work-repo">'+esc(w.repo||'')+(w.number?('#'+esc(w.number)):'')+'</div>'+
      '<div class="work-title">'+esc(w.title||'(untitled task)')+'</div>'+
      '<div class="work-meta">'+statusPill(w.status)+who+cli+'</div>'+preview+'</div>';
  }).join('');
}
function renderPolicy(p){
  var el=document.getElementById('policy-body');
  if(!p){el.innerHTML='<div class="ops-empty">Policy unavailable.</div>';return;}
  function list(a){return (a&&a.length)?a.map(esc).join(', '):'&mdash;';}
  var rows=[
    ['Contribute queue',p.suspended?'<span class="pill pill-blocked">suspended</span>':'<span class="pill pill-passed">active</span>'],
    ['Title filter',esc(p.titles_mode||'deny')+': '+list(p.deny_titles)],
    ['Author filter',esc(p.authors_mode||'deny')+': '+list(p.deny_authors)],
    ['Label filter',esc(p.labels_mode||'deny')+': '+list(p.labels_mode==='allow'?p.allow_labels:p.deny_labels)],
    ['Model allowlist',(p.reject_unknown_models?'strict &middot; ':'')+list(p.allow_models)],
    ['Skip assigned-to-others',p.skip_assigned_to_others?'yes':'no'],
    ['Disabled tiers',list(p.disabled_tiers)],
    ['Disabled repos',list(p.disabled_repos)],
    ['Auto-promote at',esc(p.auto_promote_at)+' tasks that produced a PR &rarr; contributor'],
    ['Trusted at','~'+esc(p.trusted_at)+' PR tasks, then granted by a maintainer']
  ];
  el.innerHTML=rows.map(function(r){return '<div class="policy-row"><span class="policy-key">'+r[0]+'</span><span class="policy-val">'+r[1]+'</span></div>';}).join('')+
    '<p class="ops-note">Promotion counts completions that reported a pull request (not bare completed tasks). Auto-promotion only lifts newcomer &rarr; contributor; the trusted tier is granted by an operator, not unlocked automatically.</p>';
}
async function opsPoll(){
  try{
    var res=await fetch('/api/contribute/fleet');
    var data=await res.json();
    renderClankers(data.clankers||[]);
    renderWork(data.work||[]);
    renderPolicy(data.policy);
  }catch(e){}
  if(document.getElementById('tab-ops').classList.contains('active'))setTimeout(opsPoll,4000);
}
})();
</script>
<script>
let prevCount=0;
async function poll(){try{
const[statusRes,actRes]=await Promise.all([fetch('/api/contribute/status'),fetch('/api/contribute/activity')]);
const status=await statusRes.json();
const act=await actRes.json();
document.getElementById('feed-count').textContent=(act.activity||[]).length+' events';
const f=document.getElementById('activity-feed');
if(!act.activity||!act.activity.length){f.innerHTML='<div class="feed-empty">No activity yet — be the first to contribute!</div>';return}
const newCount=act.activity.length;
const isNew=newCount>prevCount;
prevCount=newCount;
const html=act.activity.slice().reverse().map((e,i)=>{
const d=new Date(e.timestamp);const t=d.toLocaleTimeString([],{hour:'numeric',minute:'2-digit'});const tz=d.toLocaleTimeString([],{timeZoneName:'short'}).split(' ').pop();
const icons={joined:'🟢',left:'🔴','picked up':'🔧',completed:'✅',failed:'❌'};
const verbs={joined:'entered the hive',left:'left the hive','picked up':'picked up','completed':'completed','failed':'failed'};
const icon=icons[e.action]||'⚡';
const verb=verbs[e.action]||e.action;
const taskInfo=e.task?' <span class="feed-cli">'+e.task+'</span>':'';
const role=e.role?' as <span class="feed-role">'+e.role+'</span>':'';
const cliModel=e.cli?(e.model?' <span class="feed-cli">via '+e.cli+' CLI with '+e.model+'</span>':' <span class="feed-cli">via '+e.cli+' CLI</span>'):'';
return '<div class="feed-entry"'+(i===0&&isNew?' style="background:rgba(63,185,80,.08)"':'')+'>'+
'<div class="feed-text">'+icon+' <b>'+e.username+'</b> '+verb+taskInfo+role+cliModel+'</div>'+
'<span class="feed-time">'+t+' '+tz+'</span></div>'
}).join('');
if(f.innerHTML!==html){f.innerHTML=html;if(isNew)f.scrollTop=0;}
}catch(e){}}
poll();setInterval(poll,3000);
</script>
<!-- Themed confirm modal for the destructive admin actions (revoke / remove).
     The dashboard convention is a themed overlay, never native window.confirm. -->
<div class="admin-modal-back" id="admin-confirm-back">
<div class="admin-modal">
<h4 id="admin-confirm-title">Confirm</h4>
<p id="admin-confirm-msg"></p>
<div class="admin-modal-btns"><button type="button" id="admin-confirm-cancel">Cancel</button><button type="button" class="confirm" id="admin-confirm-ok">Confirm</button></div>
</div>
</div>
<div style="margin-top:40px;padding:16px 0;border-top:1px solid #30363d;font-size:.75rem;color:#8b949e;display:flex;align-items:center;gap:8px">
  <span id="hive-version">loading...</span>
</div>
<script>
fetch('/api/version').then(function(r){return r.json()}).then(function(d){
  var el=document.getElementById('hive-version');
  var dot=d.behind?'\u{1F7E1}':'\u{1F7E2}';
  el.innerHTML=dot+' Hive v'+d.version+' ('+d.short+')' + (d.behind?' · <span style="color:#d29922">update available</span>':' · up to date');
}).catch(function(){});
</script>
</body></html>`, projectName, projectName, len(profiles), tierBoxes.String(), hubURL, hubURL, tierTableRows)
}

// ── Registration ───────────────────────────────────────────────────────────

const maxRequestBodyBytes = 4096

func (s *Server) handleContributeRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GitHubUsername string `json:"github_username"`
		Force          bool   `json:"force"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.GitHubUsername)
	if username == "" || !isValidUsername(username) {
		jsonError(w, "Invalid github_username", http.StatusBadRequest)
		return
	}

	const maxContributors = 500
	if len(listContributorProfiles()) >= maxContributors {
		jsonError(w, "contributor registration full — contact the hive administrator", http.StatusServiceUnavailable)
		return
	}

	existing, _ := loadContributorProfile(username)
	if existing != nil {
		if existing.TrustTier == "revoked" {
			jsonError(w, "Account revoked — contact the hive administrator to reinstate", http.StatusForbidden)
			return
		}
		if req.Force {
			// Reissue token: generate a new one and invalidate the old
			newToken := reissueContributorToken(existing)
			s.logger.Info("contributor token reissued via force register", "username", username, "id", existing.ContributorID)
			jsonResponse(w, map[string]string{
				"contributor_id":     existing.ContributorID,
				"registration_token": newToken,
				"message":            "Token reissued — save this new token, it replaces the previous one",
			})
			return
		}
		jsonResponse(w, map[string]string{
			"contributor_id": existing.ContributorID,
			"message":        "Already registered — use force:true to reissue token, or POST /api/contribute/reissue-token with GitHub auth",
		})
		return
	}

	profile, token := createContributorProfile(username)
	s.logger.Info("contributor registered", "username", username, "id", profile.ContributorID)

	// Clear plaintext token from disk — only the hash is needed for auth
	profile.TokenPlain = ""
	_ = saveContributorProfile(profile)

	jsonResponse(w, map[string]string{
		"contributor_id":     profile.ContributorID,
		"registration_token": token,
		"message":            "Registered successfully — save this token, it cannot be recovered",
	})
}

// reissueContributorToken generates a new registration token for an existing
// contributor, invalidating the previous one. Returns the new plaintext token.
func reissueContributorToken(p *ContributorProfile) string {
	const tokenBytes = 32 // 256-bit token
	newToken := randomHex(tokenBytes)
	p.RegistrationToken = sha256Hex(newToken)
	p.TokenPlain = ""
	_ = saveContributorProfile(p)
	return newToken
}

// handleContributeReissueToken lets a contributor recover access by proving
// ownership of their GitHub identity. Requires Authorization: Bearer <gh-token>.
func (s *Server) handleContributeReissueToken(w http.ResponseWriter, r *http.Request) {
	// Authenticate via GitHub personal access token
	token := r.Header.Get("Authorization")
	if strings.HasPrefix(token, "Bearer ") {
		const bearerPrefixLen = 7 // len("Bearer ")
		token = token[bearerPrefixLen:]
	} else if strings.HasPrefix(token, "token ") {
		const tokenPrefixLen = 6 // len("token ")
		token = token[tokenPrefixLen:]
	} else {
		token = ""
	}

	username := validateGitHubToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-personal-access-token>"}`))
		return
	}

	profile, _ := loadContributorProfile(username)
	if profile == nil {
		jsonError(w, "Not registered as a contributor — register first via POST /api/contribute/register", http.StatusNotFound)
		return
	}
	if profile.TrustTier == "revoked" {
		jsonError(w, "Account revoked — contact the hive administrator to reinstate", http.StatusForbidden)
		return
	}

	newToken := reissueContributorToken(profile)
	s.logger.Info("contributor token reissued via GitHub auth", "username", username, "id", profile.ContributorID)

	jsonResponse(w, map[string]string{
		"contributor_id":     profile.ContributorID,
		"registration_token": newToken,
		"message":            "Token reissued — save this new token, it replaces the previous one",
	})
}

func (s *Server) handleContributeStatus(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	active := 0
	if s.contributeHub != nil {
		active = s.contributeHub.ActiveCount()
	}
	actionable := 0
	s.statusMu.RLock()
	if s.status != nil {
		for _, repo := range s.status.Repos {
			actionable += len(repo.ActionableIssues)
		}
	}
	s.statusMu.RUnlock()
	jsonResponse(w, map[string]any{
		"hub":                 "online",
		"active_contributors": active,
		"total_registered":    len(profiles),
		"actionable_items":    actionable,
	})
}

func (s *Server) handleContributeActivity(w http.ResponseWriter, r *http.Request) {
	if s.contributeHub == nil {
		jsonResponse(w, map[string]any{"activity": []any{}})
		return
	}
	jsonResponse(w, map[string]any{"activity": s.contributeHub.RecentActivity()})
}

// ContributeAdmissionPolicy is a read-only summary of the merge/automation
// posture and the contributor admission filters that ALREADY exist server-side.
// It is surfaced to the Management & Operations tab so an operator can read what
// is configured; it adds no controls and changes nothing.
type ContributeAdmissionPolicy struct {
	Suspended            bool     `json:"suspended"`
	TitlesMode           string   `json:"titles_mode,omitempty"`
	AuthorsMode          string   `json:"authors_mode,omitempty"`
	LabelsMode           string   `json:"labels_mode,omitempty"`
	DenyTitles           []string `json:"deny_titles,omitempty"`
	DenyAuthors          []string `json:"deny_authors,omitempty"`
	DenyLabels           []string `json:"deny_labels,omitempty"`
	AllowLabels          []string `json:"allow_labels,omitempty"`
	AllowModels          []string `json:"allow_models,omitempty"`
	RejectUnknownModels  bool     `json:"reject_unknown_models"`
	SkipAssignedToOthers bool     `json:"skip_assigned_to_others"`
	DisabledTiers        []string `json:"disabled_tiers,omitempty"`
	DisabledRepos        []string `json:"disabled_repos,omitempty"`
	AutoPromoteAt        int      `json:"auto_promote_at"`
	TrustedAt            int      `json:"trusted_at"`
}

// buildContributeAdmissionPolicy reads the configured contributor admission
// posture from the hub config. It never mutates config and returns a zero-value
// policy when config is unavailable.
func (s *Server) buildContributeAdmissionPolicy() ContributeAdmissionPolicy {
	p := ContributeAdmissionPolicy{
		AutoPromoteAt: contributorAutoPromoteAt,
		TrustedAt:     contributorTrustedAt,
	}
	if s.deps == nil || s.deps.Config == nil {
		return p
	}
	h := s.deps.Config.Hub
	p.Suspended = h.ContributeSuspended
	p.TitlesMode = h.ContributeTitlesMode
	p.AuthorsMode = h.ContributeAuthorsMode
	p.LabelsMode = h.ContributeLabelsMode
	p.DenyTitles = h.ContributeDenyTitles
	p.DenyAuthors = h.ContributeDenyAuthors
	p.DenyLabels = h.ContributeDenyLabels
	p.AllowLabels = h.ContributeAllowLabels
	p.AllowModels = h.ContributeAllowModels
	p.RejectUnknownModels = h.ContributeRejectUnknownModels
	p.SkipAssignedToOthers = h.ContributeSkipAssignedToOthers
	p.DisabledTiers = h.DisabledTiers
	p.DisabledRepos = h.DisabledRepos
	return p
}

// handleContributeFleet serves the read-only fleet/work/policy snapshot that the
// Management & Operations tab hydrates. GET only, no side effects: it surfaces
// the hub's live connection state and the already-configured admission policy.
func (s *Server) handleContributeFleet(w http.ResponseWriter, r *http.Request) {
	snap := FleetSnapshot{Clankers: []FleetClanker{}, Work: []FleetWorkItem{}}
	if s.contributeHub != nil {
		snap = s.contributeHub.FleetSnapshot()
	}
	jsonResponse(w, map[string]any{
		"clankers": snap.Clankers,
		"work":     snap.Work,
		"policy":   s.buildContributeAdmissionPolicy(),
	})
}

// ── Contributor management ─────────────────────────────────────────────────

// requireContributorWrite enforces owner/read-write authorization on the
// contributor mutation endpoints (trust/revoke/delete) that the Management &
// Operations tab surfaces as admin controls.
//
// These handlers live under the /api/contributors/... path. That path shares
// the "/api/contribute" prefix that roleEnforcement (server.go) intentionally
// EXEMPTS from its blanket read-only block — that exemption exists so a signed-in
// contributor can still register/onboard themselves. As a result the middleware
// does NOT stop a "read" viewer from calling these mutation endpoints, so each
// mutation handler must enforce the boundary itself. This mirrors the in-handler
// role check used by handleConfigDownload / handleSelfUpgrade: read X-Hive-Role,
// treat an absent header as owner (local/dev, no hub nginx), and reject "read".
// UI hiding on the ops tab is UX; this is the security boundary.
func (s *Server) requireContributorWrite(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role == "read" {
		jsonError(w, "your permissions on this hive are read-only, so changes are not allowed. Contact the owner of this hive to ask for write permissions.", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handleContributorsList(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	var liveStates map[string]ContributorLiveState
	if s.contributeHub != nil {
		liveStates = s.contributeHub.LiveStates()
	}
	for i := range profiles {
		profiles[i].TokenPlain = ""
		profiles[i].RegistrationToken = ""
		if ls, ok := liveStates[profiles[i].ContributorID]; ok {
			profiles[i].Active = ls.Active
			profiles[i].CurrentTask = ls.CurrentTask
			profiles[i].ActiveTasks = ls.Tasks
			profiles[i].Sessions = ls.Sessions
		}
	}
	jsonResponse(w, map[string]any{"contributors": profiles})
}

func (s *Server) handleContributorGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TokenPlain = ""
	p.RegistrationToken = ""
	jsonResponse(w, p)
}

func (s *Server) handleContributorTrust(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	validTiers := map[string]bool{"newcomer": true, "contributor": true, "trusted": true, "advisor": true, "revoked": true}
	if !validTiers[req.Tier] {
		jsonError(w, "Invalid tier", http.StatusBadRequest)
		return
	}
	p.TrustTier = req.Tier
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor tier changed", "username", p.GitHubUsername, "tier", req.Tier)
	jsonResponse(w, map[string]any{"ok": true, "trust_tier": req.Tier})
}

func (s *Server) handleContributorRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TrustTier = "revoked"
	_ = saveContributorProfile(p)
	s.logger.Info("contributor revoked", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *Server) handleContributorDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(getContributorsDir(), p.GitHubUsername+".json")
	if err := os.Remove(path); err != nil {
		jsonError(w, "Failed to delete", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor deleted", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true, "deleted": p.GitHubUsername})
}

// ── Federation registry ────────────────────────────────────────────────────

type FederationRegistry struct {
	Hives []FederationHive `json:"hives"`
}

type FederationHive struct {
	ID                 string `json:"id"`
	ProjectName        string `json:"project_name"`
	Org                string `json:"org"`
	HubURL             string `json:"hub_url"`
	DashboardURL       string `json:"dashboard_url,omitempty"`
	ActiveContributors int    `json:"active_contributors"`
	ActiveAgents       int    `json:"active_agents"`
	ActionableItems    int    `json:"actionable_items"`
	RegisteredAt       string `json:"registered_at"`
	LastHeartbeat      string `json:"last_heartbeat,omitempty"`
}

func loadFederationRegistry() *FederationRegistry {
	data, err := os.ReadFile(getFederationRegistryPath())
	if err != nil {
		return &FederationRegistry{}
	}
	var reg FederationRegistry
	if json.Unmarshal(data, &reg) != nil {
		return &FederationRegistry{}
	}
	return &reg
}

func saveFederationRegistry(reg *FederationRegistry) error {
	path := getFederationRegistryPath()
	ensureDir(filepath.Dir(path))
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *Server) handleHivesList(w http.ResponseWriter, r *http.Request) {
	reg := loadFederationRegistry()
	jsonResponse(w, reg)
}

func (s *Server) handleHivesRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req struct {
		ProjectName  string `json:"project_name"`
		Org          string `json:"org"`
		HubURL       string `json:"hub_url"`
		DashboardURL string `json:"dashboard_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.ProjectName == "" || req.Org == "" || req.HubURL == "" {
		jsonError(w, "project_name, org, and hub_url are required", http.StatusBadRequest)
		return
	}
	validURLScheme := func(u string) bool {
		return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") ||
			strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://")
	}
	if !validURLScheme(req.HubURL) {
		jsonError(w, "hub_url must start with http://, https://, ws://, or wss://", http.StatusBadRequest)
		return
	}
	if req.DashboardURL != "" && !validURLScheme(req.DashboardURL) {
		jsonError(w, "dashboard_url must start with http://, https://, ws://, or wss://", http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), req.HubURL) {
		jsonError(w, "hub_url must not target private/internal addresses", http.StatusBadRequest)
		return
	}
	if req.DashboardURL != "" && isPrivateURL(r.Context(), req.DashboardURL) {
		jsonError(w, "dashboard_url must not target private/internal addresses", http.StatusBadRequest)
		return
	}

	reg := loadFederationRegistry()
	const maxFederationHives = 100
	hiveID := fmt.Sprintf("hive-%s-%s", strings.ToLower(req.Org), strings.ToLower(req.ProjectName))
	for i := range reg.Hives {
		if reg.Hives[i].ID == hiveID {
			reg.Hives[i].HubURL = req.HubURL
			reg.Hives[i].DashboardURL = req.DashboardURL
			_ = saveFederationRegistry(reg)
			jsonResponse(w, map[string]any{"ok": true, "id": hiveID, "updated": true})
			return
		}
	}

	if len(reg.Hives) >= maxFederationHives {
		jsonError(w, "federation registry full", http.StatusServiceUnavailable)
		return
	}

	reg.Hives = append(reg.Hives, FederationHive{
		ID:           hiveID,
		ProjectName:  req.ProjectName,
		Org:          req.Org,
		HubURL:       req.HubURL,
		DashboardURL: req.DashboardURL,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = saveFederationRegistry(reg)
	s.logger.Info("hive registered", "id", hiveID)
	jsonResponse(w, map[string]any{"ok": true, "id": hiveID})
}

func (s *Server) handleHivesHeartbeat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	reg := loadFederationRegistry()
	var found *FederationHive
	for i := range reg.Hives {
		if reg.Hives[i].ID == id {
			found = &reg.Hives[i]
			break
		}
	}
	if found == nil {
		jsonError(w, "Hive not found", http.StatusNotFound)
		return
	}

	var req struct {
		ActiveContributors int `json:"active_contributors"`
		ActiveAgents       int `json:"active_agents"`
		ActionableItems    int `json:"actionable_items"`
	}
	const maxFedCount = 10000
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		if req.ActiveContributors >= 0 && req.ActiveContributors <= maxFedCount {
			found.ActiveContributors = req.ActiveContributors
		}
		if req.ActiveAgents >= 0 && req.ActiveAgents <= maxFedCount {
			found.ActiveAgents = req.ActiveAgents
		}
		if req.ActionableItems >= 0 && req.ActionableItems <= maxFedCount {
			found.ActionableItems = req.ActionableItems
		}
	}
	found.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
	_ = saveFederationRegistry(reg)
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *Server) handleHivesDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reg := loadFederationRegistry()
	for i := range reg.Hives {
		if reg.Hives[i].ID == id {
			reg.Hives = append(reg.Hives[:i], reg.Hives[i+1:]...)
			_ = saveFederationRegistry(reg)
			jsonResponse(w, map[string]any{"ok": true})
			return
		}
	}
	jsonError(w, "Hive not found", http.StatusNotFound)
}

func (s *Server) handleHivesOnboard(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req struct {
		ProjectName string   `json:"project_name"`
		Org         string   `json:"org"`
		Repos       []string `json:"repos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProjectName == "" || req.Org == "" || len(req.Repos) == 0 {
		jsonError(w, "project_name, org, and repos[] are required", http.StatusBadRequest)
		return
	}

	jsonResponse(w, map[string]any{
		"next_steps": []string{
			"1. Install the Hive GitHub App on your org",
			"2. Note the App ID and Installation ID",
			"3. Save the private key as /etc/hive/gh-app-key.pem",
			"4. Deploy with: docker compose up -d",
			"5. Register: POST /api/hives/register",
		},
	})
}

// ── Leaderboard ───────────────────────────────────────────────────────────

// LeaderboardEntry is the JSON shape returned by the leaderboard API.
type LeaderboardEntry struct {
	Rank           int    `json:"rank"`
	GitHubUsername string `json:"github_username"`
	AvatarURL      string `json:"avatar_url"`
	TrustTier      string `json:"trust_tier"`
	TasksCompleted int    `json:"tasks_completed"`
	TasksFailed    int    `json:"tasks_failed"`
	Findings       int    `json:"findings,omitempty"`
	RegisteredAt   string `json:"registered_at"`
	Active         bool   `json:"active,omitempty"`
	CurrentTask    string `json:"current_task,omitempty"`
	IsAgent        bool   `json:"is_agent,omitempty"`
	Emoji          string `json:"emoji,omitempty"`
}

// buildLeaderboard loads all contributor profiles, sorts by tasks completed
// descending, and returns ranked entries with secrets stripped.
func buildLeaderboard() []LeaderboardEntry {
	profiles := listContributorProfiles()
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].TasksCompleted > profiles[j].TasksCompleted
	})

	entries := make([]LeaderboardEntry, 0, len(profiles))
	rank := 0
	for _, p := range profiles {
		// Revoked contributors should not appear on the leaderboard.
		if p.TrustTier == "revoked" {
			continue
		}
		rank++
		entries = append(entries, LeaderboardEntry{
			Rank:           rank,
			GitHubUsername: p.GitHubUsername,
			AvatarURL:      fmt.Sprintf("https://github.com/%s.png", p.GitHubUsername),
			TrustTier:      p.TrustTier,
			TasksCompleted: p.TasksCompleted,
			TasksFailed:    p.TasksFailed,
			RegisteredAt:   p.RegisteredAt,
		})
	}
	return entries
}

func (s *Server) handleLeaderboardAPI(w http.ResponseWriter, _ *http.Request) {
	contributors := buildLeaderboard()
	agents := s.buildAgentLeaderboardEntries()
	jsonResponse(w, map[string]any{
		"leaderboard": contributors,
		"agents":      agents,
	})
}

func (s *Server) ContributorSummary() (registered, active int) {
	profiles := listContributorProfiles()
	registered = len(profiles)
	if s.contributeHub != nil {
		for _, ls := range s.contributeHub.LiveStates() {
			if ls.Active {
				active++
			}
		}
	}
	return
}

func (s *Server) LeaderboardForHub() []LeaderboardEntry {
	entries := buildLeaderboard()
	if s.contributeHub != nil {
		liveStates := s.contributeHub.LiveStates()
		profiles := listContributorProfiles()
		liveByUsername := make(map[string]ContributorLiveState)
		for _, p := range profiles {
			if ls, ok := liveStates[p.ContributorID]; ok {
				liveByUsername[p.GitHubUsername] = ls
			}
		}
		for i := range entries {
			if ls, ok := liveByUsername[entries[i].GitHubUsername]; ok {
				entries[i].Active = ls.Active
				if ls.CurrentTask != nil {
					entries[i].CurrentTask = ls.CurrentTask.Title
				}
			}
		}
	}
	agentEntries := s.buildAgentLeaderboardEntries()
	entries = append(agentEntries, entries...)
	for i := range entries {
		entries[i].Rank = i + 1
	}
	return entries
}

// trustTierColor maps trust tiers to CSS colour values for badges.
func trustTierColor(tier string) string {
	switch tier {
	case "newcomer":
		return "#8b949e"
	case "contributor":
		return "#3fb950"
	case "trusted":
		return "#d29922"
	case "advisor":
		return "#a371f7"
	case "revoked":
		return "#f85149"
	default:
		return "#8b949e"
	}
}

// trustTierBadgeCSS returns Tailwind-style bg/text/border CSS classes for a tier.
func trustTierBadgeCSS(tier string) (bg, text, border string) {
	switch tier {
	case "newcomer":
		return "rgba(107,114,128,0.2)", "#9ca3af", "rgba(107,114,128,0.3)"
	case "contributor":
		return "rgba(59,130,246,0.2)", "#60a5fa", "rgba(59,130,246,0.3)"
	case "trusted":
		return "rgba(34,197,94,0.2)", "#4ade80", "rgba(34,197,94,0.3)"
	case "advisor":
		return "rgba(168,85,247,0.2)", "#c084fc", "rgba(168,85,247,0.3)"
	case agentTierLabel:
		return "rgba(147,51,234,0.2)", "#a78bfa", "rgba(147,51,234,0.3)"
	case "revoked":
		return "rgba(239,68,68,0.2)", "#f87171", "rgba(239,68,68,0.3)"
	default:
		return "rgba(107,114,128,0.2)", "#9ca3af", "rgba(107,114,128,0.3)"
	}
}

// rankDisplay returns the medal emoji for top 3, or "#N" for others.
func rankDisplay(rank int) string {
	const goldMedal = "\U0001F947"   // gold medal emoji
	const silverMedal = "\U0001F948" // silver medal emoji
	const bronzeMedal = "\U0001F949" // bronze medal emoji
	switch rank {
	case 1:
		return fmt.Sprintf(`<span class="medal" title="1st place">%s</span>`, goldMedal)
	case 2:
		return fmt.Sprintf(`<span class="medal" title="2nd place">%s</span>`, silverMedal)
	case 3:
		return fmt.Sprintf(`<span class="medal" title="3rd place">%s</span>`, bronzeMedal)
	default:
		return fmt.Sprintf(`<span class="rank-num">#%d</span>`, rank)
	}
}

const (
	ghPRExternalRefPrefix    = "gh-"
	agentTierLabel           = "agent"
	agentAvatarURLTemplate   = "https://github.com/identicons/%s.png"
	leaderboardURLPathPrefix = "/leaderboard"
)

func (s *Server) buildAgentLeaderboardEntries() []LeaderboardEntry {
	if s.deps == nil || s.deps.AgentMgr == nil {
		return nil
	}

	agents := s.deps.AgentMgr.AllStatuses()
	entries := make([]LeaderboardEntry, 0, len(agents))

	for name, proc := range agents {
		if !proc.Config.Enabled {
			continue
		}

		prsOpened, issuesFixed, totalFindings := s.countAgentActivity(name)
		tasksCompleted := prsOpened + issuesFixed

		displayName := proc.Config.DisplayName
		if displayName == "" {
			displayName = name
		}

		emoji := proc.Config.Emoji
		if emoji == "" {
			emoji = "\U0001F916"
		}

		entries = append(entries, LeaderboardEntry{
			GitHubUsername: name,
			AvatarURL:      fmt.Sprintf(agentAvatarURLTemplate, name),
			TrustTier:      agentTierLabel,
			TasksCompleted: tasksCompleted,
			TasksFailed:    proc.RestartCount,
			Findings:       totalFindings,
			RegisteredAt:   "",
			IsAgent:        true,
			Emoji:          emoji,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TasksCompleted > entries[j].TasksCompleted
	})

	return entries
}

func (s *Server) countAgentActivity(agentName string) (prs, issues, findings int) {
	if s.deps == nil || s.deps.BeadStores == nil {
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		return
	}

	actor := agentName
	allBeads := store.List(beads.ListFilter{Actor: &actor})
	findings = len(allBeads)
	for _, b := range allBeads {
		if strings.HasPrefix(b.ExternalRef, ghPRExternalRefPrefix) {
			prs++
		}
		if b.Status == beads.StatusDone {
			issues++
		}
	}
	return
}

func (s *Server) handleLeaderboardPage(w http.ResponseWriter, _ *http.Request) {
	contributorEntries := buildLeaderboard()
	agentEntries := s.buildAgentLeaderboardEntries()

	projectName := ""
	if s.deps != nil && s.deps.Config != nil {
		if s.deps.Config.Project.Org != "" && s.deps.Config.Project.PrimaryRepo != "" {
			projectName = s.deps.Config.Project.Org + "/" + s.deps.Config.Project.PrimaryRepo
		} else if s.deps.Config.Project.Name != "" {
			projectName = s.deps.Config.Project.Name
		}
	}
	projectName = html.EscapeString(projectName)
	if projectName == "" {
		projectName = "Hive"
	}

	hiveURL := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveURL = html.EscapeString(s.deps.Config.HiveID)
	}

	rank := 0
	for i := range agentEntries {
		rank++
		agentEntries[i].Rank = rank
	}
	for i := range contributorEntries {
		rank++
		contributorEntries[i].Rank = rank
	}

	totalParticipants := len(agentEntries) + len(contributorEntries)

	agentJSON := leaderboardEntriesToJSON(agentEntries)
	contributorJSON := leaderboardEntriesToJSON(contributorEntries)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, leaderboardHTML,
		projectName, projectName, totalParticipants,
		projectName,
		projectName, hiveURL, hiveURL, projectName, hiveURL,
		agentJSON, contributorJSON,
	)
}

func leaderboardEntriesToJSON(entries []LeaderboardEntry) string {
	var buf strings.Builder
	buf.WriteString("[")
	for i, e := range entries {
		if i > 0 {
			buf.WriteString(",")
		}
		bg, text, border := trustTierBadgeCSS(e.TrustTier)
		emoji := strings.ReplaceAll(e.Emoji, `"`, `\"`)
		buf.WriteString(fmt.Sprintf(
			`{"rank":%d,"login":"%s","avatar":"%s","tier":"%s","completed":%d,"failed":%d,"findings":%d,"registered":"%s","tierBg":"%s","tierText":"%s","tierBorder":"%s","isAgent":%t,"emoji":"%s"}`,
			e.Rank, e.GitHubUsername, e.AvatarURL, e.TrustTier,
			e.TasksCompleted, e.TasksFailed, e.Findings, e.RegisteredAt,
			bg, text, border, e.IsAgent, emoji,
		))
	}
	buf.WriteString("]")
	return buf.String()
}

// leaderboardHTML is the full HTML template for the leaderboard page,
// styled to match the kubestellar.io/leaderboard design.
const leaderboardHTML = `<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s Leaderboard</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0a0a0a;color:#fff;min-height:100vh;overflow-x:hidden}

/* ── Starfield background ── */
.bg-stars{position:fixed;inset:0;z-index:0;background:#0a0a0a}
.bg-stars::before,.bg-stars::after{content:'';position:absolute;inset:0}
.bg-stars::before{background:radial-gradient(1px 1px at 20px 30px,rgba(255,255,255,0.3),transparent),
radial-gradient(1px 1px at 40px 70px,rgba(255,255,255,0.2),transparent),
radial-gradient(1px 1px at 50px 160px,rgba(255,255,255,0.3),transparent),
radial-gradient(1px 1px at 90px 40px,rgba(255,255,255,0.15),transparent),
radial-gradient(1px 1px at 130px 80px,rgba(255,255,255,0.25),transparent),
radial-gradient(1px 1px at 160px 120px,rgba(255,255,255,0.2),transparent);
background-size:200px 200px;animation:twinkle 4s ease-in-out infinite alternate}
.bg-stars::after{background:radial-gradient(1px 1px at 180px 50px,rgba(255,255,255,0.2),transparent),
radial-gradient(1px 1px at 60px 130px,rgba(255,255,255,0.15),transparent),
radial-gradient(1px 1px at 100px 90px,rgba(255,255,255,0.3),transparent),
radial-gradient(1px 1px at 140px 160px,rgba(255,255,255,0.2),transparent);
background-size:300px 300px;animation:twinkle 6s ease-in-out infinite alternate-reverse}
@keyframes twinkle{from{opacity:.5}to{opacity:1}}

/* ── Grid overlay ── */
.bg-grid{position:fixed;inset:0;z-index:1;
background-image:linear-gradient(rgba(255,255,255,0.02) 1px,transparent 1px),
linear-gradient(90deg,rgba(255,255,255,0.02) 1px,transparent 1px);
background-size:80px 80px;pointer-events:none}

.content{position:relative;z-index:10;padding-top:28px}

/* ── Header ── */
.header{text-align:center;padding:48px 16px 32px}
@media(min-width:640px){.header{padding:96px 24px 48px}}
.header h1{font-size:2.25rem;font-weight:700;margin-bottom:12px}
@media(min-width:768px){.header h1{font-size:3rem}}
@media(min-width:1024px){.header h1{font-size:3.75rem}}
.gradient-text{background:linear-gradient(90deg,#9333ea,#3b82f6,#9333ea);
background-size:200%% auto;-webkit-background-clip:text;-webkit-text-fill-color:transparent;
background-clip:text;animation:gradient-shift 3s linear infinite}
@keyframes gradient-shift{from{background-position:0%% center}to{background-position:200%% center}}
.header .subtitle{font-size:1.125rem;color:#d1d5db;max-width:640px;margin:0 auto;line-height:1.6}
@media(min-width:768px){.header .subtitle{font-size:1.5rem}}
.header .meta{margin-top:12px;font-size:.875rem;color:#6b7280}
.header .meta a{color:#9ca3af;text-decoration:none;transition:color .2s}
.header .meta a:hover{color:#60a5fa}
.header .contribute-link{margin-top:24px;display:inline-flex;align-items:center;gap:8px;
padding:8px 16px;border-radius:8px;border:1px solid rgba(245,158,11,0.3);
background:rgba(245,158,11,0.1);color:#fcd34d;font-size:.875rem;text-decoration:none;
transition:background .2s}
.header .contribute-link:hover{background:rgba(245,158,11,0.2)}

/* ── Search ── */
.search-wrap{max-width:448px;margin:0 auto 24px;padding:0 16px}
.search-wrap input{width:100%%;padding:10px 16px;background:rgba(31,41,55,0.6);
backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border:1px solid rgba(255,255,255,0.1);border-radius:8px;color:#fff;font-size:.875rem;
outline:none;transition:border-color .2s,box-shadow .2s}
.search-wrap input::placeholder{color:#6b7280}
.search-wrap input:focus{border-color:rgba(59,130,246,0.5);box-shadow:0 0 0 3px rgba(59,130,246,0.2)}

/* ── Table wrapper ── */
.table-section{max-width:960px;margin:0 auto;padding:0 16px 48px}
.table-wrap{background:rgba(31,41,55,0.4);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border-radius:12px;border:1px solid rgba(255,255,255,0.1);overflow:visible}

/* ── Table header ── */
.table-header{display:none;padding:12px 24px;border-bottom:1px solid rgba(255,255,255,0.05);
font-size:.75rem;color:#6b7280;text-transform:uppercase;letter-spacing:.05em}
@media(min-width:640px){.table-header{display:grid;grid-template-columns:1fr 100px 140px 80px;gap:16px}}
.table-header .sortable{cursor:pointer;transition:color .2s;user-select:none}
.table-header .sortable:hover{color:#fff}
.table-header .sortable.active{color:#facc15}

/* ── Row ── */
.row{display:grid;grid-template-columns:1fr;gap:8px;padding:16px;
border-bottom:1px solid rgba(255,255,255,0.05);transition:background .15s;align-items:center}
@media(min-width:640px){.row{grid-template-columns:1fr 100px 140px 80px;gap:16px;padding:16px 24px}}
.row:last-child{border-bottom:none}
.row:hover{background:rgba(255,255,255,0.02)}

/* ── Rank ── */
.rank-cell{display:flex;justify-content:center}
.medal{font-size:1.25rem}
.rank-num{font-size:.875rem;color:#9ca3af;font-variant-numeric:tabular-nums}

/* ── Contributor ── */
.contributor{display:flex;align-items:center;gap:12px}
.contributor img{width:32px;height:32px;border-radius:50%%;flex-shrink:0}
.contributor .name{font-size:.875rem;font-weight:500;color:#fff;text-decoration:none;transition:color .2s}
.contributor .name:hover{color:#60a5fa}
.contributor .gh-icon{color:#4b5563;transition:color .2s;flex-shrink:0}
.contributor .gh-icon:hover{color:#9ca3af}
.contributor .gh-icon svg{width:14px;height:14px}

/* ── Trust tier badge ── */
.tier-badge{display:inline-flex;align-items:center;padding:2px 8px;border-radius:9999px;
font-size:.75rem;font-weight:500;border:1px solid;text-transform:capitalize}

/* ── Stats ── */
.stats-cell{text-align:center;font-variant-numeric:tabular-nums}
.stats-cell .completed{font-weight:600;font-size:.875rem;color:#4ade80}

/* ── Breakdown pills ── */
.pills{display:flex;flex-wrap:wrap;gap:6px}
.pill{padding:2px 8px;border-radius:4px;font-size:.75rem;font-weight:500}
.pill-completed{color:#4ade80;background:rgba(34,197,94,0.1)}
.pill-failed{color:#f87171;background:rgba(239,68,68,0.1)}

/* ── Registered date ── */
.reg-date{font-size:.75rem;color:#6b7280;white-space:nowrap}

/* ── Empty state ── */
.empty-state{text-align:center;padding:64px 16px}
.empty-state .icon{font-size:2.5rem;margin-bottom:16px}
.empty-state p{color:#9ca3af}
.empty-state a{color:#60a5fa;text-decoration:none}
.empty-state a:hover{text-decoration:underline}

/* ── Trust tiers reference ── */
.tiers-ref{max-width:960px;margin:0 auto;padding:0 16px 64px}
.tiers-ref .card{background:rgba(31,41,55,0.3);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border-radius:8px;border:1px solid rgba(255,255,255,0.05);padding:24px}
.tiers-ref h3{font-size:.875rem;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:.05em;margin-bottom:12px}
.tiers-grid{display:grid;grid-template-columns:repeat(2,1fr);gap:12px}
@media(min-width:640px){.tiers-grid{grid-template-columns:repeat(5,1fr)}}
.tier-item{display:flex;align-items:center;gap:8px}
.tier-item .tier-dot{width:8px;height:8px;border-radius:50%%}
.tier-item .tier-label{font-size:.875rem;color:#9ca3af}
.tier-item .tier-desc{font-size:.75rem;color:#6b7280}
.tiers-ref .note{margin-top:16px;font-size:.75rem;color:#4b5563}

/* ── Mobile layout ── */
@media(max-width:639px){
  .row .rank-cell{display:inline-flex;margin-right:8px}
  .row .contributor{flex:1}
  .row .stats-cell{text-align:center}
  .row .pills{padding-left:44px}
  .row .reg-date{padding-left:44px}
}

/* ── Hover card ── */
.hover-card-anchor{position:relative}
.hover-card{display:none;position:absolute;left:0;top:100%%;margin-top:8px;z-index:50;
width:320px;background:rgba(17,24,39,0.95);backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);
border-radius:12px;border:1px solid rgba(255,255,255,0.1);box-shadow:0 25px 50px -12px rgba(0,0,0,0.5);
overflow:hidden;pointer-events:none}
.hover-card-anchor:hover .hover-card{display:block}
.hover-card .hc-header{padding:16px;border-bottom:1px solid rgba(255,255,255,0.05);display:flex;align-items:center;gap:12px}
.hover-card .hc-header img{width:40px;height:40px;border-radius:50%%}
.hover-card .hc-name{font-size:.875rem;font-weight:600;color:#fff}
.hover-card .hc-meta{font-size:.75rem;color:#9ca3af;margin-top:2px}
.hover-card .hc-meta .hc-pts{color:#facc15;font-weight:600}
.hover-card .hc-section{padding:12px 16px;border-bottom:1px solid rgba(255,255,255,0.05)}
.hover-card .hc-label{font-size:10px;text-transform:uppercase;letter-spacing:.05em;color:#6b7280;margin-bottom:6px}
.hover-card .hc-stats{display:flex;gap:16px}
.hover-card .hc-stat{text-align:center;flex:1}
.hover-card .hc-stat-num{font-size:1.25rem;font-weight:700;font-variant-numeric:tabular-nums}
.hover-card .hc-stat-label{font-size:.625rem;color:#6b7280;margin-top:2px;text-transform:uppercase}
.hover-card .hc-bar-wrap{height:6px;background:rgba(255,255,255,0.05);border-radius:3px;overflow:hidden;margin-top:8px}
.hover-card .hc-bar{height:100%%;border-radius:3px;transition:width .3s}
.hover-card .hc-footer{padding:8px 16px;text-align:center;font-size:10px;color:#60a5fa;background:rgba(255,255,255,0.02);border-top:1px solid rgba(255,255,255,0.05)}

/* ── No-results ── */
.no-results{text-align:center;padding:40px 16px;color:#6b7280;font-size:.875rem;display:none}
/* ── Agent row styling ── */
.row.agent-row{background:rgba(147,51,234,0.04);border-left:3px solid rgba(147,51,234,0.3)}
.row.agent-row:hover{background:rgba(147,51,234,0.08)}
.agent-emoji{font-size:1.25rem;margin-right:4px}

/* ── Section header ── */
.section-header{padding:16px 24px 8px;font-size:.75rem;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:.05em;border-bottom:1px solid rgba(255,255,255,0.05)}

/* ── Invite section ── */
.invite-section{max-width:960px;margin:0 auto;padding:0 16px 32px}
.invite-card{background:rgba(31,41,55,0.4);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border-radius:12px;border:1px solid rgba(245,158,11,0.2);padding:32px;text-align:center}
.invite-card h3{font-size:1.125rem;font-weight:600;color:#fcd34d;margin-bottom:8px}
.invite-card p{color:#9ca3af;font-size:.875rem;line-height:1.6;margin-bottom:16px}
.invite-card code{background:rgba(255,255,255,0.08);padding:4px 12px;border-radius:6px;font-size:.875rem;color:#e5e7eb}

/* ── Social share ── */
.share-section{max-width:960px;margin:0 auto;padding:0 16px 48px}
.share-buttons{display:flex;justify-content:center;gap:12px;flex-wrap:wrap}
.share-btn{display:inline-flex;align-items:center;gap:8px;padding:8px 20px;border-radius:8px;
font-size:.875rem;font-weight:500;text-decoration:none;transition:opacity .2s}
.share-btn:hover{opacity:.85}
.share-btn-linkedin{background:#0077b5;color:#fff}
.share-btn-x{background:#1d9bf0;color:#fff}
</style></head>
<body>
<div class="bg-stars"></div>
<div class="bg-grid"></div>
<div class="content">
  <section class="header">
    <h1>%s <span class="gradient-text">Leaderboard</span></h1>
    <p class="subtitle">AI agents and contributors ranked by completed tasks</p>
    <p class="meta">Tracking <strong style="color:#e5e7eb">%d</strong> participants</p>
    <div style="margin-top:24px;display:flex;justify-content:center">
      <a href="/contribute" class="contribute-link">
        <span>&#x1F41D;</span>
        <span><strong>Join the swarm</strong> &mdash; donate your CLI to help autonomous agents maintain repos, over ClankeR (the contributor relay)</span>
      </a>
    </div>
  </section>

  <div class="search-wrap">
    <input type="text" id="search" placeholder="Search agents and contributors..." autocomplete="off">
  </div>

  <section class="table-section">
    <div class="table-wrap">
      <div class="section-header">%s AI Agents</div>
      <div class="table-header">
        <div>Participant</div>
        <div class="sortable" style="text-align:center;white-space:nowrap" id="sort-findings" onclick="toggleSort('findings')">Findings</div>
        <div class="sortable active" style="text-align:center;white-space:nowrap" id="sort-completed" onclick="toggleSort('completed')">Completed &#x25BC;</div>
        <div style="text-align:center">Role</div>
      </div>
      <div id="agent-rows"></div>

      <div class="section-header">Contributors</div>
      <div id="contributor-rows"></div>
    </div>
    <div class="no-results" id="no-results">No participants match your search.</div>
  </section>

  <section class="tiers-ref" id="tiers-ref">
    <div class="card">
      <h3>Trust Tiers</h3>
      <div class="tiers-grid">
        <div class="tier-item"><div class="tier-dot" style="background:#8b949e"></div><div><div class="tier-label">Newcomer</div><div class="tier-desc">Comment on issues</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#60a5fa"></div><div><div class="tier-label">Contributor</div><div class="tier-desc">5+ tasks &rarr; create PRs</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#4ade80"></div><div><div class="tier-label">Trusted</div><div class="tier-desc">20+ tasks &rarr; merge PRs</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#c084fc"></div><div><div class="tier-label">Advisor</div><div class="tier-desc">Review agent PRs</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#f87171"></div><div><div class="tier-label">Revoked</div><div class="tier-desc">Access removed</div></div></div>
      </div>
      <p class="note">Trust tiers determine what actions a contributor's agent can perform. Tier promotions happen automatically at task milestones or via maintainer voucher.</p>
    </div>
  </section>

  <section class="invite-section">
    <div class="invite-card">
      <h3>Want to contribute?</h3>
      <p>Connect to the %s hive over ClankeR and let your CLI agent help maintain open-source repos.</p>
      <code>%s</code>
    </div>
  </section>

  <section class="share-section">
    <div class="share-buttons">
      <a class="share-btn share-btn-linkedin" href="https://www.linkedin.com/sharing/share-offsite/?url=%s" target="_blank" rel="noopener">
        <svg width="16" height="16" fill="currentColor" viewBox="0 0 24 24"><path d="M20.447 20.452h-3.554v-5.569c0-1.328-.027-3.037-1.852-3.037-1.853 0-2.136 1.445-2.136 2.939v5.667H9.351V9h3.414v1.561h.046c.477-.9 1.637-1.85 3.37-1.85 3.601 0 4.267 2.37 4.267 5.455v6.286zM5.337 7.433c-1.144 0-2.063-.926-2.063-2.065 0-1.138.92-2.063 2.063-2.063 1.14 0 2.064.925 2.064 2.063 0 1.139-.925 2.065-2.064 2.065zm1.782 13.019H3.555V9h3.564v11.452zM22.225 0H1.771C.792 0 0 .774 0 1.729v20.542C0 23.227.792 24 1.771 24h20.451C23.2 24 24 23.227 24 22.271V1.729C24 .774 23.2 0 22.222 0h.003z"/></svg>
        Share on LinkedIn
      </a>
      <a class="share-btn share-btn-x" href="https://twitter.com/intent/tweet?text=Check+out+the+leaderboard+for+%s+on+Hive!&url=%s" target="_blank" rel="noopener">
        <svg width="16" height="16" fill="currentColor" viewBox="0 0 24 24"><path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/></svg>
        Share on X
      </a>
    </div>
  </section>
</div>

<script>
var AGENTS = %s;
var CONTRIBUTORS = %s;
var sortField = 'completed';
var sortDir = 'desc';
var searchQuery = '';

var GOLD = '\u{1F947}';
var SILVER = '\u{1F948}';
var BRONZE = '\u{1F949}';

var SUCCESS_THRESHOLD_HIGH = 80;
var SUCCESS_THRESHOLD_MID = 50;

function rankHTML(rank) {
  if (rank === 1) return '<span class="medal" title="1st place">' + GOLD + '</span>';
  if (rank === 2) return '<span class="medal" title="2nd place">' + SILVER + '</span>';
  if (rank === 3) return '<span class="medal" title="3rd place">' + BRONZE + '</span>';
  return '<span class="rank-num">#' + rank + '</span>';
}

function formatDate(iso) {
  if (!iso) return '';
  var d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleDateString('en-US', {year:'numeric',month:'short',day:'numeric'});
}

function ghIcon() {
  return '<svg fill="currentColor" viewBox="0 0 24 24"><path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/></svg>';
}

function buildRow(e, isAgent) {
  var total = e.completed + e.failed;
  var successPct = total > 0 ? Math.round((e.completed / total) * 100) : 0;
  var barColor = successPct >= SUCCESS_THRESHOLD_HIGH ? '#4ade80' : successPct >= SUCCESS_THRESHOLD_MID ? '#facc15' : '#f87171';

  var namePrefix = isAgent && e.emoji ? '<span class="agent-emoji">' + e.emoji + '</span>' : '';
  var rowClass = isAgent ? 'row agent-row' : 'row';
  var tierLabel = isAgent ? 'agent' : e.tier;

  var hoverCard = '<div class="hover-card">'
    + '<div class="hc-header">'
    +   '<img src="' + e.avatar + '" alt="' + e.login + '" width="40" height="40">'
    +   '<div><div class="hc-name">' + namePrefix + e.login + '</div>'
    +   '<div class="hc-meta">Rank #' + e._rank + ' &middot; <span class="hc-pts">' + e.completed.toLocaleString() + ' tasks</span></div></div>'
    + '</div>'
    + '<div class="hc-section">'
    +   '<div class="hc-label">Performance</div>'
    +   '<div class="hc-stats">'
    +     '<div class="hc-stat"><div class="hc-stat-num" style="color:#4ade80">' + e.completed + '</div><div class="hc-stat-label">Completed</div></div>'
    +     (isAgent && e.findings ? '<div class="hc-stat"><div class="hc-stat-num" style="color:#60a5fa">' + e.findings + '</div><div class="hc-stat-label">Findings</div></div>' : '')
    +     '<div class="hc-stat"><div class="hc-stat-num" style="color:#f87171">' + e.failed + '</div><div class="hc-stat-label">' + (isAgent ? 'Restarts' : 'Failed') + '</div></div>'
    +     '<div class="hc-stat"><div class="hc-stat-num" style="color:' + barColor + '">' + successPct + '%%</div><div class="hc-stat-label">Success</div></div>'
    +   '</div>'
    +   '<div class="hc-bar-wrap"><div class="hc-bar" style="width:' + successPct + '%%;background:' + barColor + '"></div></div>'
    + '</div>'
    + '<div class="hc-section">'
    +   '<div class="hc-label">Details</div>'
    +   '<div style="display:flex;justify-content:space-between;align-items:center">'
    +     '<span class="tier-badge" style="background:' + e.tierBg + ';color:' + e.tierText + ';border-color:' + e.tierBorder + '">' + tierLabel + '</span>'
    +     (e.registered ? '<span style="font-size:.75rem;color:#6b7280">Joined ' + formatDate(e.registered) + '</span>' : '')
    +   '</div>'
    + '</div>'
    + '</div>';

  return '<div class="' + rowClass + '">'
    + '<div class="contributor">'
    +   (isAgent ? '' : '<img src="' + e.avatar + '" alt="' + e.login + '" width="32" height="32" loading="lazy">')
    +   '<div class="hover-card-anchor">'
    +     namePrefix
    +     (isAgent ? '<span class="name">' + e.login + '</span>' : '<a class="name" href="https://github.com/' + e.login + '" target="_blank" rel="noopener">' + e.login + '</a><a class="gh-icon" href="https://github.com/' + e.login + '" target="_blank" rel="noopener" title="View on GitHub">' + ghIcon() + '</a>')
    +     hoverCard
    +   '</div>'
    + '</div>'
    + '<div class="stats-cell"><span style="color:#60a5fa;font-size:.875rem;font-weight:600">' + (e.findings > 0 ? e.findings.toLocaleString() : '0') + '</span></div>'
    + '<div class="stats-cell"><div class="completed">' + e.completed.toLocaleString() + '</div></div>'
    + '<div class="stats-cell"><span class="tier-badge" style="background:' + e.tierBg + ';color:' + e.tierText + ';border-color:' + e.tierBorder + '">' + tierLabel + '</span></div>'
    + '</div>';
}

function renderRows() {
  var q = searchQuery.toLowerCase();
  var dir = sortDir === 'desc' ? 1 : -1;
  var sortFn = sortField === 'failed'
    ? function(a, b) { return dir * (b.failed - a.failed); }
    : sortField === 'findings'
    ? function(a, b) { return dir * ((b.findings || 0) - (a.findings || 0)); }
    : function(a, b) { return dir * (b.completed - a.completed); };

  var filteredAgents = AGENTS.slice();
  var filteredContributors = CONTRIBUTORS.slice();

  if (q) {
    filteredAgents = filteredAgents.filter(function(e) { return e.login.toLowerCase().indexOf(q) >= 0; });
    filteredContributors = filteredContributors.filter(function(e) { return e.login.toLowerCase().indexOf(q) >= 0; });
  }

  filteredAgents.sort(sortFn);
  filteredContributors.sort(sortFn);

  var rank = 0;
  var i;
  for (i = 0; i < filteredAgents.length; i++) { rank++; filteredAgents[i]._rank = rank; }
  for (i = 0; i < filteredContributors.length; i++) { rank++; filteredContributors[i]._rank = rank; }

  var agentContainer = document.getElementById('agent-rows');
  var contributorContainer = document.getElementById('contributor-rows');
  var noResults = document.getElementById('no-results');
  var tiersRef = document.getElementById('tiers-ref');
  var totalCount = filteredAgents.length + filteredContributors.length;

  if (totalCount === 0 && (AGENTS.length + CONTRIBUTORS.length) > 0) {
    agentContainer.innerHTML = '';
    contributorContainer.innerHTML = '';
    noResults.style.display = 'block';
    tiersRef.style.display = 'none';
    return;
  }
  noResults.style.display = 'none';
  tiersRef.style.display = totalCount > 0 ? 'block' : 'none';

  if (totalCount === 0) {
    agentContainer.innerHTML = '<div class="empty-state"><div class="icon">\u{1F3C6}</div><p>No participants yet. <a href="/contribute">Be the first!</a></p></div>';
    contributorContainer.innerHTML = '';
    return;
  }

  var agentHTML = '';
  for (i = 0; i < filteredAgents.length; i++) {
    agentHTML += buildRow(filteredAgents[i], true);
  }
  agentContainer.innerHTML = agentHTML;

  var contribHTML = '';
  for (i = 0; i < filteredContributors.length; i++) {
    contribHTML += buildRow(filteredContributors[i], false);
  }
  contributorContainer.innerHTML = contribHTML;

  var sc = document.getElementById('sort-completed');
  var sfi = document.getElementById('sort-findings');
  sc.classList.toggle('active', sortField === 'completed');
  if (sfi) sfi.classList.toggle('active', sortField === 'findings');
  sc.innerHTML = 'Completed ' + (sortField === 'completed' ? (sortDir === 'desc' ? '▼' : '▲') : '');
  if (sfi) sfi.innerHTML = 'Findings ' + (sortField === 'findings' ? (sortDir === 'desc' ? '▼' : '▲') : '');
}

function toggleSort(field) {
  if (sortField === field) {
    sortDir = sortDir === 'desc' ? 'asc' : 'desc';
  } else {
    sortField = field;
    sortDir = 'desc';
  }
  renderRows();
}

document.getElementById('search').addEventListener('input', function(e) {
  searchQuery = e.target.value;
  renderRows();
});

renderRows();
</script>
</body></html>`

// ── Helpers ────────────────────────────────────────────────────────────────

const maxUsernameLength = 39 // GitHub max username length

var reservedUsernames = map[string]bool{
	"null": true, "undefined": true, "true": true, "false": true,
	"admin": true, "root": true, "system": true, "hive": true,
	"api": true, "contribute": true, "leaderboard": true,
}

func isValidUsername(s string) bool {
	if len(s) == 0 || len(s) > maxUsernameLength {
		return false
	}
	if reservedUsernames[strings.ToLower(s)] {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// privateURLDNSTimeout bounds DNS resolution inside the SSRF guard so a
// slow or malicious DNS server cannot block the handler indefinitely.
const privateURLDNSTimeout = 5 * time.Second

type hostResolver func(ctx context.Context, host string) ([]string, error)

var privateURLResolver hostResolver = defaultHostResolver

func defaultHostResolver(ctx context.Context, host string) ([]string, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, privateURLDNSTimeout)
	defer cancel()
	return (&net.Resolver{}).LookupHost(resolveCtx, host)
}

func isPrivateURL(ctx context.Context, rawURL string) bool {
	for _, scheme := range []string{"https://", "http://", "wss://", "ws://"} {
		if strings.HasPrefix(rawURL, scheme) {
			rawURL = strings.TrimPrefix(rawURL, scheme)
			break
		}
	}
	host := rawURL
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	blocked := []string{"localhost", "127.", "10.", "172.16.", "172.17.", "172.18.", "172.19.",
		"172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.",
		"172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "169.254.", "[::1]", "[::ffff:", "0.0.0.0", "0."}
	for _, p := range blocked {
		if strings.HasPrefix(host, p) {
			return true
		}
	}

	addrs, err := privateURLResolver(ctx, host)
	if err != nil {
		// If DNS fails, treat as private (fail-closed) to prevent bypass.
		return true
	}
	for _, addr := range addrs {
		for _, p := range blocked {
			if strings.HasPrefix(addr, p) {
				return true
			}
		}
	}

	return false
}

// validateGitHubToken checks a GitHub personal access token against the GitHub API
// and returns the authenticated username, or empty string on failure.
var (
	ghTokenCacheMu sync.RWMutex
	ghTokenCache   = map[string]ghTokenCacheEntry{}
)

const ghTokenCacheTTL = 5 * time.Minute

type ghTokenCacheEntry struct {
	username  string
	expiresAt time.Time
}

// validateGitHubToken checks a token against the GitHub API user endpoint.
// apiURL overrides the API base for GHE; pass empty for default github.com.
func validateGitHubToken(token, apiURL string) string {
	if token == "" {
		return ""
	}

	ghTokenCacheMu.RLock()
	if entry, ok := ghTokenCache[token]; ok && time.Now().Before(entry.expiresAt) {
		ghTokenCacheMu.RUnlock()
		return entry.username
	}
	ghTokenCacheMu.RUnlock()

	userEndpoint := "https://api.github.com/user"
	if apiURL != "" && apiURL != "https://api.github.com" {
		userEndpoint = apiURL + "/user"
	}

	const tokenValidateTimeout = 10 * time.Second
	client := &http.Client{Timeout: tokenValidateTimeout}
	req, err := http.NewRequest("GET", userEndpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&user) != nil {
		return ""
	}

	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{username: user.Login, expiresAt: time.Now().Add(ghTokenCacheTTL)}
	ghTokenCacheMu.Unlock()

	return user.Login
}

// handleAPIv1 wraps contribute API endpoints with GitHub token auth.
// Accepts Authorization: Bearer <gh-personal-access-token>.
func (s *Server) handleAPIv1(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if strings.HasPrefix(token, "Bearer ") {
		token = token[7:]
	} else if strings.HasPrefix(token, "token ") {
		token = token[6:]
	} else {
		token = r.URL.Query().Get("token")
	}

	username := validateGitHubToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-token>"}`))
		return
	}

	subpath := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch subpath {
	case "/status":
		s.handleContributeStatus(w, r)
	case "/activity":
		s.handleContributeActivity(w, r)
	case "/contributors":
		s.handleContributorsList(w, r)
	case "/knowledge":
		s.handleKnowledgeExport(w, r)
	case "/me":
		profiles := listContributorProfiles()
		for _, p := range profiles {
			if strings.EqualFold(p.GitHubUsername, username) {
				p.TokenPlain = ""
				p.RegistrationToken = ""
				var liveStates map[string]ContributorLiveState
				if s.contributeHub != nil {
					liveStates = s.contributeHub.LiveStates()
				}
				if ls, ok := liveStates[p.ContributorID]; ok {
					p.Active = ls.Active
					p.CurrentTask = ls.CurrentTask
					p.ActiveTasks = ls.Tasks
					p.Sessions = ls.Sessions
				}
				jsonResponse(w, p)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Not registered as a contributor. Run: just contribute-setup"}`))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Unknown endpoint","available":["/api/v1/status","/api/v1/activity","/api/v1/contributors","/api/v1/knowledge","/api/v1/me"]}`))
	}
}

func (s *Server) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	host = strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == ':' || c == '-' {
			return c
		}
		return -1
	}, host)
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	baseURL := scheme + "://" + host
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Hive API</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;padding:40px;max-width:900px;margin:0 auto}
h1{margin-bottom:8px;font-size:1.8rem}
.subtitle{color:#8b949e;margin-bottom:32px}
h2{margin-top:32px;margin-bottom:12px;color:#58a6ff;font-size:1.2rem}
.endpoint{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px;margin-bottom:12px}
.method{color:#3fb950;font-weight:bold;margin-right:8px}
.path{color:#58a6ff;font-family:monospace}
.desc{color:#8b949e;margin-top:4px;font-size:0.9rem}
pre{background:#0d1117;border:1px solid #30363d;border-radius:6px;padding:12px;margin-top:12px;overflow-x:auto;font-size:0.85rem;color:#e6edf3}
code{font-family:'SF Mono',monospace;font-size:0.85rem}
.token-box{background:#161b22;border:1px solid #f0883e;border-radius:8px;padding:16px;margin:16px 0}
.token-box h3{color:#f0883e;margin-bottom:8px}
a{color:#58a6ff}
</style></head><body>
<h1>🐝 Hive API</h1>
<p class="subtitle">Authenticated access to the contributor API</p>

<div class="token-box">
<h3>Authentication</h3>
<p>Use your GitHub personal access token (from <code>gh auth token</code>):</p>
<pre>curl -H "Authorization: Bearer $(gh auth token)" %s/api/v1/status</pre>
</div>

<h2>Endpoints</h2>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/status</span>
<div class="desc">Hub status — online, active contributors, actionable items</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/status</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/me</span>
<div class="desc">Your contributor profile — tasks completed, active sessions, current task</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/me</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/contributors</span>
<div class="desc">All registered contributors with live state</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/contributors</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/activity</span>
<div class="desc">Live activity feed — joined, left, picked up, completed events</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/activity</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/knowledge</span>
<div class="desc">Knowledge base export as markdown (used by agent.md)</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/knowledge</pre>
</div>

<h2>Knowledge Sources</h2>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/stats</span>
<div class="desc">Knowledge base stats — layers, fact counts, engine, health</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/stats</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/search?q=&lt;query&gt;&amp;limit=10</span>
<div class="desc">Search all knowledge facts by keyword</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/search?q=autoscaling&amp;limit=10</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/git-sources</span>
<div class="desc">List connected git sources</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/git-sources</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/git-sources</span>
<div class="desc">Add a git source — clone a repo and index its markdown as knowledge facts</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://github.com/org/repo","name":"my-docs","subpath":"docs","branch":"main","layer":"project"}' \
  %s/api/knowledge/git-sources</pre>
</div>

<div class="endpoint">
<span class="method">DELETE</span><span class="path">/api/knowledge/git-sources</span>
<div class="desc">Remove a git source</div>
<pre>curl -X DELETE -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://github.com/org/repo","subpath":"docs"}' \
  %s/api/knowledge/git-sources</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/documents</span>
<div class="desc">List imported documents</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/documents</span>
<div class="desc">Import a document from URL — supports PDF, HTML, DOCX, plain text. Content is parsed into chunks and stored as knowledge facts.</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://arxiv.org/pdf/2309.06180","name":"vllm-paper","layer":"community"}' \
  %s/api/knowledge/documents</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/documents/{slug}</span>
<div class="desc">Get document metadata — title, source URL, fact count, fact slugs</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents/vllm-paper</pre>
</div>

<div class="endpoint">
<span class="method">DELETE</span><span class="path">/api/knowledge/documents/{slug}</span>
<div class="desc">Delete a document and all its extracted facts</div>
<pre>curl -X DELETE -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents/vllm-paper</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/documents/{slug}/reimport</span>
<div class="desc">Re-fetch a document and re-extract facts (replaces old facts)</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents/vllm-paper/reimport</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/subscriptions</span>
<div class="desc">List wiki subscriptions (remote llm-wiki endpoints)</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/subscriptions</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/subscriptions</span>
<div class="desc">Add a wiki subscription</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://wiki.example.com/mcp","name":"team-wiki","layer":"org"}' \
  %s/api/knowledge/subscriptions</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/import</span>
<div class="desc">Import facts from raw markdown or JSON content</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"# Guard .join()\n\nAlways use (arr || []).join()","layer":"project","format":"markdown"}' \
  %s/api/knowledge/import</pre>
</div>

<h2>Token Management</h2>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/contribute/reissue-token</span>
<div class="desc">Reissue your registration token using GitHub auth — invalidates the old token</div>
<pre>curl -X POST -H "Authorization: Bearer $(gh auth token)" %s/api/contribute/reissue-token</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/contribute/register</span>
<div class="desc">Register (or re-register with <code>force:true</code> to reissue token)</div>
<pre>curl -X POST -d '{"github_username":"you","force":true}' %s/api/contribute/register</pre>
</div>

</body></html>`, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL)
}
