package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
)

const (
	defaultContributorsDir    = "/data/contributors"
	contributorAutoPromoteAt  = 5
	contributorTrustedAt      = 20
	defaultFederationRegistry = "/data/federation/registry.json"

	// inviteTokenTTL bounds how long a trusted invite link stays valid (issue
	// #2598). A trusted contributor mints a link; the invitee has this long to
	// follow it and register. Long enough to share over email/chat, short enough
	// that a leaked link stops working. Attribution — not access — so a generous
	// window is fine.
	inviteTokenTTL = 14 * 24 * time.Hour
	// inviteSecretFile persists the per-instance HMAC secret used to sign invite
	// tokens, so links survive a restart. Lives beside the contributor store.
	inviteSecretFile = ".invite-secret"
	// inviteSecretBytes is the length of the generated fallback signing secret.
	inviteSecretBytes = 32
)

// inviteTrustTiers are the trust tiers permitted to mint an invite link. Only a
// trusted or advisor contributor may invite; a newcomer/contributor/anonymous
// viewer may not. Enforced server-side (handleContributeInvite) — UI hiding is
// UX only.
var inviteTrustTiers = map[string]bool{"trusted": true, "advisor": true}

var (
	inviteSecretOnce  sync.Once
	inviteSecretCache []byte
)

// inviteSigningSecret returns the HMAC key used to sign/verify invite tokens.
// It prefers the operator-configured HIVE_HUB_SECRET (already the cross-hive
// shared secret) so invites are consistent across a federated deployment; if
// that is unset it lazily generates and persists a per-instance random secret
// beside the contributor store. Either way the secret never leaves the server —
// the token the client sees is opaque.
func inviteSigningSecret() []byte {
	inviteSecretOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("HIVE_HUB_SECRET")); v != "" {
			inviteSecretCache = []byte(v)
			return
		}
		path := filepath.Join(getContributorsDir(), inviteSecretFile)
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			inviteSecretCache = []byte(strings.TrimSpace(string(data)))
			return
		}
		secret := randomHex(inviteSecretBytes)
		ensureDir(getContributorsDir())
		_ = os.WriteFile(path, []byte(secret), 0o600)
		inviteSecretCache = []byte(secret)
	})
	return inviteSecretCache
}

// mintInviteToken builds an opaque, HMAC-signed invite token that carries the
// inviter's GitHub username and an expiry. Format: base64url(inviter) "." expiry
// "." base64url(hmac). The signature covers "inviter|expiry", so neither field
// can be tampered with without invalidating the token.
func mintInviteToken(inviter string, now time.Time) string {
	exp := strconv.FormatInt(now.Add(inviteTokenTTL).Unix(), 10)
	encInviter := base64.RawURLEncoding.EncodeToString([]byte(inviter))
	mac := hmac.New(sha256.New, inviteSigningSecret())
	mac.Write([]byte(encInviter + "|" + exp))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encInviter + "." + exp + "." + sig
}

// verifyInviteToken checks an invite token's signature and expiry and returns
// the inviter username. An empty string means the token is invalid, tampered,
// or expired — the caller must treat that as "no attribution" (a plain
// self-registration), never as an error.
func verifyInviteToken(token string, now time.Time) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	const inviteTokenParts = 3
	if len(parts) != inviteTokenParts {
		return ""
	}
	encInviter, exp, sig := parts[0], parts[1], parts[2]
	mac := hmac.New(sha256.New, inviteSigningSecret())
	mac.Write([]byte(encInviter + "|" + exp))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return ""
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() > expUnix {
		return ""
	}
	inviter, err := base64.RawURLEncoding.DecodeString(encInviter)
	if err != nil || !isValidUsername(string(inviter)) {
		return ""
	}
	return string(inviter)
}

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
	// InvitedBy records the GitHub username of the TRUSTED/advisor contributor
	// who invited this person via a trusted invite link (issue #2598). It is
	// pure attribution: it never affects TrustTier (an invitee always joins as
	// "newcomer"). Empty for self-registered contributors.
	InvitedBy      string `json:"invited_by,omitempty"`
	RegisteredAt   string `json:"registered_at"`
	TasksCompleted int    `json:"total_tasks_completed"`
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
	// Path-style deep links: /contribute/onboarding|management|operations|leaderboard
	// (and the short id forms) all serve the SAME landing HTML — the client JS reads
	// location.pathname and activates the matching tab. The {tab} segment is not used
	// server-side; it exists so each tab is a real bookmarkable/shareable URL. Any
	// /contribute/<tab> is already treated as public by isPublicPath (server.go).
	s.mux.HandleFunc("GET /contribute/{tab}", s.handleContributeLanding)
	s.mux.HandleFunc("GET /api/contribute/ws", s.contributeHub.HandleWS)
	s.mux.HandleFunc("POST /api/contribute/register", s.handleContributeRegister)
	// Trusted invite link (issue #2598). A trusted/advisor contributor mints an
	// attributed invite link here; the caller's identity is resolved server-side
	// and their trust tier is verified IN-HANDLER (403 otherwise) — the /api/
	// contribute prefix is exempt from roleEnforcement's read-only block, so the
	// tier gate cannot be delegated to the middleware. UI hiding is UX only.
	s.mux.HandleFunc("POST /api/contribute/invite", s.handleContributeInvite)
	s.mux.HandleFunc("POST /api/contribute/reissue-token", s.handleContributeReissueToken)
	s.mux.HandleFunc("GET /api/contribute/status", s.handleContributeStatus)
	s.mux.HandleFunc("GET /api/contribute/activity", s.handleContributeActivity)
	s.mux.HandleFunc("GET /api/contribute/fleet", s.handleContributeFleet)
	// Read-only live event stream for the Operations command center. Under the
	// /api/contribute* prefix, so isPublicPath (server.go) makes it PUBLIC —
	// anonymous viewers may subscribe to this read-only info. GET only.
	s.mux.HandleFunc("GET /api/contribute/events", s.handleContributeEvents)
	// Read-only ready-work QUEUE snapshot (the admissible issues waiting to be
	// picked off). Also public; a JSON fallback for the SSE hello payload.
	s.mux.HandleFunc("GET /api/contribute/queue", s.handleContributeQueue)
	// Read-only OPPORTUNISTIC WORK list (#2592): a small, curated set of admissible
	// issues NOT already at the front of the ready queue, ranked by a light recency
	// heat proxy. Public like the other /api/contribute* reads; cheap to compute.
	s.mux.HandleFunc("GET /api/contribute/opportunistic", s.handleContributeOpportunistic)
	// Read-only HIVE LIMITS (#2595): the per-tier managed-queue rate limits + the
	// viewer's own daily usage when we can identify them. Public read; the "you"
	// block is resolved server-side from the session / X-Hive-User header (never a
	// client-supplied username) so a viewer only ever sees their OWN usage.
	s.mux.HandleFunc("GET /api/contribute/limits", s.handleContributeLimits)
	// Operator priority override for the ready-work queue. Owner/read-write only —
	// enforced IN-HANDLER via requireContributorWrite because the /api/contribute
	// prefix is exempt from roleEnforcement's read-only block (see that helper).
	s.mux.HandleFunc("PUT /api/contribute/queue/order", s.handleContributeQueueOrder)
	s.mux.HandleFunc("GET /api/contributors", s.handleContributorsList)
	s.mux.HandleFunc("GET /api/contributors/{id}", s.handleContributorGet)
	s.mux.HandleFunc("PUT /api/contributors/{id}/trust", s.handleContributorTrust)
	s.mux.HandleFunc("POST /api/contributors/{id}/revoke", s.handleContributorRevoke)
	s.mux.HandleFunc("POST /api/contributors/{id}/requeue", s.handleContributorRequeue)
	s.mux.HandleFunc("DELETE /api/contributors/{id}", s.handleContributorDelete)

	s.mux.HandleFunc("GET /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("POST /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("GET /api/docs", s.handleAPIDocs)

	s.mux.HandleFunc("GET /leaderboard", s.handleLeaderboardPage)
	s.mux.HandleFunc("GET /api/leaderboard", s.handleLeaderboardAPI)
	// Central per-user "Me" profile — a HUB endpoint returning one contributor's
	// cross-hive profile (identity/tier/stats/milestones/hives/rank), aggregated
	// from central hub data (contributor store + LeaderboardForHub + federation
	// registry). Read-only; public via isPublicPath's /api/leaderboard prefix. The
	// Leaderboard tab calls it to render the personal "Me" card. See me_profile.go.
	s.mux.HandleFunc("GET /api/leaderboard/contributor/{username}", s.handleContributorProfile)

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
/* ── Clickable GitHub issue/PR references (#2616) ────────────────────────────────
   Shared affordance for every repo#number reference on the Operations tab (ready
   queue, my-work, opportunistic-work, dev-log). Deliberately more visible than
   the surrounding muted-grey monospace text — link-blue + underline-on-hover +
   a small external-link glyph — so it reads as an obvious "open on GitHub"
   action, not decoration. Inherits the host element's font (monospace repo#num,
   or inline log text) so it drops into any of those contexts unchanged. */
.cc-issue-link{display:inline-flex;align-items:center;gap:3px;color:#58a6ff;text-decoration:none;font:inherit;border-radius:4px;transition:color .15s}
.cc-issue-link:hover,.cc-issue-link:focus-visible{color:#79c0ff;text-decoration:underline}
.cc-issue-link:focus-visible{outline:2px solid #58a6ff;outline-offset:2px}
.cc-issue-link-ic{flex-shrink:0;opacity:.85}
.cc-issue-link:hover .cc-issue-link-ic{opacity:1}
.work-title{font-size:.9rem;color:#e6edf3;margin:2px 0 6px}
.work-meta{display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:.75rem;color:#8b949e}
.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:.7rem;font-weight:600;border:1px solid transparent}
.pill-progress{background:rgba(88,166,255,.12);color:#58a6ff;border-color:rgba(88,166,255,.3)}
.pill-review{background:rgba(210,153,34,.12);color:#d29922;border-color:rgba(210,153,34,.3)}
.pill-passed{background:rgba(63,185,80,.12);color:#3fb950;border-color:rgba(63,185,80,.3)}
.pill-blocked{background:rgba(248,81,73,.12);color:#f85149;border-color:rgba(248,81,73,.3)}
.pill-idle{background:rgba(139,148,158,.12);color:#8b949e;border-color:rgba(139,148,158,.3)}
/* #2574 (follow-up): the Connected-clankers card is a NARROW column. The old
   layout put the multi-line identity text (.clanker-main) and the inline
   controls (.admin-actions: tier dropdown + Revoke + Remove) in the SAME
   align-items:center flex row with margin-left:auto. In the narrow column the
   controls got vertically centered over the middle of the tall text block and
   rendered ON TOP OF the "cli · model · role · on repo#N" lines. Fix: the row is
   now a 3-column CSS grid — [dot][avatar][identity] on the top line — and the
   trailing element (.admin-actions, or the non-admin .feed-time) is placed on its
   OWN line spanning the full width BELOW the identity, so it never competes
   horizontally with the multi-line text. Long repo paths in .clanker-sub wrap
   (overflow-wrap:anywhere) rather than pushing into anything. align-items:start
   keeps the dot/avatar top-aligned with the first text line. */
.clanker-row{display:grid;grid-template-columns:auto auto minmax(0,1fr);align-items:start;column-gap:10px;row-gap:8px;padding:12px 20px;border-bottom:1px solid #21262d}
/* The trailing controls / timestamp: full-width line beneath the identity. It is
   always the LAST grid child, so grid-column:1/-1 drops it below regardless of
   whether it's .admin-actions or the .feed-time fallback. */
.clanker-row>.admin-actions,.clanker-row>.feed-time{grid-column:1/-1}
.clanker-av{width:28px;height:28px;border-radius:50%%;flex-shrink:0;background:#30363d}
.clanker-main{min-width:0}
.clanker-user{font-size:.88rem;color:#e6edf3;font-weight:500}
.clanker-sub{font-size:.74rem;color:#8b949e;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;overflow-wrap:anywhere;word-break:break-word}
/* Row is align-items:start (grid), so nudge the small dot down to sit level with
   the username's first line instead of the very top of the row. */
.clanker-dot{width:8px;height:8px;border-radius:50%%;background:#3fb950;flex-shrink:0;margin-top:7px}
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
.lb-row{display:grid;grid-template-columns:56px 1fr 120px 70px 70px 80px;align-items:center;gap:8px;padding:10px 20px;border-bottom:1px solid #21262d;font-size:.85rem}
.lb-row:last-child{border-bottom:none}
/* Subtle self-highlight for the logged-in viewer's own row: a faint tint + a left
   accent border, professional not loud. Readability preserved. */
.lb-row--me{background:rgba(31,111,235,.09);box-shadow:inset 3px 0 0 0 #1f6feb}
.lb-you{display:inline-block;margin-left:8px;font-size:.62rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:#58a6ff;background:rgba(31,111,235,.14);border:1px solid rgba(31,111,235,.3);border-radius:999px;padding:1px 7px;vertical-align:middle}
.lb-head{color:#8b949e;font-weight:600;font-size:.72rem;text-transform:uppercase;letter-spacing:.04em;background:#0d1117}
.lb-rank{color:#8b949e;font-variant-numeric:tabular-nums}
.lb-name{color:#e6edf3;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.lb-tier{color:#8b949e}
.lb-stat{text-align:right;color:#c9d1d9;font-variant-numeric:tabular-nums}
.lb-head .lb-stat,.lb-head .lb-rank{text-align:right;color:#8b949e}
.lb-head .lb-rank{text-align:left}
/* ── Subtle "ranked/alive" accent pass on Operations + Leaderboard (SUBTLE +
   PROFESSIONAL — light watermarking only). Theme-aware: built entirely from the
   existing dark palette (#161b22 card / #30363d border / #0d1117 deep). Every
   accent is driven by REAL data (trust tier + real task counts); nothing is
   fabricated. Readability first — text keeps full contrast; the tints are muted,
   never neon. All motion respects the prefers-reduced-motion block below. ───── */
/* Tier medallion / rank badge. One class per REAL trust tier; the per-tier tint is
   a muted metal-ish accent (advisor/trusted = warmer gold-amber, contributor =
   cooler steel, newcomer = neutral). Small pill with a tiny CSS-drawn medallion
   dot — no external images (CSP forbids them), no glow. This is the CANONICAL
   tier-badge family; the Me-card below reuses it rather than hand-rolling its own
   tier-color helper, so leaderboard rows, ops cards, and the Me-card read as one
   ranked family. */
.tier-badge{display:inline-flex;align-items:center;gap:5px;font-size:.68rem;font-weight:600;line-height:1;padding:3px 8px 3px 6px;border-radius:999px;border:1px solid #30363d;background:#0d1117;color:#8b949e;text-transform:capitalize;white-space:nowrap}
.tier-badge::before{content:"";width:8px;height:8px;border-radius:50%%;background:currentColor;box-shadow:inset 0 0 0 1px rgba(1,4,9,.35);flex:none}
.tier-badge.tier-advisor{border-color:rgba(210,169,85,.45);background:rgba(210,169,85,.10);color:#d0a955}
.tier-badge.tier-trusted{border-color:rgba(201,162,39,.40);background:rgba(201,162,39,.08);color:#c9a94a}
.tier-badge.tier-contributor{border-color:rgba(110,163,201,.38);background:rgba(110,163,201,.08);color:#6ea3c9}
.tier-badge.tier-newcomer{border-color:#30363d;background:#0d1117;color:#8b949e}
/* Restrained gradient header band on the accented cards. Very low-contrast wash
   from the deep bg into the card colour — reads as a faint banner, not a loud
   gradient; the bottom border keeps the head crisp. */
.ops-card.card-accent>.ops-card-head{background:linear-gradient(180deg,#12161d 0%%,#161b22 100%%)}
.ops-card.card-accent>.ops-card-head h3{letter-spacing:.01em}
/* Bold-numeral stat emphasis: the primary "Done" numeral on the leaderboard and
   the key ops counts get heavier weight + slightly larger tabular figures so the
   number reads as the hero of the row without adding chrome. The Me-card's own
   stat numerals reuse this same bold/tabular treatment via .lb-stat.lb-primary. */
.lb-row .lb-stat.lb-primary{color:#e6edf3;font-weight:700;font-size:.95rem}
.lb-head .lb-stat.lb-primary{font-weight:600;font-size:.72rem;color:#8b949e}
.tier-badge.tier-lb{padding:2px 8px 2px 5px;font-size:.66rem}
/* Ops "your army" counts + card counts as bold numerals (tabular, no layout shift). */
.cc-army b{font-weight:700;font-variant-numeric:tabular-nums}
.ops-card-count.count-strong{color:#e6edf3;font-weight:700;font-variant-numeric:tabular-nums}
/* Compact tier badge inline next to a connected clanker's identity. */
.tier-badge.tier-inline{padding:1px 6px 1px 4px;font-size:.62rem;margin-left:6px;vertical-align:middle}
.tier-badge.tier-inline::before{width:6px;height:6px}
/* Larger tier-badge variant used as the hero medallion on the personal Me-card
   (see below) — same canonical tier colors/dot, just sized up for a hero slot. */
.tier-badge.tier-hero{font-size:.78rem;padding:5px 12px 5px 8px}
.tier-badge.tier-hero::before{width:10px;height:10px}
/* ── Personal "Me" card ──────────────────────────────────────────────────────
   A pride-forward personal accomplishment showcase pinned above the standings.
   It reuses the SAME canonical ranked-surface primitives the leaderboard/ops
   cards above use — .tier-badge (tier-hero variant) for the tier medallion and
   the .lb-stat.lb-primary bold-numeral treatment for its stat grid — so the
   Me-card, the leaderboard rows, and the ops cards read as ONE cohesive ranked
   family rather than two parallel styling systems. Seven cosmetic style skins
   (.me-card--style1..7) are palette/framing/density variations over the SAME
   real data. Subtle and professional, reduced-motion safe. --me-accent drives
   the per-tier accent wash (medallion ring, chips, stat numerals). */
.me-card{position:relative;background:#161b22;border:1px solid #30363d;border-radius:14px;overflow:hidden;margin-bottom:20px;--me-accent:#58a6ff;--me-accent-soft:rgba(88,166,255,.14)}
.me-card__band{position:relative;padding:20px 22px 18px;background:linear-gradient(135deg,var(--me-accent-soft),rgba(22,27,34,0) 70%%);border-bottom:1px solid #21262d}
.me-card__toprow{display:flex;align-items:center;gap:16px}
.me-medallion{position:relative;width:64px;height:64px;flex-shrink:0;border-radius:50%%;display:flex;align-items:center;justify-content:center;background:radial-gradient(circle at 50%% 35%%,var(--me-accent-soft),#0d1117 78%%);border:2px solid var(--me-accent);box-shadow:0 0 0 4px rgba(1,4,9,.35)}
.me-medallion img{width:52px;height:52px;border-radius:50%%;object-fit:cover;background:#30363d}
/* #2595 fix: the tier badge now lives in its OWN layout slot beside the name
   (.me-id__tier), NOT absolutely positioned over the avatar — so the "Contributor"
   / "Trusted" / "Newcomer" pill can never overlap the avatar image. */
.me-id__tier{margin:4px 0 2px}
.me-id__tier .tier-badge{position:static;transform:none}
.me-id{min-width:0;flex:1}
.me-id__name{font-size:1.25rem;font-weight:700;color:#e6edf3;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.me-id__lead{font-size:.85rem;color:#8b949e;margin-top:2px}
.me-id__lead b{color:var(--me-accent)}
.me-rankpill{margin-left:auto;flex-shrink:0;text-align:center;background:#0d1117;border:1px solid #30363d;border-radius:12px;padding:8px 14px}
.me-rankpill__num{font-size:1.25rem;font-weight:800;color:#e6edf3;font-variant-numeric:tabular-nums;line-height:1}
.me-rankpill__lbl{font-size:.62rem;color:#8b949e;text-transform:uppercase;letter-spacing:.05em;margin-top:3px}
.me-card__body{padding:18px 22px 20px}
.me-statgrid{display:grid;grid-template-columns:repeat(3,1fr);gap:10px}
.me-stat{background:#0d1117;border:1px solid #30363d;border-radius:10px;padding:12px 8px;text-align:center}
/* Stat numeral reuses the canonical bold/tabular "hero numeral" treatment
   (same weight/tabular-nums as .lb-stat.lb-primary), tinted by --me-accent. */
.me-stat .lb-stat.lb-primary{display:block;font-size:1.6rem;color:var(--me-accent);padding:0}
.me-stat__lbl{font-size:.66rem;color:#8b949e;text-transform:uppercase;letter-spacing:.04em;margin-top:5px}
.me-sec{margin-top:18px}
.me-sec__title{font-size:.72rem;color:#8b949e;text-transform:uppercase;letter-spacing:.05em;font-weight:600;margin-bottom:9px;display:flex;align-items:center;gap:8px}
.me-sec__title .me-soon{font-size:.6rem;font-weight:600;color:#8b949e;border:1px solid #30363d;border-radius:999px;padding:1px 7px;text-transform:none;letter-spacing:0}
.me-chips{display:flex;flex-wrap:wrap;gap:7px}
.me-chip{display:inline-flex;align-items:center;gap:5px;padding:4px 10px;border-radius:999px;font-size:.74rem;font-weight:600;border:1px solid transparent;background:#0d1117;color:#8b949e;border-color:#30363d}
.me-chip--got{background:var(--me-accent-soft);color:var(--me-accent);border-color:var(--me-accent)}
.me-chip--next{background:#0d1117;color:#c9d1d9;border-color:#30363d;border-style:dashed}
.me-chip--badge{background:#0d1117;color:#8b949e;border-color:#30363d;border-style:dashed;opacity:.85}
.me-hives{display:flex;flex-direction:column;gap:6px}
.me-hive{display:flex;align-items:center;gap:8px;font-size:.82rem;color:#c9d1d9}
.me-hive__rel{font-size:.66rem;font-weight:700;text-transform:uppercase;letter-spacing:.03em;padding:2px 7px;border-radius:6px;background:var(--me-accent-soft);color:var(--me-accent)}
.me-hive__rel--owner{background:rgba(210,153,34,.16);color:#d29922}
.me-actions{display:flex;flex-wrap:wrap;gap:10px;margin-top:18px;align-items:center}
.me-share{display:inline-flex;align-items:center;gap:7px;padding:9px 16px;border-radius:10px;font-size:.85rem;font-weight:600;text-decoration:none;background:var(--me-accent);color:#0d1117;border:1px solid var(--me-accent);cursor:pointer;font-family:inherit}
.me-share:hover{filter:brightness(1.08)}
.me-share--ghost{background:transparent;color:var(--me-accent)}
.me-stylepick{margin-left:auto;display:flex;align-items:center;gap:7px;font-size:.72rem;color:#8b949e}
.me-stylepick select{background:#0d1117;border:1px solid #30363d;color:#c9d1d9;border-radius:8px;padding:5px 8px;font-size:.78rem;font-family:inherit;cursor:pointer}
.me-signin{background:#161b22;border:1px dashed #30363d;border-radius:14px;padding:22px;text-align:center;color:#8b949e;font-size:.9rem;margin-bottom:20px}
.me-signin b{color:#e6edf3}
/* ── The 7 profile-style skins (palette / framing / density variations) ────────
   Each is a tasteful, readable, professional variant of the SAME card. They only
   change accent palette, header treatment, medallion framing, and density —
   never the data or the affordances. Default (style1) needs no override. */
.me-card--style2{--me-accent:#3fb950;--me-accent-soft:rgba(63,185,80,.14)}
.me-card--style3{--me-accent:#d29922;--me-accent-soft:rgba(210,153,34,.15)}
.me-card--style4{--me-accent:#a371f7;--me-accent-soft:rgba(163,113,247,.15)}
.me-card--style5{--me-accent:#8b949e;--me-accent-soft:rgba(139,148,158,.12)}     /* minimal / restrained */
.me-card--style5 .me-card__band{background:#161b22}
.me-card--style5 .me-medallion{background:#0d1117}
.me-card--style6{--me-accent:#f778ba;--me-accent-soft:rgba(247,120,186,.14)}
.me-card--style6 .me-card__band{background:linear-gradient(135deg,var(--me-accent-soft),rgba(22,27,34,0))}
.me-card--style7{--me-accent:#58a6ff;--me-accent-soft:rgba(88,166,255,.18)}     /* roomy "ranked" */
.me-card--style7 .me-card__band{padding:26px 24px 22px}
.me-card--style7 .me-card__body{padding:22px 24px 24px}
.me-card--style7 .me-medallion{width:74px;height:74px}
.me-card--style7 .me-medallion img{width:60px;height:60px}
@media(max-width:520px){.me-card__toprow{flex-wrap:wrap}.me-rankpill{margin-left:0}.me-statgrid{grid-template-columns:1fr}}
@media(prefers-reduced-motion:reduce){.me-card *{transition:none!important;animation:none!important}}
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
/* Repos-for-Contribute enable toggles + Tier rate-limit rows (Management mirror of
   the Governor Hub sections). Subtle, matching the rest of the admin controls. */
.admin-repos{display:flex;flex-wrap:wrap;gap:8px}
.admin-repo{display:inline-flex;align-items:center;gap:8px;padding:6px 10px;border:1px solid #30363d;border-radius:8px;background:#0d1117}
.admin-repo .admin-switch{width:32px;height:18px}
.admin-repo .admin-switch::after{width:14px;height:14px}
.admin-repo .admin-switch.on::after{left:16px}
.admin-repo__name{font-size:.76rem;color:#c9d1d9;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.admin-tier{display:grid;grid-template-columns:1fr repeat(3,64px);align-items:center;gap:8px;padding:8px 0;border-bottom:1px solid #21262d}
.admin-tier:last-child{border-bottom:none}
.admin-tier__head{display:flex;align-items:center;gap:8px;min-width:0}
.admin-tier__name{font-size:.8rem;color:#e6edf3;text-transform:capitalize}
.admin-tier input{width:100%%;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e6edf3;font:inherit;font-size:.78rem;padding:4px 6px;outline:none;text-align:right}
.admin-tier input:focus{border-color:#1f6feb}
.admin-tier input:disabled{opacity:.45}
.admin-tier__col{font-size:.62rem;color:#6e7681;text-align:right;text-transform:uppercase;letter-spacing:.03em}
.admin-tier--head{border-bottom:1px solid #30363d;padding-bottom:4px}
/* No margin-left:auto — .admin-actions is now a full-width grid row beneath the
   identity (see .clanker-row grid), left-aligned and wrapping if the buttons
   don't fit the narrow column. */
.admin-actions{display:flex;gap:6px;flex-wrap:wrap}
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
/* ── Operations command center — live SSE-driven queue / travel / dev-log /
   achievements / army framing. Subtle-professional motion only; degrades to the
   existing poll when SSE is unavailable. Additive, read-only. ─────────────── */
.cc-live{display:inline-flex;align-items:center;gap:6px;font-size:.68rem;font-weight:600;padding:2px 8px;border-radius:999px;margin-left:auto;border:1px solid rgba(63,185,80,.3);background:rgba(63,185,80,.1);color:#3fb950}
.cc-live .cc-live-dot{width:7px;height:7px;border-radius:50%%;background:#3fb950;animation:pulse 2s infinite}
.cc-live.stale{border-color:rgba(210,153,34,.3);background:rgba(210,153,34,.1);color:#d29922}
/* Polling (stale) dot: a very slow, gentle breathe rather than the brisk live
   pulse — signals "still watching, just on the calmer poll cadence". */
.cc-live.stale .cc-live-dot{background:#d29922;animation:cc-slowpulse 2.8s ease-in-out infinite}
@keyframes cc-slowpulse{0%%,100%%{opacity:1}50%%{opacity:.45}}
@media(prefers-reduced-motion:reduce){.cc-live .cc-live-dot,.cc-live.stale .cc-live-dot{animation:none!important}}
/* Ready-work queue play/pause — the SAME contribute_suspended control as the
   Management "Suspend contributions" switch, surfaced on the queue header.
   Quiet by default (bordered ghost button); the danger tint only appears once
   paused, matching .admin-switch.on.danger's accent so the two placements read
   as one state. Left of #cc-live so status (queue live/stale) and posture
   (active/paused) sit as a pair. */
#queue-suspend-wrap{display:inline-flex;align-items:center;gap:6px;margin-left:auto}
.queue-suspend-btn{display:inline-flex;align-items:center;justify-content:center;width:26px;height:26px;padding:0;border-radius:999px;border:1px solid #30363d;background:transparent;color:#8b949e;cursor:pointer;line-height:0;transition:background .15s,color .15s,border-color .15s}
/* SVG glyph is centered by the flex-box; currentColor tracks the button state.
   Using an SVG (not a &#10074; bar glyph) so the pause bars sit dead-center —
   the light-vertical-bar character carries font side-bearing that pushed the
   pair off-center inside the circle. */
.queue-suspend-btn svg{display:block;width:12px;height:12px;fill:currentColor}
.queue-suspend-btn:hover{background:rgba(139,148,158,.12);color:#c9d1d9}
.queue-suspend-btn.paused{border-color:rgba(248,81,73,.35);color:#f85149;background:rgba(248,81,73,.08)}
.queue-suspend-btn.paused:hover{background:rgba(248,81,73,.16)}
.queue-suspend-btn:disabled{opacity:.5;cursor:not-allowed}
/* Army roster header line under the clanker card */
.cc-army{display:flex;align-items:center;gap:14px;padding:10px 20px;border-bottom:1px solid #21262d;font-size:.78rem;color:#8b949e}
.cc-army b{color:#e6edf3;font-weight:600}
.cc-army-stat{display:inline-flex;align-items:center;gap:5px}
.cc-army-stat .dot{width:7px;height:7px;border-radius:50%%}
.cc-army-stat.working .dot{background:#58a6ff}
.cc-army-stat.reviewing .dot{background:#d29922}
.cc-army-stat.idle .dot{background:#8b949e}
/* Clanker rows: enter pop-in / leave fade so the roster feels alive */
@keyframes cc-popin{from{opacity:0;transform:translateY(-6px) scale(.98)}to{opacity:1;transform:none}}
@keyframes cc-fadeout{from{opacity:1}to{opacity:0;transform:translateX(8px)}}
.clanker-row.cc-enter{animation:cc-popin .4s ease}
.clanker-row.cc-leave{animation:cc-fadeout .5s ease forwards}
/* A clanker actively receiving a travelling task pulses its border briefly */
@keyframes cc-landing{0%%{box-shadow:0 0 0 0 rgba(88,166,255,.5)}100%%{box-shadow:0 0 0 6px rgba(88,166,255,0)}}
.clanker-row.cc-landing{animation:cc-landing .8s ease}
.clanker-status{font-size:.68rem;font-weight:600;padding:1px 7px;border-radius:999px;margin-left:6px;border:1px solid transparent}
.clanker-status.working{background:rgba(88,166,255,.12);color:#58a6ff;border-color:rgba(88,166,255,.3)}
.clanker-status.reviewing{background:rgba(210,153,34,.12);color:#d29922;border-color:rgba(210,153,34,.3)}
.clanker-status.idle{background:rgba(139,148,158,.12);color:#8b949e;border-color:rgba(139,148,158,.3)}
/* Ready-work QUEUE — the stack of issues waiting to be picked off. A generous
   max-height keeps a long backlog (up to ~150 items) scrolling inside the card
   instead of stretching the page; the panel scrolls, the page does not. */
.cc-queue{max-height:560px;overflow-y:auto}
.cc-q-item{display:flex;align-items:flex-start;gap:10px;padding:11px 20px;border-bottom:1px solid #21262d;animation:cc-popin .35s ease;position:relative}
.cc-q-item:first-child{background:rgba(88,166,255,.05)}
/* Drag handle (grab bar) — owner/read-write only. Hidden unless the queue root
   carries .cc-q-draggable (set by initAdmin after /api/role). Reduced-motion and
   pointer friendly. */
.cc-q-grip{display:none;flex-shrink:0;width:16px;align-self:stretch;cursor:grab;color:#6e7681;font-size:.9rem;line-height:1;align-items:center;justify-content:center;user-select:none;touch-action:none}
.cc-q-grip:hover{color:#c9d1d9}
.cc-queue.cc-q-draggable .cc-q-grip{display:flex}
.cc-queue.cc-q-draggable .cc-q-item{cursor:default}
.cc-q-item.cc-q-dragging{opacity:.5;cursor:grabbing}
.cc-q-item.cc-q-over{box-shadow:inset 0 2px 0 0 #58a6ff}
.cc-q-idx{font-size:.7rem;color:#6e7681;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;flex-shrink:0;width:22px;text-align:right;padding-top:2px}
.cc-q-body{flex:1;min-width:0}
.cc-q-repo{font-size:.72rem;color:#8b949e;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.cc-q-title{font-size:.86rem;color:#e6edf3;margin:2px 0 4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cc-q-labels{display:flex;flex-wrap:wrap;gap:4px}
.cc-q-next{font-size:.62rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:#58a6ff;flex-shrink:0;padding-top:2px}
.cc-q-item.cc-leaving{animation:cc-fadeout .45s ease forwards}
/* FLIP glide for operator drag-reorder: items that changed slot are given an
   inverse transform (see ccFlipQueue) then eased back to translateY(0). Subtle —
   an SRE ops tool, not a game — so no bounce/overshoot, just a smooth glide. */
.cc-q-item.cc-q-flip{transition:transform .26s ease}
/* ── Playlist-style queue controls (#2592 power-up) — Apple-Music register ──────
   A clean search field on the queue card and a subtle per-row "⋯" menu with
   move-to-top / move-to-position actions. Deliberately sober (an SRE ops tool):
   muted greys, the page's blue accent only on focus/hover, no game-y flourish.
   The search bar is read-only filtering so it shows for everyone; the per-row
   ACTIONS live inside the row menu, which is only rendered for owner/read-write. */
.cc-q-search{display:flex;align-items:center;gap:8px;padding:10px 20px;border-bottom:1px solid #21262d}
.cc-q-search-ic{color:#6e7681;font-size:.85rem;flex-shrink:0;line-height:1}
.cc-q-search input{flex:1;min-width:0;background:#0d1117;border:1px solid #30363d;border-radius:7px;color:#e6edf3;font:inherit;font-size:.82rem;padding:6px 10px;outline:none;transition:border-color .15s,box-shadow .15s}
.cc-q-search input::placeholder{color:#6e7681}
.cc-q-search input:focus{border-color:#1f6feb;box-shadow:0 0 0 3px rgba(31,111,235,.25)}
.cc-q-search-clear{background:none;border:none;color:#6e7681;cursor:pointer;font-size:1rem;line-height:1;padding:2px 4px;display:none}
.cc-q-search.has-text .cc-q-search-clear{display:inline-flex}
.cc-q-search-clear:hover{color:#c9d1d9}
.cc-q-filternote{padding:6px 20px;font-size:.72rem;color:#6e7681;border-bottom:1px solid #21262d}
/* Per-row "⋯" context affordance — owner/read-write only (rendered only when
   adminEnabled). Sits at the row's trailing edge, quiet until hover/open. */
.cc-q-menu-wrap{position:relative;flex-shrink:0;margin-left:auto;align-self:center}
.cc-q-menu-btn{background:none;border:none;color:#6e7681;cursor:pointer;font-size:1rem;line-height:1;padding:4px 6px;border-radius:6px}
.cc-q-menu-btn:hover,.cc-q-menu-btn[aria-expanded=true]{color:#e6edf3;background:#21262d}
.cc-q-menu{position:absolute;top:100%%;right:0;z-index:40;min-width:190px;background:#161b22;border:1px solid #30363d;border-radius:10px;box-shadow:0 8px 28px rgba(1,4,9,.55);padding:6px;display:none}
.cc-q-menu.open{display:block}
.cc-q-menu button.cc-q-act{display:flex;align-items:center;gap:8px;width:100%%;background:none;border:none;color:#c9d1d9;font:inherit;font-size:.82rem;text-align:left;padding:7px 9px;border-radius:6px;cursor:pointer}
.cc-q-menu button.cc-q-act:hover{background:#21262d;color:#e6edf3}
.cc-q-menu-ic{color:#6e7681;flex-shrink:0;width:16px;text-align:center}
.cc-q-menu-sep{height:1px;background:#21262d;margin:5px 2px}
.cc-q-moverow{display:flex;align-items:center;gap:6px;padding:7px 9px}
.cc-q-moverow label{font-size:.78rem;color:#8b949e;flex:1}
.cc-q-moverow input{width:56px;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e6edf3;font:inherit;font-size:.8rem;padding:4px 6px;outline:none}
.cc-q-moverow input:focus{border-color:#1f6feb}
.cc-q-moverow button{background:#1f6feb;border:none;color:#fff;font:inherit;font-size:.76rem;font-weight:600;padding:5px 10px;border-radius:6px;cursor:pointer}
.cc-q-moverow button:hover{background:#388bfd}
/* ── Opportunistic Work (#2592) — a small, CALM discovery panel. Intentionally
   quiet: no loud "recommended!" chrome, just a short curated list with a subtle
   heat dot and an unobtrusive "add to queue" affordance (owner/read-write only). */
.opp-list{padding:2px 0}
.opp-item{display:flex;align-items:flex-start;gap:10px;padding:11px 20px;border-bottom:1px solid #21262d}
.opp-item:last-child{border-bottom:none}
.opp-heat{flex-shrink:0;width:8px;height:8px;border-radius:50%%;margin-top:5px;background:#3fb950;box-shadow:0 0 0 3px rgba(63,185,80,.14)}
.opp-heat.warm{background:#d29922;box-shadow:0 0 0 3px rgba(210,153,34,.14)}
.opp-heat.cool{background:#6e7681;box-shadow:none}
.opp-body{flex:1;min-width:0}
.opp-repo{font-size:.72rem;color:#8b949e;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.opp-title{font-size:.86rem;color:#e6edf3;margin:2px 0 3px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.opp-reason{font-size:.7rem;color:#6e7681}
.opp-add{flex-shrink:0;align-self:center;background:none;border:1px solid #30363d;color:#c9d1d9;font:inherit;font-size:.74rem;font-weight:600;padding:5px 11px;border-radius:7px;cursor:pointer;transition:border-color .15s,color .15s,background .15s}
.opp-add:hover{border-color:#1f6feb;color:#fff;background:rgba(31,111,235,.15)}
.opp-add:disabled{opacity:.55;cursor:default;border-color:#30363d;color:#8b949e;background:none}
/* ── End-of-queue + hive-settings (#2595) — turn a short queue into an intentional,
   reassuring moment: a calm "all caught up" marker, the managed-queue rate limits
   presented readably, and the viewer's own daily quota. Sober, ranked-family styling. */
.cc-q-end{padding:18px 20px 6px;text-align:center}
.cc-q-end-badge{display:inline-flex;align-items:center;gap:8px;font-size:.82rem;color:#8b949e;background:#0d1117;border:1px solid #21262d;border-radius:999px;padding:7px 16px}
.cc-q-end-badge .cc-q-end-ic{color:#3fb950;font-size:.95rem;line-height:1}
.hive-settings{margin:14px 20px 4px;background:#0d1117;border:1px solid #21262d;border-radius:10px;padding:14px 16px}
.hive-settings h4{margin:0 0 4px;font-size:.78rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:#8b949e}
.hive-settings p.hs-lead{margin:0 0 10px;font-size:.82rem;color:#c9d1d9;line-height:1.5}
.hs-tiers{display:flex;flex-wrap:wrap;gap:8px}
.hs-tier{flex:1 1 130px;min-width:120px;background:#161b22;border:1px solid #30363d;border-radius:8px;padding:9px 11px}
.hs-tier__name{font-size:.72rem;font-weight:600;text-transform:capitalize;color:#e6edf3;display:flex;align-items:center;gap:6px}
.hs-tier__lim{font-size:.74rem;color:#8b949e;margin-top:3px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.hs-tier.is-you{border-color:#1f6feb;box-shadow:0 0 0 2px rgba(31,111,235,.18)}
.hs-tier__youtag{font-size:.6rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:#58a6ff}
/* Daily quota widget — a slim progress meter, calm. Used at end-of-queue AND on
   the Me card. Fill width is set inline from the REAL used/limit ratio. */
.quota{margin-top:12px}
.quota__head{display:flex;align-items:baseline;justify-content:space-between;gap:8px;margin-bottom:6px}
.quota__lbl{font-size:.76rem;color:#8b949e}
.quota__val{font-size:.82rem;color:#e6edf3;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.quota__bar{height:7px;border-radius:999px;background:#21262d;overflow:hidden}
.quota__fill{height:100%%;border-radius:999px;background:linear-gradient(90deg,#1f6feb,#388bfd);transition:width .4s ease}
.quota__fill.near{background:linear-gradient(90deg,#d29922,#e3b341)}
.quota__fill.full{background:linear-gradient(90deg,#f85149,#ff7b72)}
.quota__sub{font-size:.7rem;color:#6e7681;margin-top:5px}
/* Me-card quota variant — sits inside a me-sec, so it inherits the card padding. */
.me-quota .quota__lbl{color:#8b949e}
@media(prefers-reduced-motion:reduce){.quota__fill{transition:none!important}}
/* "File an issue on this page" link (#2594) — a subtle footer affordance present
   on every tab. Quiet grey, matches the sober dashboard chrome; an outbound link. */
.cc-page-foot{padding:26px 48px 34px;border-top:1px solid #21262d;margin-top:28px;display:flex;justify-content:center}
.cc-report-link{display:inline-flex;align-items:center;gap:7px;color:#8b949e;font-size:.8rem;text-decoration:none;border:1px solid #30363d;border-radius:8px;padding:7px 14px;transition:color .15s,border-color .15s,background .15s}
.cc-report-link:hover{color:#e6edf3;border-color:#484f58;background:#161b22}
.cc-report-link .cc-report-ic{font-size:.9rem;line-height:1}
/* The travelling token that flies from the queue to a clanker on task_assign */
.cc-token{position:fixed;z-index:1200;pointer-events:none;background:#1f6feb;color:#fff;font-size:.72rem;font-weight:600;padding:6px 12px;border-radius:999px;box-shadow:0 6px 20px rgba(31,111,235,.5);white-space:nowrap;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;transition:transform .9s cubic-bezier(.5,0,.2,1),opacity .9s ease;will-change:transform,opacity}
/* Dev-log — a running chat log of the development */
.cc-log{max-height:360px;overflow-y:auto;padding:4px 0}
.cc-log-line{display:flex;align-items:flex-start;gap:10px;padding:8px 20px;font-size:.83rem;border-bottom:1px solid #1c2128;animation:cc-logline .45s ease}
@keyframes cc-logline{from{opacity:0;transform:translateY(6px)}to{opacity:1;transform:none}}
.cc-log-line:last-child{border-bottom:none}
.cc-log-ic{flex-shrink:0}
.cc-log-body{flex:1;min-width:0;color:#c9d1d9;line-height:1.45}
.cc-log-body b{color:#e6edf3}
.cc-log-body .who{color:#58a6ff;font-weight:600}
.cc-log-body .ref{color:#8b949e;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.78rem}
.cc-log-body a.ref.cc-issue-link{color:#58a6ff}
.cc-log-body a.ref.cc-issue-link:hover,.cc-log-body a.ref.cc-issue-link:focus-visible{color:#79c0ff}
.cc-log-time{flex-shrink:0;color:#6e7681;font-size:.72rem;white-space:nowrap;padding-top:1px}
/* Achievement pops — tasteful badge toast, top-right, debounced */
.cc-ach-wrap{position:fixed;top:16px;right:16px;z-index:1150;display:flex;flex-direction:column;gap:8px;pointer-events:none}
.cc-ach{display:flex;align-items:center;gap:10px;background:linear-gradient(135deg,#161b22,#1c2333);border:1px solid rgba(210,153,34,.4);border-radius:10px;padding:10px 14px;box-shadow:0 8px 28px rgba(1,4,9,.55);animation:cc-ach-in .4s ease;max-width:300px}
@keyframes cc-ach-in{from{opacity:0;transform:translateX(24px)}to{opacity:1;transform:none}}
.cc-ach.cc-ach-out{animation:cc-ach-out .4s ease forwards}
@keyframes cc-ach-out{to{opacity:0;transform:translateX(24px)}}
.cc-ach-ic{font-size:1.3rem;flex-shrink:0}
.cc-ach-txt{min-width:0}
.cc-ach-h{font-size:.7rem;font-weight:700;letter-spacing:.05em;text-transform:uppercase;color:#d29922}
.cc-ach-s{font-size:.82rem;color:#e6edf3;margin-top:1px}
/* ── Operations two-region shell: MAIN area + full-height DEV-LOG RAIL ──────────
   The main area flexes to fill remaining width; the rail is a fixed-width panel
   pinned to the tab's height. When the rail collapses it shrinks to a thin strip
   and the main area reflows to reclaim the freed width. The width change is driven
   by the rail's own flex-basis so main widening is automatic (no JS resize). */
.ops-shell{display:flex;gap:20px;margin-top:24px;align-items:stretch}
.ops-main{flex:1 1 auto;min-width:0}
.ops-main .ops-grid{margin-top:0}
/* The rail: a self-contained chat/notifications panel that runs the tab's height.
   It sticks so the feed stays in view while the (taller) main area scrolls. */
.ops-rail{flex:0 0 340px;position:sticky;top:0;align-self:flex-start;max-height:calc(100vh - 80px);
  background:#161b22;border:1px solid #30363d;border-radius:12px;overflow:hidden;
  display:flex;flex-direction:column;transition:flex-basis .28s ease}
.ops-rail-inner{display:flex;flex-direction:column;min-height:0;flex:1 1 auto;opacity:1;transition:opacity .2s ease}
.ops-rail-head{padding:16px 20px;border-bottom:1px solid #30363d;display:flex;align-items:center;gap:10px;flex-shrink:0}
.ops-rail-head h3{font-size:.95rem;color:#e6edf3;margin:0}
.ops-rail .cc-log{flex:1 1 auto;max-height:none;min-height:0}
/* The collapse toggle sits on the rail's leading edge (a slim handle). */
.ops-rail-toggle{display:flex;align-items:center;gap:6px;width:100%%;background:#0d1117;border:none;
  border-bottom:1px solid #30363d;color:#8b949e;font-family:inherit;font-size:.74rem;font-weight:600;
  letter-spacing:.03em;text-transform:uppercase;padding:9px 14px;cursor:pointer;flex-shrink:0}
.ops-rail-toggle:hover{color:#e6edf3;background:#161b22}
.ops-rail-chevron{display:inline-block;font-size:1rem;line-height:1;transition:transform .28s ease}
/* Collapsed: rail narrows to a strip; the toggle label + inner feed hide; the
   chevron flips to point "open" (right) as a "show log" affordance. */
.ops-rail.collapsed{flex-basis:44px}
.ops-rail.collapsed .ops-rail-inner{opacity:0;pointer-events:none;height:0;overflow:hidden}
.ops-rail.collapsed .ops-rail-toggle-label{display:none}
.ops-rail.collapsed .ops-rail-toggle{justify-content:center;padding:9px 0}
.ops-rail.collapsed .ops-rail-chevron{transform:rotate(180deg)}
/* Narrow viewports: stack the rail BELOW the main area (full width) so the page
   never scrolls horizontally. Collapse still works; it just hides the feed body. */
@media(max-width:900px){
  .ops-shell{flex-direction:column}
  .ops-rail{flex-basis:auto;width:100%%;position:static;max-height:none}
  .ops-rail .cc-log{max-height:360px}
  .ops-rail.collapsed{flex-basis:auto}
}
@media(prefers-reduced-motion:reduce){
  .clanker-row.cc-enter,.clanker-row.cc-leave,.clanker-row.cc-landing,.cc-q-item,.cc-q-item.cc-leaving,.cc-q-item.cc-q-flip,.cc-log-line,.cc-ach,.cc-token{animation:none!important;transition:none!important}
  .ops-rail,.ops-rail-inner,.ops-rail-chevron{transition:none!important}
}
/* #2548 Branded client entry points — a find-by-SIGHT tile grid above the CLI
   selector. Each tile carries an inline SVG/glyph emblem so a contributor spots
   their tool visually; clicking a tile just drives the existing #cli-select so
   nothing about the copy-block logic changes. CSP-safe (inline assets only),
   theme-consistent with the dark palette, reduced-motion safe. */
.client-tiles{display:grid;grid-template-columns:repeat(auto-fill,minmax(132px,1fr));gap:8px;margin:4px 0 20px}
.client-tile{display:flex;align-items:flex-start;gap:9px;background:#161b22;border:1px solid #30363d;border-radius:10px;padding:9px 11px;cursor:pointer;text-align:left;font-family:inherit;color:#e6edf3;font-size:.82rem;transition:border-color .15s,background .15s,transform .1s}
.client-tile:hover{border-color:#58a6ff;background:#1b2230}
.client-tile:active{transform:translateY(1px)}
.client-tile:focus-visible{outline:2px solid #58a6ff;outline-offset:2px}
.client-tile.sel{border-color:#58a6ff;background:rgba(88,166,255,.10);box-shadow:inset 0 0 0 1px rgba(88,166,255,.35)}
.client-tile .ct-emblem{width:24px;height:24px;flex:0 0 24px;display:flex;align-items:center;justify-content:center;border-radius:6px;background:#0d1117;overflow:hidden}
.client-tile .ct-emblem svg{width:18px;height:18px;display:block}
.client-tile .ct-name{font-weight:600;line-height:1.15;min-width:0}
.client-tile .ct-name small{display:block;font-weight:400;color:#8b949e;font-size:.7rem}
/* min-width:0 keeps the name ellipsising rather than overflowing the tile. */
.client-tile .ct-body{display:flex;flex-direction:column;align-items:flex-start;gap:3px;min-width:0}
/* "Open in <tool>" onboarding affordance — deliberately understated and clearly a
   SETUP helper, never a "contributing" surface. Only rendered for a client with a
   real, vendor-documented deep-link scheme. */
.openin-row{display:none;align-items:flex-start;gap:10px;background:#0d1117;border:1px solid #30363d;border-left:3px solid #d29922;border-radius:8px;padding:11px 14px;margin:0 0 16px}
.openin-row.show{display:flex}
.openin-row .oi-body{min-width:0;font-size:.8rem;color:#8b949e;line-height:1.4}
.openin-row .oi-body strong{color:#e6edf3}
.openin-link{flex:0 0 auto;display:inline-flex;align-items:center;gap:6px;background:#161b22;border:1px solid #30363d;border-radius:6px;color:#58a6ff;text-decoration:none;font-size:.8rem;padding:6px 12px;font-family:inherit;cursor:pointer}
.openin-link:hover{border-color:#58a6ff}
.openin-link svg{width:14px;height:14px}
.oi-note{color:#d29922;font-weight:600}
/* Customizable, copy-pasteable per-client PROMPT (kept in an editable block the
   contributor can read/tweak, NOT compressed into a URL). Additive to the shell
   command copy block above it. */
.prompt-block{margin:18px 0 8px;background:#0d1117;border:1px solid #30363d;border-radius:8px;padding:14px 16px 16px;position:relative}
.prompt-block h4{margin:0 0 6px;font-size:.82rem;color:#e6edf3;font-weight:600}
.prompt-block p.pb-sub{margin:0 0 10px;color:#8b949e;font-size:.76rem;line-height:1.4}
.prompt-block textarea{width:100%%;min-height:118px;resize:vertical;background:#010409;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:10px 12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.8rem;line-height:1.5}
.prompt-block textarea:focus{outline:none;border-color:#58a6ff}
.pb-copy{position:absolute;top:10px;right:12px;background:#238636;color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:.72rem;font-family:inherit}
@media(prefers-reduced-motion:reduce){
  .client-tile{transition:none!important}
}
/* Trusted invite banner (issue #2598) — shown on onboarding when arriving via an
   attributed invite link. Subtle, informational; makes clear the invitee joins
   as a newcomer. */
.invite-banner{margin:0 0 20px;padding:11px 15px;border:1px solid #388bfd55;border-radius:8px;background:#1c2f4a55;color:#c9d1d9;font-size:.85rem;line-height:1.45}
.invite-banner b{color:#e6edf3}
.invite-banner .invite-tier{color:#8b949e}
/* Trusted "Invite someone" action inside the Me card (issue #2598). */
.me-invite{margin-top:12px;padding-top:12px;border-top:1px solid #ffffff14}
.me-invite__btn{display:inline-flex;align-items:center;gap:6px;background:#1f6feb;color:#fff;border:none;border-radius:6px;padding:7px 14px;font-size:.82rem;font-family:inherit;cursor:pointer}
.me-invite__btn:hover{background:#388bfd}
.me-invite__row{display:none;margin-top:10px;gap:8px;align-items:center;flex-wrap:wrap}
.me-invite__row.open{display:flex}
.me-invite__link{flex:1 1 220px;min-width:0;background:#010409;color:#c9d1d9;border:1px solid #30363d;border-radius:6px;padding:7px 10px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.76rem}
.me-invite__copy{background:#238636;color:#fff;border:none;border-radius:6px;padding:7px 12px;font-size:.78rem;font-family:inherit;cursor:pointer}
.me-invite__hint{width:100%%;margin-top:6px;color:#8b949e;font-size:.74rem;line-height:1.4}
</style></head><body>
<div class="page-tabs" role="tablist">
<button class="page-tab active" role="tab" id="ptab-onboarding" aria-selected="true" data-panel="tab-onboarding">Onboarding</button>
<button class="page-tab" role="tab" id="ptab-ops" aria-selected="false" data-panel="tab-ops">Operations</button>
<button class="page-tab" role="tab" id="ptab-manage" aria-selected="false" data-panel="tab-manage">Management</button>
<button class="page-tab" role="tab" id="ptab-leaderboard" aria-selected="false" data-panel="tab-leaderboard">Leaderboard</button>
</div>
<div class="tab-panel active" id="tab-onboarding" role="tabpanel" aria-labelledby="ptab-onboarding">
<div class="page">
<div class="main">
<h1>🐝 Contribute to %s</h1>
<div id="invite-banner" class="invite-banner" hidden role="status"></div>
<p class="subtitle">Donate your CLI + API tokens to help this project's AI agent swarm.</p>
<p class="subtitle" style="font-size:.95rem;margin-top:-24px;margin-bottom:32px">Powered by <strong style="color:#e6edf3">ClankeR</strong>, the contributor relay &mdash; it hands tasks from this hive's backlog to the agent running on your machine. Your compute, their backlog.</p>
<div class="stat-row">
<div class="stat"><div class="stat-num" style="color:#58a6ff">%d</div><div class="stat-label">Total</div></div>
%s
</div>
<div class="steps">
<h3>How it works</h3>
<!-- #2548 Branded client entry points: a find-by-SIGHT tile grid. Rendered by
     JS from the CLIENTS metadata (inline SVG emblems, all CSP-safe). Clicking a
     tile drives the existing #cli-select below, so the copy-block logic is
     unchanged. Falls back gracefully: the plain selector still works if JS is off. -->
<p style="color:#8b949e;margin:0 0 8px;font-size:.9rem">Find your tool:</p>
<div id="client-tiles" class="client-tiles" role="listbox" aria-label="Choose your CLI tool"></div>
<!-- "Open in <tool>" ONBOARDING affordance. Only shown for a client with a real,
     vendor-documented deep-link scheme. It opens a chat in the vendor's own app to
     help you get set up — it does NOT connect that tool to this hive's contributor
     relay. Labeled unambiguously as setup help, never as a contribution path. -->
<div id="openin-row" class="openin-row">
<div class="oi-body"><strong id="openin-title">Open in your tool</strong><br><span id="openin-desc"></span> <span class="oi-note">This is onboarding help &mdash; it opens a chat in the vendor&rsquo;s app to walk you through setup. It does NOT connect your tool to this hive; you still contribute by running the commands below.</span></div>
<a id="openin-link" class="openin-link" href="#" target="_blank" rel="noopener"><svg viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M6.5 3.5H3.5A1.5 1.5 0 0 0 2 5v7.5A1.5 1.5 0 0 0 3.5 14H11a1.5 1.5 0 0 0 1.5-1.5v-3" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/><path d="M9.5 2.5H14v4.5M14 2.5 7.5 9" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg><span id="openin-label">Open in tool</span></a>
</div>
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
<!-- #2548 Full, copy-pasteable, CUSTOMIZABLE prompt. The exact text a contributor
     pastes into their own tool lives here in an editable block they can read and
     tweak — deliberately NOT compressed into a deep-link URL. Prefilled per selected
     client by the script below; edits are preserved until the client changes. -->
<div class="prompt-block">
<button type="button" id="prompt-copy" class="pb-copy">Copy</button>
<h4>Prompt to paste into <span id="prompt-tool">your tool</span></h4>
<p class="pb-sub">Optional. Paste this into <span id="prompt-tool2">your tool</span> and it will walk you through joining this hive on your machine. Edit it freely &mdash; it is yours to customize.</p>
<textarea id="prompt-text" spellcheck="false" aria-label="Customizable onboarding prompt"></textarea>
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
if(typeof syncBranded==='function')syncBranded();
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

// ── #2548 Branded client entry points ──────────────────────────────────────
// Per-client identity (inline SVG emblems, CSP-safe), first-class parity for
// Claude / Copilot / Pi / Goose, a documented-only "Open in" deep-link, and a
// customizable copy-paste prompt. All additive: the source of truth stays
// #cli-select, and everything degrades gracefully if this block never runs.
//
// deeplink is populated ONLY where the vendor officially documents a scheme.
// Today that is Claude alone (the claude:// desktop deep link, per Anthropic's
// Help Center). We do NOT invent schemes for tools that don't document one.
// A deep link opens a chat in the vendor's app to help with SETUP — it does
// NOT connect the tool to this hive's relay, which is why the affordance is
// labeled onboarding-not-contribution in the UI.
var EMB={
claude:'<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="#d97757" d="M12 2 3.5 20h3.2l1.6-3.7h7.4L17.3 20h3.2L12 2Zm-2.4 11.2L12 7.6l2.4 5.6H9.6Z"/></svg>',
copilot:'<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12.5" r="8" fill="none" stroke="#e6edf3" stroke-width="1.6"/><circle cx="9" cy="12" r="1.3" fill="#e6edf3"/><circle cx="15" cy="12" r="1.3" fill="#e6edf3"/><path d="M12 4.5V2M8 5l-1-2M16 5l1-2" stroke="#e6edf3" stroke-width="1.4" stroke-linecap="round"/></svg>',
pi:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 8h16" stroke="#7c93ff" stroke-width="2" stroke-linecap="round"/><path d="M9 8v10M15.5 8v7.5a2 2 0 0 0 2 2" stroke="#7c93ff" stroke-width="2" stroke-linecap="round"/></svg>',
goose:'<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="#3fb0ac" d="M6 14a6 6 0 0 1 6-6c1 0 1.6.9 1 1.7 2.5.4 4 2.4 4 5.1 0 .6-.5 1.2-1.2 1.2H8.5A2.5 2.5 0 0 1 6 13.5V14Z"/><circle cx="10.5" cy="10.8" r=".8" fill="#0d1117"/><path d="M13 9.7l2.4-1" stroke="#f0b429" stroke-width="1.4" stroke-linecap="round"/></svg>',
litellm:'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="6" width="16" height="12" rx="2" fill="none" stroke="#58a6ff" stroke-width="1.5"/><path d="M8 10l2 2-2 2M12.5 14h3.5" stroke="#58a6ff" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
openrouter:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 12h4l3-4 3 8 3-4h3" fill="none" stroke="#8b5cf6" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
vllm:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5l4 14 3-9 3 9 4-14" fill="none" stroke="#f0b429" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
'llm-d':'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="5" width="16" height="14" rx="2" fill="none" stroke="#4d9375" stroke-width="1.5"/><path d="M8 9h4a3 3 0 0 1 0 6H8V9Z" fill="none" stroke="#4d9375" stroke-width="1.5" stroke-linejoin="round"/></svg>',
bob:'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="5" y="4" width="14" height="16" rx="2" fill="none" stroke="#1f70c1" stroke-width="1.5"/><path d="M9 8h3.5a2 2 0 0 1 0 4H9V8ZM9 12h4a2 2 0 0 1 0 4H9v-4Z" fill="none" stroke="#1f70c1" stroke-width="1.3" stroke-linejoin="round"/></svg>',
other:'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="6" width="16" height="12" rx="2" fill="none" stroke="#8b949e" stroke-width="1.5"/><path d="M8 12h.01M12 12h.01M16 12h.01" stroke="#8b949e" stroke-width="2.2" stroke-linecap="round"/></svg>'
};
var CLIENTS={
claude:{name:'Claude Code',tag:'Anthropic',peer:true,
  deeplink:{label:'Open in Claude',
    // claude:// desktop deep link (documented: support.claude.com "Open Claude
    // Desktop with a link"). q= prefills the prompt for the user to review/send.
    href:function(p){return 'claude://claude.ai/new?q='+encodeURIComponent(p);},
    desc:'Opens Claude Desktop with a setup prompt prefilled.'}},
copilot:{name:'GitHub Copilot',tag:'GitHub',peer:true},
pi:{name:'Pi',tag:'pi.dev',peer:true},
goose:{name:'Goose',tag:'Block (Ollama)',peer:true},
litellm:{name:'LiteLLM',tag:'your proxy',peer:true},
openrouter:{name:'OpenRouter',tag:'your key',peer:true},
vllm:{name:'vLLM',tag:'self-hosted'},
'llm-d':{name:'llm-d',tag:'self-hosted'},
bob:{name:'Bob',tag:'IBM'},
other:{name:'Other',tag:'host only'}
};
var tilesEl=document.getElementById('client-tiles');
var oiRow=document.getElementById('openin-row');
var oiLink=document.getElementById('openin-link');
var oiLabel=document.getElementById('openin-label');
var oiTitle=document.getElementById('openin-title');
var oiDesc=document.getElementById('openin-desc');
var promptText=document.getElementById('prompt-text');
var promptTool=document.getElementById('prompt-tool');
var promptTool2=document.getElementById('prompt-tool2');
var promptEdited=false;   // once the user edits, don't clobber on same client
var promptForClient='';   // which client the current prompt text was generated for
function tileOrder(){
  // Peers (Claude/Copilot/Pi/Goose/LiteLLM/OpenRouter) first, in select order, then
  // the rest — so these tools lead the grid rather than being afterthoughts. This
  // is an ORDERING signal only; peer status is no longer shown as a visible badge.
  var peers=[],rest=[];
  for(var i=0;i<sel.options.length;i++){
    var v=sel.options[i].value;var c=CLIENTS[v]||{name:v,tag:''};
    (c.peer?peers:rest).push(v);
  }
  return peers.concat(rest);
}
function buildTiles(){
  if(!tilesEl)return;
  tilesEl.innerHTML=tileOrder().map(function(v){
    var c=CLIENTS[v]||{name:v,tag:''};
    var emb=EMB[v]||EMB.other;
    return '<button type="button" class="client-tile" role="option" data-cli="'+v+'" aria-selected="false" title="'+c.name+'">'+
      '<span class="ct-emblem">'+emb+'</span>'+
      '<span class="ct-body"><span class="ct-name">'+c.name+(c.tag?'<small>'+c.tag+'</small>':'')+'</span></span></button>';
  }).join('');
}
function defaultPromptFor(v){
  var c=CLIENTS[v]||{name:v};
  return 'Help me contribute to this hive using '+c.name+' on my machine.\n'+
    'The hive hub is: '+hubURL+'\n\n'+
    'Please walk me through, step by step, using the official setup:\n'+
    '  1. Install the prerequisites (just, gh) for my OS.\n'+
    '  2. git clone -b v2 https://github.com/kubestellar/hive && cd hive\n'+
    '  3. export HIVE_HUB='+hubURL+'\n'+
    '  4. just contribute-setup '+v+'\n'+
    '  5. just contribute-hive\n\n'+
    'Explain what each step does before I run it, and stop if anything looks wrong.';
}
function syncBranded(){
  var v=sel.value;
  // Reflect selection on the tiles.
  if(tilesEl){
    var btns=tilesEl.querySelectorAll('.client-tile');
    for(var i=0;i<btns.length;i++){
      var on=btns[i].getAttribute('data-cli')===v;
      btns[i].classList.toggle('sel',on);
      btns[i].setAttribute('aria-selected',on?'true':'false');
    }
  }
  var c=CLIENTS[v]||{name:v};
  // Prompt block: (re)generate for a new client, preserve manual edits within one.
  if(promptText){
    if(promptForClient!==v||!promptEdited){
      promptText.value=defaultPromptFor(v);
      promptForClient=v;promptEdited=false;
    }
    if(promptTool)promptTool.textContent=c.name;
    if(promptTool2)promptTool2.textContent=c.name;
  }
  // "Open in" affordance — only for a client that documents a deep-link scheme.
  if(oiRow){
    if(c.deeplink){
      oiRow.classList.add('show');
      oiTitle.textContent=c.deeplink.label;
      oiLabel.textContent=c.deeplink.label;
      oiDesc.textContent=c.deeplink.desc||'';
      // Handoff carries the SAME editable prompt so the vendor chat opens on the
      // exact setup text shown here — kept in sync, never a stale URL blob.
      oiLink.setAttribute('href',c.deeplink.href((promptText&&promptText.value)||defaultPromptFor(v)));
    }else{
      oiRow.classList.remove('show');
      oiLink.setAttribute('href','#');
    }
  }
}
if(tilesEl){
  tilesEl.addEventListener('click',function(e){
    var t=e.target.closest?e.target.closest('.client-tile'):null;
    if(!t)return;
    var v=t.getAttribute('data-cli');
    if(v&&v!==sel.value){sel.value=v;modelInput.value='';}
    update();  // drives the copy block + syncBranded()
  });
}
if(promptText){
  promptText.addEventListener('input',function(){promptEdited=true;
    // keep the deep-link handoff in step with live edits
    var c=CLIENTS[sel.value];if(c&&c.deeplink&&oiLink)oiLink.setAttribute('href',c.deeplink.href(promptText.value));
  });
}
var pbCopy=document.getElementById('prompt-copy');
if(pbCopy&&promptText){
  pbCopy.addEventListener('click',function(){
    promptText.focus();promptText.select();
    var ok=false;try{ok=document.execCommand('copy')}catch(e){}
    if(!ok&&navigator.clipboard){navigator.clipboard.writeText(promptText.value).catch(function(){});ok=true;}
    pbCopy.textContent=ok?'Copied!':'Cmd+C';pbCopy.style.background='#16a34a';
    setTimeout(function(){pbCopy.textContent='Copy';pbCopy.style.background='#238636'},2000);
  });
}
buildTiles();
update();  // initial paint: copy block + branded UI in sync from first load
// ── end #2548 ───────────────────────────────────────────────────────────────
})();
</script>
</div>
<p style="color:#6e7681;font-size:.78rem;margin-top:8px">Containerized mode auto-detects docker, then podman &mdash; when both are present, Docker wins. Docker's daemon runs rootful (docker-group membership is effectively root on the host); Podman here runs rootless (user namespace via <code>--userns=keep-id</code>, SELinux labels). Force either explicitly with <code>export HIVE_CONTAINER_RUNTIME=podman</code> (or <code>docker</code>). Rootless Podman handling is best-effort today, not yet covered by CI &mdash; see <a href="https://github.com/kubestellar/hive/blob/v2/v2/docs/podman-rootless-ci.md" target="_blank" style="color:#58a6ff">docs/podman-rootless-ci.md</a>.</p>
<p style="color:#6e7681;font-size:.78rem;margin-top:8px">Don't see your CLI? <a href="https://github.com/kubestellar/hive/issues/new?title=CLI+request:+&labels=enhancement" target="_blank" style="color:#58a6ff">Open an issue</a> and we'll add support for it.</p>
<div style="margin-top:20px;display:flex;gap:12px;flex-wrap:wrap">
<button type="button" id="goto-leaderboard-tab" style="display:inline-block;padding:8px 20px;background:#161b22;border:1px solid #30363d;border-radius:8px;color:#58a6ff;text-decoration:none;font-size:.9rem;font-family:inherit;cursor:pointer">🏆 View Leaderboard</button>
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

<hr class="admin-hr">
<h3 style="font-size:.9rem;color:#e6edf3;margin:0 0 4px">Repos for Contribute</h3>
<p class="ops-note" style="margin-top:0">Which repos feed the contribute queue. A repo is enabled unless toggled off. Mirrors the Governor Hub repo list; persists as <code>disabled_repos</code>.</p>
<div id="admin-repos"></div>

<hr class="admin-hr">
<h3 style="font-size:.9rem;color:#e6edf3;margin:0 0 4px">Tier access &amp; rate limits</h3>
<p class="ops-note" style="margin-top:0">Per-tier managed-queue limits. Enable/disable a tier and set tasks per hour / per day / concurrent. 0 means unlimited. Persists as <code>tier_limits</code> + <code>disabled_tiers</code>.</p>
<div id="admin-tiers"></div>

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

<!-- Two-region shell: a MAIN area (fleet / pipeline / queue / my-work) beside a
     dedicated full-height DEV-LOG RAIL (chat/notifications-panel style). The rail is
     collapsible; collapsing widens the main area to reclaim the space. Open by
     default; the collapse state persists in localStorage (hive.ops.devlog.collapsed)
     and is honoured on load. On narrow viewports the rail drops below the main area
     (see the .ops-shell media query) so the page never scrolls horizontally. -->
<div class="ops-shell" id="ops-shell">
<div class="ops-main">
<div class="ops-grid">
<div>
<div class="ops-card card-accent">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Connected clankers</h3><span class="ops-card-count count-strong" id="clanker-count"></span></div>
<!-- Army roster header: live count + at-a-glance status split, fed by the fleet snapshot. -->
<div class="cc-army" id="cc-army">
  <span style="color:#e6edf3;font-weight:600">Your army</span>
  <span class="cc-army-stat working"><span class="dot"></span><b id="cc-army-working">0</b>&nbsp;working</span>
  <span class="cc-army-stat reviewing"><span class="dot"></span><b id="cc-army-reviewing">0</b>&nbsp;reviewing</span>
  <span class="cc-army-stat idle"><span class="dot"></span><b id="cc-army-idle">0</b>&nbsp;idle</span>
</div>
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
<!-- Command center: MY WORK (this operator's in-flight items) stacked above the
     READY-WORK QUEUE (issues waiting to be picked off, top = next up), and the live
     DEV-LOG (a running chat log of the development, now in the rail). Both panels
     are fed by REAL events — the queue from ActionableIssues (the same set
     selectTask offers from), My work from the fleet snapshot. All read-only except
     the queue's owner/read-write drag-reorder. Panel order: My work first, then
     Ready-work queue — a pure vertical swap, no id/behavior change. -->
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
<div class="ops-card card-accent" style="margin-top:20px">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Ready-work queue</h3><span class="ops-card-count" id="queue-count"></span>
<!-- #queue-suspend-btn is the SAME logical control as the Management "Suspend
     contributions" switch (#admin-suspend-switch) — not a related toggle, the
     identical Config.Hub.ContributeSuspended state surfaced a second place. Both
     read/write through setContributeSuspended(), which is the single source of
     truth for the PUT + both render updates (see below). Hidden until initAdmin()
     resolves owner/read-write; a read viewer still sees the status pill next to it
     (#queue-suspend-pill), which is read-only info. -->
<span id="queue-suspend-wrap">
<span class="pill pill-passed" id="queue-suspend-pill" style="display:none">active</span>
<button type="button" class="queue-suspend-btn" id="queue-suspend-btn" title="Pause contributions" aria-label="Pause contributions" style="display:none"><span id="queue-suspend-icon"><svg viewBox="0 0 12 12" aria-hidden="true"><rect x="2.5" y="2" width="2.4" height="8" rx="0.6"/><rect x="7.1" y="2" width="2.4" height="8" rx="0.6"/></svg></span></button>
</span>
<span class="cc-live stale" id="cc-live"><span class="cc-live-dot"></span><span id="cc-live-label">connecting</span></span></div>
<!-- Playlist-style SEARCH (#2592). A pure VIEW filter over the loaded queue
     (repo / number / title / label, case-insensitive, live) — it never changes
     the persisted order, only what is shown. Read-only, so visible to everyone. -->
<div class="cc-q-search" id="cc-q-search-wrap">
  <span class="cc-q-search-ic" aria-hidden="true">&#x1F50D;</span>
  <input type="text" id="cc-q-search" placeholder="Filter queue by repo, number, title, or label&hellip;" aria-label="Filter the ready-work queue" autocomplete="off" spellcheck="false">
  <button type="button" class="cc-q-search-clear" id="cc-q-search-clear" aria-label="Clear filter" title="Clear filter">&times;</button>
</div>
<div class="cc-q-filternote" id="cc-q-filternote" style="display:none"></div>
<div class="cc-queue" id="cc-queue"><div class="ops-empty">Loading queue&hellip;</div></div>
<!-- End-of-queue block (#2595): a calm "all caught up" marker + the hive's managed
     rate-limit settings + the viewer's daily quota. Rendered by ccRenderQueueEnd()
     only when the FULL queue is shown (no active filter). Hidden until hydrated. -->
<div id="cc-q-end" style="display:none"></div>
<p class="ops-note" style="padding:10px 20px 14px;margin:0">The stack of admissible issues waiting to be picked off &mdash; top is next up. When a clanker grabs one you&rsquo;ll see it fly from here to that clanker. Derived from this hive&rsquo;s actionable backlog; read-only.</p>
</div>
<!-- Opportunistic Work (#2592): a small, CALM discovery panel of admissible
     issues NOT already at the top of the queue, ranked by a light recency heat
     proxy. Read-only for everyone; the per-item "add to queue" pins it into the
     operator order (owner/read-write only, rendered only when adminEnabled). -->
<div class="ops-card" id="opp-card" style="margin-top:20px">
<div class="ops-card-head"><h3>Opportunistic work</h3><span class="ops-card-count" id="opp-count"></span></div>
<div class="opp-list" id="opp-list"><div class="ops-empty">Looking for fresh work&hellip;</div></div>
<p class="ops-note" style="padding:10px 20px 14px;margin:0">A light, calm read of fresh, actionable issues beyond what&rsquo;s already lined up &mdash; surfaced by recency, not a heavy recommender. Owner/read-write operators can add one to the queue; it becomes offer-priority only and still obeys every admission filter.</p>
</div>
</div>
</div>
</div>
<!-- Dedicated full-height LIVE ACTIVITY RAIL. Holds ONLY the live activity feed
     (moved here out of the former right column). Named "Live Activity" to match
     the identically-sourced feed on the Onboarding tab (#2591 — was "Development
     log", now consistent). The rail edge carries a collapse toggle; when
     collapsed the rail shrinks to a thin strip with a "show log" affordance and the
     main area reflows to fill the reclaimed width. The SSE feed, narrated lines,
     fade/slide-in animation, scrollback cap, empty state and the live status pill
     are unchanged — they were only relocated. aria-expanded on the toggle tracks
     the open/collapsed state for assistive tech. -->
<aside class="ops-rail" id="ops-rail" aria-label="Live Activity">
<button type="button" class="ops-rail-toggle" id="ops-rail-toggle" aria-expanded="true" aria-controls="ops-rail" title="Collapse log">
  <span class="ops-rail-chevron" aria-hidden="true">&rsaquo;</span>
  <span class="ops-rail-toggle-label">Log</span>
</button>
<div class="ops-rail-inner">
<div class="ops-rail-head">
  <span class="feed-dot"></span>
  <h3>Live Activity</h3>
  <span class="ops-card-count" id="cc-log-count"></span>
  <span class="cc-live stale" id="cc-live-rail" title="Live feed status"><span class="cc-live-dot"></span><span id="cc-live-rail-label">connecting</span></span>
</div>
<div class="cc-log" id="cc-log"><div class="ops-empty">Watching the hive&hellip;</div></div>
</div>
</aside>
</div>
</div>
</div>
<!-- Leaderboard tab — inline, read-only view of the contributor + agent
     leaderboard. Reuses GET /api/leaderboard (the SAME endpoint the standalone
     /leaderboard page renders from); hydrated client-side on first tab open,
     matching how the Operations tab hydrates via /api/contribute/fleet. No
     controls, no role gating — everyone (including read viewers) sees it. The
     standalone /leaderboard page is preserved for external/bookmarked use. -->
<div class="tab-panel" id="tab-leaderboard" role="tabpanel" aria-labelledby="ptab-leaderboard">
<div class="ops">
<h1>Leaderboard</h1>
<p class="subtitle" style="font-size:.95rem">Ranked by tasks completed. Human contributors and donated-compute contributors appear here; the hive&rsquo;s own internal agents and revoked contributors are excluded.</p>
<!-- Personal "Me" card. Pinned ABOVE the full standings. Hydrated from the HUB
     profile endpoint (/api/leaderboard/contributor/{username}) once we know the
     logged-in username (from /api/gh-user-auth/status). For an anonymous / unknown
     viewer it renders a subtle sign-in prompt instead of erroring. The full
     standings still render below, unchanged. -->
<div id="me-card-mount"></div>
<div class="ops-card card-accent">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Rankings</h3><span class="ops-card-count count-strong" id="leaderboard-count"></span></div>
<div id="leaderboard-list"><div class="ops-empty">Loading leaderboard&hellip;</div></div>
</div>
</div>
</div>
<!-- "File an issue on this page" (#2594). A subtle footer present on EVERY tab.
     Just an outbound link (CSP-safe, no fetch) to GitHub's new-issue form, pre-
     filled TAB-AWARE with which /contribute surface and the current URL so the
     maintainer knows exactly where the report is about. href is (re)built in JS on
     load + tab change; a static fallback href points at the bare new-issue form so
     the link works even if JS never runs. Public — everyone can file an issue. -->
<footer class="cc-page-foot">
  <a id="cc-report-link" class="cc-report-link" target="_blank" rel="noopener noreferrer"
     href="https://github.com/kubestellar/hive/issues/new?labels=enhancement">
    <span class="cc-report-ic" aria-hidden="true">&#x1F41B;</span>
    <span>Report an issue with this page</span>
  </a>
</footer>
<script>
(function(){
// ── Init-order hoist (fixes the #2603/#2604/#2606 merge-interleaving regression) ──
// These were declared FAR below their first use. var hoists the name but not the
// value, so ADMIN_TIER_ORDER.map / ccActivitySeen[k] / ccActivity.length ran against
// undefined and threw. Declaring+initializing them here — before any function that
// uses them can run — makes the ordering explicit and regression-proof.
var ADMIN_TIER_ORDER=['newcomer','contributor','trusted','advisor'];
var ccActivity=[];       // chronological (oldest→newest) activity backlog for the rail
var ccActivitySeen={};   // dedupe set keyed by ccActivityKey(); shared by poll + SSE
// Null-guarded addEventListener: a missing element (not yet parsed, or a markup
// change) must never throw at script-eval time — an uncaught throw here aborts the
// rest of this inline block and un-initializes everything below it (the exact crash
// this file is fixing). Returns silently if the id is absent.
function onEl(id,ev,fn,opts){var el=document.getElementById(id);if(el)el.addEventListener(ev,fn,opts);}
// Tab switching for the /contribute page. Additive: leaves onboarding intact.
var tabs=document.querySelectorAll('.page-tab');
var panels=document.querySelectorAll('.tab-panel');
var opsStarted=false;   // Operations fleet polling started
var adminStarted=false; // /api/role gate resolved (adminEnabled set)
var lbStarted=false;    // Leaderboard hydrated (fetches /api/leaderboard once)
// The role gate now backs controls in BOTH tabs: the admin block under Management
// AND the per-clanker trust/revoke/remove buttons under Operations. So initAdmin()
// must run when EITHER tab is first opened — otherwise a viewer who lands straight
// on Operations would never resolve their role and would lose the per-row controls.
// It is idempotent (fetches /api/role once) and independent of opsPoll().
// activateTab drives both a user click and the deep-link path (/leaderboard →
// /contribute?tab=leaderboard) through the SAME show/hide + hydration logic.
// Canonical URL scheme: each tab is a real, shareable path under /contribute.
//   Onboarding  -> /contribute            (bare; the default landing)
//   Management  -> /contribute/management
//   Operations  -> /contribute/operations
//   Leaderboard -> /contribute/leaderboard
// PANEL_SLUG maps each panel id to its clean path slug; SLUG_PANEL maps every
// accepted friendly name / short id BACK to a panel id, so both the clean name
// (management/operations) and the legacy short id (manage/ops) deep-link, and
// the pre-existing ?tab=leaderboard query form keeps working. Onboarding has no
// slug: it lives at the bare /contribute.
var PANEL_SLUG={'tab-manage':'management','tab-ops':'operations','tab-leaderboard':'leaderboard'};
var SLUG_PANEL={
  'onboarding':'tab-onboarding',
  'management':'tab-manage','manage':'tab-manage',
  'operations':'tab-ops','ops':'tab-ops',
  'leaderboard':'tab-leaderboard'
};
// Resolve a friendly name / short id to the tab BUTTON element (id ptab-*), or
// null if it names no real tab. panelToButtonId turns a data-panel value back
// into its button id (tab-manage -> ptab-manage) — the buttons keep the short
// suffix, so we strip the leading "tab-".
function panelToButtonId(dp){return 'ptab-'+dp.replace(/^tab-/,'');}
function buttonForName(name){
  if(!name)return null;
  var dp=SLUG_PANEL[name.toLowerCase()];
  if(!dp)return null;
  return document.getElementById(panelToButtonId(dp));
}
// activateTab shows/hides panels, fires lazy hydration, and (unless push===false)
// reflects the active tab in the address bar via history.pushState — no reload.
// popstate-driven activations pass push===false so Back/Forward do NOT push a new
// history entry (which would create a loop / trap the user).
function activateTab(t,push){
  if(!t)return;
  tabs.forEach(function(x){x.classList.remove('active');x.setAttribute('aria-selected','false');});
  panels.forEach(function(p){p.classList.remove('active');});
  t.classList.add('active');t.setAttribute('aria-selected','true');
  var dp=t.getAttribute('data-panel');
  var panel=document.getElementById(dp);
  if(panel)panel.classList.add('active');
  if((dp==='tab-ops'||dp==='tab-manage')&&!adminStarted){adminStarted=true;initAdmin();}
  // opsPoll() (fleet/policy/work hydration) and ccStart() (the SSE command center)
  // are INDEPENDENT: a throw in one must never prevent the other from running. The
  // fleet panels predate the command center, so a command-center start failure must
  // not leave Connected clankers / Pipeline & policy / My work stuck on "Loading…"
  // (regression #2574). Each is guarded on its own.
  if(dp==='tab-ops'&&!opsStarted){opsStarted=true;
    try{opsPoll();}catch(e){console.error('opsPoll start failed',e);}
    try{ccStart();}catch(e){console.error('ccStart failed',e);}
    // The dev-log rail collapse/persist wiring is independent too: a throw here must
    // not abort fleet hydration or the SSE feed.
    try{initOpsRail();}catch(e){console.error('initOpsRail failed',e);}
  }
  // Leaderboard hydrates client-side on first open — read-only, no role gate.
  // The personal "Me" card loads alongside the standings; a throw in one must
  // never block the other, so each is guarded independently.
  if(dp==='tab-leaderboard'&&!lbStarted){lbStarted=true;
    try{loadLeaderboard();}catch(e){console.error('loadLeaderboard failed',e);}
    try{loadMeCard();}catch(e){console.error('loadMeCard failed',e);}
  }
  // Reflect the visible tab in the URL. pushState only — never a reload. Skipped
  // when push===false (popstate replay) so we don't stack duplicate history entries.
  if(push!==false&&window.history&&window.history.pushState){
    var slug=PANEL_SLUG[dp];
    var url=slug?('/contribute/'+slug):'/contribute';
    if(url!==window.location.pathname){
      try{window.history.pushState({tab:dp},'',url);}catch(e){/* pushState may throw on file:// etc. */}
    }
  }
  // Keep the "file an issue" link TAB-AWARE: refresh its prefill so the report names
  // the surface the user is now on. Guarded — a missing link never blocks tab logic.
  try{ccUpdateReportLink(dp);}catch(e){}
}
// ── "File an issue on this page" (#2594) ───────────────────────────────────────
// Build a github.com/kubestellar/hive/issues/new URL pre-filled with WHICH tab the
// report is about + the current page URL, so the maintainer knows the exact
// surface. Uses ONLY the existing enhancement label (never a non-existent
// "contribute" label — the #2536/#2540 regression). Rebuilt on load + every tab
// change. Pure link building; no fetch, CSP-safe.
var REPORT_TAB_NAME={'tab-onboarding':'onboarding','tab-ops':'operations','tab-manage':'management','tab-leaderboard':'leaderboard'};
function ccUpdateReportLink(dp){
  var a=document.getElementById('cc-report-link');if(!a)return;
  var tabName=REPORT_TAB_NAME[dp]||'onboarding';
  var title='contribute: '+tabName+' — ';
  var href='';try{href=window.location.href;}catch(e){href='';}
  var body='Reporting an issue with the /contribute page.\n\nPage/tab: '+tabName+'\nURL: '+href+'\n\n---\n\nWhat happened / what would you like to see?\n';
  var url='https://github.com/kubestellar/hive/issues/new?labels=enhancement'+
    '&title='+encodeURIComponent(title)+
    '&body='+encodeURIComponent(body);
  a.setAttribute('href',url);
}
// Click never needs to be told to push (default push===true).
tabs.forEach(function(t){t.addEventListener('click',function(){activateTab(t);});});
// Onboarding CTA opens the Leaderboard tab in place (no navigate-away).
var gotoLb=document.getElementById('goto-leaderboard-tab');
if(gotoLb)gotoLb.addEventListener('click',function(){activateTab(document.getElementById('ptab-leaderboard'));});
// Deep link on load: prefer the path form (/contribute/<tab>), fall back to the
// legacy ?tab=<name> query form (kept for back-compat with old bookmarks and the
// /leaderboard shim). tabFromLocation returns the matching button or null.
// Because we activate WITHOUT pushing here (the URL already IS the target), the
// address bar is left exactly as the user arrived — no history churn on load.
function tabFromLocation(){
  var seg=/^\/contribute\/([^\/?#]+)/.exec(window.location.pathname);
  if(seg){var b=buttonForName(decodeURIComponent(seg[1]));if(b)return b;}
  var m=/[?&]tab=([^&]+)/.exec(window.location.search);
  if(m){var b2=buttonForName(decodeURIComponent(m[1]));if(b2)return b2;}
  return null;
}
(function(){
  var target=tabFromLocation();
  // Absent/unknown tab -> default (Onboarding) stays active. Activate without
  // pushing so we never add a spurious history entry for the initial page.
  if(target)activateTab(target,false);
  // Surface the trusted-invite banner if we arrived via ?invite=<token> (#2598).
  try{initInviteBanner();}catch(e){console.error('initInviteBanner failed',e);}
  // Build the tab-aware "file an issue" link on load even when we DON'T activate
  // (bare /contribute = onboarding, no activateTab call). Guarded.
  try{ccUpdateReportLink((target&&target.getAttribute('data-panel'))||'tab-onboarding');}catch(e){}
})();
// Back/Forward: re-derive the tab from the (now-updated) location and activate it
// WITHOUT pushing — popstate already moved history, a push here would loop. When
// the path/param names no tab (e.g. Back to bare /contribute), fall to Onboarding.
window.addEventListener('popstate',function(){
  var target=tabFromLocation()||document.getElementById('ptab-onboarding');
  activateTab(target,false);
});

// loadLeaderboard hydrates the Leaderboard tab from GET /api/leaderboard — the
// SAME endpoint the (now-folded) standalone page used. Response shape:
// {leaderboard:[...contributors], agents:[...]}. Agents rank first (as the old
// standalone page did), then contributors; both sorted by tasks completed.
function loadLeaderboard(){
  fetch('/api/leaderboard').then(function(r){return r.json();}).then(function(d){
    // #2601: the Rankings show CONTRIBUTORS only — the hive's own internal agents
    // (scanner/supervisor/quality/… returned in d.agents) are filtered OUT so real
    // human + donated-compute contributors are not buried under the bots.
    var contribs=(d&&d.leaderboard)||[];
    renderLeaderboard(contribs);
  }).catch(function(){
    var el=document.getElementById('leaderboard-list');
    if(el)el.innerHTML='<div class="ops-empty">Could not load leaderboard.</div>';
  });
}
// tierBadge renders a small tier medallion / rank badge from a REAL trust tier.
// The four known tiers each get a muted metal-ish accent class; an unknown/blank
// tier is treated as newcomer (neutral). extraCls lets callers request the compact
// leaderboard/inline variants. Nothing here is fabricated — it is a pure visual
// wrap around the tier string the leaderboard/fleet snapshot already carries.
function tierBadge(tier,extraCls){
  var known={newcomer:1,contributor:1,trusted:1,advisor:1};
  var t=String(tier||'').toLowerCase();
  if(!known[t])t='newcomer';
  return '<span class="tier-badge tier-'+t+(extraCls?(' '+extraCls):'')+'">'+esc(t)+'</span>';
}
// ccMeUsername is the logged-in viewer's GitHub username (resolved once from
// /api/gh-user-auth/status, the SAME source the Me card uses). Empty when anonymous.
// Used to SUBTLY highlight the viewer's own row in the Rankings list.
var ccMeUsername='';
function lbRow(e,rank){
  var uname=e.github_username||'';
  var name=esc(uname);
  var badge=e.is_agent?(esc(e.emoji||'\u{1F916}')+' '):'';
  // Tier medallion driven by the REAL trust_tier the /api/leaderboard entry carries.
  var tier=tierBadge(e.trust_tier,'tier-lb');
  var done=(e.tasks_completed||0);
  var failed=(e.tasks_failed||0);
  var findings=(e.findings||0);
  // Subtle self-highlight: the viewer's OWN row (username match, case-insensitive,
  // humans only — never an agent) gets .lb-row--me + a small "you" chip. Anonymous
  // viewers match nothing, so no row is highlighted.
  var isMe=(!e.is_agent&&ccMeUsername&&uname&&uname.toLowerCase()===ccMeUsername.toLowerCase());
  var youChip=isMe?' <span class="lb-you">you</span>':'';
  // "Done" (tasks_completed) is the hero numeral — real count, just emphasised.
  return '<div class="lb-row'+(isMe?' lb-row--me':'')+'">'
    +'<div class="lb-rank">#'+rank+'</div>'
    +'<div class="lb-name">'+badge+name+youChip+'</div>'
    +'<div class="lb-tier">'+tier+'</div>'
    +'<div class="lb-stat lb-primary">'+done+'</div>'
    +'<div class="lb-stat">'+failed+'</div>'
    +'<div class="lb-stat">'+findings+'</div>'
    +'</div>';
}
var lbLastData=null; // cache the last standings so a late username resolve can re-mark the me-row
// renderLeaderboard renders the Rankings from CONTRIBUTORS ONLY (#2601). The hive's
// own internal agents are excluded so real human + donated-compute contributors are
// not buried under the bots. Defensive: any is_agent entry that somehow reaches here
// is filtered out, so ranks are computed among contributors alone (the Me-card rank,
// which comes from the SAME contributor-only buildLeaderboard(), stays consistent).
function renderLeaderboard(contribs){
  contribs=(contribs||[]).filter(function(e){return !e.is_agent;});
  lbLastData={contribs:contribs};
  var el=document.getElementById('leaderboard-list');
  var cnt=document.getElementById('leaderboard-count');
  var total=contribs.length;
  if(cnt)cnt.textContent=total+(total===1?' contributor':' contributors');
  if(!el)return;
  if(total===0){el.innerHTML='<div class="ops-empty">No contributors yet — be the first to contribute!</div>';return;}
  var html='<div class="lb-head lb-row"><div class="lb-rank">#</div><div class="lb-name">Contributor</div><div class="lb-tier">Tier</div><div class="lb-stat lb-primary">Done</div><div class="lb-stat">Failed</div><div class="lb-stat">Findings</div></div>';
  var rank=0,i;
  for(i=0;i<contribs.length;i++){rank++;html+=lbRow(contribs[i],rank);}
  el.innerHTML=html;
}

// ── Personal "Me" card ───────────────────────────────────────────────────────
// Resolve the logged-in username from the SAME identity source the page already
// uses (/api/gh-user-auth/status), then fetch the CENTRAL hub profile endpoint
// and render a pride-forward personal card pinned above the standings. Anonymous
// or unknown viewers get a subtle sign-in prompt — never an error. All data is
// real (tier / stats / milestones / hives / rank come straight from the hub).
var ME_STYLE_KEY='hive.me.cardStyle';   // localStorage key for the chosen skin
var ME_STYLE_COUNT=7;                    // number of profile-style skins offered
var ME_STYLE_NAMES=['Signal blue','Verdant','Amber rank','Violet advisor','Minimal','Rose','Roomy ranked'];
// Milestone id -> the Credly badge it WOULD map to once badges go live (Part C).
// This is a static mapping surfaced as a "coming soon" placeholder — no external
// call is made. See v2/docs/credly-badges.md for the real integration design.
var ME_CREDLY_MAP={
  'tier-contributor':'Contributor badge',
  'tier-trusted':'Trusted badge',
  'tier-advisor':'Advisor badge',
  'tasks-25':'25-task milestone badge',
  'tasks-100':'100-task milestone badge'
};

function meStyleClass(){
  var n=parseInt(localStorage.getItem(ME_STYLE_KEY)||'1',10);
  if(!(n>=1&&n<=ME_STYLE_COUNT))n=1;
  return n;
}

function loadMeCard(){
  var mount=document.getElementById('me-card-mount');
  if(!mount)return;
  fetch('/api/gh-user-auth/status').then(function(r){return r.json();}).then(function(auth){
    if(!auth||!auth.logged_in||!auth.username){renderMeSignIn(mount);return;}
    var u=auth.username;
    // Remember the viewer's username so the Rankings list can highlight their own
    // row. If the standings already rendered (race), re-render to apply the mark.
    if(ccMeUsername!==u){ccMeUsername=u;if(typeof lbLastData!=='undefined'&&lbLastData){try{renderLeaderboard(lbLastData.contribs);}catch(e){}}}
    fetch('/api/leaderboard/contributor/'+encodeURIComponent(u)).then(function(r){return r.json();}).then(function(p){
      if(!p||!p.found){renderMeSignIn(mount,u);return;}
      renderMeCard(mount,p);
    }).catch(function(){renderMeSignIn(mount,u);});
  }).catch(function(){renderMeSignIn(mount);});
}

// Anonymous / not-yet-a-contributor: a subtle prompt, not an error.
function renderMeSignIn(mount,username){
  var msg=username
    ?('<b>'+esc(username)+'</b>, you don’t have a contributor profile on this hive yet. Ship a task to start your card.')
    :'<b>Sign in</b> to see your personal contributor profile — your rank, milestones, and hives.';
  mount.innerHTML='<div class="me-signin">'+msg+'</div>';
}

// Build the pre-filled, no-OAuth LinkedIn share URL from REAL achievement data.
// It opens LinkedIn's share dialog pre-populated; no LinkedIn API, no credentials.
function meLinkedInURL(p){
  var tier=p.trust_tier?(p.trust_tier.charAt(0).toUpperCase()+p.trust_tier.slice(1)):'';
  var prs=p.tasks_with_pr||0;
  var org=(p.hives&&p.hives[0]&&p.hives[0].org)||'';
  var text='I’ve shipped '+prs+' merged PR'+(prs===1?'':'s')
    +(tier?(' as a '+tier+' contributor'):' as a contributor')
    +(org?(' on '+org):'')+' via the Hive contributor program.';
  return 'https://www.linkedin.com/sharing/share-offsite/?url='
    +encodeURIComponent(window.location.origin+'/contribute/leaderboard')
    +'&summary='+encodeURIComponent(text);
}

function meMilestoneChips(p){
  var got=[],next='';
  var ms=(p.milestones||[]);
  for(var i=0;i<ms.length;i++){
    if(ms[i].attained){
      got.push('<span class="me-chip me-chip--got" title="'+esc(ms[i].detail||'')+'">'
        +(ms[i].icon?esc(ms[i].icon)+' ':'')+esc(ms[i].label)+'</span>');
    }
  }
  if(p.next_milestone){
    var nm=p.next_milestone;
    var toGo='';
    // "X to go" progression cue, computed from the real threshold vs real count.
    if(nm.id&&nm.id.indexOf('tasks-')===0){var gap=nm.value-(p.tasks_completed||0);if(gap>0)toGo=' — '+gap+' to go';}
    else if(nm.id==='tier-contributor'){var g2=nm.value-(p.tasks_with_pr||0);if(g2>0)toGo=' — '+g2+' PR-tasks to go';}
    else if(nm.id==='tier-trusted'){var g3=nm.value-(p.tasks_with_pr||0);if(g3>0)toGo=' — '+g3+' PR-tasks to go';}
    next='<span class="me-chip me-chip--next" title="'+esc(nm.detail||'')+'">○ '+esc(nm.label)+esc(toGo)+'</span>';
  }
  if(!got.length&&!next)return '<span class="me-chip">No milestones yet — ship your first task</span>';
  return got.join('')+next;
}

function meCredlyChips(p){
  // Placeholder / "coming soon" mapping: which attained milestones WOULD yield
  // which Credly badge. No external Credly call is made this round.
  var out=[],seen={},ms=(p.milestones||[]);
  for(var i=0;i<ms.length;i++){
    if(ms[i].attained&&ME_CREDLY_MAP[ms[i].id]&&!seen[ms[i].id]){
      seen[ms[i].id]=1;
      out.push('<span class="me-chip me-chip--badge" title="Planned Credly badge (not yet issued)">◇ '+esc(ME_CREDLY_MAP[ms[i].id])+'</span>');
    }
  }
  if(!out.length)out.push('<span class="me-chip me-chip--badge">Earn a tier or milestone to unlock a badge</span>');
  return out.join('');
}

function meHivesRows(p){
  var hs=(p.hives||[]);
  if(!hs.length)return '<div class="me-hive">No federated hives registered yet.</div>';
  var out=[];
  for(var i=0;i<hs.length;i++){
    var rel=hs[i].relationship||'contributor';
    var relCls=(rel==='owner')?'me-hive__rel me-hive__rel--owner':'me-hive__rel';
    var verb=(rel==='owner')?'Owner of':(rel==='member'?'Member of':'Contributing to');
    var label=esc(hs[i].project_name||hs[i].org||hs[i].id||'hive');
    out.push('<div class="me-hive"><span class="'+relCls+'">'+esc(rel)+'</span>'
      +'<span>'+esc(verb)+' <b style="color:#e6edf3">'+label+'</b>'
      +(hs[i].org?(' <span style="color:#8b949e">('+esc(hs[i].org)+')</span>'):'')+'</span></div>');
  }
  return out.join('');
}

// ── Trusted invite (issue #2598) ─────────────────────────────────────────────
// Trust tiers permitted to invite. Kept in sync with the server's
// inviteTrustTiers gate; the UI hiding here is UX only — the /api/contribute/
// invite endpoint independently verifies the caller's tier and 403s otherwise.
var INVITE_TIERS={trusted:true,advisor:true};

// meInviteSection renders the "Invite someone to contribute" affordance ONLY for
// a viewer whose real trust tier is trusted/advisor. Any other tier (newcomer /
// contributor) — and anonymous viewers never reach renderMeCard — get nothing.
function meInviteSection(p){
  if(!p||!INVITE_TIERS[p.trust_tier])return '';
  return '<div class="me-invite">'
    +'<button type="button" class="me-invite__btn" id="me-invite-btn">✉️ Invite someone to contribute</button>'
    +'<div class="me-invite__row" id="me-invite-row">'
      +'<input type="text" class="me-invite__link" id="me-invite-link" readonly aria-label="Invite link" value="">'
      +'<button type="button" class="me-invite__copy" id="me-invite-copy">Copy</button>'
      +'<div class="me-invite__hint">Anyone who signs up through this link joins as a <b>newcomer</b>, credited to you. The link is trusted-only and expires.</div>'
    +'</div>'
  +'</div>';
}

// wireMeInvite hooks up the invite button: it asks the server to MINT a link
// (which re-checks the caller's tier server-side), shows it, and offers copy.
function wireMeInvite(){
  var btn=document.getElementById('me-invite-btn');
  if(!btn)return;
  var row=document.getElementById('me-invite-row');
  var input=document.getElementById('me-invite-link');
  var copy=document.getElementById('me-invite-copy');
  btn.addEventListener('click',function(){
    btn.disabled=true;
    fetch('/api/contribute/invite',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'})
      .then(function(r){return r.json().then(function(j){return {ok:r.ok,j:j};});})
      .then(function(res){
        btn.disabled=false;
        if(!res.ok||!res.j||!res.j.invite_url){toast((res.j&&res.j.error)||'Could not create invite link',false);return;}
        input.value=res.j.invite_url;
        row.classList.add('open');
        input.focus();input.select();
      }).catch(function(){btn.disabled=false;toast('Could not create invite link',false);});
  });
  if(copy)copy.addEventListener('click',function(){
    if(!input.value)return;
    input.select();
    var done=function(){toast('Invite link copied',true);};
    if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(input.value).then(done,function(){try{document.execCommand('copy');done();}catch(e){}});}
    else{try{document.execCommand('copy');done();}catch(e){}}
  });
}

// initInviteBanner shows a subtle banner on the onboarding tab when the visitor
// arrived via an attributed invite link (?invite=<token>). It is informational
// only — the actual attribution is recorded server-side at registration. It
// stashes the token so an in-page register flow can forward it; it makes clear
// the invitee joins as a newcomer. No token is decoded client-side (opaque).
function initInviteBanner(){
  var banner=document.getElementById('invite-banner');
  if(!banner)return;
  var token='';
  try{token=new URLSearchParams(window.location.search).get('invite')||'';}catch(e){token='';}
  if(!token)return;
  try{window.sessionStorage.setItem('hive.invite',token);}catch(e){}
  banner.innerHTML='<b>You were invited to contribute.</b> Follow the setup below to join &mdash; '
    +'you’ll come in as a <b>newcomer</b>, credited to whoever invited you. '
    +'<span class="invite-tier">The invite records attribution only; it does not change your tier.</span>';
  banner.hidden=false;
}

function renderMeCard(mount,p){
  var tier=p.trust_tier||'newcomer';
  var tierNice=tier.charAt(0).toUpperCase()+tier.slice(1);
  var rankTxt=(p.rank&&p.total)?('#'+p.rank):'—';
  var rankSub=(p.rank&&p.total)?('of '+p.total):'unranked';
  var avatar=p.avatar_url||('https://github.com/'+encodeURIComponent(p.github_username)+'.png');
  var styleN=meStyleClass();
  var lead='You’re a <b>'+esc(tierNice)+'</b> contributor'
    +((p.rank&&p.total)?(' — ranked #'+p.rank+' of '+p.total+' on this hive'):'')+'.';

  var styleOpts='';
  for(var i=1;i<=ME_STYLE_COUNT;i++){
    styleOpts+='<option value="'+i+'"'+(i===styleN?' selected':'')+'>'+esc(ME_STYLE_NAMES[i-1]||('Style '+i))+'</option>';
  }

  var html=''
  +'<div class="me-card me-card--style'+styleN+'" id="me-card">'
  +'<div class="me-card__band"><div class="me-card__toprow">'
  +'<div class="me-medallion"><img src="'+esc(avatar)+'" alt="" onerror="this.style.visibility=\'hidden\'"></div>'
  +'<div class="me-id"><div class="me-id__name">'+esc(p.github_username)+'</div>'
    +'<div class="me-id__tier">'+tierBadge(p.trust_tier,'tier-hero')+'</div>'
    +'<div class="me-id__lead">'+lead+'</div></div>'
  +'<div class="me-rankpill"><div class="me-rankpill__num">'+esc(rankTxt)+'</div><div class="me-rankpill__lbl">'+esc(rankSub)+'</div></div>'
  +'</div></div>'
  +'<div class="me-card__body">'
  +'<div class="me-statgrid">'
    +'<div class="me-stat"><div class="lb-stat lb-primary">'+(p.tasks_completed||0)+'</div><div class="me-stat__lbl">Shipped</div></div>'
    +'<div class="me-stat"><div class="lb-stat lb-primary">'+(p.tasks_with_pr||0)+'</div><div class="me-stat__lbl">With PR</div></div>'
    +'<div class="me-stat"><div class="lb-stat lb-primary">'+(p.tasks_failed||0)+'</div><div class="me-stat__lbl">Failed</div></div>'
  +'</div>'
  +'<div class="me-sec"><div class="me-sec__title">Daily quota</div><div class="me-quota-wrap" id="me-quota-slot"><div class="ops-note" style="margin:0">Loading your quota&hellip;</div></div></div>'
  +'<div class="me-sec"><div class="me-sec__title">Milestones unlocked</div><div class="me-chips">'+meMilestoneChips(p)+'</div></div>'
  +'<div class="me-sec"><div class="me-sec__title">My hives</div><div class="me-hives">'+meHivesRows(p)+'</div></div>'
  +'<div class="me-sec"><div class="me-sec__title">Badges <span class="me-soon">Credly · coming soon</span></div><div class="me-chips">'+meCredlyChips(p)+'</div>'
    +'<div class="ops-note">Verifiable Credly badges are planned. Once live, each tier / milestone issues a badge with Credly’s own native LinkedIn share. See the design doc for setup.</div></div>'
  +'<div class="me-actions">'
    +'<a class="me-share" href="'+esc(meLinkedInURL(p))+'" target="_blank" rel="noopener noreferrer">\u{1F4E3} Share achievement on LinkedIn</a>'
    +'<span class="me-stylepick" title="Personalize your card — more customization coming">Profile style <select id="me-style-select" aria-label="Profile style (more customization coming)">'+styleOpts+'</select></span>'
  +'</div>'
  +meInviteSection(p)
  +'</div></div>';
  mount.innerHTML=html;

  wireMeInvite();
  // #2595 daily-quota widget on the Me card: hydrate from the shared limits read
  // (viewer's real used_day vs their tier's max_per_day). Load lazily if not cached.
  if(typeof ccLimits!=='undefined'&&ccLimits!==null){try{ccRenderMeQuota();}catch(e){}}
  else if(typeof ccLoadLimits==='function'){try{ccLoadLimits();}catch(e){}}

  var sel=document.getElementById('me-style-select');
  if(sel)sel.addEventListener('change',function(){
    var v=parseInt(sel.value,10);if(!(v>=1&&v<=ME_STYLE_COUNT))v=1;
    localStorage.setItem(ME_STYLE_KEY,String(v));
    var card=document.getElementById('me-card');
    if(card){for(var k=1;k<=ME_STYLE_COUNT;k++)card.classList.remove('me-card--style'+k);card.classList.add('me-card--style'+v);}
  });
}

var currentFilter='all';
var lastWork=[];
document.querySelectorAll('.ops-filter').forEach(function(f){f.addEventListener('click',function(){
  document.querySelectorAll('.ops-filter').forEach(function(x){x.classList.remove('active');});
  f.classList.add('active');
  currentFilter=f.getAttribute('data-filter');
  renderWork(lastWork);
});});

function esc(s){var d=document.createElement('div');d.textContent=(s==null?'':String(s));return d.innerHTML;}

// ── Clickable GitHub issue/PR references (#2616) ────────────────────────────────
// The Operations tab shows plenty of "repo#number" references (ready-work queue,
// my-work, opportunistic-work, dev-log) but until now they were plain monospace
// text — Jorge's ask is to make them real, obvious links to GitHub so this page
// can actually be used to manage work, not just read about it.
//
// ccIssueURL prefers the item's own "url" field (the backend's canonical
// issue/PR link — ReadyQueueItem/OpportunisticItem both carry it) and only
// constructs a fallback when it's absent. GitHub's /issues/<n> route redirects
// to /pull/<n> automatically when the number is actually a PR, so the
// constructed fallback works for both without knowing which one it is.
function ccIssueURL(item){
  if(item&&item.url)return item.url;
  if(item&&item.repo&&item.number)return 'https://github.com/'+item.repo+'/issues/'+item.number;
  return '';
}
// ccIssueLinkHTML renders the repo#number reference as an <a> with an obvious
// "go to GitHub" affordance: link-blue text, an external-link glyph, and a
// title tooltip. stopPropagation on click/mousedown keeps the click from ever
// reaching a row's drag/select handlers (queue drag-reorder in particular) —
// opening the issue must never start a drag. Opens in a new tab; rel carries
// noopener+noreferrer since target=_blank hands the new tab a window.opener
// handle otherwise. Returns a plain esc'd span (no link) when no URL can be
// produced, so a malformed item never renders a dead/empty link.
function ccIssueLinkHTML(item,label,extraClass){
  var url=ccIssueURL(item);
  if(!url)return '<span class="'+(extraClass||'')+'">'+esc(label)+'</span>';
  return '<a class="cc-issue-link '+(extraClass||'')+'" href="'+esc(url)+'" target="_blank" rel="noopener noreferrer" '+
    'title="Open on GitHub" onclick="event.stopPropagation();" onmousedown="event.stopPropagation();">'+
    esc(label)+
    '<svg class="cc-issue-link-ic" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true" focusable="false">'+
    '<path fill="currentColor" d="M6.22 8.72a.75.75 0 0 0 1.06 1.06l5.22-5.22v1.69a.75.75 0 0 0 1.5 0v-3.5a.75.75 0 0 0-.75-.75h-3.5a.75.75 0 0 0 0 1.5h1.69L6.22 8.72Z"/>'+
    '<path fill="currentColor" d="M3.75 3A1.75 1.75 0 0 0 2 4.75v7.5c0 .966.784 1.75 1.75 1.75h7.5A1.75 1.75 0 0 0 13 12.25v-3.5a.75.75 0 0 0-1.5 0v3.5a.25.25 0 0 1-.25.25h-7.5a.25.25 0 0 1-.25-.25v-7.5a.25.25 0 0 1 .25-.25h3.5a.75.75 0 0 0 0-1.5h-3.5Z"/>'+
    '</svg></a>';
}
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
// The confirm-modal buttons (#admin-confirm-cancel / #admin-confirm-ok) are
// emitted AFTER this <script> block closes (see #admin-confirm-back near the end
// of the page), so at script-eval time getElementById returns null here. The old
// code called .addEventListener on that null directly, which THREW and ABORTED
// the rest of this inline block — which is where ADMIN_TIER_ORDER, ccActivity and
// ccActivitySeen are initialized — leaving them undefined for the whole page
// (empty Live Activity rail + Done-filter throwing every poll). Wire the buttons
// once the DOM has fully parsed (so the elements actually exist), and null-guard
// besides, so this block can never again abort mid-way.
function _wireConfirmModal(){
  var cancel=document.getElementById('admin-confirm-cancel');
  if(cancel)cancel.addEventListener('click',function(){var b=document.getElementById('admin-confirm-back');if(b)b.classList.remove('show');_confirmCb=null;});
  var ok=document.getElementById('admin-confirm-ok');
  if(ok)ok.addEventListener('click',function(){var cb=_confirmCb;var b=document.getElementById('admin-confirm-back');if(b)b.classList.remove('show');_confirmCb=null;if(cb)cb();});
}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',_wireConfirmModal);else _wireConfirmModal();

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

// renderAdminFilter renders one admission filter (Titles / Authors / Labels). The
// CANONICAL list field is contribute_deny_<kind> — despite the "deny" in the name it
// holds the filter's list in BOTH modes (the mode decides allow vs deny; see
// config.go: the *DenyTitles/*DenyAuthors/*DenyLabels fields hold the LIST regardless
// of mode, and the backend filter — LabelsFilterPasses/FilterPasses — reads ONLY that
// field). So we bind to contribute_deny_<kind> in every mode. Hardcoding a mismatch —
// an Allow-mode filter whose tags land in a field the backend never reads — was the
// empty-queue bug: allow-mode ran against an empty effective list and admitted
// NOTHING. kind is the plural noun ("titles"/"authors"/"labels").
function adminFilterListKey(kind){return 'contribute_deny_'+kind;}
function renderAdminFilter(fieldId,label,noun,modeKey,kind){
  var el=document.getElementById(fieldId);
  if(!el||!adminHub)return;
  var mode=(adminHub[modeKey]==='allow')?'allow':'deny';
  // Bind to the CANONICAL list field the backend actually reads, in every mode.
  var listKey=adminFilterListKey(kind);
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

// ── Repos-for-Contribute enable toggles (Governor Hub mirror) ──────────────────
// A repo is ENABLED unless it appears in disabled_repos. The toggle edits the
// disabled_repos list (the field the backend + Governor Hub both use). available_
// repos comes from the config GET (the live repo set). Owner/RW only (gated by the
// enclosing admin controls); persists via the same PUT as the other filters.
// NOTE: ADMIN_TIER_ORDER is declared+initialized at the top of this IIFE (init-order
// hoist) so renderAdminTierLimits() can never see it undefined.
function renderAdminRepos(){
  var el=document.getElementById('admin-repos');
  if(!el||!adminHub)return;
  var repos=adminHub.available_repos||[];
  var disabled=adminHub.disabled_repos||[];
  if(!repos.length){el.innerHTML='<span class="admin-toggle-sub">No repos known yet — they appear once the hive syncs its backlog.</span>';return;}
  el.innerHTML='<div class="admin-repos">'+repos.map(function(r){
    var off=disabled.indexOf(r)>=0;
    return '<span class="admin-repo"><span class="admin-switch'+(off?'':' on')+'" data-repo="'+esc(r)+'"></span><span class="admin-repo__name">'+esc(r)+'</span></span>';
  }).join('')+'</div>';
}
// ── Tier access & rate limits (Governor Hub mirror) ────────────────────────────
// Per-tier enable toggle + max_per_hour / max_per_day / max_concurrent. Enable maps
// to disabled_tiers (a tier is enabled unless listed there); the numerics map to
// tier_limits[tier]. Both persist via the same PUT.
function renderAdminTierLimits(){
  var el=document.getElementById('admin-tiers');
  if(!el||!adminHub)return;
  var limits=adminHub.tier_limits||{};
  var disabled=adminHub.disabled_tiers||[];
  var head='<div class="admin-tier admin-tier--head"><span class="admin-tier__col" style="text-align:left">Tier</span><span class="admin-tier__col">Per&nbsp;hr</span><span class="admin-tier__col">Per&nbsp;day</span><span class="admin-tier__col">Concurr.</span></div>';
  el.innerHTML=head+ADMIN_TIER_ORDER.map(function(t){
    var lim=limits[t]||{};
    var off=disabled.indexOf(t)>=0;
    var h=(lim.max_per_hour||0),d=(lim.max_per_day||0),c=(lim.max_concurrent||0);
    var dis=off?' disabled':'';
    return '<div class="admin-tier">'+
      '<div class="admin-tier__head"><span class="admin-switch'+(off?'':' on')+'" data-tier="'+esc(t)+'"></span><span class="admin-tier__name">'+esc(t)+'</span></div>'+
      '<input type="number" min="0" value="'+h+'" data-tier-field="max_per_hour" data-tier="'+esc(t)+'"'+dis+' aria-label="'+esc(t)+' max per hour">'+
      '<input type="number" min="0" value="'+d+'" data-tier-field="max_per_day" data-tier="'+esc(t)+'"'+dis+' aria-label="'+esc(t)+' max per day">'+
      '<input type="number" min="0" value="'+c+'" data-tier-field="max_concurrent" data-tier="'+esc(t)+'"'+dis+' aria-label="'+esc(t)+' max concurrent">'+
    '</div>';
  }).join('');
}

// renderQueueSuspendControl paints the Ready-work queue's play/pause button +
// status pill from the SAME contribute_suspended value the Management "Suspend
// contributions" switch reads (adminHub.contribute_suspended when available, or
// the read-only policy snapshot as a fallback for a viewer with no adminHub).
// There is no separate queue-local state — this is a pure render of one shared
// value, called from every place that value can change (renderAdminControls
// after a toggle/hub load, and renderPolicy on every opsPoll tick so a change
// made on the OTHER surface — or by another operator — shows up here too).
function renderQueueSuspendControl(suspended){
  var pill=document.getElementById('queue-suspend-pill');
  if(pill){
    pill.style.display='';
    pill.className='pill '+(suspended?'pill-blocked':'pill-passed');
    pill.textContent=suspended?'paused':'active';
  }
  var btn=document.getElementById('queue-suspend-btn');
  if(!btn)return;
  if(!adminEnabled){btn.style.display='none';return;} // read viewer: status pill only, no control
  btn.style.display='';
  btn.classList.toggle('paused',!!suspended);
  btn.title=suspended?'Resume contributions':'Pause contributions';
  btn.setAttribute('aria-label',btn.title);
  var icon=document.getElementById('queue-suspend-icon');
  // SVG glyphs (not bar/triangle chars) so both stay dead-center in the circle.
  if(icon)icon.innerHTML=suspended
    ?'<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M3.5 2.2v7.6a.6.6 0 0 0 .92.5l6-3.8a.6.6 0 0 0 0-1l-6-3.8a.6.6 0 0 0-.92.5Z"/></svg>' // play triangle
    :'<svg viewBox="0 0 12 12" aria-hidden="true"><rect x="2.5" y="2" width="2.4" height="8" rx="0.6"/><rect x="7.1" y="2" width="2.4" height="8" rx="0.6"/></svg>'; // pause bars
}

// setContributeSuspended is the SINGLE handler for the contribute_suspended
// toggle, shared by the Management "Suspend contributions" switch and the
// Ready-work queue play/pause button — they are the same logical control
// surfaced twice, not two independent toggles. It PUTs through the existing
// governor-hub endpoint (adminSaveHub) and, on success, updates adminHub and
// re-renders BOTH surfaces so they can never drift apart.
var contributeSuspendBusy=false;
function setContributeSuspended(next,source){
  if(contributeSuspendBusy)return;
  contributeSuspendBusy=true;
  adminSaveHub({contribute_suspended:next},next?'Contributions paused':'Contributions resumed').then(function(ok){
    contributeSuspendBusy=false;
    if(!ok)return;
    if(adminHub)adminHub.contribute_suspended=next;
    renderAdminControls();
    renderQueueSuspendControl(next);
  });
}

onEl('queue-suspend-btn','click',function(){
  if(!adminEnabled)return;
  var next=!(adminHub&&adminHub.contribute_suspended);
  setContributeSuspended(next,'queue');
});

function renderAdminControls(){
  if(!adminEnabled||!adminHub)return;
  // Immediate toggles.
  document.getElementById('admin-suspend-switch').classList.toggle('on',!!adminHub.contribute_suspended);
  document.getElementById('admin-suspend-switch').classList.toggle('danger',!!adminHub.contribute_suspended);
  document.getElementById('admin-skip-switch').classList.toggle('on',!!adminHub.contribute_skip_assigned_to_others);
  document.getElementById('admin-reject-switch').classList.toggle('on',!!adminHub.contribute_reject_unknown_models);
  renderQueueSuspendControl(!!adminHub.contribute_suspended);
  // Filters (mirror Governor Hub: titles/authors/labels + modes, allow-models).
  renderAdminFilter('admin-filter-titles','Titles','title','contribute_titles_mode','titles');
  renderAdminFilter('admin-filter-authors','Authors','author','contribute_authors_mode','authors');
  renderAdminFilter('admin-filter-labels','Labels','label','contribute_labels_mode','labels');
  renderAdminModels();
  renderAdminRepos();
  renderAdminTierLimits();
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
    // contribute_suspended is the SAME control as the queue play/pause button —
    // route both through the one shared handler so there is exactly one code
    // path that PUTs this field and exactly one place that renders both surfaces.
    if(key==='contribute_suspended'){setContributeSuspended(next,'management');return;}
    var patch={};patch[key]=next;
    adminSaveHub(patch,next?'Enabled '+key.replace(/_/g,' '):'Disabled '+key.replace(/_/g,' ')).then(function(ok){
      if(ok){adminHub[key]=next;renderAdminControls();}
    });
  });
}

// Delegated handlers for the filter editors (mode switch, add, remove) — mark
// dirty so nothing is sent until the operator clicks Save.
onEl('ops-admin','click',function(e){
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
  // Repo enable toggle: flip membership in disabled_repos (enabled == NOT listed).
  if(t.getAttribute&&t.getAttribute('data-repo')!==null&&t.classList&&t.classList.contains('admin-switch')){
    var repo=t.getAttribute('data-repo');
    var dr=(adminHub.disabled_repos||[]).slice();
    var ri=dr.indexOf(repo);
    if(ri>=0)dr.splice(ri,1);else dr.push(repo); // toggling ON removes from disabled
    adminHub.disabled_repos=dr;adminDirty=true;renderAdminControls();return;
  }
  // Tier enable toggle: flip membership in disabled_tiers (enabled == NOT listed).
  if(t.getAttribute&&t.getAttribute('data-tier')!==null&&t.classList&&t.classList.contains('admin-switch')){
    var tier=t.getAttribute('data-tier');
    var dt=(adminHub.disabled_tiers||[]).slice();
    var ti=dt.indexOf(tier);
    if(ti>=0)dt.splice(ti,1);else dt.push(tier);
    adminHub.disabled_tiers=dt;adminDirty=true;renderAdminControls();return;
  }
});
// Tier rate-limit numeric edits: update tier_limits[tier][field] and mark dirty. A
// separate 'input' handler (numbers change on input, not click). Non-negative ints;
// blank/NaN coerces to 0 (== unlimited), matching the backend's "<=0 = unlimited".
onEl('ops-admin','input',function(e){
  var t=e.target;
  if(!t.getAttribute||t.getAttribute('data-tier-field')===null||!adminHub)return;
  var tier=t.getAttribute('data-tier'),field=t.getAttribute('data-tier-field');
  var v=parseInt(t.value,10);if(isNaN(v)||v<0)v=0;
  var tl=adminHub.tier_limits||{};if(!tl[tier])tl[tier]={};
  tl[tier][field]=v;adminHub.tier_limits=tl;adminDirty=true;
  var save=document.getElementById('admin-save-btn');if(save)save.disabled=false;
});

onEl('admin-add-model','click',function(){
  var inp=document.getElementById('admin-allow-model-input');
  if(inp&&inp.value.trim()){adminHub.contribute_allow_models=(adminHub.contribute_allow_models||[]).concat([inp.value.trim()]);inp.value='';adminDirty=true;renderAdminControls();}
});

onEl('admin-save-btn','click',function(){
  if(!adminDirty||!adminHub)return;
  // Persist the mode + the CANONICAL list field (contribute_deny_*) for each filter.
  // The mode decides allow vs deny; the list lives in the deny-named field in BOTH
  // modes (that is the only field the backend filter reads). Also clear the legacy
  // allow_labels field so a stale migration remnant can never mask the real list
  // (the empty-queue bug: an allow-mode filter with a lingering allow_labels value
  // that the backend never reads). We send it explicitly emptied.
  var patch={
    contribute_titles_mode:adminHub.contribute_titles_mode||'deny',
    contribute_authors_mode:adminHub.contribute_authors_mode||'deny',
    contribute_labels_mode:adminHub.contribute_labels_mode||'deny',
    contribute_deny_titles:adminHub.contribute_deny_titles||[],
    contribute_deny_authors:adminHub.contribute_deny_authors||[],
    contribute_deny_labels:adminHub.contribute_deny_labels||[],
    contribute_allow_labels:[],
    contribute_allow_models:adminHub.contribute_allow_models||[],
    // Governor Hub mirror sections (#2562 parity): repos-for-contribute (as the
    // disabled_repos exclusion list) + per-tier access & rate limits.
    disabled_repos:adminHub.disabled_repos||[],
    disabled_tiers:adminHub.disabled_tiers||[],
    tier_limits:adminHub.tier_limits||{}
  };
  adminSaveHub(patch,'Admission &amp; hub settings saved').then(function(ok){if(ok){adminDirty=false;renderAdminControls();}});
});

// Per-contributor actions (delegated on the clanker list). Each calls an EXISTING
// endpoint; destructive ones go through the themed confirm.
onEl('clanker-list','change',function(e){
  var sel=e.target;
  if(!adminEnabled||sel.getAttribute('data-role')!=='tier')return;
  var cid=sel.getAttribute('data-cid'),tier=sel.value;
  fetch('/api/contributors/'+encodeURIComponent(cid)+'/trust',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({tier:tier})})
    .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
    .then(function(x){if(x.ok){toast('Trust tier set to '+tier,true);opsPoll();}else{toast((x.d&&x.d.error)||'Failed to set tier',false);}})
    .catch(function(){toast('Failed to set tier',false);});
});
onEl('clanker-list','click',function(e){
  var b=e.target;
  if(!adminEnabled||b.tagName!=='BUTTON')return;
  var role=b.getAttribute('data-role');
  if(role!=='revoke'&&role!=='remove'&&role!=='requeue')return;
  var cid=b.getAttribute('data-cid'),user=b.getAttribute('data-user')||'this contributor';
  if(role==='requeue'){
    // #2568: manual requeue — release the in-flight task back to the ready queue.
    // Explicitly NOT destructive to the contributor (no revoke/remove); the task is
    // returned to the queue and booked for the same short cooldown as an auto-release,
    // so it is not instantly re-handed to a stale worker. Uses the existing
    // POST /api/contributors/{id}/requeue endpoint (owner/read-write only).
    adminConfirm('Requeue '+user+'&rsquo;s task','Release the task '+user+' is currently holding back to the ready queue. Use this when a connected clanker is wedged (connected but not progressing). The task is booked for the same short cooldown as an automatic release, so it is not instantly re-assigned to a stale worker. This uses the existing POST /api/contributors/{id}/requeue endpoint.','Requeue',function(){
      fetch('/api/contributors/'+encodeURIComponent(cid)+'/requeue',{method:'POST'})
        .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
        .then(function(x){if(x.ok){toast('Task requeued for '+user,true);opsPoll();}else{toast((x.d&&x.d.error)||'Requeue failed',false);}})
        .catch(function(){toast('Requeue failed',false);});
    });
    return;
  }
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
  // The ready-work queue may have rendered before this role check resolved (SSE
  // hello / poll fires immediately on tab open). Re-render now that adminEnabled is
  // true so the grab bars appear for this owner/read-write viewer.
  if(typeof ccRenderQueue==='function')ccRenderQueue();
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
  list=list||[];
  var el=document.getElementById('clanker-list');
  var cnt=document.getElementById('clanker-count');
  if(cnt)cnt.textContent=(list.length)+(list.length===1?' connected':' connected');
  // Update the "your army" roster FIRST and independently of the row render. The
  // roster (working/reviewing/idle) is derived from the same snapshot, so it must
  // hydrate even if building an individual clanker row throws — otherwise a single
  // malformed row leaves BOTH the list on "Loading…" AND the roster at 0/0/0
  // (regression #2574: the exact live symptom). ccUpdateArmy is itself nil-safe.
  ccUpdateArmy(list);
  if(!el)return;
  if(!list.length){el.innerHTML='<div class="ops-empty">No clankers connected right now.</div>';return;}
  el.innerHTML=list.map(function(c){
    var user=c.github_username||c.contributor_id||'clanker';
    var av=c.github_username?'<img class="clanker-av" src="https://github.com/'+esc(c.github_username)+'.png" alt="">':'<span class="clanker-av"></span>';
    // Trust tier is now surfaced as a compact medallion beside the identity (below),
    // so drop it from the middot sub-line to avoid duplicating the same string.
    var sub=[c.cli_backend,c.model,c.role].filter(Boolean).map(esc).join(' &middot; ');
    // Small tier badge from this clanker's REAL trust_tier (defaults to newcomer).
    var tierPill=tierBadge(c.trust_tier,'tier-inline');
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
      // #2568: manual requeue is only meaningful while the clanker is HOLDING a
      // task — an operator releases work they can see is wedged. Hidden when idle.
      var requeueBtn=c.current_task
        ?('<button type="button" class="admin-act" title="Release this clanker&rsquo;s in-flight task back to the ready queue (books the same short cooldown as an auto-release, so it is not instantly re-handed out)" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="requeue">Requeue task</button>')
        :'';
      actions='<div class="admin-actions">'+
        '<select class="admin-act" title="Set trust tier (maintainer voucher)" data-cid="'+cid+'" data-role="tier">'+opts+'</select>'+
        requeueBtn+
        '<button type="button" class="admin-act danger" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="revoke">Revoke</button>'+
        '<button type="button" class="admin-act danger" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="remove">Remove</button>'+
        '</div>';
    }
    // #command-center: at-a-glance status pill (working / reviewing / idle) and a
    // stable data-clanker key so the travel animation can target this row. "review"
    // is inferred when the in-flight task carries a review-ish signal; otherwise a
    // task in flight is "working" and no task is "idle".
    var st=c.current_task?'working':'idle';
    if(c.current_task&&/review|lgtm|approve/i.test((c.current_task.kind||'')+' '+(c.current_task.title||'')))st='reviewing';
    var statusPill='<span class="clanker-status '+st+'">'+st+'</span>';
    var key=(c.github_username||c.contributor_id||'').toLowerCase();
    // Enter pop-in for a clanker we haven't seen in the previous render (army framing).
    var isNew=key&&!ccKnownClankers[key];
    var rowCls='clanker-row'+(isNew?' cc-enter':'');
    return '<div class="'+rowCls+'" data-clanker="'+esc(key)+'"><span class="clanker-dot'+(c.stale?' stale':'')+'"></span>'+av+
      '<div class="clanker-main"><div class="clanker-user">'+esc(user)+statusPill+tierPill+'</div>'+
      '<div class="clanker-sub">'+(sub||'&mdash;')+'</div>'+task+'</div>'+
      (actions||('<span class="feed-time">'+esc(rel(c.connected_at))+'</span>'))+'</div>';
  }).join('');
}
// ccUpdateArmy summarises the fleet into working/reviewing/idle counts. Army framing
// derived entirely from the live fleet snapshot — no fabricated numbers.
function ccUpdateArmy(list){
  var w=0,rv=0,idle=0;
  (list||[]).forEach(function(c){
    if(c.current_task){ if(/review|lgtm|approve/i.test((c.current_task.kind||'')+' '+(c.current_task.title||'')))rv++; else w++; }
    else idle++;
  });
  var setTxt=function(id,v){var el=document.getElementById(id);if(el)el.textContent=v;};
  setTxt('cc-army-working',w);setTxt('cc-army-reviewing',rv);setTxt('cc-army-idle',idle);
  // Refresh the known-clanker set so the NEXT render only pops-in genuinely new
  // arrivals (enter animation). ccKnownClankers is also read by the SSE join event.
  var next={};(list||[]).forEach(function(c){var k=(c.github_username||c.contributor_id||'').toLowerCase();if(k)next[k]=true;});
  ccKnownClankers=next;
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
  // "Done" is special: the fleet work array holds ONLY in-flight tasks, so a naive
  // status filter is always empty. Instead, source "Done" from the completed activity
  // events (the real completion history). All/Active/Review keep filtering the
  // in-flight list as before.
  var shown;
  if(currentFilter==='done'){shown=(typeof ccCompletedWorkItems==='function')?ccCompletedWorkItems(30):[];}
  else{shown=list.filter(workMatchesFilter);}
  document.getElementById('work-count').textContent=shown.length+(shown.length===1?' item':' items');
  var el=document.getElementById('work-list');
  if(!shown.length){
    var msg=(currentFilter==='done')
      ?'No completed tasks yet — finished work will appear here.'
      :('No work items in flight'+(currentFilter!=='all'?' for this filter.':'.'));
    el.innerHTML='<div class="ops-empty">'+msg+'</div>';return;
  }
  // opsPoll re-renders this list every 4s. Without preserving state, an open
  // "Prompt preview" <details> would slam shut on the next tick (the "opens then
  // closes ~2s later" bug). Snapshot which previews the user has expanded — keyed
  // by a stable data-wkey (repo#number, or title as a fallback) — and re-apply
  // the open attribute to the matching ones after the innerHTML swap. Stays open
  // until the user clicks the summary to close it, surviving every poll.
  var openKeys={};
  var priorDetails=el.querySelectorAll('details.prompt-preview[open]');
  for(var p=0;p<priorDetails.length;p++){var pk=priorDetails[p].getAttribute('data-wkey');if(pk)openKeys[pk]=true;}
  el.innerHTML=shown.map(function(w){
    var who=w.github_username?('<span class="feed-role">'+esc(w.github_username)+'</span>'):'';
    var cli=w.cli_backend?(' &middot; '+esc(w.cli_backend)):'';
    // #2539: read-only prompt preview. Show the exact prompt the agent runs plus
    // task metadata (repo/number/title). The server never puts the github_token in
    // prompt_preview, so this can never leak the credential.
    var labels=(w.labels&&w.labels.length)?('<div class="prompt-labels">'+w.labels.map(function(l){return '<span class="pill pill-idle">'+esc(l)+'</span>';}).join(' ')+'</div>'):'';
    var repoLabel=(w.repo||'')+(w.number?('#'+w.number):'');
    var wkey=repoLabel||(w.title||'');
    var wasOpen=openKeys[wkey]?' open':'';
    var preview=w.prompt_preview
      ?('<details class="prompt-preview" data-wkey="'+esc(wkey)+'"'+wasOpen+'><summary>Prompt preview</summary>'+labels+
        '<pre class="prompt-text">'+esc(w.prompt_preview)+'</pre>'+
        '<p class="ops-note">Read-only. This is the instruction the agent receives; the scoped GitHub token is delivered separately and is never shown here.</p></details>')
      :'';
    return '<div class="work-item"><div class="work-repo">'+ccIssueLinkHTML(w,repoLabel)+'</div>'+
      '<div class="work-title">'+esc(w.title||'(untitled task)')+'</div>'+
      '<div class="work-meta">'+statusPill(w.status)+who+cli+'</div>'+preview+'</div>';
  }).join('');
}
function renderPolicy(p){
  var el=document.getElementById('policy-body');
  if(!p){el.innerHTML='<div class="ops-empty">Policy unavailable.</div>';return;}
  // Keep the queue play/pause in sync every poll tick — this is what makes the
  // two surfaces converge even when the change came from elsewhere (the other
  // tab, another operator, a page that hasn't loaded adminHub yet). If adminHub
  // is already loaded, prefer it (freshest, updated synchronously on toggle);
  // otherwise fall back to this read-only policy snapshot so a read viewer (who
  // never loads adminHub) still sees the correct paused/active status.
  renderQueueSuspendControl(adminHub?!!adminHub.contribute_suspended:!!p.suspended);
  function list(a){return (a&&a.length)?a.map(esc).join(', '):'&mdash;';}
  var rows=[
    ['Contribute queue',p.suspended?'<span class="pill pill-blocked">suspended</span>':'<span class="pill pill-passed">active</span>'],
    ['Title filter',esc(p.titles_mode||'deny')+': '+list(p.deny_titles)],
    ['Author filter',esc(p.authors_mode||'deny')+': '+list(p.deny_authors)],
    /* The canonical label list lives in deny_labels in BOTH modes (the mode name is
       legacy; the backend filter reads deny_labels regardless of mode). Reading
       allow_labels in allow-mode showed an empty list even when a real allow-list
       was configured (the source of the "Allow: (nothing)" + empty-queue confusion). */
    ['Label filter',esc(p.labels_mode||'deny')+': '+list(p.deny_labels)],
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
// safeRender runs one panel render in isolation: a throw in one panel must NOT
// prevent the others from hydrating (regression #2574 left all three stuck when a
// single render threw). Errors are logged, never silently swallowed.
function safeRender(name,fn){try{fn();}catch(e){console.error('opsPoll render failed: '+name,e);}}
async function opsPoll(){
  try{
    var res=await fetch('/api/contribute/fleet');
    var data=await res.json();
    // Each panel renders independently — one failing does not block the others.
    safeRender('clankers',function(){renderClankers((data&&data.clankers)||[]);});
    safeRender('work',function(){renderWork((data&&data.work)||[]);});
    safeRender('policy',function(){renderPolicy(data&&data.policy);});
  }catch(e){
    // fetch/parse failed — log so the "Loading…" placeholders are diagnosable, and
    // fall through to reschedule so a transient failure self-heals on the next poll.
    console.error('opsPoll fetch failed',e);
  }
  var tab=document.getElementById('tab-ops');
  if(tab&&tab.classList.contains('active'))setTimeout(opsPoll,4000);
}

// ══ Operations command center: live SSE stream driving the ready-work queue, the
//    task-assign travel animation, the dev-log narration, achievements, and army
//    enter/leave motion. All from REAL events (ActivityEntry + ActionableIssues).
//    Degrades gracefully: if EventSource is unsupported or the stream drops, we
//    fall back to polling /api/contribute/queue so the tab still works. ═════════
var ccStarted=false;
var ccQueue=[];            // current ready-work items (top = next up)
var ccLogLines=[];         // dev-log scrollback
var ccLogCap=60;           // capped scrollback length
var ccEs=null;             // EventSource handle
var ccQueuePollTimer=null; // fallback poll timer
var ccKnownClankers={};    // username -> true, for enter/leave detection
var ccCompleteStreak={};   // username -> consecutive completes (achievement combos)
var ccLastAch=0;           // debounce achievement pops

function ccQueueKey(q){return (q.repo||'')+'#'+(q.number||'');}

// ccQueueSearch is the current VIEW filter text (lower-cased). It changes only what
// is SHOWN — never the persisted order. Empty = show all. The reorder ACTIONS below
// always operate on the FULL ccQueue by qkey, so acting on a filtered row moves the
// RIGHT item in the real order, not the filtered index.
var ccQueueSearch='';
// ccQueueMatches: does an item pass the current search? Case-insensitive over repo,
// number, title and every label — the fields the row shows.
function ccQueueMatches(q){
  if(!ccQueueSearch)return true;
  var hay=((q.repo||'')+' #'+(q.number||'')+' '+(q.title||'')+' '+((q.labels||[]).join(' '))).toLowerCase();
  return hay.indexOf(ccQueueSearch)>=0;
}
function ccRenderQueue(flip){
  var el=document.getElementById('cc-queue');if(!el)return;
  // Item count badge, same style as "My work"'s #work-count — kept in sync on
  // every render (initial load, SSE queue push, poll fallback, drag-reorder).
  var qc=document.getElementById('queue-count');
  if(qc)qc.textContent=ccQueue.length+' ready';
  // flip=true (set only from the drag-drop / move handlers) records each row's rect
  // BEFORE the rebuild so ccFlipPlay can glide displaced rows to their new slots
  // instead of a hard jump. Every other caller (initial load, SSE queue push, poll
  // fallback) omits it and gets the plain re-render — glide only on operator moves.
  var first=flip?ccFlipFirst(el):null;
  // Drag-reorder is an operator CONTROL: only owner/read-write viewers get grab
  // bars. adminEnabled is set true by initAdmin ONLY after /api/role reports owner
  // or read-write; a read/anon viewer never gets the handles and cannot reorder.
  // The server enforces the same boundary independently (403 on the order endpoint).
  el.classList.toggle('cc-q-draggable',!!adminEnabled);
  if(!ccQueue.length){el.innerHTML='<div class="ops-empty">No work waiting &mdash; the backlog is clear or everything is in flight.</div>';ccUpdateFilterNote(0,0);return;}
  var shown=0,total=ccQueue.length;
  // Render over the FULL model, tagging each row with its TRUE position (i) so the
  // shown index and the move-to menu reflect the real queue position even while a
  // search filter hides other rows. Filtered-out rows are simply skipped from the
  // HTML — the model is untouched, so a subsequent action still targets the right
  // qkey in the full order. Drag-reorder is DISABLED while a filter is active (a
  // drag over a partial list would be ambiguous); the ⋯ menu is the filtered path.
  var filtering=!!ccQueueSearch;
  el.innerHTML=ccQueue.map(function(q,i){
    if(!ccQueueMatches(q))return '';
    shown++;
    // Show ALL of the issue's gh labels as pills (the backend already carries the
    // full label set). "My work" items render every label the same way, so the
    // queue is consistent with them. esc() guards each label.
    var labels=(q.labels&&q.labels.length)?('<div class="cc-q-labels">'+q.labels.map(function(l){return '<span class="pill pill-idle">'+esc(l)+'</span>';}).join('')+'</div>'):'';
    var next=(i===0)?'<span class="cc-q-next">next up</span>':'';
    // The grab bar is always in the DOM but only VISIBLE via CSS when the queue
    // root carries .cc-q-draggable (owner/read-write). draggable is disabled while a
    // filter is active so a partial-list drop can't misplace an item. aria-hidden:
    // purely a mouse/pointer affordance.
    var canDrag=adminEnabled&&!filtering;
    var grip=adminEnabled?'<span class="cc-q-grip" aria-hidden="true" title="Drag to reprioritise">&#x283F;</span>':'';
    // Per-row "⋯" context menu — Apple-Music style. Owner/read-write ONLY (rendered
    // only when adminEnabled). Carries move-to-top + move-to-position, both keyed on
    // this row's qkey so they act on the right item in the FULL order.
    var menu=adminEnabled?ccQueueMenuHTML(ccQueueKey(q),i,total):'';
    return '<div class="cc-q-item"'+(canDrag?' draggable="true"':'')+' data-qkey="'+esc(ccQueueKey(q))+'">'+grip+'<span class="cc-q-idx">'+(i+1)+'</span>'+
      '<div class="cc-q-body"><div class="cc-q-repo">'+ccIssueLinkHTML(q,(q.repo||'')+'#'+(q.number||''))+'</div>'+
      '<div class="cc-q-title" title="'+esc(q.title||'')+'">'+esc(q.title||'(untitled)')+'</div>'+labels+'</div>'+next+menu+'</div>';
  }).join('');
  if(filtering&&shown===0){el.innerHTML='<div class="ops-empty">No queued items match &ldquo;'+esc(ccQueueSearch)+'&rdquo;.</div>';}
  ccUpdateFilterNote(shown,total);
  // Drag binding only when NOT filtering (a partial list would drop ambiguously).
  if(adminEnabled&&!filtering)ccBindQueueDrag(el);
  if(adminEnabled)ccBindQueueMenus(el);
  if(first)ccFlipPlay(el,first);
  // End-of-queue block (#2595): the "all caught up" marker + hive settings + quota.
  // Only when the full list is shown (a filtered view isn't "the end of the queue").
  ccRenderQueueEnd(!filtering);
}
// ccQueueMenuHTML renders the per-row ⋯ menu markup (owner/read-write only). pos is
// the row's ZERO-based position in the full queue; total is the queue length. The
// move-to-position input is pre-filled with the row's current 1-based position.
function ccQueueMenuHTML(key,pos,total){
  var atTop=(pos===0);
  return '<span class="cc-q-menu-wrap">'+
    '<button type="button" class="cc-q-menu-btn" aria-haspopup="true" aria-expanded="false" title="More actions" data-qkey="'+esc(key)+'">&#x22EF;</button>'+
    '<div class="cc-q-menu" role="menu">'+
      '<button type="button" class="cc-q-act" role="menuitem" data-act="top" data-qkey="'+esc(key)+'"'+(atTop?' disabled style="opacity:.5;cursor:default"':'')+'><span class="cc-q-menu-ic">&#x2B06;</span>Move to top</button>'+
      '<div class="cc-q-menu-sep"></div>'+
      '<div class="cc-q-moverow">'+
        '<label for="mv-'+esc(key)+'">Move to&nbsp;#</label>'+
        '<input type="number" id="mv-'+esc(key)+'" min="1" max="'+total+'" value="'+(pos+1)+'" data-qkey="'+esc(key)+'" aria-label="Target position">'+
        '<button type="button" class="cc-q-act-go" data-qkey="'+esc(key)+'">Go</button>'+
      '</div>'+
    '</div>'+
  '</span>';
}
// ccUpdateFilterNote shows a small "showing N of M" line while a filter is active,
// and hides it when the filter is clear. Purely informational.
function ccUpdateFilterNote(shown,total){
  var n=document.getElementById('cc-q-filternote');if(!n)return;
  if(!ccQueueSearch){n.style.display='none';n.textContent='';return;}
  n.style.display='';
  n.textContent='Showing '+shown+' of '+total+' — filter is a view only; the queue order is unchanged.';
}
// ── Per-row ⋯ menu wiring (owner/read-write only) ──────────────────────────────
// A single open menu at a time; clicking the ⋯ toggles it, clicking elsewhere or
// pressing Escape closes it. Actions read data-qkey so they target the right item
// in the FULL ccQueue regardless of any active search filter.
function ccCloseQueueMenus(){
  var open=document.querySelectorAll('.cc-q-menu.open');
  for(var i=0;i<open.length;i++)open[i].classList.remove('open');
  var btns=document.querySelectorAll('.cc-q-menu-btn[aria-expanded=true]');
  for(var j=0;j<btns.length;j++)btns[j].setAttribute('aria-expanded','false');
}
function ccBindQueueMenus(root){
  // Clicks INSIDE an open menu (the number input, its label, whitespace) must not
  // bubble to the global document dismiss handler — otherwise focusing the
  // "Move to #" field instantly closes the menu before Go can run. Swallow the
  // bubble on the menu container itself; the ⋯/top/Go handlers still fire because
  // they run in the same bubble phase before it reaches this element.
  var menus=root.querySelectorAll('.cc-q-menu');
  for(var m=0;m<menus.length;m++){(function(menu){
    menu.addEventListener('mousedown',function(e){e.stopPropagation();});
    menu.addEventListener('click',function(e){e.stopPropagation();});
  })(menus[m]);}
  var btns=root.querySelectorAll('.cc-q-menu-btn');
  for(var i=0;i<btns.length;i++){(function(btn){
    btn.addEventListener('click',function(e){
      e.stopPropagation();
      var menu=btn.parentNode.querySelector('.cc-q-menu');
      var isOpen=menu.classList.contains('open');
      ccCloseQueueMenus();
      if(!isOpen){menu.classList.add('open');btn.setAttribute('aria-expanded','true');}
    });
  })(btns[i]);}
  var acts=root.querySelectorAll('.cc-q-act[data-act=top]');
  for(var a=0;a<acts.length;a++){(function(act){
    act.addEventListener('click',function(e){e.stopPropagation();if(act.disabled)return;ccCloseQueueMenus();ccMoveToTop(act.getAttribute('data-qkey'));});
  })(acts[a]);}
  var gos=root.querySelectorAll('.cc-q-act-go');
  for(var g=0;g<gos.length;g++){(function(go){
    var key=go.getAttribute('data-qkey');
    // cssEscId already returns the escaped FULL id ('mv-'+key), so only the '#'
    // selector prefix is added here. (Prepending '#mv-' would double the 'mv-' and
    // never match — the bug that made "Move to #" silently no-op.)
    var input=root.querySelector('#'+cssEscId(key));
    function apply(){ccCloseQueueMenus();ccMoveToPosition(key,input?parseInt(input.value,10):NaN);}
    go.addEventListener('click',function(e){e.stopPropagation();apply();});
    if(input)input.addEventListener('keydown',function(e){if(e.key==='Enter'){e.preventDefault();apply();}});
  })(gos[g]);}
}
// cssEscId escapes a qkey for use in a querySelector id lookup (the key contains
// '/' and '#'). Prefer CSS.escape when present; fall back to a manual escape.
function cssEscId(id){
  var raw='mv-'+id;
  if(window.CSS&&CSS.escape)return CSS.escape(raw);
  return raw.replace(/([^a-zA-Z0-9_-])/g,'\\$1');
}
// ── Move-to-top / move-to-position (playlist actions) ──────────────────────────
// Both operate on the FULL ccQueue by qkey (never the filtered index), reorder the
// model, re-render with the FLIP glide, and persist the SAME ContributeQueueOrder
// via the existing PUT endpoint — so all four controls (drag, search+act, top,
// position) write the one authoritative order and only change OFFER PRIORITY.
function ccQueueIndexOf(key){
  for(var i=0;i<ccQueue.length;i++){if(ccQueueKey(ccQueue[i])===key)return i;}
  return -1;
}
function ccMoveToTop(key){ccMoveToPosition(key,1);}
function ccMoveToPosition(key,n){
  var from=ccQueueIndexOf(key);if(from<0)return;
  // Validate N (1..len). Clamp rather than reject so an out-of-range value lands at
  // the nearest valid edge instead of silently no-op'ing.
  if(isNaN(n))return;
  if(n<1)n=1;if(n>ccQueue.length)n=ccQueue.length;
  var to=n-1;if(to===from)return;
  var moved=ccQueue.splice(from,1)[0];
  ccQueue.splice(to,0,moved);
  ccRenderQueue(true); // FLIP glide so the row visibly travels to its new slot.
  ccPersistQueueOrder();
}

// ── Operator drag-reorder (grab bars) — owner/read-write only ──────────────────
// Dependency-free HTML5 drag-and-drop. On drop it recomputes ccQueue from the new
// DOM order, re-renders (so indices / "next up" update), and PERSISTS the order to
// the authenticated endpoint. The persisted order becomes the offer-priority
// override that ReadyQueue AND selectTask honour — but it only reorders OFFER
// PRIORITY; the server still applies every admission/cooldown filter, so a pinned
// issue that is filtered out or no longer actionable is skipped, never forced in.
var ccDragKey=null; // qkey of the row currently being dragged
function ccBindQueueDrag(root){
  var items=root.querySelectorAll('.cc-q-item');
  for(var i=0;i<items.length;i++){(function(it){
    it.addEventListener('dragstart',function(e){
      ccDragKey=it.getAttribute('data-qkey');it.classList.add('cc-q-dragging');
      try{e.dataTransfer.effectAllowed='move';e.dataTransfer.setData('text/plain',ccDragKey);}catch(err){}
    });
    it.addEventListener('dragend',function(){it.classList.remove('cc-q-dragging');
      var all=root.querySelectorAll('.cc-q-item');for(var k=0;k<all.length;k++)all[k].classList.remove('cc-q-over');});
    it.addEventListener('dragover',function(e){e.preventDefault();try{e.dataTransfer.dropEffect='move';}catch(err){}it.classList.add('cc-q-over');});
    it.addEventListener('dragleave',function(){it.classList.remove('cc-q-over');});
    it.addEventListener('drop',function(e){
      e.preventDefault();it.classList.remove('cc-q-over');
      var from=ccDragKey,to=it.getAttribute('data-qkey');if(!from||from===to)return;
      // Reorder the ccQueue model: pull the dragged item, insert it before the drop target.
      var fromIdx=-1,toIdx=-1;
      for(var a=0;a<ccQueue.length;a++){if(ccQueueKey(ccQueue[a])===from)fromIdx=a;if(ccQueueKey(ccQueue[a])===to)toIdx=a;}
      if(fromIdx<0||toIdx<0)return;
      var moved=ccQueue.splice(fromIdx,1)[0];
      // After splice the target index may have shifted; recompute against the moved-out array.
      toIdx=-1;for(var b=0;b<ccQueue.length;b++){if(ccQueueKey(ccQueue[b])===to){toIdx=b;break;}}
      if(toIdx<0)toIdx=ccQueue.length;
      ccQueue.splice(toIdx,0,moved);
      ccRenderQueue(true); // FLIP: glide displaced items to their new slots instead of a hard jump.
      ccPersistQueueOrder();
    });
  })(items[i]);}
}
// ── FLIP animation for drag-reorder (First-Last-Invert-Play) ───────────────────
// Dependency-free: record each row's bounding rect BEFORE the re-render (First),
// let ccRenderQueue() rebuild the DOM in the new order, then read each row's rect
// AFTER (Last). For every row keyed the same before/after that actually moved,
// apply an inverse translateY so it appears at its old spot, then transition it to
// translateY(0) — a smooth glide, not a snap. Reads are batched before writes to
// avoid layout thrash. Rows that did not move (delta 0) are left alone. Skipped
// entirely under prefers-reduced-motion, matching the rest of this page's motion.
function ccFlipFirst(root){
  var first={};
  var items=root.querySelectorAll('.cc-q-item');
  for(var i=0;i<items.length;i++){
    var k=items[i].getAttribute('data-qkey');
    if(k)first[k]=items[i].getBoundingClientRect().top;
  }
  return first;
}
function ccFlipPlay(root,first){
  if(window.matchMedia&&matchMedia('(prefers-reduced-motion:reduce)').matches)return;
  var items=root.querySelectorAll('.cc-q-item');
  // Batch reads (Last) before any writes (Invert), then batch writes, then batch
  // the rAF that clears the inversion — no interleaved read/write layout thrash.
  var moves=[];
  for(var i=0;i<items.length;i++){
    var it=items[i],k=it.getAttribute('data-qkey');
    if(!k||!(k in first))continue;
    var last=it.getBoundingClientRect().top;
    var delta=first[k]-last;
    if(Math.abs(delta)<1)continue; // didn't move (or scrolled out of view symmetrically) — no-op
    moves.push([it,delta]);
  }
  if(!moves.length)return;
  for(var m=0;m<moves.length;m++){
    moves[m][0].style.transition='none';
    moves[m][0].style.transform='translateY('+moves[m][1]+'px)';
  }
  // Force one reflow so the inverted position is committed before we animate to 0.
  void root.offsetHeight;
  requestAnimationFrame(function(){
    for(var n=0;n<moves.length;n++){
      var el=moves[n][0];
      el.classList.add('cc-q-flip');
      el.style.transition='';
      el.style.transform='';
    }
    setTimeout(function(){
      for(var p=0;p<moves.length;p++)moves[p][0].classList.remove('cc-q-flip');
    },300); // matches .cc-q-flip transition duration (260ms) + a small margin
  });
}
function ccPersistQueueOrder(){
  var order=ccQueue.map(ccQueueKey);
  fetch('/api/contribute/queue/order',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({order:order})})
    .then(function(r){if(!r.ok)throw new Error('http '+r.status);return r.json();})
    .catch(function(){/* a read viewer would 403 here, but the UI never shows handles to them */});
}
function ccSetLive(state){ // 'live' | 'poll' | 'connecting'
  // Drive BOTH the queue-head pill (#cc-live, unchanged) and the mirror pill that
  // now lives in the dev-log rail head (#cc-live-rail). Same SSE stream feeds both;
  // each is set independently so a missing node never blocks the other.
  var text=state==='live'?'live':(state==='poll'?'polling':'connecting');
  [['cc-live','cc-live-label'],['cc-live-rail','cc-live-rail-label']].forEach(function(ids){
    var el=document.getElementById(ids[0]),lbl=document.getElementById(ids[1]);
    if(!el||!lbl)return;
    if(state==='live')el.classList.remove('stale');else el.classList.add('stale');
    lbl.textContent=text;
  });
}

// ── Dev-log RAIL collapse: open by default, persisted in localStorage ──────────
// Key hive.ops.devlog.collapsed = '1' when the user last collapsed the rail, absent
// otherwise. On first ever load the key is absent so the rail is EXPANDED. Guarded
// against a throwing/blocked localStorage (private mode, quota) — the rail still
// works, it just won't remember across loads.
var OPS_RAIL_KEY='hive.ops.devlog.collapsed';
var opsRailInit=false;
function ccRailRead(){try{return localStorage.getItem(OPS_RAIL_KEY)==='1';}catch(e){return false;}}
function ccRailWrite(collapsed){try{if(collapsed)localStorage.setItem(OPS_RAIL_KEY,'1');else localStorage.removeItem(OPS_RAIL_KEY);}catch(e){}}
function ccRailApply(rail,btn,collapsed){
  rail.classList.toggle('collapsed',collapsed);
  if(btn){
    btn.setAttribute('aria-expanded',collapsed?'false':'true');
    btn.setAttribute('title',collapsed?'Show log':'Collapse log');
  }
}
function initOpsRail(){
  if(opsRailInit)return;
  var rail=document.getElementById('ops-rail'),btn=document.getElementById('ops-rail-toggle');
  if(!rail||!btn)return; // rail markup absent — nothing to wire
  opsRailInit=true;
  // Honour the remembered choice on load (default: expanded).
  ccRailApply(rail,btn,ccRailRead());
  btn.addEventListener('click',function(){
    var collapsed=!rail.classList.contains('collapsed');
    ccRailApply(rail,btn,collapsed);
    ccRailWrite(collapsed);
  });
}

// ── Dev-log narration: build a human-readable line from an ActivityEntry ───────
// ccTaskRefLink turns the "picked up" activity's task string — always built
// server-side as "<kind> <repo>#<number>: <title>" (contribute_ws.go taskDesc)
// — into a clickable GitHub link when it matches that exact shape. "completed"/
// "failed" carry an opaque internal task ID (ct-<repo>-<number>-<ts>), not
// repo#number, so those are deliberately left as plain text rather than risk a
// wrong/broken link from a shape this code doesn't control.
function ccTaskRefLink(task){
  var m=/^\S+\s+([\w.-]+\/[\w.-]+)#(\d+):/.exec(task||'');
  if(!m)return '<span class="ref">'+esc(task)+'</span>';
  return ccIssueLinkHTML({repo:m[1],number:m[2]},task,'ref');
}
function ccNarrate(e){
  var icons={joined:'🟢',left:'⚪',"picked up":'🔧',completed:'✅',failed:'❌',promoted:'🎖️'};
  var ic=icons[e.action]||'⚡';
  var who='<span class="who">'+esc(e.username||'someone')+'</span>';
  var ref=e.task?' <span class="ref">'+esc(e.task)+'</span>':'';
  var pickedRef=e.task?' '+ccTaskRefLink(e.task):'';
  var body;
  switch(e.action){
    case 'joined': body=who+' entered the hive'+(e.cli?' <span class="ref">via '+esc(e.cli)+'</span>':''); break;
    case 'left': body=who+' left the hive'; break;
    case 'picked up': body=who+' grabbed'+pickedRef; break;
    case 'completed': body=who+' completed'+ref; break;
    case 'failed': body=who+' hit a snag on'+ref; break;
    case 'promoted': body=who+' was promoted to <b>'+esc(e.task||e.role||'contributor')+'</b>'; break;
    default: body=who+' '+esc(e.action)+ref;
  }
  return {ic:ic,body:body,ts:e.timestamp};
}
function ccRenderLog(){
  var el=document.getElementById('cc-log');if(!el)return;
  var cnt=document.getElementById('cc-log-count');if(cnt)cnt.textContent=ccLogLines.length+(ccLogLines.length===1?' event':' events');
  if(!ccLogLines.length){el.innerHTML='<div class="ops-empty">Watching the hive&hellip;</div>';return;}
  // Newest at TOP (reads best for a live feed).
  el.innerHTML=ccLogLines.slice().reverse().map(function(l){
    var t='';try{var d=new Date(l.ts);if(!isNaN(d))t=d.toLocaleTimeString([],{hour:'numeric',minute:'2-digit'});}catch(e){}
    return '<div class="cc-log-line"><span class="cc-log-ic">'+l.ic+'</span><div class="cc-log-body">'+l.body+'</div><span class="cc-log-time">'+esc(t)+'</span></div>';
  }).join('');
  el.scrollTop=0;
}
function ccPushLog(e){
  ccLogLines.push(ccNarrate(e));
  if(ccLogLines.length>ccLogCap)ccLogLines=ccLogLines.slice(ccLogLines.length-ccLogCap);
  ccRenderLog();
}

// ── Achievements: derived from REAL streak/threshold logic in the event stream ──
function ccAchievement(head,sub,ic){
  var now=Date.now();if(now-ccLastAch<1200)return; // debounce so pops don't spam
  ccLastAch=now;
  var wrap=document.getElementById('cc-ach-wrap');if(!wrap)return;
  var d=document.createElement('div');d.className='cc-ach';
  d.innerHTML='<span class="cc-ach-ic">'+(ic||'🏆')+'</span><div class="cc-ach-txt"><div class="cc-ach-h">'+esc(head)+'</div><div class="cc-ach-s">'+sub+'</div></div>';
  wrap.appendChild(d);
  setTimeout(function(){d.classList.add('cc-ach-out');setTimeout(function(){d.remove();},420);},3600);
}
function ccMaybeAchieve(e){
  if(e.action==='completed'){
    var u=e.username||'?';
    ccCompleteStreak[u]=(ccCompleteStreak[u]||0)+1;
    var n=ccCompleteStreak[u];
    if(n===3)ccAchievement('Triple combo','<span class="who">'+esc(u)+'</span> shipped 3 in a row','🔥');
    else if(n>3&&n%%5===0)ccAchievement(n+'× streak','<span class="who">'+esc(u)+'</span> is on a roll','⚡');
  } else if(e.action==='failed'){
    if(e.username)ccCompleteStreak[e.username]=0; // a failure breaks the streak
  } else if(e.action==='promoted'){
    ccAchievement('Achievement unlocked','<span class="who">'+esc(e.username||'a clanker')+'</span> reached <b>'+esc(e.task||'contributor')+'</b>','🎖️');
  }
}

// ── The travel animation: on "picked up", fly a token from the queue to the
//    clanker that grabbed it, then remove the item from the queue. Robust when the
//    exact queue item is not rendered (a generic token flies from the queue area).
function ccTravel(e){
  var key=(e.username||'').toLowerCase();
  var target=document.querySelector('.clanker-row[data-clanker="'+(window.CSS&&CSS.escape?CSS.escape(key):key)+'"]');
  // Source: the matching queue item if present, else the queue card itself.
  var qEl=null;
  if(e.task){var qk=String(e.task).replace(/\s+/g,'');
    var items=document.querySelectorAll('#cc-queue .cc-q-item');
    for(var i=0;i<items.length;i++){if(items[i].getAttribute('data-qkey')===e.task){qEl=items[i];break;}}
  }
  var src=qEl||document.getElementById('cc-queue');
  if(src&&target&&!(window.matchMedia&&matchMedia('(prefers-reduced-motion:reduce)').matches)){
    var a=src.getBoundingClientRect(),b=target.getBoundingClientRect();
    var tok=document.createElement('div');tok.className='cc-token';
    tok.textContent=e.task||'task';
    tok.style.left=(a.left+12)+'px';tok.style.top=(a.top+8)+'px';
    document.body.appendChild(tok);
    var dx=(b.left+18)-(a.left+12),dy=(b.top+b.height/2)-(a.top+8);
    requestAnimationFrame(function(){tok.style.transform='translate('+dx+'px,'+dy+'px) scale(.85)';tok.style.opacity='.2';});
    setTimeout(function(){tok.remove();if(target){target.classList.add('cc-landing');setTimeout(function(){target.classList.remove('cc-landing');},820);}},960);
  } else if(target){
    target.classList.add('cc-landing');setTimeout(function(){target.classList.remove('cc-landing');},820);
  }
  // Drop the item from the local queue model with a leave animation.
  if(qEl){qEl.classList.add('cc-leaving');}
  if(e.task){ccQueue=ccQueue.filter(function(q){return ccQueueKey(q)!==e.task;});}
  setTimeout(ccRenderQueue,480);
}

// ── Shared activity store (resilient Live Activity rail) ───────────────────────
// The rail used to depend SOLELY on the SSE stream, which returns nothing on hosted
// spokes — so it sat on "Watching the hive…" forever even though /api/contribute/
// activity (the source Onboarding's Live Activity uses) was full. We now seed + poll
// the rail from that reliable endpoint and layer SSE on top for liveness, via a
// shared deduped store. ccActivity is chronological (oldest→newest); ccActivitySeen
// keys events by timestamp+username+action+task so an event arriving via BOTH poll
// and SSE is counted once.
// NOTE: ccActivity + ccActivitySeen are declared+initialized at the top of this
// IIFE (init-order hoist) so ccIngestActivity()/ccCompletedWorkItems() — which run
// via opsPoll long before this line — can never see them undefined.
var ccActivityCap=200;
var ccActivityPollTimer=null;
var ccSSEDelivered=false;
function ccActivityKey(e){return (e.timestamp||'')+'|'+(e.username||'')+'|'+(e.action||'')+'|'+(e.task||'');}
function ccIngestActivity(e){
  if(!e||!e.action)return false;
  var k=ccActivityKey(e);
  if(ccActivitySeen[k])return false;
  ccActivitySeen[k]=1;
  ccActivity.push(e);
  if(ccActivity.length>ccActivityCap){
    var drop=ccActivity.splice(0,ccActivity.length-ccActivityCap);
    for(var i=0;i<drop.length;i++)delete ccActivitySeen[ccActivityKey(drop[i])];
  }
  return true;
}
// Rebuild the rail scrollback from the shared store (newest ccLogCap entries),
// narrated via the same ccNarrate live events use so the format matches.
function ccRebuildLogFromActivity(){
  var src=ccActivity.slice(Math.max(0,ccActivity.length-ccLogCap));
  ccLogLines=src.map(ccNarrate);
  ccRenderLog();
}
// ccCompletedWorkItems derives "Done" My-work rows from the completed activity events
// in the shared store: the fleet work array holds ONLY in-flight tasks, so the "Done"
// filter was always empty. Newest first, capped, deduped by task. Each row mirrors the
// in-flight row shape so renderWork can display it.
function ccCompletedWorkItems(cap){
  var out=[],seen={};
  for(var i=ccActivity.length-1;i>=0&&out.length<(cap||30);i--){
    var e=ccActivity[i];
    if(!e||e.action!=='completed')continue;
    var task=e.task||'';
    if(task&&seen[task])continue;if(task)seen[task]=1;
    var repo=task,number='';
    var h=task.lastIndexOf('#');
    if(h>=0){repo=task.slice(0,h);number=task.slice(h+1);}
    out.push({repo:repo,number:number,title:task||'(completed task)',status:'done',
      github_username:e.username||'',cli_backend:e.cli||'',_ts:e.timestamp});
  }
  return out;
}
// Poll the reliable activity endpoint, ingest new entries, refresh the rail. This is
// the resilience layer: the rail is NEVER blank when the endpoint has data, whether
// or not SSE delivers. Reschedules while the Operations tab is open.
function ccPollActivity(){
  fetch('/api/contribute/activity').then(function(r){return r.json();}).then(function(d){
    var list=(d&&d.activity)||[];
    var added=false;
    for(var i=0;i<list.length;i++){if(ccIngestActivity(list[i]))added=true;}
    if(added)ccRebuildLogFromActivity();
    else if(!ccLogLines.length&&ccActivity.length)ccRebuildLogFromActivity();
    // Keep the "Done" My-work view current from the (now-updated) completed events.
    if(added&&currentFilter==='done')renderWork(lastWork);
    // Reflect REAL connectivity: if SSE has never delivered a frame, we are running
    // on the polling fallback — say so rather than sitting on "connecting" forever.
    if(!ccSSEDelivered)ccSetLive('poll');
  }).catch(function(){/* transient; next poll self-heals */});
  var tab=document.getElementById('tab-ops');
  if(tab&&tab.classList.contains('active'))ccActivityPollTimer=setTimeout(ccPollActivity,6000);
}

// ── Consume one activity event from the stream ─────────────────────────────────
function ccOnActivity(e){
  if(!e||!e.action)return;
  ccSSEDelivered=true;
  // Route SSE events through the SAME store so they dedupe against the poll and the
  // rail stays consistent. Only a genuinely-new event narrates/animates.
  if(ccIngestActivity(e)){
    ccRebuildLogFromActivity();
    ccMaybeAchieve(e);
    if(e.action==='picked up')ccTravel(e);
    if(currentFilter==='done')renderWork(lastWork);
  }
}

// ── SSE lifecycle with graceful fallback ───────────────────────────────────────
function ccHydrate(payload){
  if(payload.queue){ccQueue=payload.queue.slice();ccRenderQueue();}
  if(payload.replay&&payload.replay.length){
    // Route the SSE replay through the SHARED store so it dedupes against the poll
    // seed (no double-count of events that arrive via both paths).
    var added=false;
    payload.replay.forEach(function(e){if(ccIngestActivity(e))added=true;});
    if(added){ccRebuildLogFromActivity();if(currentFilter==='done')renderWork(lastWork);}
  }
}
function ccQueuePoll(){ // fallback when SSE is down: refresh queue only
  fetch('/api/contribute/queue').then(function(r){return r.json();}).then(function(d){
    if(d&&d.queue){ccQueue=d.queue.slice();ccRenderQueue();}
  }).catch(function(){});
  ccQueuePollTimer=setTimeout(ccQueuePoll,6000);
}
function ccStopFallback(){if(ccQueuePollTimer){clearTimeout(ccQueuePollTimer);ccQueuePollTimer=null;}}
function ccStart(){
  if(ccStarted)return;ccStarted=true;
  // Seed + poll the Live Activity rail from the RELIABLE polling endpoint (the same
  // source Onboarding uses) so the rail shows the backlog immediately and stays
  // current even when the SSE stream delivers nothing (hosted spokes). SSE, when it
  // works, layers live updates on top via the shared, deduped activity store.
  try{ccPollActivity();}catch(e){console.error('activity poll init failed',e);}
  // Wire the playlist-style search filter and start the (light) opportunistic-work
  // poll. Both are independent of the SSE lifecycle: a throw here must not block the
  // live queue stream, so each is guarded.
  try{ccInitQueueSearch();}catch(e){console.error('queue search init failed',e);}
  try{ccStartOpportunistic();}catch(e){console.error('opportunistic init failed',e);}
  if(!('EventSource' in window)){ccSetLive('poll');ccQueuePoll();return;}
  function connect(){
    ccSetLive('connecting');
    try{ccEs=new EventSource('/api/contribute/events');}catch(err){ccSetLive('poll');ccQueuePoll();return;}
    ccEs.onopen=function(){ccSetLive('live');ccStopFallback();};
    ccEs.onmessage=function(m){
      try{var ev=JSON.parse(m.data);}catch(err){return;}
      if(ev.type==='hello')ccHydrate(ev);
      else if(ev.type==='activity'&&ev.activity)ccOnActivity(ev.activity);
    };
    ccEs.onerror=function(){
      // Stream dropped. Show polling state, start the queue fallback, and let the
      // browser's built-in EventSource auto-reconnect re-establish the live stream.
      ccSetLive('poll');
      if(!ccQueuePollTimer)ccQueuePoll();
      // If the connection is fully closed (not merely reconnecting), rebuild it.
      if(ccEs&&ccEs.readyState===2){try{ccEs.close();}catch(e){}ccEs=null;setTimeout(connect,4000);}
    };
  }
  connect();
}
// ── Playlist SEARCH wiring (#2592) ─────────────────────────────────────────────
// Live, case-insensitive VIEW filter. Updates ccQueueSearch and re-renders; it does
// NOT touch ccQueue's order, so clearing restores the full list unchanged. Guarded
// so a missing node never throws.
function ccInitQueueSearch(){
  var input=document.getElementById('cc-q-search');
  var wrap=document.getElementById('cc-q-search-wrap');
  var clear=document.getElementById('cc-q-search-clear');
  if(!input)return;
  input.addEventListener('input',function(){
    ccQueueSearch=input.value.trim().toLowerCase();
    if(wrap)wrap.classList.toggle('has-text',!!input.value);
    ccRenderQueue();
  });
  if(clear)clear.addEventListener('click',function(){
    input.value='';ccQueueSearch='';if(wrap)wrap.classList.remove('has-text');
    ccRenderQueue();input.focus();
  });
  // Escape clears the filter (and closes any open row menu).
  input.addEventListener('keydown',function(e){if(e.key==='Escape'&&input.value){input.value='';ccQueueSearch='';if(wrap)wrap.classList.remove('has-text');ccRenderQueue();}});
}

// ── Opportunistic Work (#2592): fetch, render, add-to-queue ─────────────────────
// A light poll of the read-only discovery endpoint. Calm cadence (30s) — this is a
// chill panel, not a live ticker. Each item's "add to queue" pins it to the FRONT
// of ContributeQueueOrder via the SAME PUT endpoint the queue controls use; if the
// item is not currently admissible the server simply won't offer it, and we surface
// that gracefully rather than pretending it's queued.
var ccOppItems=[];
var ccOppTimer=null;
function ccStartOpportunistic(){
  ccOppPoll();
}
function ccOppPoll(){
  fetch('/api/contribute/opportunistic').then(function(r){return r.json();}).then(function(d){
    ccOppItems=(d&&d.opportunistic)||[];
    ccRenderOpportunistic();
  }).catch(function(){/* leave the last render; a transient failure self-heals next poll */});
  var tab=document.getElementById('tab-ops');
  if(tab&&tab.classList.contains('active'))ccOppTimer=setTimeout(ccOppPoll,30000);
}
// heatClass buckets the light heat score into a calm 3-step dot (hot/warm/cool).
// Thresholds are gentle — this is a mood indicator, not a precise gauge.
function ccOppHeatClass(h){h=h||0;if(h>=6)return '';if(h>=3)return 'warm';return 'cool';}
function ccRenderOpportunistic(){
  var el=document.getElementById('opp-list');if(!el)return;
  var cnt=document.getElementById('opp-count');
  if(cnt)cnt.textContent=ccOppItems.length?(ccOppItems.length+' found'):'';
  if(!ccOppItems.length){el.innerHTML='<div class="ops-empty">Nothing fresh to surface right now &mdash; the backlog is quiet.</div>';return;}
  el.innerHTML=ccOppItems.map(function(o){
    var key=(o.repo||'')+'#'+(o.number||'');
    var reason=o.reason?('<div class="opp-reason">'+esc(o.reason)+'</div>'):'';
    // "Add to queue" is an owner/read-write ACTION — rendered only when adminEnabled.
    // A read/anon viewer sees the item but no add control (server also 403s the PUT).
    var add=adminEnabled?('<button type="button" class="opp-add" data-oppkey="'+esc(key)+'" title="Add to the top of the ready-work queue">Add to queue</button>'):'';
    return '<div class="opp-item">'+
      '<span class="opp-heat '+ccOppHeatClass(o.heat)+'" aria-hidden="true"></span>'+
      '<div class="opp-body"><div class="opp-repo">'+ccIssueLinkHTML(o,key)+'</div>'+
      '<div class="opp-title" title="'+esc(o.title||'')+'">'+esc(o.title||'(untitled)')+'</div>'+reason+'</div>'+
      add+'</div>';
  }).join('');
  if(adminEnabled)ccBindOppAdd(el);
}
function ccBindOppAdd(root){
  var btns=root.querySelectorAll('.opp-add');
  for(var i=0;i<btns.length;i++){(function(btn){
    btn.addEventListener('click',function(){ccOppAddToQueue(btn.getAttribute('data-oppkey'),btn);});
  })(btns[i]);}
}
// ccOppAddToQueue pins an opportunistic item to the FRONT of the persisted order.
// It builds the new order = [key, ...existing order minus key] and PUTs it through
// the same endpoint. On success it nudges the queue to refresh so the item appears
// (if admissible). If the item is not currently admissible it won't show in the
// queue — we tell the operator so rather than implying it was force-queued.
function ccOppAddToQueue(key,btn){
  if(!key)return;
  // Current authoritative order = the live queue's keys (the server stores exactly
  // this on every reorder). Prepend the new key, drop any existing copy.
  var order=[key];
  for(var i=0;i<ccQueue.length;i++){var k=ccQueueKey(ccQueue[i]);if(k!==key)order.push(k);}
  if(btn){btn.disabled=true;btn.textContent='Adding…';}
  fetch('/api/contribute/queue/order',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({order:order})})
    .then(function(r){if(!r.ok)throw new Error('http '+r.status);return r.json();})
    .then(function(){
      // Refresh the queue snapshot so a now-admissible item surfaces at the top.
      return fetch('/api/contribute/queue').then(function(r){return r.json();});
    })
    .then(function(d){
      if(d&&d.queue){ccQueue=d.queue.slice();ccRenderQueue();}
      var inQueue=ccQueueIndexOf(key)>=0;
      if(btn){
        if(inQueue){btn.textContent='Added ✓';}
        // Admissible-but-filtered: pinned in the order, but not offered right now.
        else{btn.textContent='Pinned (not admissible yet)';btn.title='Pinned to the queue order, but this item is not admissible right now (cooldown/filter/in-flight), so it is not offered. It will surface when it becomes admissible.';}
      }
    })
    .catch(function(){if(btn){btn.disabled=false;btn.textContent='Add to queue';}});
}

// ── Hive settings / rate limits + daily quota (#2595) ──────────────────────────
// A read of the managed-queue's per-tier rate limits + the viewer's own daily
// usage. Cached after first load (limits change rarely); the me-card and the
// end-of-queue block both render from it. Public read — everyone sees the tier
// table; the "you" quota block appears only when the viewer is identified.
var ccLimits=null;
function ccLoadLimits(cb){
  fetch('/api/contribute/limits').then(function(r){return r.json();}).then(function(d){
    ccLimits=d||{};
    if(cb)try{cb();}catch(e){}
    // Refresh any already-rendered surfaces now that we have the data.
    try{ccRenderQueueEnd(!ccQueueSearch);}catch(e){}
    try{ccRenderMeQuota();}catch(e){}
  }).catch(function(){/* leave ccLimits null; surfaces degrade quietly */});
}
// tierLabel — capitalised tier name for display.
function ccTierLabel(t){t=String(t||'');return t?(t.charAt(0).toUpperCase()+t.slice(1)):'';}
// limNum renders a limit value, showing "unlimited" for the 0 (== no cap) sentinel.
function ccLimNum(n){n=n||0;return n>0?String(n):'unlimited';}
// ccLimitsLead builds the human-friendly "managed queue" sentence from real tiers.
function ccLimitsLead(){
  if(!ccLimits||!ccLimits.tiers||!ccLimits.tiers.length)return '';
  var by={};ccLimits.tiers.forEach(function(t){by[t.tier]=t;});
  var parts=[];
  ['newcomer','contributor','trusted'].forEach(function(name){
    if(by[name]&&by[name].max_per_hour>0)parts.push(ccTierLabel(name)+'s get '+by[name].max_per_hour+'/hour');
  });
  if(!parts.length)return 'This hive runs a managed queue with trust-based rate limits.';
  return 'This hive runs a managed queue: '+parts.join(', ')+' — rate limits scale with your trust tier, so it stays fair, not spammy.';
}
// ccTierTableHTML renders the per-tier limit cards, highlighting the viewer's tier.
function ccTierTableHTML(){
  if(!ccLimits||!ccLimits.tiers||!ccLimits.tiers.length)return '';
  var youTier=(ccLimits.you&&ccLimits.you.tier)||'';
  return '<div class="hs-tiers">'+ccLimits.tiers.map(function(t){
    var isYou=(t.tier===youTier);
    return '<div class="hs-tier'+(isYou?' is-you':'')+'">'+
      '<div class="hs-tier__name">'+esc(ccTierLabel(t.tier))+(isYou?' <span class="hs-tier__youtag">you</span>':'')+'</div>'+
      '<div class="hs-tier__lim">'+ccLimNum(t.max_per_hour)+'/hr · '+ccLimNum(t.max_per_day)+'/day</div>'+
    '</div>';
  }).join('')+'</div>';
}
// ccQuotaHTML renders the daily-quota meter from the viewer's REAL used_day count
// vs their tier's max_per_day. variant: '' (end-of-queue) or 'me-quota' (me-card).
// Returns '' when we have no identified viewer or no daily cap (unlimited tiers).
function ccQuotaHTML(variant){
  var you=ccLimits&&ccLimits.you;
  if(!you)return '';
  var max=you.max_per_day||0;
  var used=(typeof you.used_day==='number')?you.used_day:0;
  if(max<=0){
    // Unlimited tier — no meter, just an honest note.
    return '<div class="quota '+(variant||'')+'"><div class="quota__head"><span class="quota__lbl">Your daily usage ('+esc(ccTierLabel(you.tier))+')</span><span class="quota__val">'+used+' today · no daily cap</span></div></div>';
  }
  var pct=Math.max(0,Math.min(100,Math.round(used/max*100)));
  var cls=pct>=100?'full':(pct>=80?'near':'');
  var remaining=Math.max(0,max-used);
  return '<div class="quota '+(variant||'')+'">'+
    '<div class="quota__head"><span class="quota__lbl">Your daily quota ('+esc(ccTierLabel(you.tier))+' set)</span><span class="quota__val">'+used+' / '+max+' tasks</span></div>'+
    '<div class="quota__bar"><div class="quota__fill '+cls+'" style="width:'+pct+'%%"></div></div>'+
    '<div class="quota__sub">'+(remaining>0?(remaining+' left in your allowance today.'):'You&rsquo;ve used your daily allowance — it refreshes on a rolling 24h window.')+'</div>'+
  '</div>';
}
// ccRenderQueueEnd paints the end-of-queue block (#2595). show=false (a filter is
// active) hides it — a partial view isn't "the end". Loads limits lazily on first
// need. The block always includes the calm "caught up" marker + hive settings;
// the quota meter is added only for an identified viewer.
function ccRenderQueueEnd(show){
  var el=document.getElementById('cc-q-end');if(!el)return;
  if(!show){el.style.display='none';return;}
  if(ccLimits===null){ccLoadLimits();/* will re-call on load */}
  el.style.display='';
  var caughtUp='<div class="cc-q-end"><span class="cc-q-end-badge"><span class="cc-q-end-ic" aria-hidden="true">&#x2713;</span>End of queue reached &mdash; you&rsquo;re all caught up</span></div>';
  var settings='';
  if(ccLimits&&ccLimits.tiers&&ccLimits.tiers.length){
    settings='<div class="hive-settings"><h4>Managed queue &amp; rate limits</h4>'+
      '<p class="hs-lead">'+esc(ccLimitsLead())+'</p>'+
      ccTierTableHTML()+
      ccQuotaHTML('')+
    '</div>';
  }
  el.innerHTML=caughtUp+settings;
}
// ccRenderMeQuota injects the daily-quota widget into the Me card (#2595) if the
// card is mounted and we have the viewer's quota. Idempotent — replaces any prior
// widget. Called after the me-card renders and after limits load.
function ccRenderMeQuota(){
  var slot=document.getElementById('me-quota-slot');if(!slot)return;
  var html=ccQuotaHTML('me-quota');
  slot.innerHTML=html||'<div class="ops-note" style="margin:0">Ship a task to start tracking your daily quota.</div>';
}

// ── Global menu-dismiss: click outside or Escape closes any open row menu ───────
document.addEventListener('click',function(){ccCloseQueueMenus();});
document.addEventListener('keydown',function(e){if(e.key==='Escape')ccCloseQueueMenus();});
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
<!-- Command-center overlays: achievement pops (top-right) + the travelling-task
     token layer. Fixed, pointer-events:none, purely presentational. -->
<div class="cc-ach-wrap" id="cc-ach-wrap"></div>
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
		// Invite is an optional trusted invite token (issue #2598). When present
		// and valid, the new profile records who invited them (InvitedBy). It
		// NEVER changes the tier — an invitee always joins as "newcomer".
		Invite string `json:"invite,omitempty"`
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
			// SECURITY (#2610): rotating an EXISTING contributor's token is
			// exactly what POST /api/contribute/reissue-token does, and that
			// endpoint 401s without a caller identity it can verify server-side.
			// force:true must enforce the SAME gate, otherwise the reissue-token
			// auth check is trivially bypassed by rotating through register.
			// Resolve the caller server-side (never from the request body) and
			// require it to be the owner of this contributor profile.
			caller := s.resolveContributeCaller(r)
			if caller == "" {
				jsonError(w, "Sign in with GitHub (or send Authorization: Bearer <gh-token>) to reissue an existing contributor's token, or use POST /api/contribute/reissue-token.", http.StatusUnauthorized)
				return
			}
			if !strings.EqualFold(caller, username) {
				jsonError(w, "You can only reissue your own contributor token.", http.StatusForbidden)
				return
			}
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

	// Attribution (issue #2598): if a valid trusted invite token accompanies the
	// registration, record who invited them. This is attribution ONLY — the tier
	// is still "newcomer" from createContributorProfile; an invite never elevates.
	// A missing/expired/tampered token is silently ignored (plain self-register).
	if inviter := verifyInviteToken(req.Invite, time.Now()); inviter != "" && inviter != username {
		profile.InvitedBy = inviter
		s.logger.Info("contributor registered via trusted invite", "username", username, "invited_by", inviter)
	}

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

// resolveViewerUsername returns the GitHub username of the logged-in caller as
// the server sees it, mirroring handleGHUserAuthStatus: a per-user session first
// (direct-route spokes), then the hub-injected X-Hive-User header (hub-proxied),
// then the persisted owner token (single-owner spokes). Returns "" if the caller
// is anonymous. This is the SERVER-SIDE identity — it is not client-supplied, so
// the trust gate below cannot be spoofed by a request body.
func (s *Server) resolveViewerUsername(r *http.Request) string {
	if sess := s.sessionFromRequest(r); sess != nil {
		return sess.Username
	}
	if hubUser := r.Header.Get("X-Hive-User"); hubUser != "" {
		return hubUser
	}
	if s.directRouteAuthzEnabled() {
		return ""
	}
	tokenData, err := os.ReadFile(userTokenPath)
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(tokenData))
	if token == "" {
		return ""
	}
	user, err := github.ValidateToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		return ""
	}
	return user.Login
}

// resolveContributeCaller returns the server-verified GitHub identity of the
// caller for contributor mutations, combining the two auth paths already used
// elsewhere in this file:
//   - the session / hub-injected identity (resolveViewerUsername), as the
//     invite handler uses; and
//   - an Authorization: Bearer <gh-token> validated against GitHub, exactly as
//     handleContributeReissueToken does.
//
// It returns "" when the caller is anonymous. The identity is never taken from
// the request body, so it cannot be spoofed by a client. Used to gate the
// register force:true rotation with the same authority as reissue-token (#2610).
func (s *Server) resolveContributeCaller(r *http.Request) string {
	if u := s.resolveViewerUsername(r); u != "" {
		return u
	}
	authz := r.Header.Get("Authorization")
	var token string
	if strings.HasPrefix(authz, "Bearer ") {
		const bearerPrefixLen = 7 // len("Bearer ")
		token = authz[bearerPrefixLen:]
	} else if strings.HasPrefix(authz, "token ") {
		const tokenPrefixLen = 6 // len("token ")
		token = authz[tokenPrefixLen:]
	}
	if token == "" {
		return ""
	}
	return validateGitHubToken(token, s.deps.Config.GitHub.OAuthAPIURL())
}

// handleContributeInvite mints a trusted, attributed invite link (issue #2598).
// It resolves the caller's identity server-side, loads their contributor
// profile, and requires their trust tier to be trusted or advisor — a newcomer,
// contributor, or anonymous caller gets 403. The returned token encodes the
// inviter so that whoever registers via the link is attributed to them while
// still joining as a plain newcomer (the register path never elevates tier).
func (s *Server) handleContributeInvite(w http.ResponseWriter, r *http.Request) {
	username := s.resolveViewerUsername(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to invite someone to contribute.", http.StatusUnauthorized)
		return
	}
	profile, _ := loadContributorProfile(username)
	if profile == nil {
		jsonError(w, "You need a contributor profile on this hive before you can invite others.", http.StatusForbidden)
		return
	}
	if !inviteTrustTiers[profile.TrustTier] {
		jsonError(w, "Only trusted or advisor contributors can invite others. Keep shipping to earn trust.", http.StatusForbidden)
		return
	}

	token := mintInviteToken(username, time.Now())
	// Build a shareable /contribute onboarding link carrying the invite token.
	// Same-origin only; the client just copies/shares it (no external fetch).
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	origin := scheme + "://" + r.Host
	inviteURL := origin + "/contribute/onboarding?invite=" + token

	s.logger.Info("trusted invite link minted", "inviter", username, "tier", profile.TrustTier)
	jsonResponse(w, map[string]any{
		"invite_url": inviteURL,
		"invite":     token,
		"inviter":    username,
		"expires_in": int(inviteTokenTTL.Seconds()),
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
	// #2567: identify WHICH surface answered. The Hub discovery front door and a
	// selected spoke both serve this exact handler with disjoint-looking payloads
	// and, until now, no discriminator — a wrong-base-URL request returned 200 and
	// looked valid. Add a "surface" discriminator plus the protocol/api version and
	// the served git SHA so a client can tell hub from spoke and a wrong URL fails
	// LOUDLY (identifiable) instead of silently. All fields are ADDITIVE — existing
	// consumers of the four fields above are unaffected. Also mirror the protocol
	// version in a response header for cheap client-side checks.
	w.Header().Set("X-Hive-Contribute-Protocol", contributorProtocolVersion)
	jsonResponse(w, map[string]any{
		"hub":                 "online",
		"active_contributors": active,
		"total_registered":    len(profiles),
		"actionable_items":    actionable,
		"surface":             s.contributeSurface(),
		"api_version":         contributorProtocolVersion,
		"served_sha":          versionShort,
	})
}

// contributeSurface reports which contributor surface this deployment presents
// so the /api/contribute/status response can discriminate the Hub discovery
// front door from a selected spoke (#2567). A hive that sits behind the hub's
// nginx auth-proxy (HubProxied) is a hosted SPOKE; otherwise it is the hub /
// standalone discovery surface. Read-only; derived from existing config, adds no
// new state or configuration.
func (s *Server) contributeSurface() string {
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Dashboard.HubProxied {
		return surfaceSpoke
	}
	return surfaceHub
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

// handleContributeQueue serves the read-only ready-work QUEUE — the admissible
// issues waiting to be picked off, derived from the SAME ActionableIssues set
// selectTask offers from (see ReadyQueue). GET only, public, no side effects. It
// is both a JSON fallback for browsers without EventSource and the same payload
// the SSE "hello" frame carries, so the queue renders even if the stream drops.
func (s *Server) handleContributeQueue(w http.ResponseWriter, r *http.Request) {
	queue := []ReadyQueueItem{}
	if s.contributeHub != nil {
		queue = s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	}
	jsonResponse(w, map[string]any{"queue": queue})
}

// handleContributeOpportunistic serves the read-only OPPORTUNISTIC WORK list
// (#2592): a small, curated set of admissible issues surfaced by a light recency
// heat proxy (see OpportunisticWork). GET only, PUBLIC (the /api/contribute prefix
// is exempt from the read-only block, and this is a read with no side effects), so
// anonymous viewers can SEE the discovery list. The "add to queue" ACTION goes
// through the existing owner/read-write queue-order endpoint, not this read.
func (s *Server) handleContributeOpportunistic(w http.ResponseWriter, r *http.Request) {
	items := []OpportunisticItem{}
	if s.contributeHub != nil {
		items = s.contributeHub.OpportunisticWork(opportunisticDefaultLimit)
	}
	jsonResponse(w, map[string]any{"opportunistic": items})
}

// tierLimitView is one tier's managed-queue caps, rendered readably by the UI.
type tierLimitView struct {
	Tier          string `json:"tier"`
	MaxPerHour    int    `json:"max_per_hour"`
	MaxPerDay     int    `json:"max_per_day"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// limitsTierOrder is the trust progression, so the UI lists tiers newcomer→advisor
// rather than in Go map iteration order (non-deterministic).
var limitsTierOrder = []string{"newcomer", "contributor", "trusted", "advisor"}

// handleContributeLimits serves the hive's per-tier rate limits (#2595) plus the
// VIEWER's own daily/hourly usage when we can identify them. This makes the managed
// queue's trust-based rate limiting visible and reassuring — real config values only
// (Config.Hub.TierLimits, enforced in selectTask). GET only, PUBLIC (read, no side
// effects). The "you" block is resolved server-side from the session / X-Hive-User
// header — never a client param — so a viewer only ever sees their OWN usage; an
// anonymous viewer gets the tier table with no "you" block.
func (s *Server) handleContributeLimits(w http.ResponseWriter, r *http.Request) {
	var tiers []tierLimitView
	limitMap := map[string]config.TierRate{}
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.TierLimits != nil {
		limitMap = s.deps.Config.Hub.TierLimits
	}
	for _, t := range limitsTierOrder {
		if tr, ok := limitMap[t]; ok {
			tiers = append(tiers, tierLimitView{Tier: t, MaxPerHour: tr.MaxPerHour, MaxPerDay: tr.MaxPerDay, MaxConcurrent: tr.MaxConcurrent})
		}
	}
	for name, tr := range limitMap {
		known := false
		for _, t := range limitsTierOrder {
			if t == name {
				known = true
				break
			}
		}
		if !known {
			tiers = append(tiers, tierLimitView{Tier: name, MaxPerHour: tr.MaxPerHour, MaxPerDay: tr.MaxPerDay, MaxConcurrent: tr.MaxConcurrent})
		}
	}

	resp := map[string]any{"tiers": tiers}

	username := ""
	if sess := s.sessionFromRequest(r); sess != nil {
		username = sess.Username
	} else if hu := r.Header.Get("X-Hive-User"); hu != "" {
		username = hu
	}
	if username != "" {
		profile := findContributor(username)
		tier := "newcomer"
		identity := username
		if profile != nil {
			if profile.TrustTier != "" {
				tier = profile.TrustTier
			}
			if profile.ContributorID != "" {
				identity = profile.ContributorID
			}
		}
		you := map[string]any{"username": username, "tier": tier}
		if s.contributeHub != nil {
			hour, day := s.contributeHub.rateWindowCounts(identity, time.Now())
			you["used_hour"] = hour
			you["used_day"] = day
		}
		if tr, ok := limitMap[tier]; ok {
			you["max_per_hour"] = tr.MaxPerHour
			you["max_per_day"] = tr.MaxPerDay
			you["max_concurrent"] = tr.MaxConcurrent
		}
		resp["you"] = you
	}
	jsonResponse(w, resp)
}

// maxQueueOrderKeys caps how many priority keys the operator override may carry.
// It is well above readyQueueDefaultLimit (the whole visible queue could be
// pinned) yet bounds a pathological / hostile payload so it can neither bloat
// hive.yaml nor slow the per-selectTask ordering lookup.
const maxQueueOrderKeys = 512

// queueOrderKeyPattern validates one "owner/repo#number" priority key. Keeping the
// stored override to well-formed keys means a malformed entry can never match a
// candidate (it would simply be a permanent no-op) and keeps hive.yaml clean.
var queueOrderKeyPattern = regexp.MustCompile(`^[^\s/#]+/[^\s/#]+#[0-9]+$`)

// handleContributeQueueOrder persists the OPERATOR PRIORITY OVERRIDE for the
// ready-work queue — the ordered "owner/repo#number" list the operator produced by
// dragging queue rows on the Operations tab. It is a CONTROL, so it is owner/read-
// write ONLY, enforced server-side by requireContributorWrite (a read/anon caller
// gets 403). It stores the order into Config.Hub.ContributeQueueOrder through the
// SAME refreshAndPersist path the Governor Hub admission settings use, so it
// survives restart. The override only changes OFFER PRIORITY: ReadyQueue and
// selectTask both apply it AFTER their admission/cooldown/disabled/in-flight
// exclusions, so a pinned issue that is filtered out or stale is skipped, never
// resurrected. It never bypasses any filter.
func (s *Server) handleContributeQueueOrder(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(body.Order) > maxQueueOrderKeys {
		jsonError(w, "too many queue-order keys", http.StatusBadRequest)
		return
	}
	// Sanitise: keep only well-formed, unique keys, preserving the operator's order.
	// A malformed or duplicate key is dropped rather than rejected so a partially
	// stale UI payload still persists the good keys.
	seen := make(map[string]struct{}, len(body.Order))
	cleaned := make([]string, 0, len(body.Order))
	for _, k := range body.Order {
		k = strings.TrimSpace(k)
		if k == "" || !queueOrderKeyPattern.MatchString(k) {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		cleaned = append(cleaned, k)
	}
	s.deps.Config.Hub.ContributeQueueOrder = cleaned
	s.auditFromRequest(r, "contribute_queue_order", auditDetail("keys", strconv.Itoa(len(cleaned))), "")
	s.refreshAndPersist()
	s.logger.Info("contribute queue order updated", "keys", len(cleaned))
	jsonResponse(w, map[string]any{"ok": true, "order": cleaned})
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

// handleContributorRequeue is the operator MANUAL requeue / release action
// (kubestellar/hive#2568, the tractable slice). It lets an owner/read-write operator
// who can SEE a connected clanker is wedged — holding a task but not making progress —
// release that task back to the ready queue. It is a CONTROL, so it is owner/read-
// write ONLY, enforced server-side by requireContributorWrite (a read/anon caller
// gets 403), exactly like trust/revoke/remove.
//
// It intentionally reuses the SAME release+cooldown machinery the automatic
// disconnect-release (#2356/#2435) and ready-abandon (#2545) paths use — see
// ContributeWSHub.RequeueContributorTask — so a manual requeue can NOT recreate the
// duplicate-assignment race #2492/#2557 closed: the released issue books the same
// short failure cooldown and is therefore not instantly re-handed to a stale worker.
// It does NOT enforce wsTaskTimeout automatically (manual action only) and does NOT
// implement the deferred renewable-lease / generation-token protocol (that is the
// follow-up design half of #2568). Requeuing a contributor with no in-flight task is
// a 404 (nothing to release) rather than a silent no-op.
func (s *Server) handleContributorRequeue(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "Contributor relay is not available", http.StatusServiceUnavailable)
		return
	}
	// Key the live release by the registered ContributorID (what the ops tab passes),
	// matching how the hub tracks connections. GitHubUsername is only used for logs.
	released := s.contributeHub.RequeueContributorTask(p.ContributorID)
	if released == 0 {
		jsonError(w, "That contributor has no in-flight task to requeue.", http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "contributor_requeue", auditDetail("username", p.GitHubUsername), "")
	s.logger.Info("contributor task requeued by operator", "username", p.GitHubUsername, "sessions_released", released)
	jsonResponse(w, map[string]any{"ok": true, "released": released})
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

// handleLeaderboardPage is kept for backward compatibility with the /leaderboard
// route and any external bookmarks. The leaderboard now lives INLINE as a tab on
// the /contribute page (hydrated from GET /api/leaderboard), so this handler is a
// deep-link shim: it redirects to the canonical path-style tab URL
// /contribute/leaderboard, where the tab JS reads location.pathname on load and
// opens the Leaderboard tab. The former standalone full-page render was folded
// into that tab to avoid a duplicate. (The legacy /contribute?tab=leaderboard
// query form still works on load for back-compat, but the canonical shareable
// URL is now the path form.)
func (s *Server) handleLeaderboardPage(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/contribute/leaderboard", http.StatusFound)
}

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
<div class="desc">Register a new contributor (open), or re-register an existing one with <code>force:true</code> to reissue its token. Reissuing an existing token requires proving you own that identity — send <code>Authorization: Bearer &lt;gh-token&gt;</code> — or use <code>/api/contribute/reissue-token</code>.</div>
<pre>curl -X POST -d '{"github_username":"you"}' %s/api/contribute/register
# reissue an existing token (authenticated):
curl -X POST -H "Authorization: Bearer $GH_TOKEN" -d '{"github_username":"you","force":true}' %s/api/contribute/register</pre>
</div>

</body></html>`, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL)
}
