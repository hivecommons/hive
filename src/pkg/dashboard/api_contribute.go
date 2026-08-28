package dashboard

import (
	"bytes"
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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/hub"
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
	// contributorProfileFileMode is owner-only (audit N12, CWE-522). Contributor
	// profiles carry the registration-token hash and PII; at the previous 0644
	// every UID in the pod, agents included, could read them. Matches the 0600
	// the invite secret has always used.
	contributorProfileFileMode = 0o600
)

// inviteTrustTiers are the trust tiers permitted to mint an invite link. Only a
// trusted, merger, or advisor contributor may invite; a newcomer/contributor/anonymous
// viewer may not. Enforced server-side (handleContributeInvite) — UI hiding is
// UX only.
var inviteTrustTiers = map[string]bool{"trusted": true, "merger": true, "advisor": true}

var (
	inviteSecretOnce  sync.Once
	inviteSecretCache []byte
)

// inviteSigningSecret returns the HMAC key used to sign/verify invite tokens.
//
// Resolution order, most to least identity-bound:
//
//  1. hub.SpokeInviteKey() — the PER-HIVE invite key, either hub-injected as
//     HIVE_INVITE_KEY or self-derived from HIVE_HUB_SECRET + HIVE_ID as
//     HMAC(master, "hive-invite-v1" || 0x00 || hiveID). Both lanes are per-hive,
//     so an invite link minted on one tenant is meaningless on another.
//  2. A lazily generated, persisted per-instance random secret beside the
//     contributor store, when the hive cannot identify itself at all.
//
// !! The RAW MASTER lane is DELETED. !!
//
// It read HIVE_HUB_SECRET and used the master ITSELF as the HMAC key. Measured on
// the live fleet, that was the lane actually in use on 65/65 spokes —
// HIVE_INVITE_KEY is emitted by the provisioning template but is not carried by
// the perhive_env_reconcile sweep, so no live spoke has ever been handed it.
// Since the master is fleet-uniform (65/65 spokes, one distinct value), every
// spoke signed invites with an identical key and the per-hive binding that
// provisionInviteKey exists to provide was not in force anywhere.
//
// Self-deriving in lane 1 is what makes deleting the master lane safe WITHOUT
// waiting for a re-provision: the per-hive invite key is a pure function of the
// master and the HIVE_ID a spoke already holds, so every spoke computes the
// correct value the moment it rolls this code — the same in-place cutover
// SpokeHeartbeatKey's lane 2 uses for the bearer (audit F2).
//
// Either way the secret never leaves the server — the token the client sees is
// opaque. NOTE: invite tokens are signed with this key, so a hive whose key
// CHANGES invalidates in-flight invite links; this change re-keys each spoke
// exactly once, and an invalid invite degrades to "no attribution" (a plain
// self-registration), never to an error. That is why the invite key is safe to
// cut over in place while the terminal key is not re-keyed here at all.
func inviteSigningSecret() []byte {
	inviteSecretOnce.Do(func() {
		if v := strings.TrimSpace(hub.SpokeInviteKey()); v != "" {
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
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
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
	// LabelInterests is the contributor's OPT-IN list of GitHub issue labels they
	// want to help with (issue #2637) — e.g. a contributor with an NVIDIA machine
	// subscribes to "nvidia" so nvidia-labelled work surfaces first for them. It is
	// a SOFT signal only: the Operations ready-work queue highlights and sorts
	// matching issues to the front FOR THIS VIEWER, but never hard-filters the
	// queue, so a contributor with no interests set (or an issue with no labels) is
	// never starved of work. Matching is exact on the label NAME, case-insensitive.
	// Stored here (the existing per-contributor profile store) rather than in a new
	// subsystem; empty/omitted for contributors who set none.
	LabelInterests []string `json:"label_interests,omitempty"`
	// AgentRoleGrants is the operator-managed per-contributor allow-list for
	// claiming spoke agent roles that require explicit grant (for example
	// ci-maintainer). It never changes the contributor's trust tier or credentials.
	AgentRoleGrants []string `json:"agent_role_grants,omitempty"`
	// AssignedAgentRole is the owner-selected effective clanker role. Empty means
	// no owner override (the relay's optional HIVE_AGENT_ROLE claim may apply);
	// "none" is an explicit owner override to general contribute work.
	AssignedAgentRole string `json:"assigned_agent_role,omitempty"`
	// ── Contributor dossier (self-service, ALL optional — see dossier.go) ──
	// Free-choice identity fields the contributor sets themselves via
	// POST /api/contribute/dossier. They gate nothing, decay never, and are
	// sanitised/bounded on write (and HTML-escaped again on render).
	Archetype       string   `json:"archetype,omitempty"`
	Specializations []string `json:"specializations,omitempty"`
	Testimony       string   `json:"testimony,omitempty"`
	EquippedTitle   string   `json:"equipped_title,omitempty"`
	// CredlyName is the contributor's Credly vanity name ([a-z0-9-] only); when
	// set, the heraldry endpoint mirrors their PUBLIC Credly badges.
	CredlyName string `json:"credly_name,omitempty"`
	// EmblemSeed seeds the CSS-generative identity emblem (client-side only).
	EmblemSeed string `json:"emblem_seed,omitempty"`
	// Collaborators is the append-only record of people this contributor has
	// worked alongside — see collaborators.go. Written symmetrically to both
	// parties; never decays, never removed.
	Collaborators []CollaboratorRecord `json:"collaborators,omitempty"`
	Active        bool                 `json:"active,omitempty"`
	CurrentTask   *WSTaskAssign        `json:"current_task,omitempty"`
	ActiveTasks   []WSTaskAssign       `json:"active_tasks,omitempty"`
	Sessions      int                  `json:"sessions,omitempty"`
	// Version is the profile's optimistic-concurrency token (kubestellar/hive H2,
	// CWE-613/639). It is bumped on every persisted change. A caller that loaded the
	// profile at version N may only persist its edit if the on-disk version is still
	// N (saveContributorProfileCAS); a concurrent writer that already advanced the
	// version wins, and the stale writer must reload and re-apply. Before this, a WS
	// path holding a profile pointer captured at auth could save it back minutes
	// later and silently CLOBBER an admin revoke that happened in between —
	// restoring "contributor" over "revoked" and re-granting write access. Absent
	// (0) on legacy on-disk profiles, which the CAS treats as "unversioned" and
	// upgrades on first write. omitempty so existing files are byte-compatible.
	Version int `json:"version,omitempty"`
}

type ContributorRateLimits struct {
	MaxConcurrent int `json:"max_concurrent_tasks"`
	MaxPerHour    int `json:"max_tasks_per_hour"`
	MaxPerDay     int `json:"max_tasks_per_day"`
}

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
	// Seed the collaborator graph from invite attribution recorded before
	// collaborators existed, so the dossier zone is not empty on hives that have
	// been running invites for months. Idempotent — a pair already on record is
	// skipped, so repeated restarts never inflate the counts.
	if n := backfillInviteCollaborations(); n > 0 {
		s.logger.Info("backfilled invite collaborations", "pairs", n)
	}
	s.mux.HandleFunc("GET /contribute", s.handleContributeLanding)
	// Path-style deep links: /contribute/onboarding|management|operations|leaderboard
	// (and the short id forms) all serve the SAME landing HTML — the client JS reads
	// location.pathname and activates the matching tab. The {tab} segment is not used
	// server-side; it exists so each tab is a real bookmarkable/shareable URL. Any
	// /contribute/<tab> is already treated as public by isPublicPath (server.go).
	s.mux.HandleFunc("GET /contribute/{tab}", s.handleContributeLanding)
	// Public dossier permalink: /contribute/dossier/{username} renders ANY
	// contributor's record, so a dossier is a shareable artifact rather than a
	// surface only its owner can see. Two path segments, so it never collides
	// with the single-segment {tab} pattern above. Owner-only controls (the edit
	// form, invite minting, style picker, quota) are withheld unless the viewer
	// IS the subject — see handleContributorDossierPage.
	s.mux.HandleFunc("GET /contribute/dossier/{username}", s.handleContributorDossierPage)
	s.mux.HandleFunc("GET /api/contribute/ws", s.contributeHub.HandleWS)
	s.mux.HandleFunc("POST /api/contribute/register", s.handleContributeRegister)
	// Trusted invite link (issue #2598). A trusted/merger/advisor contributor mints an
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
	// Read-only persistent hourly metrics (7-day, 168 buckets) feeding the
	// Operations + Leaderboard sparklines: queue depth, tasks/hour, fleet size,
	// and per-contributor completions. Public like the other /api/contribute*
	// reads (only counts + already-public usernames; no tokens, no PII). GET only,
	// no side effects. See contribute_metrics.go.
	s.mux.HandleFunc("GET /api/contribute/metrics", s.handleContributeMetrics)
	// Read-only TRIAGE ladder (#2612 part b): the contribute issues grouped into a
	// Warp-style lifecycle (Triaging → Ready → Implementing → Reviewing → Closed),
	// DERIVED LIVE from the ready queue + fleet snapshot + the PR→issue link (part
	// c) — no new persistent store. Public like the other /api/contribute* reads
	// (only public issue metadata + public PR numbers; no tokens, no PII). Fetched
	// after page load so a slow GitHub PR-link lookup never delays the page render.
	s.mux.HandleFunc("GET /api/contribute/triage", s.handleContributeTriage)
	// Operator priority override for the ready-work queue. Owner/read-write only —
	// enforced IN-HANDLER via requireContributorWrite because the /api/contribute
	// prefix is exempt from roleEnforcement's read-only block (see that helper).
	s.mux.HandleFunc("PUT /api/contribute/queue/order", s.handleContributeQueueOrder)
	// Operator HOLD toggle for the ready-work queue. Owner/read-write only, gated
	// exactly like queue/order above (in-handler requireContributorWrite). Parks an
	// issue so it is never offered until Resumed — a persistent hold DISTINCT from
	// the time-based cooldown. Body: {"key":"owner/repo#number","held":true|false}.
	s.mux.HandleFunc("POST /api/contribute/queue/hold", s.handleContributeQueueHold)
	// Resume-all (bulk clear): drops the ENTIRE operator hold set in one call so an
	// operator does not have to Resume parked issues one at a time. Same owner/read-
	// write gate and refreshAndPersist path as the single-issue hold endpoint above.
	s.mux.HandleFunc("POST /api/contribute/queue/hold/clear", s.handleContributeQueueHoldClear)
	// Contributor-owned LABEL INTERESTS (#2637): a contributor's opt-in list of
	// GitHub labels they can help with, used to surface/prioritise matching issues
	// FOR THEM on the Operations queue. Self-service (identity resolved server-side,
	// never a client param), so a contributor reads/writes only their OWN interests;
	// it is a preference, not an operator control. GET reads, PUT replaces.
	s.mux.HandleFunc("GET /api/contribute/interests", s.handleContributeInterests)
	s.mux.HandleFunc("PUT /api/contribute/interests", s.handleContributeInterests)
	// Contributor-owned DOSSIER fields (archetype / specializations / testimony /
	// equipped title / credly link / emblem seed) — self-service like interests
	// above; identity resolved server-side, every field optional. See dossier.go.
	s.mux.HandleFunc("GET /api/contribute/dossier", s.handleContributeDossier)
	s.mux.HandleFunc("POST /api/contribute/dossier", s.handleContributeDossier)
	s.mux.HandleFunc("GET /api/contributors", s.handleContributorsList)
	s.mux.HandleFunc("GET /api/contributors/{id}", s.handleContributorGet)
	s.mux.HandleFunc("PUT /api/contributors/{id}/trust", s.handleContributorTrust)
	s.mux.HandleFunc("PUT /api/contributors/{id}/agent-role", s.handleContributorAgentRole)
	s.mux.HandleFunc("PUT /api/contributors/{id}/agent-role-grants", s.handleContributorAgentRoleGrants)
	s.mux.HandleFunc("POST /api/contributors/{id}/revoke", s.handleContributorRevoke)
	s.mux.HandleFunc("POST /api/contributors/{id}/requeue", s.handleContributorRequeue)
	s.mux.HandleFunc("DELETE /api/contributors/{id}", s.handleContributorDelete)

	s.mux.HandleFunc("GET /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("POST /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("GET /api/docs", s.handleAPIDocs)

	s.mux.HandleFunc("GET /leaderboard", s.handleLeaderboardPage)
	s.mux.HandleFunc("GET /api/leaderboard", s.handleLeaderboardAPI)
	s.mux.HandleFunc("GET /api/leaderboard/style", s.handleLeaderboardStyle)
	// Central per-user "Me" profile — a HUB endpoint returning one contributor's
	// cross-hive profile (identity/tier/stats/milestones/hives/rank), aggregated
	// from central hub data (contributor store + LeaderboardForHub + federation
	// registry). Read-only; public via isPublicPath's /api/leaderboard prefix. The
	// Leaderboard tab calls it to render the personal "Me" card. See me_profile.go.
	s.mux.HandleFunc("GET /api/leaderboard/contributor/{username}", s.handleContributorProfile)
	// HERALDRY — the trimmed public Credly badges for one contributor (their own
	// credly.com profile mirrored through a 6h cache). Read-only; lives under the
	// /api/leaderboard prefix so isPublicPath makes it public like the profile
	// endpoint above. See dossier.go.
	s.mux.HandleFunc("GET /api/leaderboard/contributor/{username}/heraldry", s.handleContributorHeraldry)

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

// contributorSaveMu serializes profile writes across ALL goroutines (H2,
// CWE-613/639). Every persisted mutation goes through saveContributorProfile, which
// reloads the on-disk profile under this lock, reconciles it with the caller's copy,
// bumps the version, and writes — so two concurrent writers (e.g. a WS stats update
// and an admin revoke) can never race to clobber each other's change. The lock is
// process-wide (the store is a single directory on the hive's PVC) and held only for
// the brief read-reconcile-write window.
var contributorSaveMu sync.Mutex

// terminalTiers are trust tiers that, once written to disk, are SERVER-AUTHORITATIVE
// and TERMINAL for non-admin writers (H2). A WS-path save carrying a live in-memory
// profile pointer must never be able to move the tier OUT of one of these — that was
// the account-un-revoke primitive: a stale WS save after an admin revoke restored
// "contributor". Only the admin trust/revoke handlers (which pass adminOverride) may
// change a terminal tier.
func isTerminalTier(tier string) bool {
	return tier == "revoked"
}

func saveContributorProfile(p *ContributorProfile) error {
	return saveContributorProfileCAS(p, false)
}

// saveContributorProfileCAS persists a contributor profile under the global save
// lock with optimistic-concurrency and the revocation-terminal invariant (H2,
// CWE-613/639). It:
//
//   - reloads the CURRENT on-disk profile (if any) under contributorSaveMu;
//   - enforces the terminal-tier fence UNLESS adminOverride: if the disk copy is in a
//     terminal tier (revoked) but the incoming copy is not, the incoming TrustTier is
//     OVERRIDDEN back to the disk value, so a stale WS save cannot un-revoke an
//     account. Admin paths (trust/revoke) pass adminOverride=true to intentionally
//     change a terminal tier;
//   - detects a lost update: when the caller's Version is non-zero and the disk
//     Version has advanced past it, the write is rejected with errProfileConflict so
//     the caller can reload+retry rather than clobber the newer state;
//   - bumps Version and writes atomically (temp + rename).
//
// A first-ever write (no disk file) or a legacy unversioned profile (Version 0)
// proceeds and is upgraded to version 1+.
func saveContributorProfileCAS(p *ContributorProfile, adminOverride bool) error {
	if strings.Contains(p.GitHubUsername, "..") || strings.Contains(p.GitHubUsername, "/") || strings.Contains(p.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save")
	}
	contributorSaveMu.Lock()
	defer contributorSaveMu.Unlock()

	// Reload the authoritative on-disk copy (bypasses any in-memory staleness).
	if cur, err := loadContributorProfile(p.GitHubUsername); err == nil && cur != nil {
		// Revocation is server-authoritative and terminal for non-admin writers:
		// never let a non-admin save move the tier out of a terminal state.
		if !adminOverride && isTerminalTier(cur.TrustTier) && !isTerminalTier(p.TrustTier) {
			p.TrustTier = cur.TrustTier
		}
		// Optimistic-concurrency: reject a stale writer whose base version is behind
		// the disk. A zero base version (legacy/unversioned caller) is exempt so
		// existing call sites keep working; those saves still get the terminal-tier
		// fence above.
		if p.Version != 0 && p.Version < cur.Version {
			return errProfileConflict
		}
		p.Version = cur.Version
	}
	p.Version++

	ensureDir(getContributorsDir())
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(getContributorsDir(), p.GitHubUsername+".json")
	tmpPath := path + ".tmp"
	// SECURITY (audit N12, CWE-522): owner-only. These profiles hold the
	// registration-token HASH plus contributor PII (username, avatar, trust
	// tier, activity), and 0644 made every one of them readable by any UID in
	// the pod — including agent UIDs. Compare the invite secret at :79, which
	// has always been 0600. The mode goes on the TEMP file, before the rename,
	// so there is no window where the final path is world-readable.
	if err := os.WriteFile(tmpPath, data, contributorProfileFileMode); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// errProfileConflict is returned by saveContributorProfileCAS when a stale writer's
// base version is behind the current on-disk version (H2). The caller should reload
// the profile, re-apply its intended change, and retry.
var errProfileConflict = fmt.Errorf("contributor profile version conflict")

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
	// Slow path: scan all profiles to match by contributor_id OR by GitHub
	// username case-insensitively. The fast path above is an exact-case file
	// lookup, but a GitHub login's case is not stable across the surfaces that
	// call us: the profile file is written under whatever case first registered
	// it, while a viewer resolved from the OAuth session (resolveViewerUsername)
	// can arrive in a different case. The Leaderboard's "YOU" badge already
	// matches the viewer to their row case-insensitively (uname.toLowerCase()
	// === ccMeUsername.toLowerCase()), so the interests attach on the queue
	// endpoint must resolve the SAME contributor the SAME way — otherwise a
	// signed-in, leaderboard-present contributor whose stored filename differs
	// only in case gets a nil profile and the "My label interests" editor never
	// un-hides (issue #2637 follow-up). EqualFold mirrors the leaderboard match.
	profiles := listContributorProfiles()
	for i := range profiles {
		if profiles[i].ContributorID == id || strings.EqualFold(profiles[i].GitHubUsername, id) {
			return &profiles[i]
		}
	}
	return nil
}

// ── Landing page ───────────────────────────────────────────────────────────

// handleContributeLanding renders the public sign-up page for ClankeR, the
// contributor relay: it explains the deal, offers per-CLI copy-paste setup
// commands, and shows a live feed of contributor activity.
// handleContributorDossierPage serves the public dossier permalink
// /contribute/dossier/{username}.
//
// It deliberately serves the SAME landing HTML as every other /contribute
// surface: the client reads location.pathname, activates the Profile tab and
// renders the named contributor's record. One renderer, two entry points — the
// tab is simply "your own username, already signed in".
//
// Nothing sensitive is gated here, because nothing sensitive is served here: the
// dossier is built from BuildContributorProfile, which returns only data the
// public leaderboard already exposes. Owner-only CONTROLS are hidden client-side
// as a UX affordance, while the endpoints behind them (dossier save, invite
// minting) each resolve the caller server-side and refuse to act for anyone but
// their owner — the same posture the rest of this file takes.
func (s *Server) handleContributorDossierPage(w http.ResponseWriter, r *http.Request) {
	if !validGitHubUsername(r.PathValue("username")) {
		http.Redirect(w, r, "/contribute/leaderboard", http.StatusFound)
		return
	}
	s.handleContributeLanding(w, r)
}

// jsStringLiteral renders v as a complete, self-quoting JavaScript string
// literal that is safe to emit inside an inline <script> element (#3315).
//
// Two distinct escapes are required and neither alone is sufficient:
//
//   - json.Marshal handles the JavaScript string grammar — quotes, backslashes,
//     newlines and other control characters — so the value cannot terminate the
//     literal. HTML escaping does NOT do this job: inside <script> the parser is
//     in script data state, so "&#39;" is NOT decoded back to a quote and an
//     html.EscapeString'd value is not protected at all.
//   - JSON does not escape "/", so a value containing "</script>" would close
//     the enclosing element before the JS parser ever sees the string. Breaking
//     that byte sequence with "<\/" keeps the HTML tokenizer inside the script
//     element while remaining the identical string value to JavaScript.
//
// The result INCLUDES its surrounding quotes, so callers must interpolate it
// bare (var x=%s), never inside quotes of their own (var x='%s').
func jsStringLiteral(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		// json.Marshal only fails here for un-encodable types; a string is
		// always encodable. Fail closed with an empty literal rather than
		// emitting anything unescaped.
		return `""`
	}
	out := string(b)
	// Case-insensitively neutralize any "</script" and the HTML comment opener,
	// both of which end script data state regardless of JS quoting.
	out = scriptCloseRe.ReplaceAllString(out, `<\/$1`)
	out = strings.ReplaceAll(out, "<!--", `<\!--`)
	return out
}

// scriptCloseRe matches the "</" that begins a script-element end tag, in any
// letter case, so it can be neutralized inside a JS string literal.
var scriptCloseRe = regexp.MustCompile(`(?i)</(script)`)

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
	// Tier colors route through the palette tokens so the light ramp reaches
	// them (#4560); each token's dark value is the exact hex this table used.
	tierStats := []tierStat{
		{"Active", "var(--cc-green)", activeCount},
		{"Newcomer", "var(--cc-amber)", tierCounts["newcomer"]},
		{"Contributor", "var(--cc-accent)", tierCounts["contributor"]},
		{"Trusted", "var(--cc-green)", tierCounts["trusted"]},
		{"Merger", "var(--cc-pink)", tierCounts["merger"]},
		{"Advisor", "var(--cc-purple)", tierCounts["advisor"]},
		{"Revoked", "var(--cc-red)", tierCounts["revoked"]},
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
	//     contributor scopes.
	//   - Merger is the explicit maintainer/owner-granted trust tier for queueing
	//     others' PRs for auto-merge. The server-side queue endpoint still forbids
	//     queueing your own PR.
	tierTableRows := fmt.Sprintf(
		`<tr><td>Contributor</td><td>%d tasks that produced a PR</td><td>Create PRs, push code</td></tr>`+
			`<tr><td>Trusted</td><td>~%d PR tasks, then granted by a maintainer</td><td>Extra review scope (checks:read)</td></tr>`+
			`<tr><td>Merger</td><td>Granted by a maintainer/owner</td><td>Queue others' PRs for auto-merge — never your own</td></tr>`,
		contributorAutoPromoteAt, contributorTrustedAt,
	)

	customStyleHeadHTML := ""
	customStyleNoticeHTML := ""
	if rawStyle := strings.TrimSpace(r.URL.Query().Get("style")); rawStyle != "" {
		if _, src, report, err := getCustomStyle(r.Context(), rawStyle, customStyleScopeLeaderboard); err == nil {
			styleKey := leaderboardCustomStyleCacheKey(src)
			escapedSrc := html.EscapeString(styleKey)
			styleKeyJSON, _ := json.Marshal(styleKey)
			droppedJSON, _ := json.Marshal(report.Dropped)
			customStyleHeadHTML = fmt.Sprintf(
				`<link id="leaderboard-custom-style-link" rel="stylesheet" href="/api/leaderboard/style?src=%s"><script>window.HIVE_LEADERBOARD_CUSTOM_STYLE_SRC=%s;window.HIVE_LEADERBOARD_CUSTOM_STYLE_DROPPED=%s;</script>`,
				url.QueryEscape(styleKey),
				string(styleKeyJSON),
				string(droppedJSON),
			)
			if report.Dropped > 0 {
				customStyleNoticeHTML = fmt.Sprintf(`<div class="lb-custom-style-note lb-custom-style-note--warn" id="leaderboard-custom-style-note" role="status" title="Some CSS was removed because it can fetch external resources, uses unsupported at-rules, or contains legacy executable CSS.">Custom style active: <code>%s</code> (%d rules removed by sanitizer)</div>`, escapedSrc, report.Dropped)
			} else {
				customStyleNoticeHTML = fmt.Sprintf(`<div class="lb-custom-style-note" id="leaderboard-custom-style-note" role="status">Custom style active: <code>%s</code></div>`, escapedSrc)
			}
		} else {
			customStyleNoticeHTML = `<div class="lb-custom-style-note lb-custom-style-note--warn" id="leaderboard-custom-style-note" role="status">Custom style could not be loaded — using default <button type="button" data-action="dismiss-parent">Dismiss</button></div>`
		}
	}

	// SECURITY (#3315): encode the two values that land in a JS string context
	// AT THE SINK. json.Marshal yields a complete, quoted JS string literal with
	// quotes, backslashes, newlines and control characters escaped, so neither a
	// hostile Host header nor a hostile project name can terminate the literal.
	// Additionally neutralize "</script" (JSON does not escape "/"), which would
	// otherwise close the enclosing <script> element regardless of JS quoting.
	hubURLJS := jsStringLiteral(hubURL)
	projectNameJS := jsStringLiteral(projectName)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// {{HIVE_BRANCH}} is substituted into the TEMPLATE, before Fprintf renders
	// it, so the onboarding clone command names the branch this hub is actually
	// built from instead of a hardcoded one (kubestellar/hive#3990).
	//
	// It is a pre-pass rather than another %s because this format string carries
	// eleven positional arguments across several thousand lines; inserting a
	// verb mid-template would renumber every argument after it. Substituting on
	// the template also means no rendered value — project name, Host header —
	// can ever contain the sentinel and be rewritten by it.
	//
	// Rendered into a buffer, not straight to w: this page's inline <script>
	// content varies per response (hubURL derives from the Host header, and the
	// optional custom-style script from the ?style= query), so its CSP
	// script-src-elem hashes can only be computed from the finished document.
	// applyDocumentScriptSrcElem below stamps them before the first Write
	// (#3848 part 1 / #3907, see csp_script_src.go).
	var page bytes.Buffer
	fmt.Fprintf(&page, strings.ReplaceAll(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Contribute to %s</title>
<!-- #4549 theme FOUC guard. Runs BEFORE the stylesheet below is parsed, so a
     visitor who pinned a theme never sees a frame of the other one. Kept to the
     single attribute write on purpose: everything else about the control (the
     button label, persistence, the cycle) lives in the deferred block at the
     foot of the document, because none of it affects the first paint. An inline
     script element is fine under CSP — applyDocumentScriptSrcElem stamps a
     sha256 for every inline script in the finished document (csp_script_src.go);
     it is inline on*= ATTRIBUTES that are forbidden (ADR-0016), which is why the
     button dispatches through data-action instead of onclick. -->
<script>
(function(){try{var t=localStorage.getItem('hive.contribute.theme');if(t==='light'||t==='dark')document.documentElement.setAttribute('data-theme',t);}catch(e){}})();
</script>
<style>
/* Michroma display face, base64-embedded (no network fonts). Used ONLY by the
   dossier hero name + rank designation (.dz-heroname / .dz-rankpill .rank-name). */
%s
/* ── Theme tokens (#2612 part d) ─────────────────────────────────────────────
   The /contribute page shipped with a hardcoded GitHub-dark palette and zero
   light-mode infrastructure. These custom properties name the palette once so
   the whole sheet can flip to a light appearance without touching any rule. The
   :root defaults below are the ORIGINAL dark hex values byte-for-byte, so the
   dark appearance is unchanged; only the neutral surface/border/text ramp is
   tokenized (accents like blue/green/amber/red read fine on both themes and are
   pinned by exact-match tests, so they stay literal and are exposed here only as
   named tokens for the light-mode fixups below).
   Light mode activates on BOTH signals — the OS preference AND an explicit
   :root[data-theme="light"] hook — so a future in-page toggle can force it
   (the toggle itself is out of scope). data-theme="dark" always wins back to
   dark even under a light OS preference. Values are Primer-light neutrals so the
   surface blends with the surrounding Docusaurus docs chrome.
   #4549: each block also declares color-scheme, which is what tells the browser
   how to paint the chrome CSS cannot reach — the <select> popups on the
   onboarding tab, scrollbars, and the focus ring. Without it a forced-light page
   still drops a black dropdown over a white form. */
:root{
  color-scheme:dark;
  --cc-bg:#0d1117;
  --cc-bg-deep:#010409;
  --cc-surface:#161b22;
  --cc-border:#30363d;
  --cc-border-2:#21262d;
  --cc-text:#e6edf3;
  --cc-text-2:#c9d1d9;
  --cc-muted:#8b949e;
  --cc-muted-2:#6e7681;
  --cc-code-bg:#0d1117;
  --cc-accent:#58a6ff;
  --cc-accent-2:#79c0ff;
  --cc-accent-fg:#1f6feb;
  --cc-green:#3fb950;
  --cc-amber:#d29922;
  --cc-red:#f85149;
  --cc-pink:#f778ba;
  --cc-purple:#bc8cff;
}
@media(prefers-color-scheme:light){:root:not([data-theme="dark"]){
  color-scheme:light;
  --cc-bg:#ffffff;
  --cc-bg-deep:#f6f8fa;
  --cc-surface:#f6f8fa;
  --cc-border:#d0d7de;
  --cc-border-2:#eaeef2;
  --cc-text:#1f2328;
  --cc-text-2:#32383f;
  --cc-muted:#636c76;
  --cc-muted-2:#7d858e;
  --cc-code-bg:#eff1f3;
  --cc-accent:#0969da;
  --cc-accent-2:#0550ae;
  --cc-accent-fg:#0969da;
  --cc-green:#1a7f37;
  --cc-amber:#9a6700;
  --cc-red:#cf222e;
  --cc-pink:#bf3989;
  --cc-purple:#8250df;
}}
:root[data-theme="light"]{
  color-scheme:light;
  --cc-bg:#ffffff;
  --cc-bg-deep:#f6f8fa;
  --cc-surface:#f6f8fa;
  --cc-border:#d0d7de;
  --cc-border-2:#eaeef2;
  --cc-text:#1f2328;
  --cc-text-2:#32383f;
  --cc-muted:#636c76;
  --cc-muted-2:#7d858e;
  --cc-code-bg:#eff1f3;
  --cc-accent:#0969da;
  --cc-accent-2:#0550ae;
  --cc-accent-fg:#0969da;
  --cc-green:#1a7f37;
  --cc-amber:#9a6700;
  --cc-red:#cf222e;
  --cc-pink:#bf3989;
  --cc-purple:#8250df;
}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:var(--cc-bg);color:var(--cc-text);margin:0;min-height:100vh}
.page{display:flex;min-height:100vh;width:100%%}
.main{flex:3;padding:40px 48px;overflow-y:auto}
.sidebar{flex:1;background:var(--cc-surface);border-left:1px solid var(--cc-border);display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow-y:auto}
h1{font-size:2rem;margin-bottom:8px}
.subtitle{color:var(--cc-muted);font-size:1.1rem;margin-bottom:32px}
.stat-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(80px,1fr));gap:10px;margin-bottom:24px}
.stat{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:10px;padding:14px 8px;text-align:center}
.stat-num{font-size:1.5rem;font-weight:700;color:var(--cc-accent)}
.stat-label{font-size:.7rem;color:var(--cc-muted);margin-top:4px}
.steps{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:12px;padding:24px;margin-top:24px}
.steps h3{margin-top:0;color:var(--cc-accent)}
.steps ol{padding-left:20px;line-height:2}
code{background:var(--cc-bg);padding:2px 8px;border-radius:4px;font-size:.9rem}
.how{margin-top:32px}
.how h3{color:var(--cc-text)}
.how p{color:var(--cc-muted);line-height:1.6}
.tier-table{width:100%%;border-collapse:collapse;margin-top:16px}
.tier-table th,.tier-table td{padding:8px 12px;text-align:left;border-bottom:1px solid var(--cc-border);font-size:.85rem}
.tier-table th{color:var(--cc-muted);font-weight:600}
.feed-header{padding:20px 20px 12px;border-bottom:1px solid var(--cc-border);display:flex;align-items:center;gap:8px}
.feed-header h3{font-size:.95rem;color:var(--cc-text)}
.feed-dot{width:8px;height:8px;border-radius:50%%;background:var(--cc-green);animation:pulse 2s infinite}
@keyframes pulse{0%%,100%%{opacity:1}50%%{opacity:.4}}
.feed-count{font-size:.75rem;color:var(--cc-muted);margin-left:auto}
.feed-scroll{flex:1;overflow-y:auto;padding:0}
.feed-entry{padding:10px 20px;border-bottom:1px solid var(--cc-border-2);font-size:.85rem;animation:fadeIn .3s ease;display:flex;align-items:flex-start;gap:12px}
@keyframes fadeIn{from{opacity:0;transform:translateY(-4px)}to{opacity:1;transform:translateY(0)}}
.feed-entry:hover{background:rgba(88,166,255,.04)}
.feed-text{flex:1;min-width:0}
.feed-time{color:var(--cc-muted);font-size:.75rem;white-space:nowrap;flex-shrink:0}
.feed-role{color:var(--cc-accent);font-weight:500}
.feed-cli{color:var(--cc-muted);font-size:.8rem}
.feed-empty{padding:40px 20px;text-align:center;color:var(--cc-muted);font-size:.85rem}
@media(max-width:768px){.page{flex-direction:column}.sidebar{border-left:none;border-top:1px solid var(--cc-border);max-width:none;max-height:300px}}
/* Management & Operations tab chrome — additive, does not touch onboarding content */
/* #4537 follow-up: five tabs at 14px 20px plus 96px of gutters are ~700px, so
   between 600px (where the phone breakpoint's wrap kicks in) and ~700px the bar
   still overflowed the document and the trailing tabs were unreachable. Wrap in
   the base rule — a no-op at any width where the tabs fit on one line. */
.page-tabs{display:flex;flex-wrap:wrap;gap:2px;background:var(--cc-surface);border-bottom:1px solid var(--cc-border);padding:0 48px}
.page-tab{background:none;border:none;color:var(--cc-muted);font-size:.95rem;font-weight:500;padding:14px 20px;cursor:pointer;border-bottom:2px solid transparent;font-family:inherit}
.page-tab:hover{color:var(--cc-text)}
.page-tab.active{color:var(--cc-text);border-bottom-color:var(--cc-accent)}
.tab-panel{display:none}
.tab-panel.active{display:block}
.ops{padding:40px 48px;overflow-y:auto}
.ops h1{font-size:1.7rem;margin-bottom:6px}
/* #4537: minmax(0,1fr), not 1fr. A 1fr track's automatic minimum is min-content,
   and the cards it holds contain white-space:nowrap text (.cc-q-title, .cc-army,
   the clanker sub-lines), so the track resolved to the widest unbreakable line
   and the whole panel outgrew .ops. The explicit 0 minimum lets the track shrink
   to the viewport and hands the overflow back to the ellipsis/wrapping the cards
   already declare. */
.ops-grid{display:grid;grid-template-columns:340px minmax(0,1fr);gap:20px;margin-top:24px}
@media(max-width:900px){.ops-grid{grid-template-columns:minmax(0,1fr)}}
.ops-card{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:12px;padding:0;overflow:hidden}
.ops-card-head{padding:16px 20px;border-bottom:1px solid var(--cc-border);display:flex;align-items:center;gap:10px}
.ops-card-head h3{font-size:.95rem;color:var(--cc-text);margin:0}
.ops-card-count{font-size:.75rem;color:var(--cc-muted);margin-left:auto}
.ops-filters{display:flex;gap:4px;padding:12px 20px;border-bottom:1px solid var(--cc-border-2);flex-wrap:wrap}
.ops-filter{background:var(--cc-bg);border:1px solid var(--cc-border);color:var(--cc-muted);font-size:.78rem;padding:4px 12px;border-radius:999px;cursor:pointer;font-family:inherit}
.ops-filter.active{background:#1f6feb;border-color:var(--cc-accent-fg);color:#fff}
.work-list{max-height:520px;overflow-y:auto}
.work-item{padding:14px 20px;border-bottom:1px solid var(--cc-border-2);cursor:pointer}
.work-item:hover{background:rgba(88,166,255,.04)}
.work-item.selected{background:rgba(88,166,255,.08)}
.work-repo{font-size:.75rem;color:var(--cc-muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
/* ── Clickable GitHub issue/PR references (#2616) ────────────────────────────────
   Shared affordance for every repo#number reference on the Operations tab (ready
   queue, my-work, opportunistic-work, dev-log). Deliberately more visible than
   the surrounding muted-grey monospace text — link-blue + underline-on-hover +
   a small external-link glyph — so it reads as an obvious "open on GitHub"
   action, not decoration. Inherits the host element's font (monospace repo#num,
   or inline log text) so it drops into any of those contexts unchanged. */
.cc-issue-link{display:inline-flex;align-items:center;gap:3px;color:var(--cc-accent);text-decoration:none;font:inherit;border-radius:4px;transition:color .15s}
.cc-issue-link:hover,.cc-issue-link:focus-visible{color:var(--cc-accent-2);text-decoration:underline}
.cc-issue-link:focus-visible{outline:2px solid var(--cc-accent);outline-offset:2px}
.cc-issue-link-ic{flex-shrink:0;opacity:.85}
.cc-issue-link:hover .cc-issue-link-ic{opacity:1}
.work-title{font-size:.9rem;color:var(--cc-text);margin:2px 0 6px}
.work-meta{display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:.75rem;color:var(--cc-muted)}
.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:.7rem;font-weight:600;border:1px solid transparent}
.pill-progress{background:rgba(88,166,255,.12);color:var(--cc-accent);border-color:rgba(88,166,255,.3)}
.pill-review{background:rgba(210,153,34,.12);color:var(--cc-amber);border-color:rgba(210,153,34,.3)}
.pill-passed{background:rgba(63,185,80,.12);color:var(--cc-green);border-color:rgba(63,185,80,.3)}
.pill-blocked{background:rgba(248,81,73,.12);color:var(--cc-red);border-color:rgba(248,81,73,.3)}
.pill-idle{background:rgba(139,148,158,.12);color:var(--cc-muted);border-color:rgba(139,148,158,.3)}
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
/* #4537: the first two tracks are SIZED, not auto. .admin-actions below spans
   1/-1, and a spanning item's min-content contribution is distributed into the
   spanned tracks that can take it — the third is minmax(0,1fr) with a fixed 0
   minimum and absorbs none, so both auto tracks grew until the row was wider
   than its card. The dot and the avatar are 8px and 28px at every width, so
   naming those widths is what auto already resolved to when nothing spanned;
   it just leaves nothing for the spanning row to inflate. */
.clanker-row{display:grid;grid-template-columns:8px 28px minmax(0,1fr);align-items:start;column-gap:10px;row-gap:8px;padding:12px 20px;border-bottom:1px solid var(--cc-border-2)}
/* The trailing controls / timestamp: full-width line beneath the identity. It is
   always the LAST grid child, so grid-column:1/-1 drops it below regardless of
   whether it's .admin-actions or the .feed-time fallback. */
.clanker-row>.admin-actions,.clanker-row>.feed-time{grid-column:1/-1}
.clanker-av{width:28px;height:28px;border-radius:50%%;flex-shrink:0;background:var(--cc-border)}
.clanker-main{min-width:0}
.clanker-user{font-size:.88rem;color:var(--cc-text);font-weight:500}
.clanker-sub{font-size:.74rem;color:var(--cc-muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;overflow-wrap:anywhere;word-break:break-word}
/* Row is align-items:start (grid), so nudge the small dot down to sit level with
   the username's first line instead of the very top of the row. */
.clanker-dot{width:8px;height:8px;border-radius:50%%;background:var(--cc-green);flex-shrink:0;margin-top:7px}
.clanker-dot.stale{background:var(--cc-muted)}
.pipeline{display:flex;align-items:center;gap:6px;flex-wrap:wrap;margin:14px 0}
.pipe-node{background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:8px;padding:8px 14px;font-size:.82rem;color:var(--cc-text)}
.pipe-node .lgtm{color:var(--cc-green);font-size:.72rem}
.pipe-arrow{color:var(--cc-muted)}
.policy-row{display:flex;justify-content:space-between;gap:12px;padding:8px 0;border-bottom:1px solid var(--cc-border-2);font-size:.85rem}
.policy-row:last-child{border-bottom:none}
.policy-key{color:var(--cc-muted)}
.policy-val{color:var(--cc-text);text-align:right;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-word}
.ops-empty{padding:32px 20px;text-align:center;color:var(--cc-muted);font-size:.85rem}
.lb-row{display:grid;grid-template-columns:56px 1fr 120px 70px 70px 80px 72px;align-items:center;gap:8px;padding:10px 20px;border-bottom:1px solid var(--cc-border-2);font-size:.85rem}
.lb-row:last-child{border-bottom:none}
/* Subtle self-highlight for the logged-in viewer's own row: a faint tint + a left
   accent border, professional not loud. Readability preserved. */
.lb-row--me{background:rgba(31,111,235,.09);box-shadow:inset 3px 0 0 0 #1f6feb}
.lb-you{display:inline-block;margin-left:8px;font-size:.62rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-accent);background:rgba(31,111,235,.14);border:1px solid rgba(31,111,235,.3);border-radius:999px;padding:1px 7px;vertical-align:middle}
.lb-head{color:var(--cc-muted);font-weight:600;font-size:.72rem;text-transform:uppercase;letter-spacing:.04em;background:var(--cc-bg)}
.lb-rank{color:var(--cc-muted);font-variant-numeric:tabular-nums}
.lb-name{color:var(--cc-text);font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.lb-name__link{color:inherit;text-decoration:none}
.lb-name__link:hover{color:var(--cc-accent);text-decoration:underline}
.lb-tier{color:var(--cc-muted)}
.lb-stat{text-align:right;color:var(--cc-text-2);font-variant-numeric:tabular-nums}
.lb-head .lb-stat,.lb-head .lb-rank{text-align:right;color:var(--cc-muted)}
.lb-head .lb-rank{text-align:left}
/* ── Subtle "ranked/alive" accent pass on Operations + Leaderboard (SUBTLE +
   PROFESSIONAL — light watermarking only). Theme-aware: built entirely from the
   existing dark palette (var(--cc-surface) card / var(--cc-border) border / var(--cc-bg) deep). Every
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
.tier-badge{display:inline-flex;align-items:center;gap:5px;font-size:.68rem;font-weight:600;line-height:1;padding:3px 8px 3px 6px;border-radius:999px;border:1px solid var(--cc-border);background:var(--cc-bg);color:var(--cc-muted);text-transform:capitalize;white-space:nowrap}
.tier-badge::before{content:"";width:8px;height:8px;border-radius:50%%;background:currentColor;box-shadow:inset 0 0 0 1px rgba(1,4,9,.35);flex:none}
.tier-badge.tier-advisor{border-color:rgba(210,169,85,.45);background:rgba(210,169,85,.10);color:#d0a955}
.tier-badge.tier-merger{border-color:rgba(247,120,186,.42);background:rgba(247,120,186,.10);color:var(--cc-pink)}
.tier-badge.tier-trusted{border-color:rgba(201,162,39,.40);background:rgba(201,162,39,.08);color:#c9a94a}
.tier-badge.tier-contributor{border-color:rgba(110,163,201,.38);background:rgba(110,163,201,.08);color:#6ea3c9}
.tier-badge.tier-newcomer{border-color:var(--cc-border);background:var(--cc-bg);color:var(--cc-muted)}
/* Restrained gradient header band on the accented cards. Very low-contrast wash
   from the deep bg into the card colour — reads as a faint banner, not a loud
   gradient; the bottom border keeps the head crisp. */
.ops-card.card-accent>.ops-card-head{background:linear-gradient(180deg,#12161d 0%%,var(--cc-surface) 100%%)}
.ops-card.card-accent>.ops-card-head h3{letter-spacing:.01em}
/* Bold-numeral stat emphasis: the primary "Done" numeral on the leaderboard and
   the key ops counts get heavier weight + slightly larger tabular figures so the
   number reads as the hero of the row without adding chrome. The Me-card's own
   stat numerals reuse this same bold/tabular treatment via .lb-stat.lb-primary. */
.lb-row .lb-stat.lb-primary{color:var(--cc-text);font-weight:700;font-size:.95rem}
.lb-head .lb-stat.lb-primary{font-weight:600;font-size:.72rem;color:var(--cc-muted)}
.tier-badge.tier-lb{padding:2px 8px 2px 5px;font-size:.66rem}
/* Ops "your army" counts + card counts as bold numerals (tabular, no layout shift). */
.cc-army b{font-weight:700;font-variant-numeric:tabular-nums}
.ops-card-count.count-strong{color:var(--cc-text);font-weight:700;font-variant-numeric:tabular-nums}
/* Small circled-i info affordance next to a header (cooldown explainer, #2649
   companion). A borderless button carrying the ⓘ glyph; hover/focus brightens it.
   The popover is an absolutely-positioned card toggled open by JS (aria-expanded),
   anchored to the wrapper so it sits just under the glyph. */
.info-affordance{position:relative;display:inline-flex;align-items:center}
.info-btn{background:none;border:0;padding:0 2px;margin-left:4px;color:var(--cc-muted-2);cursor:pointer;font-size:.85rem;line-height:1;vertical-align:middle}
.info-btn:hover,.info-btn:focus{color:var(--cc-accent);outline:none}
.info-pop{position:absolute;top:130%%;left:0;z-index:40;width:300px;max-width:78vw;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:8px;padding:10px 12px;box-shadow:0 8px 24px rgba(1,4,9,.7);color:var(--cc-text-2);font-size:.74rem;line-height:1.5;font-weight:400;text-align:left;white-space:normal}
.info-pop[hidden]{display:none}
.info-pop h4{margin:0 0 4px;font-size:.76rem;color:var(--cc-text);font-weight:600}
.info-pop ul{margin:4px 0 0;padding-left:16px}
.info-pop li{margin:2px 0}
.info-pop code{background:var(--cc-surface);border:1px solid var(--cc-border-2);border-radius:4px;padding:0 3px;font-size:.7rem}
.custom-css-help .info-btn{font-size:.72rem;font-weight:600;color:var(--cc-accent)}
.custom-css-pop{width:min(320px,calc(100vw - 32px));z-index:10002}
.custom-css-example{box-sizing:border-box;width:100%%;margin:6px 0 4px;background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-text-2);font:12px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;padding:6px}
/* Compact tier badge inline next to a connected clanker's identity. */
.tier-badge.tier-inline{padding:1px 6px 1px 4px;font-size:.62rem;margin-left:6px;vertical-align:middle}
.tier-badge.tier-inline::before{width:6px;height:6px}
/* Larger tier-badge variant used as the hero medallion on the personal Me-card
   (see below) — same canonical tier colors/dot, just sized up for a hero slot. */
.tier-badge.tier-hero{font-size:.78rem;padding:5px 12px 5px 8px}
.tier-badge.tier-hero::before{width:10px;height:10px}
/* ── Contributor dossier (faithful port of the approved character-sheet mockup)
   The signed-in "Me" surface is a zoned dossier SHEET, not a single card:
   masthead + epigraph, ZONE A full-width identity plate over the generative
   emblem field, a 5fr/7fr sheet grid (Deeds of Record | Operator Profile),
   the full-width Golden Path, Triumphs+Heraldry | Collaborators, Field Log |
   Theaters of Operation, and a record footer. --me-accent is the viewer's
   ceremony rank metal by default; the 7 style skins only re-tint it. Reduced-
   motion safe. No decay, no streaks, no nags. */
.me-card{position:relative;margin-bottom:20px;--me-accent:#58a6ff;--me-accent-soft:rgba(88,166,255,.14)}
/* Masthead: "{project} · contributor record" (accent) + "DOSSIER {user}" (mono). */
.dz-masthead{display:flex;justify-content:space-between;align-items:baseline;margin-bottom:4px}
.dz-masthead .brand{font-size:.82rem;font-weight:700;letter-spacing:.04em;color:var(--cc-accent)}
.dz-masthead .id{font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;font-size:.72rem;color:var(--cc-muted-2);text-transform:uppercase}
.dz-epigraph{font-size:.82rem;color:var(--cc-muted);margin:0 0 20px}
.dz-epigraph em{font-style:italic;color:var(--cc-text-2)}
/* Zone-card primitives (mockup .card/.zone/.zone-head with the metal dot). */
.dz-zcard{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:12px;padding:20px 22px 22px}
.dz-zone-head{display:flex;align-items:center;gap:8px;font-size:.78rem;font-weight:700;letter-spacing:.06em;text-transform:uppercase;color:var(--cc-muted);padding-bottom:10px;margin-bottom:14px;border-bottom:1px solid var(--cc-border-2)}
.dz-zone-head::before{content:"";width:8px;height:8px;border-radius:50%%;background:var(--me-accent)}
/* The sheet grid: 5fr/7fr two-column rhythm, stacking on small screens. */
.dz-grid{display:grid;grid-template-columns:5fr 7fr;gap:16px;margin-bottom:16px}
@media(max-width:860px){.dz-grid{grid-template-columns:1fr}}
/* ZONE A — identity plate: full-width, bottom-anchored content over the
   generative EMBLEM FIELD (layered conic/radial/repeating-linear gradients,
   deterministically seeded from emblem_seed/username via the --a1/--a2/--p1/
   --p2 custom props the client sets inline). color-mix keeps the darkening
   tied to --cc-bg so the plate degrades gracefully in light mode. */
.dz-identity{position:relative;overflow:hidden;margin-bottom:16px;border:1px solid var(--cc-border);border-radius:14px;min-height:300px;display:flex;flex-direction:column;justify-content:flex-end;background:var(--cc-surface)}
@media(max-width:860px){.dz-identity{min-height:220px}}
.me-emblem{position:absolute;inset:0;pointer-events:none;--a1:210deg;--a2:160deg;--p1:30%%;--p2:62%%;background:repeating-linear-gradient(var(--a2),transparent 0 22px,rgba(255,255,255,.02) 22px 23px),conic-gradient(from var(--a1) at var(--p1) 20%%,transparent 0deg,var(--me-accent-soft) 40deg,transparent 90deg,var(--me-accent-soft) 165deg,transparent 210deg,var(--me-accent-soft) 300deg,transparent 360deg),radial-gradient(700px 340px at var(--p2) 0%%,var(--me-accent-soft),transparent 70%%),linear-gradient(180deg,color-mix(in srgb,var(--cc-bg) 10%%,transparent),color-mix(in srgb,var(--cc-bg) 92%%,transparent) 85%%),var(--cc-bg-deep)}
.dz-identity-inner{position:relative;padding:28px 26px 22px;display:flex;gap:20px;align-items:center;flex-wrap:wrap}
/* Circular medallion with the rank-metal ring. */
.dz-medallion{width:84px;height:84px;flex:none;border-radius:50%%;display:flex;align-items:center;justify-content:center;background:radial-gradient(circle at 50%% 35%%,var(--me-accent-soft),var(--cc-bg) 78%%);border:2px solid var(--me-accent);box-shadow:0 0 0 4px rgba(1,4,9,.35)}
.dz-medallion img{width:70px;height:70px;border-radius:50%%;object-fit:cover;background:var(--cc-border)}
.dz-namebloc{flex:1;min-width:240px}
/* Hero name: the Michroma display treatment (embedded above — no network). */
.dz-heroname{font-family:'Michroma','Arial Narrow',sans-serif;font-size:clamp(1.6rem,3.6vw,2.6rem);font-weight:400;letter-spacing:.1em;line-height:1.1;text-transform:uppercase;color:var(--cc-text);margin:0}
/* Rank pill: the ceremony DESIGNATION (rank metal) big, "trust · tier" small. */
.dz-rankpill{flex:none;text-align:center;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:12px;padding:10px 16px}
.dz-rankpill .rank-name{font-family:'Michroma','Arial Narrow',sans-serif;font-size:.92rem;letter-spacing:.14em;color:var(--me-accent);text-transform:uppercase}
.dz-rankpill .rank-sub{font-size:.64rem;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--cc-muted);margin-top:4px}
/* Livebar strip along the bottom of the plate — only when a task is live. */
.dz-livebar{position:relative;display:flex;align-items:center;gap:10px;flex-wrap:wrap;padding:10px 26px 12px;border-top:1px solid var(--cc-border-2);background:color-mix(in srgb,var(--cc-bg-deep) 40%%,transparent);font-size:.74rem;color:var(--cc-text-2)}
.dz-livebar .dot{width:8px;height:8px;border-radius:50%%;background:var(--cc-green);flex:none}
@media(prefers-reduced-motion:no-preference){.dz-livebar .dot{animation:dzpulse 2s infinite}@keyframes dzpulse{50%%{opacity:.35}}}
.dz-livebar .live-tag{font-weight:700;font-size:.68rem;letter-spacing:.06em;color:var(--cc-green)}
.dz-livebar .live-dim{color:var(--cc-muted);font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;font-size:.7rem}
/* Founding mark: real registration order only (first twenty), never faked. */
.me-founding{display:inline-block;margin-top:8px;padding:2px 9px;border-radius:999px;font-size:.64rem;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--me-accent);border:1px solid var(--me-accent);background:var(--me-accent-soft)}
/* ZONE B — Deeds of Record: 2-col grid of stat blocks. The numeral keeps the
   canonical bold .lb-stat.lb-primary treatment, tinted only for standing. */
.dz-deeds{display:grid;grid-template-columns:1fr 1fr;gap:10px}
.dz-deed{background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:10px;padding:12px 10px;text-align:center}
.dz-deed .num{font-size:1.4rem;font-weight:700;color:var(--cc-text);font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace}
.dz-deed .num small{font-size:.85rem;color:var(--cc-muted);font-weight:400}
.dz-deed .cap{font-size:.66rem;color:var(--cc-muted);margin-top:4px}
.dz-deed--standing .num{color:var(--me-accent)}
/* Full-width Golden Path card: zone-head left, "next designation" right. */
.dz-path-head{display:flex;justify-content:space-between;align-items:baseline;flex-wrap:wrap;gap:8px}
.dz-path-head .dz-zone-head{margin:0;border:none;padding:0}
.dz-path-next{font-size:.82rem;color:var(--cc-muted)}
.dz-path-next b{font-family:'Michroma','Arial Narrow',sans-serif;font-size:.88rem;letter-spacing:.1em;color:var(--cc-text);font-weight:400}
/* ZONE D — Triumphs: milestone SEALS (mockup .seal blocks; attained ones are
   solid, the next one to chase renders with a dashed border, never a nag). */
.dz-seals{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:10px}
.dz-seal{background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:10px;padding:12px 14px}
.dz-seal .glyph{width:8px;height:8px;border-radius:50%%;background:var(--me-accent);margin-bottom:8px}
.dz-seal .t-name{font-size:.78rem;font-weight:700;letter-spacing:.04em;color:var(--cc-text)}
.dz-seal .t-sub{font-size:.7rem;color:var(--cc-muted);margin-top:5px;line-height:1.5}
.dz-seal--next{border-style:dashed}
.dz-seal--next .glyph{background:var(--cc-border)}
.dz-seal--next .t-name{color:var(--cc-text-2)}
/* Heraldry divider inside the Triumphs card. */
.dz-heraldry-head{margin-top:18px;padding-top:14px;border-top:1px dashed var(--cc-border);display:flex;justify-content:space-between;align-items:baseline;font-size:.7rem;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--cc-muted);margin-bottom:14px}
/* ZONE E — Collaborators: the empty state shown until the first real joint
   operation is recorded. An invitation to go and meet someone, not a placeholder. */
.dz-collab-empty{font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;font-size:.78rem;color:var(--cc-muted);line-height:1.8}
/* Collaborators — the people you have worked alongside. Each row links to that
   contributor's own dossier, so the collection is navigable. */
.dz-collabs{display:grid;gap:8px}
.dz-collab{display:flex;align-items:center;gap:10px;padding:7px 9px;border-radius:9px;
  border:1px solid var(--cc-border);background:var(--cc-bg);text-decoration:none;color:inherit}
.dz-collab:hover{border-color:var(--me-accent)}
.dz-collab__av{width:28px;height:28px;border-radius:50%%;flex:none;background:var(--cc-surface)}
.dz-collab__body{display:flex;flex-direction:column;min-width:0}
.dz-collab__name{font-size:.84rem;font-weight:600;color:var(--cc-text)}
.dz-collab__how{font-size:.7rem;color:var(--cc-muted-2);text-transform:uppercase;letter-spacing:.04em}
/* ZONE F — Field Log rows: mono time-ago + entry. */
.dz-flog{display:grid;gap:10px}
.dz-frow{display:grid;grid-template-columns:5.5rem 1fr;gap:12px;align-items:baseline}
.dz-frow .f-when{font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;font-size:.7rem;color:var(--cc-muted-2)}
.dz-frow .f-what{font-size:.8rem;color:var(--cc-text-2);line-height:1.55}
.dz-frow .f-what b{font-weight:600;color:var(--cc-accent)}
.dz-frow .f-what span{color:var(--cc-muted)}
/* Record footer: HIVE // id + the per-hive closing quote. */
.dz-footer{display:flex;justify-content:space-between;align-items:baseline;flex-wrap:wrap;gap:8px;margin-top:8px;padding:14px 4px 0;border-top:1px solid var(--cc-border-2);font-size:.72rem;color:var(--cc-muted-2);font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;text-transform:uppercase}
.dz-footer .quote{color:var(--cc-muted);text-transform:none}
/* Theaters of Operation: hives render as rows — name + relationship pill. */
.me-hives{display:grid;gap:8px}
.me-hive{display:flex;align-items:center;gap:10px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:10px;padding:10px 14px;font-size:.82rem;color:var(--cc-text-2)}
.me-hive__name{font-size:.82rem;font-weight:700;letter-spacing:.03em;color:var(--cc-text);flex:1;text-transform:uppercase}
.me-hive__rel{font-size:.66rem;font-weight:700;text-transform:uppercase;letter-spacing:.03em;padding:2px 7px;border-radius:6px;background:var(--me-accent-soft);color:var(--me-accent)}
.me-hive__rel--owner{background:rgba(210,153,34,.16);color:var(--cc-amber)}
.me-actions{display:flex;flex-wrap:wrap;gap:10px;margin-top:18px;align-items:center}
.me-share{display:inline-flex;align-items:center;gap:7px;padding:9px 16px;border-radius:10px;font-size:.85rem;font-weight:600;text-decoration:none;background:var(--me-accent);color:var(--cc-bg);border:1px solid var(--me-accent);cursor:pointer;font-family:inherit}
.me-share:hover{filter:brightness(1.08)}
.me-share--ghost{background:transparent;color:var(--me-accent)}
.me-stylepick{margin-left:auto;display:flex;align-items:center;gap:7px;font-size:.72rem;color:var(--cc-muted)}
.me-stylepick select{background:var(--cc-bg);border:1px solid var(--cc-border);color:var(--cc-text-2);border-radius:8px;padding:5px 8px;font-size:.78rem;font-family:inherit;cursor:pointer}
.me-signin{background:var(--cc-surface);border:1px dashed var(--cc-border);border-radius:14px;padding:22px;text-align:center;color:var(--cc-muted);font-size:.9rem;margin-bottom:20px}
.me-signin b{color:var(--cc-text)}
/* Leaderboard standing strip — the one-line remnant of the dossier on the
   standings tab. Deliberately unobtrusive: the Rankings are the content here. */
.me-standing{display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap;
  background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:12px;
  padding:10px 16px;margin-bottom:16px;font-size:.86rem;color:var(--cc-muted)}
.me-standing b{color:var(--cc-text)}
.me-standing__link{color:var(--cc-accent);text-decoration:none;font-weight:600;white-space:nowrap}
.me-standing__link:hover{text-decoration:underline}
/* ── The 7 profile-style skins (palette / framing / density variations) ────────
   Each is a tasteful, readable, professional variant of the SAME card. They only
   change accent palette, header treatment, medallion framing, and density —
   never the data or the affordances. Default (style1) needs no override. */
/* style1 "Rank metal" is the DEFAULT: it takes its accent from the viewer's
   ceremony rank metal (--me-metal/--me-metal-soft, set inline by the client
   from the trust tier). Styles 2..7 override --me-accent directly, so picking
   any other skin beats the rank metal — exactly like the old Signal blue. */
.me-card--style1{--me-accent:var(--me-metal,#58a6ff);--me-accent-soft:var(--me-metal-soft,rgba(88,166,255,.14))}
.me-card--style2{--me-accent:#3fb950;--me-accent-soft:rgba(63,185,80,.14)}
.me-card--style3{--me-accent:#d29922;--me-accent-soft:rgba(210,153,34,.15)}
.me-card--style4{--me-accent:#a371f7;--me-accent-soft:rgba(163,113,247,.15)}
.me-card--style5{--me-accent:var(--cc-muted);--me-accent-soft:rgba(139,148,158,.12)}     /* minimal / restrained */
.me-card--style5 .dz-identity{min-height:0}
.me-card--style5 .me-emblem{display:none}
.me-card--style5 .dz-medallion{background:var(--cc-bg)}
.me-card--style6{--me-accent:#f778ba;--me-accent-soft:rgba(247,120,186,.14)}
.me-card--style6 .me-emblem{opacity:.75}
.me-card--style7{--me-accent:#58a6ff;--me-accent-soft:rgba(88,166,255,.18)}     /* roomy "ranked" */
.me-card--style7 .dz-identity{min-height:340px}
.me-card--style7 .dz-identity-inner{padding:34px 30px 26px}
/* ── Contributor dossier additions (me-card v2) ───────────────────────────────
   Identity band gains an equipped-title callsign + designation line; the body
   gains the operator-profile rows (archetype / specializations / loadout /
   sponsor / active), the testimony blockquote, THE GOLDEN PATH progress zone and the
   HERALDRY hall (the viewer's own public Credly badges, floating free — no
   boxes). Everything is tinted by the SAME --me-accent the 7 style skins drive,
   so every skin themes the dossier for free. No decay, no streaks, no nags. */
.me-callsign{font-size:.74rem;font-weight:600;letter-spacing:.18em;color:var(--me-accent);text-transform:uppercase;margin-bottom:4px}
.me-desig{font-size:.82rem;color:var(--cc-muted);margin-top:8px}
.me-desig b{font-weight:600;color:var(--cc-text-2)}
.me-prows{display:grid;gap:9px}
.me-prow{display:grid;grid-template-columns:128px 1fr;gap:12px;align-items:baseline}
.me-prow .k{font-size:.68rem;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--cc-muted)}
.me-prow .v{font-size:.86rem;color:var(--cc-text)}
.me-prow .v.mono{font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;font-size:.78rem}
.me-prow .v .unset{color:var(--cc-muted-2)}
.me-specs{display:flex;flex-wrap:wrap;gap:6px}
.me-spec{display:inline-block;padding:2px 8px;border-radius:999px;font-size:.7rem;font-weight:600;border:1px solid var(--me-accent);background:var(--me-accent-soft);color:var(--me-accent)}
.me-testimony{margin-top:14px;padding:12px 14px;background:var(--cc-bg);border-left:3px solid var(--me-accent);border-radius:6px;font-size:.9rem;color:var(--cc-text-2)}
.me-testimony .attr{display:block;font-size:.64rem;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--cc-muted);margin-top:6px}
.me-dossier-invite{margin-top:14px;font-size:.8rem}.me-dossier-invite a{color:var(--me-accent);text-decoration:none;cursor:pointer}
.me-dossier-invite a:hover{text-decoration:underline}
.me-dossier-form{display:none;margin-top:12px;padding:14px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:10px}
.me-dossier-form.open{display:grid;gap:10px}
.me-dossier-form label{display:grid;gap:4px;font-size:.66rem;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--cc-muted)}
.me-dossier-form input,.me-dossier-form textarea{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:8px;color:var(--cc-text);font-family:inherit;font-size:.85rem;padding:8px 10px;outline:none;resize:vertical}
.me-dossier-form input:focus,.me-dossier-form textarea:focus{border-color:var(--me-accent)}
.me-dossier-form .hint{font-size:.68rem;font-weight:400;letter-spacing:0;text-transform:none;color:var(--cc-muted-2)}
.me-dossier-form .actions{display:flex;gap:10px;align-items:center}
.me-dossier-save{padding:8px 16px;border-radius:10px;font-size:.82rem;font-weight:600;background:var(--me-accent);color:var(--cc-bg);border:1px solid var(--me-accent);cursor:pointer;font-family:inherit}
.me-dossier-cancel{background:transparent;border:none;color:var(--cc-muted);font-size:.78rem;cursor:pointer;font-family:inherit}
.me-path-reqs{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:14px 22px;margin-top:12px}
.me-path-req{margin-top:4px}
.me-path-req .req-top{display:flex;justify-content:space-between;align-items:baseline;margin-bottom:5px}
.me-path-req .req-name{font-size:.68rem;font-weight:600;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-muted)}
.me-path-req .req-num{font-family:ui-monospace,'SF Mono',SFMono-Regular,Menlo,Consolas,monospace;font-size:.72rem;color:var(--cc-text-2)}
.me-path-req .bar{height:6px;border-radius:999px;background:var(--cc-border-2);position:relative;overflow:hidden}
.me-path-req .bar i{position:absolute;inset:0 auto 0 0;border-radius:999px;background:var(--me-accent);display:block}
/* Ceremony ladder: RECRUIT · OPERATOR · SPECIALIST · WARDEN · VANGUARD · PARAGON */
.me-ladder{display:flex;flex-wrap:wrap;gap:6px;align-items:center;margin-top:14px;font-size:.68rem;font-weight:600;color:var(--cc-muted-2)}
.me-ladder .rung{display:flex;align-items:center;gap:6px}
.me-ladder .rung::after{content:"\00B7";color:var(--cc-border);margin-left:6px}
.me-ladder .rung:last-child::after{content:none}
.me-ladder .rung.attained{color:var(--me-accent)}
/* A rung no trust tier can grant yet — visibly out of reach, never implied next. */
.me-ladder .rung.aspirational{opacity:.45;font-style:italic}
.me-ladder .rung.current{color:var(--cc-text)}
.me-path-note{margin-top:10px;font-size:.72rem;color:var(--cc-muted)}
.me-heraldry{display:grid;grid-template-columns:repeat(auto-fill,minmax(128px,1fr));gap:20px 12px}
.me-arms{position:relative;text-align:center;text-decoration:none;padding:4px 4px 2px;transition:transform .18s ease;display:block}
.me-arms:hover{transform:translateY(-3px)}
.me-arms .shield{width:96px;height:96px;margin:0 auto;display:block;position:relative;z-index:1;filter:drop-shadow(0 10px 14px rgba(1,4,9,.65))}
.me-arms .shield::before{content:"";position:absolute;inset:-14px;z-index:-1;border-radius:50%%;background:radial-gradient(closest-side,var(--me-accent-soft),transparent 72%%)}
.me-arms .shield img{width:100%%;height:100%%;object-fit:contain}
.me-arms .plinth{display:block;width:64px;height:8px;margin:2px auto 0;border-radius:50%%;background:radial-gradient(closest-side,rgba(1,4,9,.85),transparent)}
.me-arms .ribbon{display:inline-block;margin-top:8px;max-width:100%%;font-size:.66rem;font-weight:700;letter-spacing:.05em;text-transform:uppercase;color:var(--cc-text);line-height:1.35}
.me-arms .a-sub{display:block;font-size:.62rem;color:var(--cc-muted);margin-top:3px;line-height:1.45}
.me-heraldry-note{font-size:.78rem;color:var(--cc-muted)}
.me-heraldry-note a{color:var(--me-accent);text-decoration:none;cursor:pointer}
.lb-title{font-size:.68rem;font-weight:600;letter-spacing:.06em;color:var(--cc-accent);margin-left:7px}
@media(max-width:560px){.me-prow{grid-template-columns:1fr;gap:2px}}
@media(max-width:520px){.dz-identity-inner{flex-wrap:wrap}.dz-deeds{grid-template-columns:1fr}}
@media(prefers-reduced-motion:reduce){.me-card *{transition:none!important;animation:none!important}}
.ops-note{color:var(--cc-muted-2);font-size:.78rem;margin-top:12px;line-height:1.5}
.ops-note code{background:var(--cc-bg);padding:1px 6px;border-radius:4px}
.prompt-preview{margin-top:10px;border-top:1px solid var(--cc-border-2);padding-top:8px}
.prompt-preview summary{cursor:pointer;color:var(--cc-accent);font-size:.78rem;list-style:none}
.prompt-preview summary::-webkit-details-marker{display:none}
.prompt-preview summary::before{content:'\25B8 ';color:var(--cc-muted)}
.prompt-preview[open] summary::before{content:'\25BE '}
.prompt-labels{margin:8px 0 4px;display:flex;flex-wrap:wrap;gap:4px}
.prompt-text{margin-top:8px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:8px;padding:12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.75rem;color:var(--cc-text-2);white-space:pre-wrap;word-break:break-word;max-height:220px;overflow-y:auto}
.prompt-preview .ops-note{margin-top:8px}
/* #2534 Operator admin controls — mirror the Governor Hub config controls into the
   Management & Operations tab. Owner/read-write only; a read viewer never sees them. */
.ops-admin{display:none}
.ops-admin.enabled{display:block}
.admin-badge{font-size:.68rem;font-weight:600;padding:2px 8px;border-radius:999px;background:rgba(210,153,34,.12);color:var(--cc-amber);border:1px solid rgba(210,153,34,.3);margin-left:auto}
.admin-body{padding:16px 20px}
.admin-toggle{display:flex;align-items:center;gap:10px;padding:8px 0}
.admin-switch{width:38px;height:20px;border-radius:999px;background:var(--cc-border);position:relative;cursor:pointer;flex-shrink:0;transition:background .15s}
.admin-switch::after{content:'';position:absolute;top:2px;left:2px;width:16px;height:16px;border-radius:50%%;background:var(--cc-text);transition:left .15s}
.admin-switch.on{background:#1f6feb}
.admin-switch.on.danger{background:var(--cc-red)}
.admin-switch.on::after{left:20px}
.admin-toggle-label{font-size:.85rem;color:var(--cc-text)}
.admin-toggle-sub{font-size:.74rem;color:var(--cc-muted)}
.admin-field{margin:14px 0}
.admin-field>label{display:block;font-size:.78rem;color:var(--cc-muted);margin-bottom:6px}
.admin-modeseg{display:inline-flex;border:1px solid var(--cc-border);border-radius:6px;overflow:hidden;margin-bottom:6px}
.admin-modeseg button{background:var(--cc-bg);border:none;color:var(--cc-muted);font-size:.72rem;padding:3px 10px;cursor:pointer;font-family:inherit}
.admin-modeseg button.on{background:#1f6feb;color:#fff}
.admin-chips{display:flex;flex-wrap:wrap;gap:4px;margin-bottom:6px}
.admin-chip{display:inline-flex;align-items:center;gap:4px;padding:2px 8px;border-radius:999px;font-size:.72rem;background:rgba(139,148,158,.12);color:var(--cc-text-2);border:1px solid var(--cc-border)}
.admin-chip .x{cursor:pointer;opacity:.7}
.admin-chip .x:hover{opacity:1;color:var(--cc-red)}
.admin-addrow{display:flex;gap:4px}
.admin-addrow input{flex:1;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-text);font-size:.78rem;padding:5px 8px;font-family:inherit}
.admin-addrow button,.admin-save{background:#238636;border:1px solid #2ea043;color:#fff;font-size:.75rem;padding:5px 12px;border-radius:6px;cursor:pointer;font-family:inherit}
.admin-addrow button{background:var(--cc-border-2);border-color:var(--cc-border);color:var(--cc-text-2)}
.admin-save{margin-top:8px}
.admin-save:disabled{opacity:.5;cursor:default}
.admin-hr{border:none;border-top:1px solid var(--cc-border-2);margin:16px 0}
/* Repos-for-Contribute enable toggles + Tier rate-limit rows (Management mirror of
   the Governor Hub sections). Subtle, matching the rest of the admin controls. */
.admin-repos{display:flex;flex-wrap:wrap;gap:8px}
.admin-repo{display:inline-flex;align-items:center;gap:8px;padding:6px 10px;border:1px solid var(--cc-border);border-radius:8px;background:var(--cc-bg)}
.admin-repo .admin-switch{width:32px;height:18px}
.admin-repo .admin-switch::after{width:14px;height:14px}
.admin-repo .admin-switch.on::after{left:16px}
.admin-repo__name{font-size:.76rem;color:var(--cc-text-2);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.admin-tier{display:grid;grid-template-columns:1fr repeat(3,64px);align-items:center;gap:8px;padding:8px 0;border-bottom:1px solid var(--cc-border-2)}
.admin-tier:last-child{border-bottom:none}
.admin-tier__head{display:flex;align-items:center;gap:8px;min-width:0}
.admin-tier__name{font-size:.8rem;color:var(--cc-text);text-transform:capitalize}
.admin-tier input{width:100%%;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-text);font:inherit;font-size:.78rem;padding:4px 6px;outline:none;text-align:right}
.admin-tier input:focus{border-color:var(--cc-accent-fg)}
.admin-tier input:disabled{opacity:.45}
.admin-tier__col{font-size:.62rem;color:var(--cc-muted-2);text-align:right;text-transform:uppercase;letter-spacing:.03em}
.admin-tier--head{border-bottom:1px solid var(--cc-border);padding-bottom:4px}
/* No margin-left:auto — .admin-actions is now a full-width grid row beneath the
   identity (see .clanker-row grid), left-aligned and wrapping if the buttons
   don't fit the narrow column. */
.admin-actions{display:flex;gap:6px;flex-wrap:wrap}
.admin-act{background:var(--cc-border-2);border:1px solid var(--cc-border);color:var(--cc-text-2);font-size:.7rem;padding:3px 9px;border-radius:6px;cursor:pointer;font-family:inherit}
.admin-act:hover{border-color:var(--cc-muted)}
.admin-act.danger:hover{border-color:var(--cc-red);color:var(--cc-red)}
.admin-act select{background:var(--cc-bg);border:1px solid var(--cc-border);color:var(--cc-text-2);font-size:.7rem;border-radius:6px;padding:2px 4px;font-family:inherit}
.agent-role-grants{display:flex;align-items:center;gap:6px;flex-wrap:wrap;width:100%%;font-size:.7rem;color:var(--cc-muted)}
.agent-role-grants__label{font-weight:600;color:var(--cc-text-2)}
/* #4537: a flex item defaults to min-width:auto, so this one held the literal
   "Acting as" plus a <select> whose intrinsic width is its widest option
   ("none (general work)") and refused to shrink — .admin-actions' flex-wrap had
   nothing it was allowed to break, so the whole line ran off the card. */
.clanker-act-as{display:inline-flex;align-items:center;flex-wrap:wrap;gap:4px;min-width:0;max-width:100%%;color:var(--cc-muted);font-size:.72rem}
/* The tier and acting-as controls ARE the <select> (class on the element), which
   is why the .admin-act select descendant rule above never matched them. */
select.admin-act{min-width:0;max-width:100%%}
.agent-role-chip{display:inline-flex;align-items:center;gap:4px;padding:2px 7px;border-radius:999px;border:1px solid rgba(88,166,255,.28);background:rgba(88,166,255,.08);color:var(--cc-accent-2)}
.agent-role-chip button{border:none;background:transparent;color:inherit;cursor:pointer;padding:0;line-height:1;opacity:.75;font:inherit}
.agent-role-chip button:hover{opacity:1;color:var(--cc-red)}
.agent-role-add{background:var(--cc-bg);border:1px solid var(--cc-border);color:var(--cc-text-2);font-size:.7rem;border-radius:6px;padding:2px 4px;font-family:inherit}
.admin-modal-back{display:none;position:fixed;inset:0;background:rgba(1,4,9,.7);z-index:1000;align-items:center;justify-content:center}
.admin-modal-back.show{display:flex}
.admin-modal{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:12px;max-width:420px;width:90%%;padding:22px}
.admin-modal h4{margin:0 0 8px;font-size:1rem;color:var(--cc-text)}
.admin-modal p{font-size:.85rem;color:var(--cc-muted);line-height:1.5;margin:0 0 18px}
.admin-modal-btns{display:flex;gap:8px;justify-content:flex-end}
.admin-modal-btns button{font-size:.8rem;padding:6px 14px;border-radius:6px;cursor:pointer;font-family:inherit;border:1px solid var(--cc-border);background:var(--cc-border-2);color:var(--cc-text-2)}
.admin-modal-btns button.confirm{background:#da3633;border-color:var(--cc-red);color:#fff}
.admin-note{color:var(--cc-muted-2);font-size:.76rem;margin-top:10px;line-height:1.5}
/* ── Operations command center — live SSE-driven queue / travel / dev-log /
   achievements / army framing. Subtle-professional motion only; degrades to the
   existing poll when SSE is unavailable. Additive, read-only. ─────────────── */
.cc-live{display:inline-flex;align-items:center;gap:6px;font-size:.68rem;font-weight:600;padding:2px 8px;border-radius:999px;margin-left:auto;border:1px solid rgba(63,185,80,.3);background:rgba(63,185,80,.1);color:var(--cc-green)}
.cc-live .cc-live-dot{width:7px;height:7px;border-radius:50%%;background:var(--cc-green);animation:pulse 2s infinite}
.cc-live.stale{border-color:rgba(210,153,34,.3);background:rgba(210,153,34,.1);color:var(--cc-amber)}
/* Polling (stale) dot: a very slow, gentle breathe rather than the brisk live
   pulse — signals "still watching, just on the calmer poll cadence". */
.cc-live.stale .cc-live-dot{background:var(--cc-amber);animation:cc-slowpulse 2.8s ease-in-out infinite}
@keyframes cc-slowpulse{0%%,100%%{opacity:1}50%%{opacity:.45}}
@media(prefers-reduced-motion:reduce){.cc-live .cc-live-dot,.cc-live.stale .cc-live-dot{animation:none!important}}
/* Ready-work queue play/pause — the SAME contribute_suspended control as the
   Management "Suspend contributions" switch, surfaced on the queue header.
   Quiet by default (bordered ghost button); the danger tint only appears once
   paused, matching .admin-switch.on.danger's accent so the two placements read
   as one state. Left of #cc-live so status (queue live/stale) and posture
   (active/paused) sit as a pair. */
#queue-suspend-wrap{display:inline-flex;align-items:center;gap:6px;margin-left:auto}
.queue-suspend-btn{display:inline-flex;align-items:center;justify-content:center;width:26px;height:26px;padding:0;border-radius:999px;border:1px solid var(--cc-border);background:transparent;color:var(--cc-muted);cursor:pointer;line-height:0;transition:background .15s,color .15s,border-color .15s}
/* SVG glyph is centered by the flex-box; currentColor tracks the button state.
   Using an SVG (not a &#10074; bar glyph) so the pause bars sit dead-center —
   the light-vertical-bar character carries font side-bearing that pushed the
   pair off-center inside the circle. */
.queue-suspend-btn svg{display:block;width:12px;height:12px;fill:currentColor}
.queue-suspend-btn:hover{background:rgba(139,148,158,.12);color:var(--cc-text-2)}
.queue-suspend-btn.paused{border-color:rgba(248,81,73,.35);color:var(--cc-red);background:rgba(248,81,73,.08)}
.queue-suspend-btn.paused:hover{background:rgba(248,81,73,.16)}
.queue-suspend-btn:disabled{opacity:.5;cursor:not-allowed}
/* Army roster header line under the clanker card */
.cc-army{display:flex;align-items:center;gap:14px;padding:10px 20px;border-bottom:1px solid var(--cc-border-2);font-size:.78rem;color:var(--cc-muted)}
.cc-army b{color:var(--cc-text);font-weight:600}
.cc-army-stat{display:inline-flex;align-items:center;gap:5px}
.cc-army-stat .dot{width:7px;height:7px;border-radius:50%%}
.cc-army-stat.working .dot{background:var(--cc-accent)}
.cc-army-stat.reviewing .dot{background:var(--cc-amber)}
.cc-army-stat.idle .dot{background:var(--cc-muted)}
/* Clanker rows: enter pop-in / leave fade so the roster feels alive */
@keyframes cc-popin{from{opacity:0;transform:translateY(-6px) scale(.98)}to{opacity:1;transform:none}}
@keyframes cc-fadeout{from{opacity:1}to{opacity:0;transform:translateX(8px)}}
.clanker-row.cc-enter{animation:cc-popin .4s ease}
.clanker-row.cc-leave{animation:cc-fadeout .5s ease forwards}
/* A clanker actively receiving a travelling task pulses its border briefly */
@keyframes cc-landing{0%%{box-shadow:0 0 0 0 rgba(88,166,255,.5)}100%%{box-shadow:0 0 0 6px rgba(88,166,255,0)}}
.clanker-row.cc-landing{animation:cc-landing .8s ease}
.clanker-status{font-size:.68rem;font-weight:600;padding:1px 7px;border-radius:999px;margin-left:6px;border:1px solid transparent}
.clanker-status.working{background:rgba(88,166,255,.12);color:var(--cc-accent);border-color:rgba(88,166,255,.3)}
.clanker-status.reviewing{background:rgba(210,153,34,.12);color:var(--cc-amber);border-color:rgba(210,153,34,.3)}
.clanker-status.idle{background:rgba(139,148,158,.12);color:var(--cc-muted);border-color:rgba(139,148,158,.3)}
/* Ready-work QUEUE — the stack of issues waiting to be picked off. A generous
   max-height keeps a long backlog (up to ~150 items) scrolling inside the card
   instead of stretching the page; the panel scrolls, the page does not. */
.cc-queue{max-height:560px;overflow-y:auto}
/* The enter animation is OPT-IN via .cc-q-enter (added only to genuinely-new rows),
   NOT baked into .cc-q-item — otherwise every poll re-render replayed cc-popin on
   every row and the whole queue "blinked". Mirrors .clanker-row.cc-enter above. */
.cc-q-item{display:flex;align-items:flex-start;gap:10px;padding:11px 20px;border-bottom:1px solid var(--cc-border-2);position:relative}
.cc-q-item.cc-q-enter{animation:cc-popin .35s ease}
.cc-q-item:first-child{background:rgba(88,166,255,.05)}
/* Drag handle (grab bar) — owner/read-write only. Hidden unless the queue root
   carries .cc-q-draggable (set by initAdmin after /api/role). Reduced-motion and
   pointer friendly. */
.cc-q-grip{display:none;flex-shrink:0;width:16px;align-self:stretch;cursor:grab;color:var(--cc-muted-2);font-size:.9rem;line-height:1;align-items:center;justify-content:center;user-select:none;touch-action:none}
.cc-q-grip:hover{color:var(--cc-text-2)}
.cc-queue.cc-q-draggable .cc-q-grip{display:flex}
.cc-queue.cc-q-draggable .cc-q-item{cursor:default}
.cc-q-item.cc-q-dragging{opacity:.5;cursor:grabbing}
.cc-q-item.cc-q-over{box-shadow:inset 0 2px 0 0 #58a6ff}
.cc-q-idx{font-size:.7rem;color:var(--cc-muted-2);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;flex-shrink:0;width:22px;text-align:right;padding-top:2px}
.cc-q-body{flex:1;min-width:0}
.cc-q-repo{font-size:.72rem;color:var(--cc-muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.cc-q-title{font-size:.86rem;color:var(--cc-text);margin:2px 0 4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cc-q-labels{display:flex;flex-wrap:wrap;gap:4px}
.cc-q-next{font-size:.62rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-accent);flex-shrink:0;padding-top:2px}
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
.cc-q-search{display:flex;align-items:center;gap:8px;padding:10px 20px;border-bottom:1px solid var(--cc-border-2)}
.cc-q-search-ic{color:var(--cc-muted-2);font-size:.85rem;flex-shrink:0;line-height:1}
.cc-q-search input{flex:1;min-width:0;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:7px;color:var(--cc-text);font:inherit;font-size:.82rem;padding:6px 10px;outline:none;transition:border-color .15s,box-shadow .15s}
.cc-q-search input::placeholder{color:var(--cc-muted-2)}
.cc-q-search input:focus{border-color:var(--cc-accent-fg);box-shadow:0 0 0 3px rgba(31,111,235,.25)}
.cc-q-search-clear{background:none;border:none;color:var(--cc-muted-2);cursor:pointer;font-size:1rem;line-height:1;padding:2px 4px;display:none}
.cc-q-search.has-text .cc-q-search-clear{display:inline-flex}
.cc-q-search-clear:hover{color:var(--cc-text-2)}
.cc-q-filternote{padding:6px 20px;font-size:.72rem;color:var(--cc-muted-2);border-bottom:1px solid var(--cc-border-2)}
/* ── My label interests (#2637) — contributor-declared label affinity ───────────
   A quiet self-service editor on the queue card: chips for the labels this viewer
   subscribed to, plus an add field. Shown only to a signed-in contributor. Matching
   queue rows are highlighted (.cc-q-mine) and a small "for you" tag explains why.
   Sober palette to match the SRE ops register; the page's green accent marks a
   personal match without shouting. */
.cc-interests{padding:10px 20px;border-bottom:1px solid var(--cc-border-2)}
.cc-interests-head{display:flex;flex-wrap:wrap;align-items:baseline;gap:8px;margin-bottom:6px}
.cc-interests-title{font-size:.74rem;font-weight:700;letter-spacing:.03em;text-transform:uppercase;color:var(--cc-muted)}
.cc-interests-hint{font-size:.68rem;color:var(--cc-muted-2)}
.cc-interests-chips{display:flex;flex-wrap:wrap;gap:5px;margin-bottom:6px}
.cc-interests-empty{font-size:.72rem;color:var(--cc-muted-2)}
.cc-interests-empty code{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:4px;padding:0 4px;font-size:.9em}
.cc-interest-chip{display:inline-flex;align-items:center;gap:5px;padding:2px 9px;border-radius:999px;font-size:.74rem;background:rgba(46,160,67,.12);color:var(--cc-green);border:1px solid rgba(46,160,67,.3)}
.cc-interest-x{cursor:pointer;opacity:.7;font-size:.95rem;line-height:1}
.cc-interest-x:hover{opacity:1}
/* #2677: read-only mirror of a contributor's OWN label interests, shown on their
   row in the operator "Connected clankers" fleet list (Operations tab). Reuses
   the .cc-interest-chip visual (same green affinity color) in a compact, non-
   interactive line so an owner gets a fleet-wide view without editing anything
   here — editing stays contributor-owned via My label interests above. */
.clanker-interests{display:flex;flex-wrap:wrap;align-items:center;gap:4px;margin-top:3px;font-size:.68rem;color:var(--cc-muted-2)}
.clanker-interests-label{color:var(--cc-muted-2)}
.clanker-interest-chip{display:inline-flex;padding:1px 7px;border-radius:999px;font-size:.68rem;background:rgba(46,160,67,.12);color:var(--cc-green);border:1px solid rgba(46,160,67,.3)}
/* #2547 peer compatibility: the hub-vs-client protocol comparison, rendered ONLY
   when the versions actually differ so a healthy fleet stays quiet. Amber (not
   red) on purpose — a drifted peer is still fully served; this is a notice, not
   an error state, and must not read as "this clanker is broken/blocked". */
.clanker-proto{margin-top:3px;font-size:.68rem;color:var(--cc-amber)}
.clanker-proto.incompatible{color:var(--cc-red)}
/* #2637 owner roster: an OWNER-facing aggregate of which labels connected
   contributors subscribe to, and who — so the owner can label matching issues to
   route work. Reuses the green .cc-interest-chip affinity color. Read-only. */
.label-affinity{margin-top:14px;padding-top:12px;border-top:1px solid var(--cc-border-2)}
.label-affinity-head{display:flex;align-items:center;gap:2px;margin-bottom:8px}
.label-affinity-title{font-size:.82rem;font-weight:600;color:var(--cc-text-2)}
.label-affinity-body{display:flex;flex-direction:column;gap:7px}
.affinity-row{display:flex;align-items:baseline;flex-wrap:wrap;gap:8px;font-size:.78rem}
.affinity-chip{display:inline-flex;align-items:center;gap:5px;padding:2px 9px;border-radius:999px;font-size:.74rem;background:rgba(46,160,67,.12);color:var(--cc-green);border:1px solid rgba(46,160,67,.3);flex-shrink:0}
.affinity-count{font-weight:700;color:var(--cc-green);font-size:.7rem}
.affinity-who{color:var(--cc-muted);word-break:break-word}
.affinity-empty{font-size:.74rem;color:var(--cc-muted-2);line-height:1.5}
.cc-interests-add{display:flex;gap:6px}
.cc-interests-add input{flex:1;min-width:0;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:7px;color:var(--cc-text);font:inherit;font-size:.8rem;padding:5px 9px;outline:none;transition:border-color .15s,box-shadow .15s}
.cc-interests-add input::placeholder{color:var(--cc-muted-2)}
.cc-interests-add input:focus{border-color:var(--cc-accent-fg);box-shadow:0 0 0 3px rgba(31,111,235,.25)}
.cc-interests-add button{background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:7px;color:var(--cc-text-2);cursor:pointer;font:inherit;font-size:.78rem;padding:5px 12px}
.cc-interests-add button:hover{border-color:var(--cc-green);color:var(--cc-green)}
/* A queue row matching one of the viewer's label interests: a soft green rail on
   the leading edge + faint tint. Never hides the row — pure emphasis. */
.cc-q-item.cc-q-mine{background:rgba(46,160,67,.06);box-shadow:inset 3px 0 0 0 #2ea043}
.cc-q-item.cc-q-mine:first-child{background:rgba(46,160,67,.1)}
.cc-q-mine-tag{margin-left:7px;font-size:.6rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-green);background:rgba(46,160,67,.12);border:1px solid rgba(46,160,67,.3);border-radius:999px;padding:0 6px;vertical-align:middle}
/* Per-row "⋯" context affordance — owner/read-write only (rendered only when
   adminEnabled). Sits at the row's trailing edge, quiet until hover/open. */
.cc-q-menu-wrap{position:relative;flex-shrink:0;margin-left:auto;align-self:center}
.cc-q-menu-btn{background:none;border:none;color:var(--cc-muted-2);cursor:pointer;font-size:1rem;line-height:1;padding:4px 6px;border-radius:6px}
.cc-q-menu-btn:hover,.cc-q-menu-btn[aria-expanded=true]{color:var(--cc-text);background:var(--cc-border-2)}
.cc-q-menu{position:fixed;top:0;left:0;right:auto;bottom:auto;z-index:10002;min-width:190px;background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:10px;box-shadow:0 8px 28px rgba(1,4,9,.55);padding:6px;display:none}
.cc-q-menu.open{display:block}
/* Fixed-positioned so the per-row menu escapes the scrolling .cc-queue overflow
   clip; ccBindQueueMenus measures the trigger and flips/clamps inside the viewport
   (and visible queue panel) before paint. */
.cc-q-menu button.cc-q-act{display:flex;align-items:center;gap:8px;width:100%%;background:none;border:none;color:var(--cc-text-2);font:inherit;font-size:.82rem;text-align:left;padding:7px 9px;border-radius:6px;cursor:pointer}
.cc-q-menu button.cc-q-act:hover{background:var(--cc-border-2);color:var(--cc-text)}
.cc-q-menu-ic{color:var(--cc-muted-2);flex-shrink:0;width:16px;text-align:center}
.cc-q-menu-sep{height:1px;background:var(--cc-border-2);margin:5px 2px}
.cc-q-moverow{display:flex;align-items:center;gap:6px;padding:7px 9px}
.cc-q-moverow label{font-size:.78rem;color:var(--cc-muted);flex:1}
/* color-scheme makes the native number-input spinner arrows theme-aware, so they
   render legibly against the field in BOTH appearances (previously black-on-black
   and effectively invisible on dark). The tokenized background/color are kept as a
   belt-and-braces fallback for engines that don't honour color-scheme on the
   control; the field itself flips with the (#2612) light/dark tokens. */
.cc-q-moverow input[type=number]{width:56px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-text);font:inherit;font-size:.8rem;padding:4px 6px;outline:none;color-scheme:light dark}
.cc-q-moverow input:focus{border-color:var(--cc-accent-fg)}
.cc-q-moverow button{background:#1f6feb;border:none;color:#fff;font:inherit;font-size:.76rem;font-weight:600;padding:5px 10px;border-radius:6px;cursor:pointer}
.cc-q-moverow button:hover{background:#388bfd}
/* Optional hold-reason field (#queue-hold-reason) in the ⋯ menu — a compact inline
   note the operator can fill before Hold. Empty is fine (holding without a note). */
.cc-q-holdreason{padding:2px 9px 7px}
.cc-q-holdreason-input{width:100%%;box-sizing:border-box;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-text);font:inherit;font-size:.76rem;padding:4px 7px;outline:none}
.cc-q-holdreason-input:focus{border-color:var(--cc-accent-fg)}
/* On-hold rows (#queue-hold): a manually-parked issue stays VISIBLE but is clearly
   not going to be offered — dimmed to ~55%% opacity with an amber "on hold" pill.
   Never hidden, so the operator can always see and Resume it. */
.cc-q-item.cc-q-held{opacity:.55}
.cc-q-item.cc-q-held:hover{opacity:.8}
.cc-q-held-tag{margin-left:7px;font-size:.6rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-amber);background:rgba(210,153,34,.12);border:1px solid rgba(210,153,34,.3);border-radius:999px;padding:0 6px;vertical-align:middle}
/* Resume-all (#queue-hold): a small amber header button. Hidden until there is at
   least one held issue and the viewer is owner/read-write (JS-toggled display). */
.queue-resume-all-btn{margin-left:8px;font-size:.7rem;font-weight:600;color:var(--cc-amber);background:rgba(210,153,34,.1);border:1px solid rgba(210,153,34,.35);border-radius:6px;padding:2px 8px;cursor:pointer;vertical-align:middle;line-height:1.4}
.queue-resume-all-btn:hover{background:rgba(210,153,34,.2)}
/* ── Opportunistic Work (#2592) — a small, CALM discovery panel. Intentionally
   quiet: no loud "recommended!" chrome, just a short curated list with a subtle
   heat dot and an unobtrusive "add to queue" affordance (owner/read-write only). */
.opp-list{padding:2px 0}
.opp-item{display:flex;align-items:flex-start;gap:10px;padding:11px 20px;border-bottom:1px solid var(--cc-border-2)}
.opp-item:last-child{border-bottom:none}
.opp-heat{flex-shrink:0;width:8px;height:8px;border-radius:50%%;margin-top:5px;background:var(--cc-green);box-shadow:0 0 0 3px rgba(63,185,80,.14)}
.opp-heat.warm{background:var(--cc-amber);box-shadow:0 0 0 3px rgba(210,153,34,.14)}
.opp-heat.cool{background:var(--cc-muted-2);box-shadow:none}
.opp-body{flex:1;min-width:0}
.opp-repo{font-size:.72rem;color:var(--cc-muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.opp-title{font-size:.86rem;color:var(--cc-text);margin:2px 0 3px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.opp-reason{font-size:.7rem;color:var(--cc-muted-2)}
.opp-add{flex-shrink:0;align-self:center;background:none;border:1px solid var(--cc-border);color:var(--cc-text-2);font:inherit;font-size:.74rem;font-weight:600;padding:5px 11px;border-radius:7px;cursor:pointer;transition:border-color .15s,color .15s,background .15s}
.opp-add:hover{border-color:var(--cc-accent-fg);color:#fff;background:rgba(31,111,235,.15)}
.opp-add:disabled{opacity:.55;cursor:default;border-color:var(--cc-border);color:var(--cc-muted);background:none}
/* ── End-of-queue + hive-settings (#2595) — turn a short queue into an intentional,
   reassuring moment: a calm "all caught up" marker, the managed-queue rate limits
   presented readably, and the viewer's own daily quota. Sober, ranked-family styling. */
.cc-q-end{padding:18px 20px 6px;text-align:center}
.cc-q-end-badge{display:inline-flex;align-items:center;gap:8px;font-size:.82rem;color:var(--cc-muted);background:var(--cc-bg);border:1px solid var(--cc-border-2);border-radius:999px;padding:7px 16px}
.cc-q-end-badge .cc-q-end-ic{color:var(--cc-green);font-size:.95rem;line-height:1}
.hive-settings{margin:14px 20px 4px;background:var(--cc-bg);border:1px solid var(--cc-border-2);border-radius:10px;padding:14px 16px}
.hive-settings h4{margin:0 0 4px;font-size:.78rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-muted)}
.hive-settings p.hs-lead{margin:0 0 10px;font-size:.82rem;color:var(--cc-text-2);line-height:1.5}
.hs-tiers{display:flex;flex-wrap:wrap;gap:8px}
.hs-tier{flex:1 1 130px;min-width:120px;background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:8px;padding:9px 11px}
.hs-tier__name{font-size:.72rem;font-weight:600;text-transform:capitalize;color:var(--cc-text);display:flex;align-items:center;gap:6px}
.hs-tier__lim{font-size:.74rem;color:var(--cc-muted);margin-top:3px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.hs-tier.is-you{border-color:var(--cc-accent-fg);box-shadow:0 0 0 2px rgba(31,111,235,.18)}
.hs-tier__youtag{font-size:.6rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:var(--cc-accent)}
/* Daily quota widget — a slim progress meter, calm. Used at end-of-queue AND on
   the Me card. Fill width is set inline from the REAL used/limit ratio. */
.quota{margin-top:12px}
.quota__head{display:flex;align-items:baseline;justify-content:space-between;gap:8px;margin-bottom:6px}
.quota__lbl{font-size:.76rem;color:var(--cc-muted)}
.quota__val{font-size:.82rem;color:var(--cc-text);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.quota__bar{height:7px;border-radius:999px;background:var(--cc-border-2);overflow:hidden}
.quota__fill{height:100%%;border-radius:999px;background:linear-gradient(90deg,#1f6feb,#388bfd);transition:width .4s ease}
.quota__fill.near{background:linear-gradient(90deg,#d29922,#e3b341)}
.quota__fill.full{background:linear-gradient(90deg,#f85149,#ff7b72)}
.quota__sub{font-size:.7rem;color:var(--cc-muted-2);margin-top:5px}
/* Me-card quota variant — sits inside a me-sec, so it inherits the card padding. */
.me-quota .quota__lbl{color:var(--cc-muted)}
@media(prefers-reduced-motion:reduce){.quota__fill{transition:none!important}}
/* Sparklines (#persistent-history): tiny dependency-free inline-SVG trend charts
   fed by /api/contribute/metrics (7-day hourly history). Muted stroke to sit
   quietly in the dark theme; static — no animation, so nothing to gate behind
   prefers-reduced-motion. The SVG scales to its slot via width/height attrs. */
.spark{display:inline-block;vertical-align:middle;line-height:0}
.spark svg{display:block;overflow:visible}
.spark-inline{margin-left:8px}
/* Header-adjacent sparkline sits next to a panel title/count without shoving it. */
.ops-card-head .spark{margin-left:auto}
/* Leaderboard per-row sparkline: occupies its own narrow column, muted so the
   numerals stay the focus. */
.lb-spark{display:flex;align-items:center;justify-content:flex-end}
/* Hive-wide trend strip pinned above the standings. */
.lb-trend{display:flex;align-items:center;gap:10px;padding:8px 20px 12px;color:var(--cc-muted);font-size:.76rem;border-bottom:1px solid var(--cc-border-2)}
.lb-trend .spark{margin-left:auto}
/* "File an issue on this page" link (#2594) — a subtle footer affordance present
   on every tab. Quiet grey, matches the sober dashboard chrome; an outbound link. */
.cc-page-foot{padding:26px 48px 34px;border-top:1px solid var(--cc-border-2);margin-top:28px;display:flex;justify-content:center}
.cc-report-link{display:inline-flex;align-items:center;gap:7px;color:var(--cc-muted);font-size:.8rem;text-decoration:none;border:1px solid var(--cc-border);border-radius:8px;padding:7px 14px;transition:color .15s,border-color .15s,background .15s}
.cc-report-link:hover{color:var(--cc-text);border-color:#484f58;background:var(--cc-surface)}
.cc-report-link .cc-report-ic{font-size:.9rem;line-height:1}
/* The travelling token that flies from the queue to a clanker on task_assign */
.cc-token{position:fixed;z-index:1200;pointer-events:none;background:#1f6feb;color:#fff;font-size:.72rem;font-weight:600;padding:6px 12px;border-radius:999px;box-shadow:0 6px 20px rgba(31,111,235,.5);white-space:nowrap;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;transition:transform .9s cubic-bezier(.5,0,.2,1),opacity .9s ease;will-change:transform,opacity}
/* Dev-log — a running chat log of the development */
.cc-log{max-height:360px;overflow-y:auto;padding:4px 0}
.cc-log-line{display:flex;align-items:flex-start;gap:10px;padding:8px 20px;font-size:.83rem;border-bottom:1px solid var(--cc-border-2);animation:cc-logline .45s ease}
@keyframes cc-logline{from{opacity:0;transform:translateY(6px)}to{opacity:1;transform:none}}
.cc-log-line:last-child{border-bottom:none}
.cc-log-ic{flex-shrink:0}
.cc-log-body{flex:1;min-width:0;color:var(--cc-text-2);line-height:1.45}
.cc-log-body b{color:var(--cc-text)}
.cc-log-body .who{color:var(--cc-accent);font-weight:600}
.cc-log-body .ref{color:var(--cc-muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.78rem}
.cc-log-body a.ref.cc-issue-link{color:var(--cc-accent)}
.cc-log-body a.ref.cc-issue-link:hover,.cc-log-body a.ref.cc-issue-link:focus-visible{color:var(--cc-accent-2)}
.cc-log-time{flex-shrink:0;color:var(--cc-muted-2);font-size:.72rem;white-space:nowrap;padding-top:1px}
/* Achievement pops — tasteful badge toast, top-right, debounced */
.cc-ach-wrap{position:fixed;top:16px;right:16px;z-index:1150;display:flex;flex-direction:column;gap:8px;pointer-events:none}
.cc-ach{display:flex;align-items:center;gap:10px;background:linear-gradient(135deg,var(--cc-surface),#1c2333);border:1px solid rgba(210,153,34,.4);border-radius:10px;padding:10px 14px;box-shadow:0 8px 28px rgba(1,4,9,.55);animation:cc-ach-in .4s ease;max-width:300px}
@keyframes cc-ach-in{from{opacity:0;transform:translateX(24px)}to{opacity:1;transform:none}}
.cc-ach.cc-ach-out{animation:cc-ach-out .4s ease forwards}
@keyframes cc-ach-out{to{opacity:0;transform:translateX(24px)}}
.cc-ach-ic{font-size:1.3rem;flex-shrink:0}
.cc-ach-txt{min-width:0}
.cc-ach-h{font-size:.7rem;font-weight:700;letter-spacing:.05em;text-transform:uppercase;color:var(--cc-amber)}
.cc-ach-s{font-size:.82rem;color:var(--cc-text);margin-top:1px}
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
  background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:12px;overflow:hidden;
  display:flex;flex-direction:column;transition:flex-basis .28s ease}
.ops-rail-inner{display:flex;flex-direction:column;min-height:0;flex:1 1 auto;opacity:1;transition:opacity .2s ease}
.ops-rail-head{padding:16px 20px;border-bottom:1px solid var(--cc-border);display:flex;align-items:center;gap:10px;flex-shrink:0}
.ops-rail-head h3{font-size:.95rem;color:var(--cc-text);margin:0}
.ops-rail .cc-log{flex:1 1 auto;max-height:none;min-height:0}
/* The collapse toggle sits on the rail's leading edge (a slim handle). */
.ops-rail-toggle{display:flex;align-items:center;gap:6px;width:100%%;background:var(--cc-bg);border:none;
  border-bottom:1px solid var(--cc-border);color:var(--cc-muted);font-family:inherit;font-size:.74rem;font-weight:600;
  letter-spacing:.03em;text-transform:uppercase;padding:9px 14px;cursor:pointer;flex-shrink:0}
.ops-rail-toggle:hover{color:var(--cc-text);background:var(--cc-surface)}
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
  .clanker-row.cc-enter,.clanker-row.cc-leave,.clanker-row.cc-landing,.cc-q-item,.cc-q-item.cc-q-enter,.cc-q-item.cc-leaving,.cc-q-item.cc-q-flip,.cc-log-line,.cc-ach,.cc-token{animation:none!important;transition:none!important}
  .ops-rail,.ops-rail-inner,.ops-rail-chevron{transition:none!important}
}
/* #2548 Branded client entry points — a find-by-SIGHT tile grid above the CLI
   selector. Each tile carries an inline SVG/glyph emblem so a contributor spots
   their tool visually; clicking a tile just drives the existing #cli-select so
   nothing about the copy-block logic changes. CSP-safe (inline assets only),
   theme-consistent with the dark palette, reduced-motion safe. */
.client-tiles{display:grid;grid-template-columns:repeat(auto-fill,minmax(132px,1fr));gap:8px;margin:4px 0 20px}
.client-tile{display:flex;align-items:flex-start;gap:9px;background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:10px;padding:9px 11px;cursor:pointer;text-align:left;font-family:inherit;color:var(--cc-text);font-size:.82rem;transition:border-color .15s,background .15s,transform .1s}
.client-tile:hover{border-color:var(--cc-accent);background:#1b2230}
.client-tile:active{transform:translateY(1px)}
.client-tile:focus-visible{outline:2px solid var(--cc-accent);outline-offset:2px}
.client-tile.sel{border-color:var(--cc-accent);background:rgba(88,166,255,.10);box-shadow:inset 0 0 0 1px rgba(88,166,255,.35)}
.client-tile .ct-emblem{width:24px;height:24px;flex:0 0 24px;display:flex;align-items:center;justify-content:center;border-radius:6px;background:var(--cc-bg);overflow:hidden}
.client-tile .ct-emblem svg{width:18px;height:18px;display:block}
.client-tile .ct-name{font-weight:600;line-height:1.15;min-width:0}
.client-tile .ct-name small{display:block;font-weight:400;color:var(--cc-muted);font-size:.7rem}
/* min-width:0 keeps the name ellipsising rather than overflowing the tile. */
.client-tile .ct-body{display:flex;flex-direction:column;align-items:flex-start;gap:3px;min-width:0}
/* "Open in <tool>" onboarding affordance — deliberately understated and clearly a
   SETUP helper, never a "contributing" surface. Only rendered for a client with a
   real, vendor-documented deep-link scheme. */
.openin-row{display:none;align-items:flex-start;gap:10px;background:var(--cc-bg);border:1px solid var(--cc-border);border-left:3px solid #d29922;border-radius:8px;padding:11px 14px;margin:0 0 16px}
.openin-row.show{display:flex}
.openin-row .oi-body{min-width:0;font-size:.8rem;color:var(--cc-muted);line-height:1.4}
.openin-row .oi-body strong{color:var(--cc-text)}
.openin-link{flex:0 0 auto;display:inline-flex;align-items:center;gap:6px;background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-accent);text-decoration:none;font-size:.8rem;padding:6px 12px;font-family:inherit;cursor:pointer}
.openin-link:hover{border-color:var(--cc-accent)}
.openin-link svg{width:14px;height:14px}
.oi-note{color:var(--cc-amber);font-weight:600}
/* Customizable, copy-pasteable per-client PROMPT (kept in an editable block the
   contributor can read/tweak, NOT compressed into a URL). Additive to the shell
   command copy block above it. */
.prompt-block{margin:18px 0 8px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:8px;padding:14px 16px 16px;position:relative}
.prompt-block h4{margin:0 0 6px;font-size:.82rem;color:var(--cc-text);font-weight:600}
.prompt-block p.pb-sub{margin:0 0 10px;color:var(--cc-muted);font-size:.76rem;line-height:1.4}
.prompt-block textarea{width:100%%;min-height:118px;resize:vertical;background:var(--cc-bg-deep);color:var(--cc-text);border:1px solid var(--cc-border);border-radius:6px;padding:10px 12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.8rem;line-height:1.5}
.prompt-block textarea:focus{outline:none;border-color:var(--cc-accent)}
.pb-copy{position:absolute;top:10px;right:12px;background:#238636;color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:.72rem;font-family:inherit}
@media(prefers-reduced-motion:reduce){
  .client-tile{transition:none!important}
}
/* Trusted invite banner (issue #2598) — shown on onboarding when arriving via an
   attributed invite link. Subtle, informational; makes clear the invitee joins
   as a newcomer. */
.invite-banner{margin:0 0 20px;padding:11px 15px;border:1px solid #388bfd55;border-radius:8px;background:#1c2f4a55;color:var(--cc-text-2);font-size:.85rem;line-height:1.45}
.invite-banner b{color:var(--cc-text)}
.invite-banner .invite-tier{color:var(--cc-muted)}
/* Trusted "Invite someone" action inside the Me card (issue #2598). */
.me-invite{margin-top:12px;padding-top:12px;border-top:1px solid #ffffff14}
.me-invite__btn{display:inline-flex;align-items:center;gap:6px;background:#1f6feb;color:#fff;border:none;border-radius:6px;padding:7px 14px;font-size:.82rem;font-family:inherit;cursor:pointer}
.me-invite__btn:hover{background:#388bfd}
.me-invite__row{display:none;margin-top:10px;gap:8px;align-items:center;flex-wrap:wrap}
.me-invite__row.open{display:flex}
.me-invite__link{flex:1 1 220px;min-width:0;background:var(--cc-bg-deep);color:var(--cc-text-2);border:1px solid var(--cc-border);border-radius:6px;padding:7px 10px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.76rem}
.me-invite__copy{background:#238636;color:#fff;border:none;border-radius:6px;padding:7px 12px;font-size:.78rem;font-family:inherit;cursor:pointer}
.me-invite__hint{width:100%%;margin-top:6px;color:var(--cc-muted);font-size:.74rem;line-height:1.4}
/* ── Triage ladder (#2612 part b) — a lifecycle view over contribute issues.
   Themed entirely with the (d) tokens so it flips light/dark with the rest of the
   page. The ladder is a row of level chips (count per rung); below it, per-level
   groups list the issues with an optional PR badge (part c). Sober SRE register:
   muted neutrals, the level accent only on the chip dot + count. */
.cc-triage-ladder{display:flex;flex-wrap:wrap;gap:8px;padding:14px 20px;border-bottom:1px solid var(--cc-border-2)}
.cc-triage-chip{display:inline-flex;align-items:center;gap:7px;padding:5px 12px;border-radius:999px;font-size:.76rem;font-weight:600;color:var(--cc-text-2);background:var(--cc-bg);border:1px solid var(--cc-border)}
.cc-triage-chip .cc-tl-dot{width:8px;height:8px;border-radius:50%%;flex:none;background:var(--cc-muted)}
.cc-triage-chip .cc-tl-n{font-variant-numeric:tabular-nums;color:var(--cc-text);font-weight:700}
.cc-triage-chip .cc-tl-lbl{color:var(--cc-muted);font-weight:600}
/* Per-level accent on the dot only — meaning without shouting. */
.cc-triage-chip.lv-triaging .cc-tl-dot{background:var(--cc-muted)}
.cc-triage-chip.lv-ready .cc-tl-dot{background:var(--cc-accent)}
.cc-triage-chip.lv-implementing .cc-tl-dot{background:var(--cc-accent-fg)}
.cc-triage-chip.lv-reviewing .cc-tl-dot{background:var(--cc-amber)}
.cc-triage-chip.lv-closed .cc-tl-dot{background:var(--cc-green)}
.cc-triage-groups{max-height:520px;overflow-y:auto}
.cc-tg{border-bottom:1px solid var(--cc-border-2)}
.cc-tg:last-child{border-bottom:none}
.cc-tg-head{display:flex;align-items:center;gap:8px;padding:10px 20px;font-size:.74rem;font-weight:700;letter-spacing:.03em;text-transform:uppercase;color:var(--cc-muted);position:sticky;top:0;background:var(--cc-surface);z-index:1}
.cc-tg-head .cc-tg-dot{width:8px;height:8px;border-radius:50%%;flex:none;background:var(--cc-muted)}
.cc-tg.lv-ready .cc-tg-dot{background:var(--cc-accent)}
.cc-tg.lv-implementing .cc-tg-dot{background:var(--cc-accent-fg)}
.cc-tg.lv-reviewing .cc-tg-dot{background:var(--cc-amber)}
.cc-tg.lv-closed .cc-tg-dot{background:var(--cc-green)}
.cc-tg-count{margin-left:auto;color:var(--cc-muted-2);font-weight:600;font-variant-numeric:tabular-nums}
.cc-tg-item{display:flex;align-items:flex-start;gap:10px;padding:10px 20px;border-top:1px solid var(--cc-border-2)}
.cc-tg-body{flex:1;min-width:0}
.cc-tg-repo{font-size:.72rem;color:var(--cc-muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.cc-tg-title{font-size:.86rem;color:var(--cc-text);margin:2px 0 0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cc-tg-empty{padding:8px 20px 12px;font-size:.76rem;color:var(--cc-muted-2)}
/* PR→issue badge (#2612 part c) — a small link chip on a queue/triage row telling
   whether a fixing PR is open or merged. Reuses the status-pill palette so its
   meaning matches the rest of the page (open = review-amber, merged = done-green). */
.cc-pr-badge{flex-shrink:0;display:inline-flex;align-items:center;gap:4px;align-self:center;font-size:.68rem;font-weight:600;padding:2px 8px;border-radius:999px;text-decoration:none;border:1px solid transparent;white-space:nowrap}
.cc-pr-badge.pr-open{background:rgba(210,153,34,.12);color:var(--cc-amber);border-color:rgba(210,153,34,.3)}
.cc-pr-badge.pr-merged{background:rgba(63,185,80,.12);color:var(--cc-green);border-color:rgba(63,185,80,.3)}
.cc-pr-badge:hover{filter:brightness(1.1);text-decoration:underline}
/* Inline PR badge riding on a ready-queue row (smaller, sits after the title). */
.cc-q-body .cc-pr-badge{margin-top:4px}
/* ── (#2612 part d) Light-mode fixups for the handful of DARK one-off surfaces
   that are not part of the tokenized neutral ramp (small gradient washes and a
   couple of hover fills baked as literal dark hex). On a light appearance those
   would read as near-black patches, so re-point them at the light tokens. Also a
   gentle contrast lift on the muted status-pill fills, whose ~12%% dark-tuned
   rgba washes go faint on white — a gentle solid tint keeps them legible without
   changing their hue/meaning. Applies under BOTH light signals. */
@media(prefers-color-scheme:light){:root:not([data-theme="dark"]) .ops-card.card-accent>.ops-card-head{background:linear-gradient(180deg,var(--cc-border-2) 0%%,var(--cc-surface) 100%%)}
:root:not([data-theme="dark"]) .cc-ach{background:var(--cc-surface)}
:root:not([data-theme="dark"]) .client-tile:hover{background:var(--cc-border-2)}
:root:not([data-theme="dark"]) .invite-banner{background:#ddf4ff;border-color:#b6e3ff}
:root:not([data-theme="dark"]) .cc-report-link:hover{border-color:var(--cc-muted)}
:root:not([data-theme="dark"]) .pill-progress{background:rgba(9,105,218,.10);color:var(--cc-accent-fg);border-color:rgba(9,105,218,.30)}
:root:not([data-theme="dark"]) .pill-review,:root:not([data-theme="dark"]) .clanker-status.reviewing{background:rgba(154,103,0,.12);color:var(--cc-amber);border-color:rgba(154,103,0,.30)}
:root:not([data-theme="dark"]) .pill-passed{background:rgba(26,127,55,.12);color:var(--cc-green);border-color:rgba(26,127,55,.30)}
:root:not([data-theme="dark"]) .pill-blocked{background:rgba(207,34,46,.10);color:var(--cc-red);border-color:rgba(207,34,46,.30)}
:root:not([data-theme="dark"]) .cc-live{background:rgba(26,127,55,.08);border-color:rgba(26,127,55,.30);color:#116329}
:root:not([data-theme="dark"]) .cc-live.stale{background:rgba(154,103,0,.08);border-color:rgba(154,103,0,.30);color:#7d4e00}
}
:root[data-theme="light"] .ops-card.card-accent>.ops-card-head{background:linear-gradient(180deg,var(--cc-border-2) 0%%,var(--cc-surface) 100%%)}
:root[data-theme="light"] .cc-ach{background:var(--cc-surface)}
:root[data-theme="light"] .client-tile:hover{background:var(--cc-border-2)}
:root[data-theme="light"] .invite-banner{background:#ddf4ff;border-color:#b6e3ff}
:root[data-theme="light"] .cc-report-link:hover{border-color:var(--cc-muted)}
:root[data-theme="light"] .pill-progress{background:rgba(9,105,218,.10);color:var(--cc-accent-fg);border-color:rgba(9,105,218,.30)}
:root[data-theme="light"] .pill-review,:root[data-theme="light"] .clanker-status.reviewing{background:rgba(154,103,0,.12);color:var(--cc-amber);border-color:rgba(154,103,0,.30)}
:root[data-theme="light"] .pill-passed{background:rgba(26,127,55,.12);color:var(--cc-green);border-color:rgba(26,127,55,.30)}
:root[data-theme="light"] .pill-blocked{background:rgba(207,34,46,.10);color:var(--cc-red);border-color:rgba(207,34,46,.30)}
:root[data-theme="light"] .cc-live{background:rgba(26,127,55,.08);border-color:rgba(26,127,55,.30);color:#116329}
:root[data-theme="light"] .cc-live.stale{background:rgba(154,103,0,.08);border-color:rgba(154,103,0,.30);color:#7d4e00}
/* ── (#2612 part d) Conservative de-crowd: small uniform breathing room on the
   densest grids (the onboarding stat row and the Operations cards). No card
   removed, no structural/layout change — just a touch more gap/padding so the
   packed panels read less cramped, identically in both themes. */
.stat-row{gap:12px}
.stat{padding:16px 10px}
.ops-grid{gap:24px}
.ops-card-head{padding:18px 20px}
.lb-custom-style-note{margin:0 0 16px;padding:10px 12px;border:1px solid rgba(88,166,255,.35);border-radius:8px;background:rgba(88,166,255,.10);color:var(--cc-text);font-size:.86rem}
.lb-custom-style-note code{color:var(--cc-accent)}
.lb-custom-style-note--warn{border-color:rgba(210,153,34,.45);background:rgba(210,153,34,.12)}
.lb-custom-style-note button{margin-left:8px;background:transparent;border:1px solid var(--cc-border);border-radius:6px;color:var(--cc-text);padding:2px 8px;cursor:pointer}
/* ── #4549 Theme control ─────────────────────────────────────────────────────
   The light ramp above has existed since #2612 but was reachable only if the
   visitor's OS already asked for it — there was no in-page selector and no
   stored preference, so a visitor on a dark-set device could not get light and
   a visitor on a light-set device could not pin dark. This is that selector.
   It writes :root[data-theme], the hook the ramp already keys on, so no palette
   work rides along.
   auto is the ABSENCE of the attribute, not a resolved value: a visitor who
   never touches the control keeps exactly today's behaviour, and one who
   returns to auto starts following their OS again live, including a change
   made while the page is open.
   The bar is now .page-chrome (tabs + control); .page-tabs keeps its class,
   role=tablist and its own children unchanged, and hands the surface/border it
   used to paint up to the wrapper so the two sit in one continuous bar. The
   control deliberately does NOT go inside the tablist — a non-tab child of a
   role=tablist is announced as a stray tab. */
.page-chrome{display:flex;align-items:stretch;background:var(--cc-surface);border-bottom:1px solid var(--cc-border)}
.page-chrome>.page-tabs{flex:1 1 auto;min-width:0;background:none;border-bottom:none}
.theme-toggle{flex:0 0 auto;align-self:center;display:inline-flex;align-items:center;gap:6px;margin:0 48px 0 12px;padding:5px 11px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:999px;color:var(--cc-muted);font-family:inherit;font-size:.76rem;line-height:1;cursor:pointer}
.theme-toggle:hover{color:var(--cc-text);border-color:var(--cc-muted)}
.theme-toggle:focus-visible{outline:2px solid var(--cc-accent);outline-offset:2px}
.theme-toggle__glyph{font-size:.92rem;line-height:1}
/* The swap must not animate. The sheet puts transitions on tiles, buttons, the
   ops rail and the quota bar; letting every one of them run at once turns a
   theme change into a half-second wobble across the whole page. ccApplyTheme
   sets this for one frame around the attribute write. Independent of the
   prefers-reduced-motion blocks: this suppresses a transition nobody asked for
   on ANY setting, rather than honouring a stated preference. */
:root.cc-theme-switching *,:root.cc-theme-switching *::before,:root.cc-theme-switching *::after{transition:none!important;animation:none!important}
/* ── #4537 Phone portrait (~390 CSS px). The page had breakpoints at 900/860/768/
   560/520 and then nothing, so an iPhone in portrait fell off the right edge.
   Four compounding causes, fixed here plus two structural ones above
   (.clanker-row track sizing, .clanker-act-as shrinkability):

   1. .page-tabs could neither wrap nor scroll. Five tabs at 14px 20px come to
      roughly 600px, plus 96px of gutters — about 700px against 390px — so the
      bar overflowed the document and "Management" onward were unreachable.
   2. .ops / .main set only overflow-y:auto, which makes the computed
      overflow-x auto as well. That turns the panel into a scroll container
      clipping at its padding box, i.e. flush with the screen edge, which is why
      the cut looked like the display rather than a card border. Naming the
      horizontal axis makes that deliberate instead of inherited.
   3. padding:40px 48px at every width spends a quarter of the screen on
      gutters, leaving ~294px of content column.
   4. .lb-row's seven tracks are ~470px of fixed widths before gaps or padding.

   Kept as one block at the end of the sheet so the phone overrides win on
   source order without inflating specificity. It stays ahead of the %%s custom
   CSS slot below, so an operator's own stylesheet still has the last word. */
@media(max-width:600px){
  /* Wrap rather than scroll: a horizontal scroller with no visible scrollbar
     hides the trailing tabs just as effectively as the overflow did. Wrapping
     puts all five on screen and tappable. */
  .page-tabs{flex-wrap:wrap;padding:0 16px}
  .page-chrome{flex-wrap:wrap}
  .theme-toggle{margin:0 16px 6px auto}
  .page-tab{flex:0 0 auto;padding:12px 12px;font-size:.88rem}

  /* Gutters back to something a phone can spare, and the horizontal axis stated
     outright. The overflow sources are fixed above, so hidden is a backstop
     against a stray wide child, not the thing holding the layout together. */
  .main,.ops{padding:28px 16px;overflow-x:hidden}

  /* The army roster is a nowrap flex row of four labels; let it wrap. */
  .cc-army{flex-wrap:wrap;gap:6px 14px}

  /* Slightly tighter card-internal gutters on the densest rows. */
  .clanker-row,.lb-row,.cc-q-item,.cc-tg-item,.cc-tg-head{padding-left:14px;padding-right:14px}

  /* Leaderboard: 56px/1fr/120px/70px/70px/80px/72px cannot fit, so the seven cells reflow
     onto three lines — rank + contributor, then tier + the three counts, then
     the sparkline — instead of scrolling sideways. Placement is by child
     position because .lb-head and the body rows emit the same seven children in
     the same order, so the header keeps sitting over the column it labels. The
     stat tracks stay fixed rem widths for exactly that reason: auto would size
     per row and the header would drift out of alignment with the numbers. */
  .lb-row{grid-template-columns:2.1rem minmax(0,1fr) 3.2rem 3.2rem 3.4rem;column-gap:6px;row-gap:4px}
  .lb-row>:nth-child(1){grid-column:1;grid-row:1}
  .lb-row>:nth-child(2){grid-column:2/-1;grid-row:1}
  .lb-row>:nth-child(3){grid-column:1/3;grid-row:2}
  .lb-row>:nth-child(4){grid-column:3;grid-row:2}
  .lb-row>:nth-child(5){grid-column:4;grid-row:2}
  .lb-row>:nth-child(6){grid-column:5;grid-row:2}
  .lb-row>:nth-child(7){grid-column:1/-1;grid-row:3;text-align:left}
  /* The uppercase, letter-spaced header labels are the widest thing in the stat
     tracks; drop them a notch so "Findings" fits 3.4rem. */
  .lb-head .lb-stat,.lb-head .lb-rank{font-size:.6rem}
  .lb-spark{justify-content:flex-start}
  .lb-trend{padding-left:14px;padding-right:14px}
}
</style>%s</head><body>
<div class="page-chrome">
<div class="page-tabs" role="tablist">
<button class="page-tab active" role="tab" id="ptab-onboarding" aria-selected="true" data-panel="tab-onboarding">Onboarding</button>
<button class="page-tab" role="tab" id="ptab-ops" aria-selected="false" data-panel="tab-ops">Operations</button>
<button class="page-tab" role="tab" id="ptab-manage" aria-selected="false" data-panel="tab-manage">Management</button>
<button class="page-tab" role="tab" id="ptab-leaderboard" aria-selected="false" data-panel="tab-leaderboard">Leaderboard</button>
<button class="page-tab" role="tab" id="ptab-profile" aria-selected="false" data-panel="tab-profile">Profile</button>
</div>
<button type="button" class="theme-toggle" id="cc-theme-toggle" data-action="cycle-theme" data-theme-mode="auto" title="Theme: Auto &mdash; follows your system appearance" aria-label="Theme: Auto (follows your system appearance). Activate for Light."><span class="theme-toggle__glyph" aria-hidden="true">&#9680;</span><span class="theme-toggle__text">Auto</span></button>
</div>
<div class="tab-panel active" id="tab-onboarding" role="tabpanel" aria-labelledby="ptab-onboarding">
<div class="page">
<div class="main">
<h1>🐝 Contribute to %s</h1>
<div id="invite-banner" class="invite-banner" hidden role="status"></div>
<p class="subtitle">Donate your CLI + API tokens to help this project's AI agent swarm.</p>
<p class="subtitle" style="font-size:.95rem;margin-top:-24px;margin-bottom:32px">Powered by <strong style="color:var(--cc-text)">ClankeR</strong>, the contributor relay &mdash; it hands tasks from this hive's backlog to the agent running on your machine. Your compute, their backlog. Bring your own inference &mdash; how you want to contribute is up to you.</p>
<div class="stat-row">
<div class="stat"><div class="stat-num" style="color:var(--cc-accent)">%d</div><div class="stat-label">Total</div></div>
%s
</div>
<div class="steps">
<h3>How it works</h3>
<!-- #2548 Branded client entry points: a find-by-SIGHT tile grid. Rendered by
     JS from the CLIENTS metadata (inline SVG emblems, all CSP-safe). Clicking a
     tile drives the existing #cli-select below, so the copy-block logic is
     unchanged. Falls back gracefully: the plain selector still works if JS is off. -->
<p style="color:var(--cc-muted);margin:0 0 8px;font-size:.9rem">Find your tool:</p>
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
<label style="font-size:.9rem;color:var(--cc-muted)">OS:</label>
<select id="os-select" style="background:var(--cc-surface);color:var(--cc-text);border:1px solid var(--cc-border);border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="macos" selected>macOS</option>
<option value="linux">Linux</option>
<option value="windows">Windows</option>
</select>
</span>
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:var(--cc-muted)">Choose your CLI:</label>
<select id="cli-select" style="background:var(--cc-surface);color:var(--cc-text);border:1px solid var(--cc-border);border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="claude" data-install="npm i -g @anthropic-ai/claude-code" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="">Claude Code</option>
<option value="codex" data-install="npm i -g @openai/codex" data-host-install="npm i -g @openai/codex\ncodex login --device-auth   # or export CODEX_API_KEY / OPENAI_API_KEY for API-key mode" data-model-flag="--model" data-default-model="" data-env="# Optional: Codex reasoning effort — the relay passes it as -c model_reasoning_effort.\n# export AGENT_REASONING_EFFORT=high">OpenAI Codex</option>
<option value="copilot" data-install="" data-host-install="npm install -g @github/copilot # uses your existing gh auth" data-model-flag="--model" data-default-model="">GitHub Copilot</option>
<option value="pi" data-install="" data-host-install="curl -fsSL https://pi.dev/install.sh | sh" data-model-flag="--model" data-default-model="">Pi</option>
<option value="goose" data-install="" data-host-install="# Install Goose: https://github.com/block/goose/releases\n# Install Ollama: https://ollama.com/download\nollama pull llama3.2:3b\nexport GOOSE_PROVIDER=ollama GOOSE_MODEL=llama3.2:3b" data-model-flag="" data-default-model="">Goose</option>
<option value="litellm" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# Your own LiteLLM proxy — exported locally, never sent to the hive\nexport HIVE_LITELLM_ENDPOINT=https://your-litellm-host:4000\nexport HIVE_LITELLM_API_KEY=sk-your-litellm-key  # only if your proxy needs one">LiteLLM (Claude Code + your proxy)</option>
<option value="openrouter" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# OpenRouter — Claude Code routed through OpenRouter\nexport HIVE_LITELLM_ENDPOINT=https://openrouter.ai/api/v1\nexport HIVE_LITELLM_API_KEY=sk-or-...  # your OpenRouter key">OpenRouter (Claude Code + your key)</option>
<option value="vllm" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# vLLM — self-hosted OpenAI-compatible server\nexport HIVE_LITELLM_ENDPOINT=http://your-vllm-host:8000/v1\nexport HIVE_LITELLM_API_KEY=sk-your-vllm-key  # only if your server needs one">vLLM (self-hosted)</option>
<option value="llm-d" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# llm-d — self-hosted OpenAI-compatible endpoint\nexport HIVE_LITELLM_ENDPOINT=http://your-llm-d-host:8000/v1\nexport HIVE_LITELLM_API_KEY=sk-your-llm-d-key  # only if your endpoint needs one">llm-d (self-hosted)</option>
<option value="bob" data-install="" data-host-install="curl -fsSL https://bob.ibm.com/download/bobshell.sh | bash" data-model-flag="" data-default-model="" data-env="# Bob (IBM bobshell) — get a key at https://bob.ibm.com (Scope: Inference).\n# Exported locally, never sent to the hive.\nexport BOBSHELL_API_KEY=your-bob-api-key">Bob</option>
<option value="watsonx" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# IBM watsonx.ai — OpenAI-compatible gateway, bring your own project + key.\n# watsonx auth is an IAM-minted JWT, not a raw bearer key — your local\n# Claude-Code setup or a small local proxy handles the token exchange.\n# Exported locally, never sent to the hive.\nexport HIVE_LITELLM_ENDPOINT=https://us-south.ml.cloud.ibm.com/ml/gateway/v1\nexport HIVE_LITELLM_API_KEY=your-ibm-cloud-api-key\nexport WATSONX_PROJECT_ID=your-watsonx-project-id">watsonx.ai (IBM Granite + your key)</option>
<option value="agy" data-install="" data-host-install="# Antigravity CLI (Google): https://antigravity.google/product/antigravity-cli\nbrew install --cask antigravity-cli\nagy   # sign in once with your Google account, interactively, then exit" data-model-flag="--model" data-default-model="" data-env="# Optional: agy effort — REQUIRED alongside a model, or agy ignores the model.\n# export AGENT_REASONING_EFFORT=low   # low|medium|high">Antigravity (agy)</option>
<option value="other" data-install="" data-host-install="# Install your CLI tool" data-model-flag="" data-default-model="">Other (host only)</option>
</select>
</span>
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:var(--cc-muted)">Mode:</label>
<select id="mode-select" style="background:var(--cc-surface);color:var(--cc-text);border:1px solid var(--cc-border);border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="containerized">Containerized (recommended)</option>
<option value="host">Host (non-containerized)</option>
<option value="kubernetes">Kubernetes (cluster)</option>
</select>
</span>
<span id="runtime-group" style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:var(--cc-muted)">Runtime:</label>
<select id="runtime-select" style="background:var(--cc-surface);color:var(--cc-text);border:1px solid var(--cc-border);border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="">Auto-detect</option>
<option value="docker">Docker</option>
<option value="podman">Podman</option>
</select>
</span>
</div>
<div id="model-row" style="margin-bottom:12px;display:none;align-items:center;gap:8px">
<label style="font-size:.9rem;color:var(--cc-muted)">Model (optional):</label>
<input id="model-input" type="text" placeholder="e.g. claude-sonnet-4-6, gpt-4o" style="background:var(--cc-surface);color:var(--cc-text);border:1px solid var(--cc-border);border-radius:6px;padding:6px 12px;font-size:.85rem;flex:1;max-width:300px" data-input-action="updateCmds">
</div>
<!-- #2549 Kubernetes-mode note. Hidden except in Kubernetes mode. States the two
     honest constraints up front: only headless-capable backends run in a cluster
     (a headless pod has no TTY), and the credential stored in the cluster Secret
     is a long-lived personal token that is more exposed than a laptop file, with
     the per-task credential boundary tracked in #2537. -->
<div id="k8s-note" style="display:none;margin-bottom:12px;background:var(--cc-surface);border:1px solid var(--cc-border);border-left:3px solid #d29922;border-radius:6px;padding:12px 14px;font-size:.85rem;color:var(--cc-text-2);line-height:1.5">
<strong style="color:var(--cc-text)">Kubernetes is the advanced path.</strong> It needs a cluster, a kubeconfig and RBAC &mdash; not a first-timer&rsquo;s happy path. The workload runs the relay <strong>headless</strong> (no TTY), so only headless-capable backends work in a cluster: <strong>Claude Code, LiteLLM, Copilot, Codex</strong>. Other backends will refuse work at pod startup.<br>
<span style="color:var(--cc-muted)">Credential note (interim): the generated Secret stores a long-lived personal <code>GH_TOKEN</code> &mdash; base64, not encrypted, and readable by anyone with <code>get secrets</code> in that namespace or by cluster-scoped operators/backups. That is materially more exposed than a <code>0600</code> file on your laptop. Revoke any time with <code>gh auth logout</code>. Gating the credential on explicit task acceptance is tracked in <a href="https://github.com/kubestellar/hive/issues/2537" target="_blank" rel="noopener" style="color:var(--cc-accent)">#2537</a> and is not solved by this path.</span>
</div>
<!-- Host-only note. Shown for backends the containerized/Kubernetes paths cannot
     run, so the mode flip to Host is explained rather than mysterious: "other"
     has no image, and agy's Google sign-in is interactive with no API-key mode,
     so no unattended container can hold a session. -->
<div id="hostonly-note" style="display:none;margin-bottom:12px;background:var(--cc-surface);border:1px solid var(--cc-border);border-left:3px solid #d29922;border-radius:6px;padding:12px 14px;font-size:.85rem;color:var(--cc-text-2);line-height:1.5">
<strong style="color:var(--cc-text)">This backend runs on your host, not in a container.</strong> The mode selector has been switched to <strong>Host</strong> for you. <strong>Antigravity (agy)</strong> signs in through an interactive Google OAuth flow &mdash; a browser URL plus a pasted code, with no API-key mode &mdash; so a container or pod has no session to inherit, and the contributor image does not ship the binary. Run <code>agy</code> once to sign in, then start the relay on the host. Its print mode (<code>agy -p</code>) is verified, so headless host runs work; Kubernetes does not.
</div>
<div id="multi-hub-note" style="margin-bottom:12px;background:var(--cc-surface);border:1px solid var(--cc-border);border-left:3px solid #58a6ff;border-radius:6px;padding:12px 14px;font-size:.85rem;color:var(--cc-text-2);line-height:1.5">
<strong style="color:var(--cc-text)">Contribute to multiple hives:</strong> after registering with each hive, set <code>HIVE_HUB</code> to comma-separated WebSocket URLs and <code>HIVE_REGISTRATION_TOKEN</code> to the matching comma-separated tokens in the same order. One relay shares one CLI/tmux session, works on one task at a time, keeps each hub connected with its own heartbeat, and rotates only when the active hub says no task is available. Added by <a href="https://github.com/hanthor" target="_blank" rel="noopener" style="color:var(--cc-accent)">@hanthor</a> in <a href="https://github.com/kubestellar/hive/pull/2846" target="_blank" rel="noopener" style="color:var(--cc-accent)">#2846</a>.
</div>
<p style="color:var(--cc-muted);margin-bottom:8px">Copy and paste these commands to get started:</p>
<div style="margin-top:16px;background:var(--cc-bg);border:1px solid var(--cc-border);border-radius:8px;padding:16px;position:relative">
<button id="copy-btn" style="position:absolute;top:8px;right:8px;background:#238636;color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:.75rem">Copy</button>
<pre id="copy-cmds" style="color:var(--cc-text);font-size:.85rem;margin:0;overflow-x:auto;white-space:pre"># Default shown: macOS + Claude Code + containerized mode.
# Use the OS / CLI / Mode / Runtime selectors above to customize.
brew install just gh
git clone -b {{HIVE_BRANCH}} https://github.com/kubestellar/hive && cd hive
export HIVE_HUB=%s
# Optional: export HIVE_AGENT_ROLE=scanner (or a granted privileged role such as ci-maintainer) to claim a spoke-agent lane.
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
// SECURITY (#3315): both values are JSON-encoded at the sink (they arrive as
// complete JS string literals, quotes included) rather than being dropped raw
// into '...' quotes. hubURL derives from X-Forwarded-Host/r.Host and
// ccProjectName from operator config; both were previously kept safe only by
// sanitizers ~1200 lines away (a charset filter and html.EscapeString), so a
// change to either of those would silently have opened a script-literal
// break-out here. HTML escaping is NOT sufficient in a script data context.
var hubURL=%s;
var ccProjectName=%s;
// Shared with the second script block's IIFE (the dossier renderer lives there
// and needs the project name for its masthead). Without this the bare reference
// in renderMeCard resolves to nothing and every dossier render throws.
window.ccProjectName=ccProjectName;
// Prerequisite line (just + gh) per OS, using each project's own documented
// install method. macOS stays brew install just gh — the historical default.
var prereqByOS={
macos:'brew install just gh',
linux:'curl --proto \'=https\' --tlsv1.2 -sSf https://just.systems/install.sh | bash -s -- --to ~/.local/bin\n(type -p wget >/dev/null || (sudo apt update && sudo apt install wget -y)) && sudo mkdir -p -m 755 /etc/apt/keyrings && out=$(mktemp) && wget -nv -O$out https://cli.github.com/packages/githubcli-archive-keyring.gpg && cat $out | sudo tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null && sudo chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg && sudo mkdir -p -m 755 /etc/apt/sources.list.d && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null && sudo apt update && sudo apt install gh -y',
windows:'winget install --id Casey.Just --exact\nwinget install --id GitHub.cli'
};
var roleHelp='# Optional: export HIVE_AGENT_ROLE=scanner (or a granted privileged role such as ci-maintainer) to claim a spoke-agent lane.\n';
var containerTpl='PREREQ\ngit clone -b {{HIVE_BRANCH}} https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\nROLEHELPjust contribute-setup CLI\njust contribute-hive';
var hostTpl='PREREQ\nINSTALL\ngit clone -b {{HIVE_BRANCH}} https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\nROLEHELPjust contribute-setup CLI\njust contribute-hive CLI local';
// Kubernetes (#2549): register locally, then generate + apply a headless
// contributor workload (Deployment, #2660) into a cluster you already have.
// just contribute-k8s prints the manifest; it never touches your cluster on
// its own, so you can read it before piping to kubectl. Only the headless-
// capable backends run this way (see K8S_HEADLESS_BACKENDS).
var k8sTpl='PREREQ\ngit clone -b {{HIVE_BRANCH}} https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\nROLEHELPjust contribute-setup CLI\n# Review the manifest, then apply into your current kube-context:\njust contribute-k8s hive-contributor | kubectl apply -f -\nkubectl -n hive-contributor rollout status deploy/hive-contributor';
// Backends with a verified headless (non-interactive) entry point — must match
// HEADLESS_BACKENDS in bin/contributor-relay.sh and the Justfile. A pod has no
// TTY, so only these run in a cluster; anything else refuses work at startup.
var K8S_HEADLESS_BACKENDS={claude:1,litellm:1,copilot:1,codex:1,watsonx:1,goose:1};
// Backends that can only run on the contributor's own host. "other" has no
// image by definition. agy is host-only for a different reason: it signs in
// through an interactive Google OAuth flow (browser URL + pasted code) with no
// API-key mode, so neither a container nor a pod can inherit a contributor's
// session — verified against agy 1.1.13, where a clean container demands a
// fresh browser login no matter what is mounted, and the contributor image does
// not ship the binary either. Selecting one flips Mode to Host rather than
// generating commands that cannot work. NOTE agy IS headless-capable on a host
// (agy -p, see HEADLESS_BACKENDS in bin/contributor-relay.sh); it is the
// credential, not the capability, that keeps it out of K8S_HEADLESS_BACKENDS.
var HOST_ONLY_BACKENDS=['other','agy'];
function isHostOnly(c){return HOST_ONLY_BACKENDS.indexOf(c)>=0;}
var modelRow=document.getElementById('model-row');
var modelInput=document.getElementById('model-input');
function updateCmds(){update();}
// Event delegation (#3848): replaces the former inline oninput= attribute so
// CSP script-src-attr can be 'none'.
if(modelInput){modelInput.addEventListener('input',updateCmds);}
function update(){
var os=osSel.value;
var prereq=prereqByOS[os]||prereqByOS.macos;
var cli=sel.value;
var opt=sel.options[sel.selectedIndex];
var mode=modeSel.value;
var modelFlag=opt.getAttribute('data-model-flag')||'';
var model=(modelInput.value||'').trim();
if(isHostOnly(cli)&&mode!=='host'){modeSel.value='host';mode='host';}
var k8sNote=document.getElementById('k8s-note');
if(k8sNote)k8sNote.style.display=(mode==='kubernetes')?'block':'none';
var hostOnlyNote=document.getElementById('hostonly-note');
if(hostOnlyNote)hostOnlyNote.style.display=isHostOnly(cli)?'block':'none';
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
if(mode==='kubernetes'){
// A cluster pod exports its config via envFrom, so no HIVE_CONTAINER_RUNTIME
// line — but model/env exports still belong before contribute-setup so the
// generated ConfigMap picks them up. If the chosen backend has no headless
// mode, prepend a visible warning comment (the Justfile also warns on stderr).
var warn=K8S_HEADLESS_BACKENDS[cli]?'':'# WARNING: '+cli+' has no headless mode; it will refuse work in a cluster.\n# Pick Claude Code, LiteLLM, Copilot, Codex or Goose for Kubernetes.\n';
var k8sPre=envLines+modelLine;
cmds.textContent=warn+k8sTpl.replace('PREREQ',prereq).replace('ROLEHELP',roleHelp).replace(/CLI/g,cli).replace('just contribute-setup',k8sPre+'just contribute-setup');
}else if(mode==='host'){
tpl=hostTpl;
install=opt.getAttribute('data-host-install');
if(!install)install='# '+cli+' uses your existing gh auth';
cmds.textContent=tpl.replace('PREREQ',prereq).replace('INSTALL',install.replace(/\\n/g,'\n')).replace('ROLEHELP',roleHelp).replace(/CLI/g,cli).replace('just contribute-setup',preLines+'just contribute-setup');
}else{
cmds.textContent=containerTpl.replace('PREREQ',prereq).replace('ROLEHELP',roleHelp).replace(/CLI/g,cli).replace('just contribute-setup',preLines+'just contribute-setup');
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
// Claude / Codex / Copilot / Pi / Goose, a documented-only "Open in" deep-link, and a
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
codex:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4l6.93 4v8L12 20l-6.93-4V8z" fill="none" stroke="#10a37f" stroke-width="1.5" stroke-linejoin="round"/><path d="M10 10.5l2 1.5-2 1.5M13.5 15h3" stroke="#10a37f" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>',
copilot:'<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12.5" r="8" fill="none" stroke="#e6edf3" stroke-width="1.6"/><circle cx="9" cy="12" r="1.3" fill="#e6edf3"/><circle cx="15" cy="12" r="1.3" fill="#e6edf3"/><path d="M12 4.5V2M8 5l-1-2M16 5l1-2" stroke="#e6edf3" stroke-width="1.4" stroke-linecap="round"/></svg>',
pi:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 8h16" stroke="#7c93ff" stroke-width="2" stroke-linecap="round"/><path d="M9 8v10M15.5 8v7.5a2 2 0 0 0 2 2" stroke="#7c93ff" stroke-width="2" stroke-linecap="round"/></svg>',
goose:'<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="#3fb0ac" d="M6 14a6 6 0 0 1 6-6c1 0 1.6.9 1 1.7 2.5.4 4 2.4 4 5.1 0 .6-.5 1.2-1.2 1.2H8.5A2.5 2.5 0 0 1 6 13.5V14Z"/><circle cx="10.5" cy="10.8" r=".8" fill="#0d1117"/><path d="M13 9.7l2.4-1" stroke="#f0b429" stroke-width="1.4" stroke-linecap="round"/></svg>',
litellm:'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="6" width="16" height="12" rx="2" fill="none" stroke="#58a6ff" stroke-width="1.5"/><path d="M8 10l2 2-2 2M12.5 14h3.5" stroke="#58a6ff" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
openrouter:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 12h4l3-4 3 8 3-4h3" fill="none" stroke="#8b5cf6" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
vllm:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5l4 14 3-9 3 9 4-14" fill="none" stroke="#f0b429" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
'llm-d':'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="5" width="16" height="14" rx="2" fill="none" stroke="#4d9375" stroke-width="1.5"/><path d="M8 9h4a3 3 0 0 1 0 6H8V9Z" fill="none" stroke="#4d9375" stroke-width="1.5" stroke-linejoin="round"/></svg>',
bob:'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="5" y="4" width="14" height="16" rx="2" fill="none" stroke="#1f70c1" stroke-width="1.5"/><path d="M9 8h3.5a2 2 0 0 1 0 4H9V8ZM9 12h4a2 2 0 0 1 0 4H9v-4Z" fill="none" stroke="#1f70c1" stroke-width="1.3" stroke-linejoin="round"/></svg>',
watsonx:'<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8" fill="none" stroke="#1f70c1" stroke-width="1.5"/><path d="M12 7v10M8.5 9.5l7 5M15.5 9.5l-7 5" stroke="#1f70c1" stroke-width="1.4" stroke-linecap="round"/></svg>',
agy:'<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4.5 19 19H5z" fill="none" stroke="#a78bfa" stroke-width="1.6" stroke-linejoin="round"/><path d="M12 20.5v-5" stroke="#a78bfa" stroke-width="1.5" stroke-linecap="round"/><circle cx="12" cy="10.5" r="1.4" fill="#a78bfa"/></svg>',
other:'<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="6" width="16" height="12" rx="2" fill="none" stroke="#8b949e" stroke-width="1.5"/><path d="M8 12h.01M12 12h.01M16 12h.01" stroke="#8b949e" stroke-width="2.2" stroke-linecap="round"/></svg>'
};
var CLIENTS={
claude:{name:'Claude Code',tag:'Anthropic',peer:true,
  deeplink:{label:'Open in Claude',
    // claude:// desktop deep link (documented: support.claude.com "Open Claude
    // Desktop with a link"). q= prefills the prompt for the user to review/send.
    href:function(p){return 'claude://claude.ai/new?q='+encodeURIComponent(p);},
    desc:'Opens Claude Desktop with a setup prompt prefilled.'}},
codex:{name:'Codex',tag:'OpenAI',peer:true},
copilot:{name:'GitHub Copilot',tag:'GitHub',peer:true},
pi:{name:'Pi',tag:'pi.dev',peer:true},
goose:{name:'Goose',tag:'Block (Ollama)',peer:true},
litellm:{name:'LiteLLM',tag:'your proxy',peer:true},
openrouter:{name:'OpenRouter',tag:'your key',peer:true},
vllm:{name:'vLLM',tag:'self-hosted'},
'llm-d':{name:'llm-d',tag:'self-hosted'},
bob:{name:'Bob',tag:'IBM'},
watsonx:{name:'watsonx.ai',tag:'IBM'},
agy:{name:'Antigravity',tag:'Google (host)'},
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
  // Peers (Claude/Codex/Copilot/Pi/Goose/LiteLLM/OpenRouter) first, in select order, then
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
    '  2. git clone -b {{HIVE_BRANCH}} https://github.com/kubestellar/hive && cd hive\n'+
    '  3. export HIVE_HUB='+hubURL+'\n'+
    '     For multiple hives, HIVE_HUB can be comma-separated when HIVE_REGISTRATION_TOKEN has matching tokens in the same order.\n'+
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
<p style="color:var(--cc-muted-2);font-size:.78rem;margin-top:8px">Containerized mode auto-detects docker, then podman &mdash; when both are present, Docker wins. Docker's daemon runs rootful (docker-group membership is effectively root on the host); Podman here runs rootless (user namespace via <code>--userns=keep-id</code>, SELinux labels). Force either explicitly with <code>export HIVE_CONTAINER_RUNTIME=podman</code> (or <code>docker</code>). Rootless Podman handling is best-effort today, not yet covered by CI &mdash; see <a href="https://github.com/kubestellar/hive/blob/HEAD/src/docs/podman-rootless-ci.md" target="_blank" style="color:var(--cc-accent)">docs/podman-rootless-ci.md</a>.</p>
<p style="color:var(--cc-muted-2);font-size:.78rem;margin-top:8px">Don't see your CLI? <a href="https://github.com/kubestellar/hive/issues/new?title=CLI+request:+&labels=enhancement" target="_blank" style="color:var(--cc-accent)">Open an issue</a> and we'll add support for it.</p>
<div style="margin-top:20px;display:flex;gap:12px;flex-wrap:wrap">
<button type="button" id="goto-leaderboard-tab" style="display:inline-block;padding:8px 20px;background:var(--cc-surface);border:1px solid var(--cc-border);border-radius:8px;color:var(--cc-accent);text-decoration:none;font-size:.9rem;font-family:inherit;cursor:pointer">🏆 View Leaderboard</button>
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
<p class="subtitle" style="font-size:.95rem">Operator admin controls for the contributor (&ldquo;clanker&rdquo;) fleet, mirrored from the Governor Hub configuration. Owner &amp; read-write only &mdash; a read viewer sees no controls here. Live monitoring of the fleet lives under the <strong style="color:var(--cc-text)">Operations</strong> tab.</p>

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
<div class="admin-toggle">
<div class="admin-switch" id="admin-cooldown-switch" data-key="contribute_cooldown_enabled"></div>
<div><div class="admin-toggle-label">Task cooldown</div><div class="admin-toggle-sub">After a task completes with a verified PR, keep that issue out of the queue for the period below. Off = no cooldown gating. Failure quarantine is separate and always on.</div></div>
</div>
<div class="admin-field" id="admin-cooldown-hours-wrap" style="margin-left:50px">
<label>Cooldown period (hours) <span style="color:var(--cc-muted-2)">— 168 = one week (default). Range 1&ndash;8760.</span></label>
<input type="number" id="admin-cooldown-hours" min="1" max="8760" style="max-width:120px">
<!-- Live tally of issues currently within their cooldown window (#2649 companion),
     hydrated by ccRenderCooldownCount from the fleet payload. Hidden when 0. -->
<div id="admin-cooldown-count" class="admin-toggle-sub" style="margin-top:4px;display:none"></div>
</div>

<hr class="admin-hr">
<h3 style="font-size:.9rem;color:var(--cc-text);margin:0 0 4px">Admission filters</h3>
<p class="ops-note" style="margin-top:0">The queue-shaping levers. Deny (default) skips matches; Allow serves only matches.</p>

<div class="admin-field" id="admin-filter-titles"></div>
<div class="admin-field" id="admin-filter-authors"></div>
<div class="admin-field" id="admin-filter-labels"></div>

<div class="admin-field">
<label>Allowed models <span style="color:var(--cc-muted-2)">— wildcards (*) and /regex/. Empty = allow all.</span></label>
<div class="admin-chips" id="admin-allow-models"></div>
<div class="admin-addrow"><input type="text" id="admin-allow-model-input" placeholder="e.g. claude-opus*, /gemini-\d/"><button type="button" id="admin-add-model">Add</button></div>
<div class="admin-toggle" style="padding-top:8px"><div class="admin-switch" id="admin-reject-switch" data-key="contribute_reject_unknown_models"></div><div class="admin-toggle-sub">Reject unknown models at connect time (only when the allowlist is non-empty).</div></div>
</div>

<hr class="admin-hr">
<h3 style="font-size:.9rem;color:var(--cc-text);margin:0 0 4px">Repos for Contribute</h3>
<p class="ops-note" style="margin-top:0">Which repos feed the contribute queue. A repo is enabled unless toggled off. Mirrors the Governor Hub repo list; persists as <code>disabled_repos</code>.</p>
<div id="admin-repos"></div>

<hr class="admin-hr">
<h3 style="font-size:.9rem;color:var(--cc-text);margin:0 0 4px">Tier access &amp; rate limits</h3>
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
<p class="subtitle" style="font-size:.95rem">A live view over the contributor (&ldquo;clanker&rdquo;) fleet and its in-flight work. The panels below surface what this hive already knows; the per-clanker trust / revoke / remove controls are owner &amp; read-write only. Admin controls (suspend, admission filters) live under the <strong style="color:var(--cc-text)">Management</strong> tab.</p>

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
<div class="ops-card-head"><span class="feed-dot"></span><h3>Connected clankers</h3><span class="ops-card-count count-strong" id="clanker-count"></span><!-- 7-day fleet-size trend (#persistent-history) --><span class="spark spark-inline" id="spark-fleet" title="Connected clankers, last 7 days (hourly)"></span></div>
<!-- Army roster header: live count + at-a-glance status split, fed by the fleet snapshot. -->
<div class="cc-army" id="cc-army">
  <span style="color:var(--cc-text);font-weight:600">Your army</span>
  <span class="cc-army-stat working"><span class="dot"></span><b id="cc-army-working">0</b>&nbsp;working</span>
  <span class="cc-army-stat reviewing"><span class="dot"></span><b id="cc-army-reviewing">0</b>&nbsp;reviewing</span>
  <span class="cc-army-stat idle"><span class="dot"></span><b id="cc-army-idle">0</b>&nbsp;idle</span>
</div>
<div id="clanker-list"><div class="ops-empty">Loading fleet&hellip;</div></div>
</div>
<div class="ops-card" style="margin-top:20px">
<div class="ops-card-head"><h3>Pipeline &amp; policy</h3><!-- Tasks-completed/hour throughput trend (#persistent-history) --><span class="spark spark-inline" id="spark-throughput" title="Tasks completed per hour, last 7 days"></span></div>
<div style="padding:16px 20px">
<div class="pipeline">
<span class="pipe-node">opened</span><span class="pipe-arrow">&rarr;</span>
<span class="pipe-node">review <span class="lgtm">[lgtm]</span></span><span class="pipe-arrow">&rarr;</span>
<span class="pipe-node">approved</span><span class="pipe-arrow">&rarr;</span>
<span class="pipe-node">merged</span>
</div>
<div id="policy-body"><div class="ops-empty">Loading policy&hellip;</div></div>
<!-- Owner-facing Label interests roster (#2637). The contributor editor (#cc-interests)
     and the per-clanker "prefers:" mirror only surface interests to the contributor
     themselves / on a connected row; this gives the OWNER an actionable aggregate:
     which labels the connected contributors subscribe to, and who — so the owner can
     label matching issues to route work ("I have nvidia contributors, so I'll label
     nvidia issues"). Always shows the explainer (even with an empty roster) so the
     owner knows the feature exists; the aggregate is hydrated each poll by
     ccRenderInterestRoster() from the fleet payload's per-clanker label_interests. -->
<div class="label-affinity" id="label-affinity">
<div class="label-affinity-head">
<span class="label-affinity-title">Contributor label interests</span>
<span class="info-affordance">
<button type="button" class="info-btn" id="affinity-info-btn" aria-haspopup="true" aria-expanded="false" aria-controls="affinity-info-pop" aria-label="What are label interests?" title="What are label interests?">&#9432;</button>
<div class="info-pop" id="affinity-info-pop" role="tooltip" hidden>
<h4>What are label interests?</h4>
Contributors subscribe to labels (e.g. <code>nvidia</code>) so matching issues are highlighted and routed to them &mdash; a soft hint, nothing is hidden. Label an issue to steer it toward the contributors who asked for that kind of work.
</div>
</span>
</div>
<div class="label-affinity-body" id="label-affinity-body"><div class="ops-empty">Loading interests&hellip;</div></div>
</div>
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
<div class="ops-card-head"><span class="feed-dot"></span><h3>Ready-work queue</h3><span class="ops-card-count" id="queue-count"></span><!-- Resume-all (#queue-hold): bulk-clears the operator hold set. Hidden by default;
     ccRenderResumeAll() reveals it only for an owner/read-write viewer when at least
     one issue is on hold. Themed confirm (adminConfirm), never native confirm. --><button type="button" class="queue-resume-all-btn" id="queue-resume-all-btn" style="display:none" title="Resume every held issue">&#x25B6; Resume all</button><!-- Cooldown explainer (#2649 companion): a circled-i affordance whose popover
     explains what "in cooldown" in the count means and how an issue lands there.
     Numbers here are the REAL server constants (168h with-PR, ~4h no-PR, ~6h
     quarantine after 3 consecutive failures) — keep them in sync with
     completedTaskCooldownHours / completedNoPRCooldownHours / quarantineCooldownHours
     / consecutiveFailureQuarantineThreshold in contribute_ws.go. -->
<span class="info-affordance">
<button type="button" class="info-btn" id="cooldown-info-btn" aria-haspopup="true" aria-expanded="false" aria-controls="cooldown-info-pop" aria-label="What is cooldown?" title="What is cooldown?">&#9432;</button>
<div class="info-pop" id="cooldown-info-pop" role="tooltip" hidden>
<h4>What is cooldown?</h4>
Cooldown briefly holds a just-worked issue out of the ready queue so it isn&rsquo;t instantly re-offered while a PR settles. An issue enters cooldown when it is:
<ul>
<li>completed <b>with a verified PR</b> &mdash; held for the full cooldown period (default <code>168h</code> / 7 days, configurable in Management &rarr; Task cooldown).</li>
<li>completed with <b>no PR</b> &mdash; held ~<code>4h</code>.</li>
<li><b>failed</b> &mdash; a short cooldown; after <code>3</code> consecutive failures the issue is quarantined for ~<code>6h</code>.</li>
</ul>
It clears automatically when the period elapses. An operator can shorten or disable the with-PR period in the Management tab.
</div>
</span><!-- 7-day queue-depth trend (#persistent-history), hydrated by ccMetricsPoll --><span class="spark spark-inline" id="spark-queue" title="Ready-work queue depth, last 7 days (hourly)"></span>
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
<!-- My label interests (#2637): a signed-in contributor's OPT-IN set of labels they
     can help with. Matching issues are highlighted and floated to the top of THIS
     viewer's queue. Soft signal — nothing is filtered out, so leaving it empty just
     shows the shared queue. Hidden until the viewer is known to have a contributor
     profile (populated by ccApplyInterests from the /api/contribute/queue response). -->
<div class="cc-interests" id="cc-interests" style="display:none">
  <div class="cc-interests-head">
    <span class="cc-interests-title">My label interests</span>
    <span class="cc-interests-hint">Issues with these labels are highlighted and shown first for you. Soft signal &mdash; nothing else is hidden.</span>
  </div>
  <div class="cc-interests-chips" id="cc-interests-chips"></div>
  <div class="cc-interests-add">
    <input type="text" id="cc-interests-input" placeholder="e.g. nvidia" aria-label="Add a label interest" autocomplete="off" spellcheck="false">
    <button type="button" id="cc-interests-add-btn">Add</button>
  </div>
</div>
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
<!-- ── Triage ladder (#2612 part b) — a Warp-style lifecycle view over the hive's
     contribute issues, grouped Triaging → Ready → Implementing → Reviewing →
     Closed. Each level is DERIVED LIVE from the ready queue + fleet snapshot +
     the PR→issue link (part c); there is no persistent per-issue lifecycle store
     (a future enhancement, out of scope). A SECTION within Operations — NOT a new
     page/tab. Fetched from /api/contribute/triage after load so a slow GitHub
     PR-link lookup never delays the page. Full-width card below the ops grid. -->
<div class="ops-card cc-triage-card" id="cc-triage-card" style="margin-top:20px">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Issue triage</h3><span class="ops-card-count count-strong" id="cc-triage-total"></span></div>
<!-- Compact ladder summary: one chip per level with its live count. -->
<div class="cc-triage-ladder" id="cc-triage-ladder"><div class="ops-empty">Loading triage&hellip;</div></div>
<!-- Grouped per-level issue lists (each collapsible-ish section, capped). -->
<div class="cc-triage-groups" id="cc-triage-groups"></div>
<p class="ops-note" style="padding:10px 20px 14px;margin:0">A live lifecycle view of this hive&rsquo;s contribute issues &mdash; each issue is placed on the ladder from what the hive can observe right now (the ready queue, the fleet&rsquo;s in-flight work, and whether a fixing PR is open or merged). Read-only and recomputed on each load; there is no stored per-issue state.</p>
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
%s
<!-- Standing strip. The full dossier now lives on its OWN tab (/contribute/profile)
     so the standings are the primary content here again; all that remains is a
     one-line "where you stand" cue that links across. Anonymous viewers get the
     same one-line footprint, not a card. -->
<div id="me-standing-mount"></div>
<div class="ops-card card-accent">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Rankings</h3><span class="ops-card-count count-strong" id="leaderboard-count"></span></div>
<div id="leaderboard-list"><div class="ops-empty">Loading leaderboard&hellip;</div></div>
</div>
</div>
</div>
<div class="tab-panel" id="tab-profile" role="tabpanel" aria-labelledby="ptab-profile">
<div class="ops">
<!-- The contributor dossier. Hydrated from the HUB profile endpoint
     (/api/leaderboard/contributor/{username}) once the logged-in username is
     known (from /api/gh-user-auth/status). Anonymous / unknown viewers get a
     subtle sign-in prompt rather than an error. The SAME renderer serves the
     public route /contribute/dossier/{username}; owner-only controls are gated
     server-side there and by ME_IS_OWNER here. -->
<div id="me-card-mount"></div>
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
// ── Event delegation (#3848) ─────────────────────────────────────────────────
// Replaces former inline on*= handler attributes so CSP script-src-attr can be
// 'none'. Behaviors are keyed off data-* attributes:
//   data-action="dismiss-parent"  → remove the button's parent element
//   data-action="cycle-theme"     → advance the theme selector (#4549)
//   data-hide-on-error="1"        → hide <img> whose load failed
//   data-select-on-click="1"      → select input contents on click
//   data-stop-prop="1"            → stop click/mousedown propagating to parents

// ── Theme selector (#4549) ───────────────────────────────────────────────────
// Three states cycled in this order. "auto" means the attribute is ABSENT, not
// resolved to a value: the sheet already follows prefers-color-scheme when no
// data-theme is set, so removing it hands control back to the OS live. Resolving
// auto to a concrete value here would freeze the page against an OS change made
// while it is open, which is the one thing the default must not do.
// The <head> guard already applied the stored value before first paint; this
// block owns the label, the persistence and the cycle, none of which the first
// paint needs. The storage key is deliberately NOT the one static/index.html's
// own toggle uses: that surface keys off a body class and this one off
// :root[data-theme], so a shared key would have each surface writing a value the
// other cannot read. TestContributeThemeKeyDoesNotCollideWithDashboard pins that.
var CC_THEME_KEY='hive.contribute.theme';
var CC_THEME_ORDER=['auto','light','dark'];
var CC_THEME_UI={
  auto:{glyph:'\u25D0',text:'Auto',hint:'follows your system appearance'},
  light:{glyph:'\u2600',text:'Light',hint:'forced light'},
  dark:{glyph:'\u263E',text:'Dark',hint:'forced dark'}
};
// Any value that is not a pinned theme reads as auto, so a corrupt or foreign
// entry degrades to the default rather than to a broken attribute. Private-mode
// Safari throws on localStorage access, hence the try around every touch.
function ccReadTheme(){try{var v=localStorage.getItem(CC_THEME_KEY);return(v==='light'||v==='dark')?v:'auto';}catch(e){return 'auto';}}
function ccApplyTheme(mode,persist){
  var r=document.documentElement;
  r.classList.add('cc-theme-switching');
  if(mode==='auto')r.removeAttribute('data-theme');else r.setAttribute('data-theme',mode);
  if(persist){try{if(mode==='auto')localStorage.removeItem(CC_THEME_KEY);else localStorage.setItem(CC_THEME_KEY,mode);}catch(e){}}
  var btn=document.getElementById('cc-theme-toggle');
  if(btn){
    var ui=CC_THEME_UI[mode]||CC_THEME_UI.auto;
    var next=CC_THEME_UI[CC_THEME_ORDER[(CC_THEME_ORDER.indexOf(mode)+1)%%CC_THEME_ORDER.length]]||CC_THEME_UI.auto;
    var g=btn.querySelector('.theme-toggle__glyph');if(g)g.textContent=ui.glyph;
    var t=btn.querySelector('.theme-toggle__text');if(t)t.textContent=ui.text;
    btn.setAttribute('data-theme-mode',mode);
    btn.setAttribute('title','Theme: '+ui.text+' \u2014 '+ui.hint);
    // Name the CURRENT state and what activating does, since the control is a
    // cycle rather than a two-way switch — aria-pressed cannot express three.
    btn.setAttribute('aria-label','Theme: '+ui.text+' ('+ui.hint+'). Activate for '+next.text+'.');
  }
  // Drop the no-transition guard once the swapped frame is painted. rAF does not
  // fire in a background tab, so the timeout is the floor that guarantees the
  // class can never stick and freeze every transition on the page.
  var clear=function(){r.classList.remove('cc-theme-switching');};
  if(window.requestAnimationFrame)requestAnimationFrame(function(){requestAnimationFrame(clear);});
  setTimeout(clear,150);
}
function ccCycleTheme(){ccApplyTheme(CC_THEME_ORDER[(CC_THEME_ORDER.indexOf(ccReadTheme())+1)%%CC_THEME_ORDER.length],true);}
// Sync the button's label with whatever the head guard already applied. Runs
// with persist=false so merely loading the page never writes storage.
ccApplyTheme(ccReadTheme(),false);

document.addEventListener('click',function(e){
  if(!e.target.closest)return;
  var d=e.target.closest('[data-action="dismiss-parent"]');
  if(d&&d.parentElement){d.parentElement.remove();return;}
  var th=e.target.closest('[data-action="cycle-theme"]');
  if(th){ccCycleTheme();return;}
  var s=e.target.closest('[data-select-on-click]');
  if(s&&s.select){s.select();}
});
document.addEventListener('error',function(e){
  var t=e.target;
  if(t&&t.getAttribute&&t.getAttribute('data-hide-on-error')){t.style.visibility='hidden';}
},true);
['click','mousedown'].forEach(function(type){
  document.addEventListener(type,function(e){
    if(e.target.closest&&e.target.closest('[data-stop-prop]')){e.stopPropagation();}
  },true);
});
})();
</script>
<script>
(function(){
// ── Init-order hoist (fixes the #2603/#2604/#2606 merge-interleaving regression) ──
// These were declared FAR below their first use. var hoists the name but not the
// value, so ADMIN_TIER_ORDER.map / ccActivitySeen[k] / ccActivity.length ran against
// undefined and threw. Declaring+initializing them here — before any function that
// uses them can run — makes the ordering explicit and regression-proof.
var ADMIN_TIER_ORDER=['newcomer','contributor','trusted','merger','advisor'];
// ccProjectName is declared in the ONBOARDING script block's IIFE, so it is not
// in scope here. It is republished on window there; mirror it into this closure
// as part of the same init-order discipline. renderMeCard's masthead reads it,
// and a bare cross-IIFE reference threw ReferenceError on every dossier render.
var ccProjectName=(typeof window!=='undefined'&&window.ccProjectName)||'Hive';
// ADMIN_COOLDOWN_DEFAULT_HOURS mirrors the server default (contributeCooldownDefaultHours,
// 168h = one week) so the period input shows the effective default when unset.
// ADMIN_COOLDOWN_MIN/MAX_HOURS mirror the server clamp bounds.
var ADMIN_COOLDOWN_DEFAULT_HOURS=168;
var ADMIN_COOLDOWN_MIN_HOURS=1;
var ADMIN_COOLDOWN_MAX_HOURS=8760;
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
var profileStarted=false; // Profile tab hydrated (fetches the dossier once)
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
var PANEL_SLUG={'tab-manage':'management','tab-ops':'operations','tab-leaderboard':'leaderboard','tab-profile':'profile'};
var SLUG_PANEL={
  'onboarding':'tab-onboarding',
  'management':'tab-manage','manage':'tab-manage',
  'operations':'tab-ops','ops':'tab-ops',
  'leaderboard':'tab-leaderboard',
  'profile':'tab-profile','dossier':'tab-profile','me':'tab-profile'
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
    // Triage ladder (#2612 part b): fetched after the tab opens so a slow GitHub
    // PR-link lookup never delays the page. A throw must not abort the panels above.
    try{ccTriagePoll();}catch(e){console.error('ccTriagePoll failed',e);}
  }
  // Leaderboard hydrates client-side on first open — read-only, no role gate.
  // The standings and the standing strip are independent: a throw in one must
  // never block the other.
  if(dp==='tab-leaderboard'&&!lbStarted){lbStarted=true;
    try{loadLeaderboard();}catch(e){console.error('loadLeaderboard failed',e);}
    try{loadMeStanding();}catch(e){console.error('loadMeStanding failed',e);}
  }
  // Profile hydrates the full dossier on first open, on its own tab.
  if(dp==='tab-profile'&&!profileStarted){profileStarted=true;
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
var REPORT_TAB_NAME={'tab-onboarding':'onboarding','tab-ops':'operations','tab-manage':'management','tab-leaderboard':'leaderboard','tab-profile':'profile'};
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
    // Ensure the per-row + hive-wide sparklines have data even when the Ops tab
    // was never opened (opsPoll never ran). Reuses this hive's metrics endpoint;
    // Ops-only spark slots simply no-op when absent. See #persistent-history.
    ccMetricsPoll();
  }).catch(function(){
    var el=document.getElementById('leaderboard-list');
    if(el)el.innerHTML='<div class="ops-empty">Could not load leaderboard.</div>';
  });
}
// tierBadge renders a small tier medallion / rank badge from a REAL trust tier.
// The five known tiers each get a muted metal-ish accent class; an unknown/blank
// tier is treated as newcomer (neutral). extraCls lets callers request the compact
// leaderboard/inline variants. Nothing here is fabricated — it is a pure visual
// wrap around the tier string the leaderboard/fleet snapshot already carries.
function tierBadge(tier,extraCls){
  var known={newcomer:1,contributor:1,trusted:1,merger:1,advisor:1};
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
  // Self-chosen dossier title (equipped_title) — a small quoted accent after the
  // name. Optional; absent for contributors who set none (never a placeholder).
  var title=(!e.is_agent&&e.equipped_title)?(' <span class="lb-title">“'+esc(e.equipped_title)+'”</span>'):'';
  // Human contributors link to their public dossier — the standings become a way
  // INTO the records. Agents have no dossier, so they stay plain text.
  var nameCell=(!e.is_agent&&uname)
    ?('<a class="lb-name__link" href="/contribute/dossier/'+encodeURIComponent(uname)+'">'+name+'</a>')
    :name;
  // "Done" (tasks_completed) is the hero numeral — real count, just emphasised.
  return '<div class="lb-row'+(isMe?' lb-row--me':'')+'">'
    +'<div class="lb-rank">#'+rank+'</div>'
    +'<div class="lb-name">'+badge+nameCell+title+youChip+'</div>'
    +'<div class="lb-tier">'+tier+'</div>'
    +'<div class="lb-stat lb-primary">'+done+'</div>'
    +'<div class="lb-stat">'+failed+'</div>'
    +'<div class="lb-stat">'+findings+'</div>'
    // Per-contributor completion sparkline (#persistent-history). Filled in by
    // ccRenderLeaderboardSparklines from this hive's /api/contribute/metrics
    // per_user_done, matched on github_username via data-user. Empty until metrics
    // load (renders a flat baseline), never fabricated.
    +'<div class="lb-spark" data-user="'+esc(uname)+'"></div>'
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
  // Hive-wide total-tasks trend (#persistent-history) pinned above the standings —
  // the sum of tasks_done per hour over the last 7 days. Hydrated by
  // ccRenderLeaderboardSparklines; empty (flat) until metrics load.
  var trend='<div class="lb-trend"><span>Hive throughput &middot; last 7 days</span><span class="spark" id="spark-lb-trend" title="Total tasks completed per hour, last 7 days"></span></div>';
  var html=trend+'<div class="lb-head lb-row"><div class="lb-rank">#</div><div class="lb-name">Contributor</div><div class="lb-tier">Tier</div><div class="lb-stat lb-primary">Done</div><div class="lb-stat">Failed</div><div class="lb-stat">Findings</div><div class="lb-stat">Trend</div></div>';
  var rank=0,i;
  for(i=0;i<contribs.length;i++){rank++;html+=lbRow(contribs[i],rank);}
  el.innerHTML=html;
  // Paint sparklines now that the rows exist (metrics may already be cached from a
  // prior opsPoll tick; if not, the next tick fills them in).
  ccRenderLeaderboardSparklines();
}

// ── Personal "Me" card ───────────────────────────────────────────────────────
// Resolve the logged-in username from the SAME identity source the page already
// uses (/api/gh-user-auth/status), then fetch the CENTRAL hub profile endpoint
// and render a pride-forward personal card pinned above the standings. Anonymous
// or unknown viewers get a subtle sign-in prompt — never an error. All data is
// real (tier / stats / milestones / hives / rank come straight from the hub).
var ME_STYLE_KEY='hive.me.cardStyle';   // localStorage key for the chosen skin
var ME_STYLE_COUNT=7;                    // number of profile-style skins offered
var ME_STYLE_NAMES=['Rank metal','Verdant','Amber rank','Violet advisor','Minimal','Rose','Roomy ranked'];
// Ceremony rank metals: trust tier → [designation, metal accent, soft wash].
// The metal drives style1 ("Rank metal", the default skin) via --me-metal;
// picking any other skin overrides --me-accent and beats the metal.
var ME_RANK_META={
  newcomer:['RECRUIT','#9aa3ae','rgba(154,163,174,.12)'],
  contributor:['OPERATOR','#58a6ff','rgba(88,166,255,.14)'],
  trusted:['SPECIALIST','#4db8a0','rgba(77,184,160,.14)'],
  merger:['SPECIALIST','#4db8a0','rgba(77,184,160,.14)'],
  advisor:['WARDEN','#8a97f7','rgba(138,151,247,.14)']
};
// The ladder shows every rung a real trust tier can stand on, plus the two that
// are explicitly aspirational. MERGER is a capability grant (it confers merge
// rights), not a ceremony step, so it shares SPECIALIST's rung rather than
// inventing a designation for it — the ladder measures standing, not permissions.
//
// VANGUARD and PARAGON are deliberately UNCLAIMED. Gold (#e8c15a) is VANGUARD's
// metal and no trust tier grants it: per the design spec it takes sustained
// presence plus maintainer recognition, and the first one should be an event,
// not a side effect of reaching the top trust tier. Ceremony stays decoupled
// from the security model — that is the whole point of a separate ladder.
var ME_LADDER=['RECRUIT','OPERATOR','SPECIALIST','WARDEN','VANGUARD','PARAGON'];
// Ladder index the viewer currently stands on. Every real tier maps to a rung;
// an unknown tier falls back to 0 rather than rendering nothing.
var ME_LADDER_AT={newcomer:0,contributor:1,trusted:2,merger:2,advisor:3};
// Rungs at or beyond this index are aspirational: no trust tier grants them yet,
// and the ladder marks them as such rather than implying they are next.
var ME_LADDER_ASPIRATIONAL_AT=4;
// Per-hive flavor strings (a hive can later configure these; the defaults ship
// on generic hives). EPIGRAPH sits under the dossier masthead; FOOTER_QUOTE
// closes the record.
var EPIGRAPH={text:'Be the one who moves, not the one who is moved.',attr:'Zavala'};
var FOOTER_QUOTE='Every record here began with one PR.';

// meEmblemProps deterministically derives the generative emblem-field custom
// props (--a1/--a2/--p1/--p2) from a seed string (emblem_seed or the GitHub
// username) via a djb2 hash — same seed, same emblem, forever.
function meEmblemProps(seed){
  var h=5381;
  for(var i=0;i<seed.length;i++){h=(((h<<5)+h)+seed.charCodeAt(i))>>>0;}
  return {
    a1:(h%%360)+'deg',
    a2:(Math.floor(h/7)%%360)+'deg',
    p1:(20+(h%%51))+'%%',
    p2:(20+(Math.floor(h/13)%%61))+'%%'
  };
}

function meStyleClass(){
  var n=parseInt(localStorage.getItem(ME_STYLE_KEY)||'1',10);
  if(!(n>=1&&n<=ME_STYLE_COUNT))n=1;
  return n;
}
function leaderboardCustomStyleLabel(src){
  var base=String(src||'').split('@')[0].split('/');
  return base.length>=2?(base[0]+'/'+base[1]):'custom';
}
function clearLeaderboardCustomStyleParam(){
  try{
    var u=new URL(window.location.href);
    if(!u.searchParams.has('style'))return;
    u.searchParams.delete('style');
    history.replaceState(null,'',u.pathname+(u.searchParams.toString()?('?'+u.searchParams.toString()):'')+u.hash);
  }catch(e){}
  window.HIVE_LEADERBOARD_CUSTOM_STYLE_SRC='';
  var link=document.getElementById('leaderboard-custom-style-link');
  if(link)link.remove();
  var note=document.getElementById('leaderboard-custom-style-note');
  if(note)note.remove();
}

// loadMeStanding fills the Leaderboard tab's one-line standing strip: where the
// viewer places, and a link across to their full dossier. Deliberately tiny —
// the standings are the content of that tab, not the dossier.
// ME_VIEW_USERNAME is set when the page was opened as a PUBLIC dossier
// permalink (/contribute/dossier/<username>) — the record being read then
// belongs to someone who may not be the viewer. Empty on the Profile tab, which
// always shows the signed-in viewer's own record.
var ME_VIEW_USERNAME=(function(){
  try{
    var m=window.location.pathname.match(/^\/contribute\/dossier\/([A-Za-z0-9-]{1,39})\/?$/);
    return m?m[1]:'';
  }catch(e){return '';}
})();
// ME_IS_OWNER gates the owner-only CONTROLS (edit form, invite, style picker,
// quota). UX only: every endpoint behind those controls independently resolves
// the caller server-side and refuses to act for anyone but its owner.
var ME_IS_OWNER=true;

function loadMeStanding(){
  var mount=document.getElementById('me-standing-mount');
  if(!mount)return;
  fetch('/api/gh-user-auth/status').then(function(r){return r.json();}).then(function(auth){
    if(!auth||!auth.logged_in||!auth.username){
      mount.innerHTML='<div class="me-standing"><span><b>Sign in</b> to see where you stand.</span></div>';
      return;
    }
    var u=auth.username;
    if(ccMeUsername!==u){ccMeUsername=u;if(typeof lbLastData!=='undefined'&&lbLastData){try{renderLeaderboard(lbLastData.contribs);}catch(e){}}}
    fetch('/api/leaderboard/contributor/'+encodeURIComponent(u)).then(function(r){return r.json();}).then(function(p){
      if(!p||!p.found){
        mount.innerHTML='<div class="me-standing"><span><b>'+esc(u)+'</b> — ship a task to enter the standings.</span></div>';
        return;
      }
      var place=(p.rank&&p.total)?('You stand <b>#'+p.rank+'</b> of '+p.total):'You are on the board';
      mount.innerHTML='<div class="me-standing"><span>'+place+'</span>'
        +'<a class="me-standing__link" href="/contribute/profile" id="me-standing-link">View your dossier &#9656;</a></div>';
      var lnk=document.getElementById('me-standing-link');
      if(lnk)lnk.addEventListener('click',function(ev){
        ev.preventDefault();
        activateTab(document.getElementById('ptab-profile'));
      });
    }).catch(function(e){console.error('standing load failed',e);});
  }).catch(function(e){console.error('auth status failed',e);});
}

function loadMeCard(){
  var mount=document.getElementById('me-card-mount');
  if(!mount)return;
  // Render the dossier for one username, marking whether the viewer owns it.
  function show(who,isOwner){
    ME_IS_OWNER=isOwner;
    fetch('/api/leaderboard/contributor/'+encodeURIComponent(who)).then(function(r){return r.json();}).then(function(p){
      if(!p||!p.found){
        if(isOwner){renderMeSignIn(mount,who);}
        else{mount.innerHTML='<div class="me-signin">No contributor record for <b>'+esc(who)+'</b> on this hive.</div>';}
        return;
      }
      // A throw inside renderMeCard is a BUG, not a missing profile. Reporting it
      // as "you have no profile" is what hid the ccProjectName ReferenceError for
      // a whole commit cycle — so render failures now say so, and log the stack.
      try{renderMeCard(mount,p);}
      catch(e){console.error('renderMeCard failed',e);renderMeError(mount,e);}
    }).catch(function(e){console.error('profile load failed',e);
      mount.innerHTML='<div class="me-signin">Could not load this dossier.</div>';});
  }
  fetch('/api/gh-user-auth/status').then(function(r){return r.json();}).then(function(auth){
    var viewer=(auth&&auth.logged_in&&auth.username)?auth.username:'';
    // Remember the viewer's username so the Rankings list can highlight their own
    // row. If the standings already rendered (race), re-render to apply the mark.
    if(viewer&&ccMeUsername!==viewer){ccMeUsername=viewer;if(typeof lbLastData!=='undefined'&&lbLastData){try{renderLeaderboard(lbLastData.contribs);}catch(e){}}}
    // A public permalink shows THAT contributor, signed in or not.
    if(ME_VIEW_USERNAME){
      show(ME_VIEW_USERNAME,viewer.toLowerCase()===ME_VIEW_USERNAME.toLowerCase());
      return;
    }
    if(!viewer){renderMeSignIn(mount);return;}
    show(viewer,true);
  }).catch(function(e){console.error('auth status failed',e);
    if(ME_VIEW_USERNAME){show(ME_VIEW_USERNAME,false);}
    else{renderMeSignIn(mount);}});
}

// A render failure is shown as a failure — never disguised as a missing profile.
function renderMeError(mount,e){
  mount.innerHTML='<div class="me-signin">Your dossier could not be rendered. '
    +'This is a bug, not a problem with your account — the details are in the browser console.</div>';
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
    +encodeURIComponent(window.location.origin+'/contribute/dossier/'+encodeURIComponent(p.github_username))
    +'&summary='+encodeURIComponent(text);
}

// meSeals renders the Triumphs zone: milestone SEALS (mockup .seal blocks).
// Attained milestones are solid seals; the nearest not-yet-attained milestone
// renders once as a dashed "next" seal with the honest "X to go" cue.
function meSeals(p){
  var out=[];
  var ms=(p.milestones||[]);
  for(var i=0;i<ms.length;i++){
    if(ms[i].attained){
      out.push('<div class="dz-seal"><div class="glyph"></div><div class="t-name">'
        +(ms[i].icon?esc(ms[i].icon)+' ':'')+esc(ms[i].label)+'</div>'
        +'<div class="t-sub">'+esc(ms[i].detail||'')+'</div></div>');
    }
  }
  if(p.next_milestone){
    var nm=p.next_milestone;
    var toGo='';
    // "X to go" progression cue, computed from the real threshold vs real count.
    if(nm.id&&nm.id.indexOf('tasks-')===0){var gap=nm.value-(p.tasks_completed||0);if(gap>0)toGo=' — '+gap+' to go';}
    else if(nm.id==='tier-contributor'){var g2=nm.value-(p.tasks_with_pr||0);if(g2>0)toGo=' — '+g2+' PR-tasks to go';}
    else if(nm.id==='tier-trusted'){var g3=nm.value-(p.tasks_with_pr||0);if(g3>0)toGo=' — '+g3+' PR-tasks to go';}
    else if(nm.id==='tier-merger'){toGo=' — maintainer grant required';}
    out.push('<div class="dz-seal dz-seal--next"><div class="glyph"></div><div class="t-name">'+esc(nm.label)+'</div>'
      +'<div class="t-sub">'+esc(nm.detail||'')+esc(toGo)+'</div></div>');
  }
  if(!out.length)return '<p class="dz-collab-empty">The record awaits its first entry.</p>';
  return '<div class="dz-seals">'+out.join('')+'</div>';
}

// meDeedsGrid renders the DEEDS OF RECORD stat blocks: tasks shipped, PRs
// landed, standing (#rank / total), plus — when the server's cached public
// GitHub fetch succeeded — service years and renown (followers). The GitHub
// blocks are simply absent when the fields are; nothing is fabricated.
//
// The failure count is deliberately NOT here. This is a public honour grid, and
// broadcasting a contributor's failures contradicts the dossier's own rule that
// nothing decays and nothing nags. The number remains visible in the Rankings
// table, which is the operational view.
function meDeedsGrid(p){
  var deeds=[
    [String(p.tasks_completed||0),'tasks shipped',''],
    [String(p.tasks_with_pr||0),'PRs landed',''],
    [(p.rank&&p.total)?('#'+p.rank+' <small>/ '+p.total+'</small>'):'—','standing',' dz-deed--standing']
  ];
  if(p.service_years!=null)deeds.push([String(p.service_years)+' <small>yrs</small>','github service','']);
  if(p.renown!=null)deeds.push([String(p.renown),'renown · followers','']);
  var out='';
  for(var i=0;i<deeds.length;i++){
    out+='<div class="dz-deed'+deeds[i][2]+'"><div class="num">'+deeds[i][0]+'</div><div class="cap">'+deeds[i][1]+'</div></div>';
  }
  return out;
}

// meLadder renders the ceremony ladder with the viewer's current rung
// highlighted (▸ prefix); earlier rungs read as attained in the rank metal.
// Rungs no tier can grant yet are marked aspirational rather than implied next.
function meLadder(p){
  var at=ME_LADDER_AT[p.trust_tier||'newcomer']||0;
  var out='<div class="me-ladder">';
  for(var i=0;i<ME_LADDER.length;i++){
    var cls=i<at?' attained':(i===at?' current':'');
    if(i>=ME_LADDER_ASPIRATIONAL_AT&&i!==at)cls+=' aspirational';
    out+='<span class="rung'+cls+'">'+(i===at?'▸ ':'')+ME_LADDER[i]+'</span>';
  }
  return out+'</div>';
}

// meCollaborators renders the people this contributor has worked alongside —
// a collection that GROWS and never decays. Each entry carries how they met and
// how many occasions there have been, both straight from the server record;
// nothing here is inferred client-side. The empty state is an honest invitation
// rather than a permanent placeholder.
var ME_COLLAB_HOW={invite:'invited',issue:'worked an issue together',presence:'served at the same time'};
function meCollaborators(p){
  var cs=(p.collaborators||[]);
  if(!cs.length){
    return '<p class="dz-collab-empty">No joint operations recorded yet.<br>'
      +'Invite someone, or take up an issue another contributor has worked.</p>';
  }
  var out='<div class="dz-collabs">';
  for(var i=0;i<cs.length;i++){
    var c=cs[i];
    var how=ME_COLLAB_HOW[c.how]||'worked together';
    var occ=(c.occasions>1)?(' · '+c.occasions+' occasions'):'';
    out+='<a class="dz-collab" href="/contribute/dossier/'+encodeURIComponent(c.username)+'">'
      +'<img class="dz-collab__av" src="https://github.com/'+encodeURIComponent(c.username)+'.png" alt="" '
        +'data-hide-on-error="1">'
      +'<span class="dz-collab__body"><span class="dz-collab__name">'+esc(c.username)+'</span>'
      +'<span class="dz-collab__how">'+esc(how)+esc(occ)+'</span></span></a>';
  }
  return out+'</div>';
}

function meHivesRows(p){
  var hs=(p.hives||[]);
  if(!hs.length)return '<div class="me-hive"><span style="color:var(--cc-muted);font-size:.8rem">No federated hives registered yet.</span></div>';
  var out=[];
  for(var i=0;i<hs.length;i++){
    var rel=hs[i].relationship||'contributor';
    var relCls=(rel==='owner')?'me-hive__rel me-hive__rel--owner':'me-hive__rel';
    var label=esc(hs[i].project_name||hs[i].org||hs[i].id||'hive');
    out.push('<div class="me-hive"><span class="me-hive__name">'+label
      +(hs[i].org?(' <span style="color:var(--cc-muted);font-weight:400;text-transform:none">('+esc(hs[i].org)+')</span>'):'')+'</span>'
      +'<span class="'+relCls+'">'+esc(rel)+'</span></div>');
  }
  return out.join('');
}

// ── Trusted invite (issue #2598) ─────────────────────────────────────────────
// Trust tiers permitted to invite. Kept in sync with the server's
// inviteTrustTiers gate; the UI hiding here is UX only — the /api/contribute/
// invite endpoint independently verifies the caller's tier and 403s otherwise.
var INVITE_TIERS={trusted:true,merger:true,advisor:true};

// meInviteSection renders the "Invite someone to contribute" affordance ONLY for
// a viewer whose real trust tier is trusted/merger/advisor. Any other tier (newcomer /
// contributor) — and anonymous viewers never reach renderMeCard — get nothing.
function meInviteSection(p){
  if(!ME_IS_OWNER)return ''; // minting invites is never offered on someone else's record
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

// meFieldLogSeed renders the honest server-known entries for the Field Log:
// the most recent completed task and the record opening. Nothing synthetic —
// if the live activity feed has richer per-contributor entries, meLoadFieldLog
// replaces these after fetch.
function meFieldLogSeed(p){
  var rows=[];
  if(p.last_completed_task){
    var lt=p.last_completed_task;
    var ref=(lt.repo?esc(lt.repo):'')+(lt.number?('#'+lt.number):'');
    var when=p.last_active?meTimeAgo(p.last_active):'—';
    rows.push('<div class="dz-frow"><span class="f-when">'+esc(when)+'</span><span class="f-what">shipped '
      +(ref?('<b>'+ref+'</b> '):'')+(lt.title?('<span>· '+esc(lt.title)+'</span>'):'')+'</span></div>');
  }
  var est=meYearMonth(p.registered_at);
  rows.push('<div class="dz-frow"><span class="f-when">'+esc(est||'—')+'</span><span class="f-what">record opened <span>· enlisted on this hive</span></span></div>');
  return rows.join('');
}

// meLoadFieldLog hydrates the Field Log from the hive's REAL recent activity
// feed (/api/contribute/activity), filtered to the viewer's own entries. When
// the feed holds nothing for them the honest server-known seed rows stay.
function meLoadFieldLog(username){
  var slot=document.getElementById('me-flog-slot');
  if(!slot)return;
  fetch('/api/contribute/activity').then(function(r){return r.json();}).then(function(d){
    var acts=(d&&d.activity||[]).filter(function(e){
      return e.username&&e.username.toLowerCase()===username.toLowerCase();
    });
    if(!acts.length)return;
    var verbs={joined:'entered the hive',left:'left the hive','picked up':'picked up',completed:'shipped',failed:'failed'};
    var rows=acts.slice(-6).reverse().map(function(e){
      var what=(verbs[e.action]||esc(e.action))+(e.task?(' <b>'+esc(e.task)+'</b>'):'')
        +(e.cli?(' <span>via '+esc(e.cli)+(e.model?(' · '+esc(e.model)):'')+'</span>'):'');
      return '<div class="dz-frow"><span class="f-when">'+esc(meTimeAgo(e.timestamp)||'')+'</span><span class="f-what">'+what+'</span></div>';
    });
    slot.innerHTML=rows.join('');
  }).catch(function(){});
}

function renderMeCard(mount,p){
  var tier=p.trust_tier||'newcomer';
  var avatar=p.avatar_url||('https://github.com/'+encodeURIComponent(p.github_username)+'.png');
  var styleN=meStyleClass();
  var rankMeta=ME_RANK_META[tier]||ME_RANK_META.newcomer;
  var em=meEmblemProps(p.emblem_seed||p.github_username||'');

  var styleOpts='';
  var customStyleSrc=window.HIVE_LEADERBOARD_CUSTOM_STYLE_SRC||'';
  for(var i=1;i<=ME_STYLE_COUNT;i++){
    styleOpts+='<option value="'+i+'"'+(!customStyleSrc&&i===styleN?' selected':'')+'>'+esc(ME_STYLE_NAMES[i-1]||('Style '+i))+'</option>';
  }
  if(customStyleSrc){
    var dropped=parseInt(window.HIVE_LEADERBOARD_CUSTOM_STYLE_DROPPED||'0',10)||0;
    var customLabel=dropped>0?('Custom ('+dropped+' rules removed by sanitizer)'):('Custom ('+leaderboardCustomStyleLabel(customStyleSrc)+')');
    styleOpts+='<option value="custom" selected title="'+esc(dropped>0?'Some CSS was removed by the sanitizer; see Custom CSS help.':leaderboardCustomStyleLabel(customStyleSrc))+'">'+esc(customLabel)+'</option>';
  }

  // Identity plate extras: the equipped title (self-chosen, quoted, in the
  // accent) above the name, and a designation line (archetype · est. date).
  var callsign=p.equipped_title?('<div class="me-callsign">“'+esc(p.equipped_title)+'”</div>'):'';
  var desigBits=[];
  if(p.archetype)desigBits.push('<b>'+esc(p.archetype)+'</b>');
  var estYM=meYearMonth(p.registered_at);
  if(estYM)desigBits.push('est. '+esc(estYM));
  var desig=desigBits.length?('<div class="me-desig">'+desigBits.join(' · ')+'</div>'):'';
  // Founding mark: only when the server established a REAL registration order
  // within the founding cohort — otherwise absent, never faked.
  var founding=(p.founding_position&&p.founding_position>=1&&p.founding_position<=20)
    ?'<div><span class="me-founding">Founding cohort · first twenty</span></div>':'';

  // Livebar: rendered ONLY when a task is genuinely live on the hub.
  var livebar='';
  if(p.current_task){
    var loadoutBits=[p.cli_backend,p.model].filter(function(x){return !!x;}).map(esc);
    if(p.sessions)loadoutBits.push(p.sessions+' session'+(p.sessions===1?'':'s'));
    livebar='<div class="dz-livebar"><span class="dot"></span><span class="live-tag">ON OPERATION</span>'
      +'<span>'+(p.current_task.number?('#'+p.current_task.number+' · '):'')+esc(p.current_task.title||'')+'</span>'
      +(loadoutBits.length?('<span class="live-dim">'+loadoutBits.join(' · ')+'</span>'):'')
      +'</div>';
  }

  // Next-designation ladder rung for the Golden Path header. A rung at or beyond
  // the aspirational cut is NOT announced as "next": nothing a contributor can
  // count toward grants it, and pairing that word with the progress bars below
  // would invent a gate. Per the design spec those designations are conferred
  // (sustained presence plus maintainer recognition), so the header says so.
  var ladderAt=ME_LADDER_AT[tier]||0;
  var nextIdx=ladderAt+1;
  var nextRung=nextIdx<ME_LADDER.length?ME_LADDER[nextIdx]:'';
  var nextIsConferred=nextIdx>=ME_LADDER_ASPIRATIONAL_AT;

  var footId=(p.hives&&p.hives[0]&&p.hives[0].id)?p.hives[0].id:ccProjectName;

  var html=''
  +'<div class="me-card me-card--style'+styleN+'" id="me-card" style="--me-metal:'+rankMeta[1]+';--me-metal-soft:'+rankMeta[2]+'">'
  // Masthead + epigraph (per-hive flavor; defaults ship on generic hives).
  +'<header class="dz-masthead"><span class="brand">'+ccProjectName+' · contributor record</span>'
    +'<span class="id">DOSSIER '+esc(p.github_username)+'</span></header>'
  +(EPIGRAPH&&EPIGRAPH.text?('<p class="dz-epigraph"><em>“'+esc(EPIGRAPH.text)+'”</em>'+(EPIGRAPH.attr?(' — '+esc(EPIGRAPH.attr)):'')+'</p>'):'')
  // ZONE A — identity plate.
  +'<section class="dz-identity" aria-label="Identity">'
  +'<div class="me-emblem" style="--a1:'+em.a1+';--a2:'+em.a2+';--p1:'+em.p1+';--p2:'+em.p2+'"></div>'
  +'<div class="dz-identity-inner">'
  +'<div class="dz-medallion"><img src="'+esc(avatar)+'" alt="" data-hide-on-error="1"></div>'
  +'<div class="dz-namebloc">'+callsign+'<h1 class="dz-heroname">'+esc(p.github_username)+'</h1>'+desig+founding+'</div>'
  +'<div class="dz-rankpill"><div class="rank-name">'+esc(rankMeta[0])+'</div><div class="rank-sub">trust · '+esc(tier)+'</div></div>'
  +'</div>'+livebar+'</section>'
  // ZONE B | ZONE C — Deeds of Record | Operator Profile.
  +'<div class="dz-grid">'
  +'<section class="dz-zcard" aria-label="Deeds of record"><div class="dz-zone-head">Deeds of Record</div>'
    +'<div class="dz-deeds">'+meDeedsGrid(p)+'</div></section>'
  +'<section class="dz-zcard" aria-label="Profile"><div class="dz-zone-head">Operator Profile</div>'
    +meProfileRows(p)+meTestimonySection(p)+meDossierForm(p)+'</section>'
  +'</div>'
  // Full-width — The Golden Path.
  +'<section class="dz-zcard" style="margin-bottom:16px" aria-label="The Golden Path">'
  +'<div class="dz-path-head"><div class="dz-zone-head">The Golden Path</div>'
    +(nextRung?('<div class="dz-path-next">'+(nextIsConferred?'<b>'+nextRung+'</b> is conferred, not counted toward':'next designation <b>'+nextRung+'</b>')+'</div>'):'')+'</div>'
  +mePathProgress(p)+meLadder(p)
  +'<div class="me-path-note">The path does not expire. Nothing here is lost by stepping away.</div></section>'
  // ZONE D | ZONE E — Triumphs (+ Heraldry) | Collaborators.
  +'<div class="dz-grid">'
  +'<section class="dz-zcard" aria-label="Triumphs"><div class="dz-zone-head">Triumphs</div>'
    +meSeals(p)
    +'<div class="dz-heraldry-head"><span>Heraldry · verified via Credly</span></div>'
    +'<div id="me-heraldry-slot"><div class="ops-note" style="margin:0">Loading heraldry&hellip;</div></div></section>'
  +'<section class="dz-zcard" aria-label="Collaborators"><div class="dz-zone-head">Collaborators</div>'
    +meCollaborators(p)+'</section>'
  +'</div>'
  // ZONE F | ZONE G — Field Log | Theaters of Operation.
  +'<div class="dz-grid">'
  +'<section class="dz-zcard" aria-label="Field log"><div class="dz-zone-head">Field Log</div>'
    +'<div class="dz-flog" id="me-flog-slot">'+meFieldLogSeed(p)+'</div></section>'
  +'<section class="dz-zcard" aria-label="Theaters of operation"><div class="dz-zone-head">Theaters of Operation</div>'
    +'<div class="me-hives">'+meHivesRows(p)+'</div></section>'
  +'</div>'
  // Record footer.
  +'<footer class="dz-footer"><span>HIVE // '+esc(footId)+'</span><span class="quote">'+esc(FOOTER_QUOTE)+'</span></footer>'
  // Below the dossier: the OWNER's own controls — quota, share, style, invite.
  // A visitor reading someone else's record gets none of them: they are personal
  // affordances, not part of the record itself.
  +(ME_IS_OWNER?(
     '<section class="dz-zcard" style="margin-top:16px" aria-label="Daily quota"><div class="dz-zone-head">Daily quota</div>'
    +'<div class="me-quota-wrap" id="me-quota-slot"><div class="ops-note" style="margin:0">Loading your quota&hellip;</div></div>'
    +'<div class="me-actions">'
      +'<a class="me-share" href="'+esc(meLinkedInURL(p))+'" target="_blank" rel="noopener noreferrer">\u{1F4E3} Share achievement on LinkedIn</a>'
      +'<span class="me-stylepick">Profile style <select id="me-style-select" aria-label="Profile style">'+styleOpts+'</select></span>'
      +'<span class="info-affordance custom-css-help"><button type="button" class="info-btn" id="custom-css-info-btn" aria-haspopup="true" aria-expanded="false" aria-controls="custom-css-info-pop" aria-label="Custom CSS stylesheet help" title="Custom CSS">Custom CSS</button>'
      +'<div class="info-pop custom-css-pop" id="custom-css-info-pop" role="tooltip" hidden><h4>Custom CSS</h4>'
      +'Use <code>?style=owner/repo/path/theme.css@ref</code> to load a theme. Example:'
      +'<input class="custom-css-example" readonly aria-label="Custom CSS example" value="?style=castrojo/themes/lb/bluefin.css@main" data-select-on-click="1">'
      +'Omit <code>@ref</code> to use the repo&rsquo;s <code>HEAD</code>. Public GitHub repos only; CSS is sanitized server-side and capped at <code>128 KiB</code>. Allowed: custom properties, attribute and pseudo selectors, <code>calc()</code>/<code>clamp()</code>/gradients, and <code>@media</code>, <code>@supports</code>, <code>@container</code>, <code>@keyframes</code>. <code>@font-face</code> is kept only with same-origin or <code>data:</code> sources. Removed: <code>@import</code>, external <code>url()</code> fetches, CSS escapes, and legacy executable CSS. Add <code>&amp;report=1</code> to the style API URL for sanitizer details. The same param works on <code>/</code> and <code>/snapshot</code>.</div></span>'
    +'</div>'
    +meInviteSection(p)
    +'</section>'):'')
  +'</div>';
  mount.innerHTML=html;

  wireMeInvite();
  _wireCustomCSSInfo();
  wireMeDossier(p);
  loadMeHeraldry(p.github_username);
  meLoadFieldLog(p.github_username);
  // #2595 daily-quota widget: the viewer's OWN remaining quota, so it is loaded
  // only on their own record — a visitor has no quota to show here.
  if(ME_IS_OWNER){
    if(typeof ccLimits!=='undefined'&&ccLimits!==null){try{ccRenderMeQuota();}catch(e){}}
    else if(typeof ccLoadLimits==='function'){try{ccLoadLimits();}catch(e){}}
  }

  var sel=document.getElementById('me-style-select');
  if(sel)sel.addEventListener('change',function(){
    if(sel.value==='custom')return;
    var v=parseInt(sel.value,10);if(!(v>=1&&v<=ME_STYLE_COUNT))v=1;
    localStorage.setItem(ME_STYLE_KEY,String(v));
    clearLeaderboardCustomStyleParam();
    var customOpt=sel.querySelector('option[value="custom"]');
    if(customOpt)customOpt.remove();
    var card=document.getElementById('me-card');
    if(card){for(var k=1;k<=ME_STYLE_COUNT;k++)card.classList.remove('me-card--style'+k);card.classList.add('me-card--style'+v);}
  });
}

// ── Contributor dossier (me-card v2) ─────────────────────────────────────────
// meYearMonth extracts "YYYY-MM" from an RFC3339 timestamp for the "est." line.
function meYearMonth(iso){
  if(!iso||iso.length<7)return '';
  var m=/^(\d{4})-(\d{2})/.exec(iso);
  return m?(m[1]+'-'+m[2]):'';
}

// meTimeAgo renders a coarse relative time ("2h ago") from an RFC3339 string.
// Only ever called with a server-vetted RECENT timestamp (last_active is
// omitted server-side beyond 14 days — absence is never rendered).
function meTimeAgo(iso){
  var t=Date.parse(iso);
  if(isNaN(t))return '';
  var s=Math.max(0,Math.floor((Date.now()-t)/1000));
  if(s<60)return 'just now';
  if(s<3600)return Math.floor(s/60)+'m ago';
  if(s<86400)return Math.floor(s/3600)+'h ago';
  return Math.floor(s/86400)+'d ago';
}

// meProfileRows renders the dossier operator-profile rows. Unset fields render
// as a quiet em-dash — never a nag, never a completion meter.
function meProfileRows(p){
  var unset='<span class="unset">—</span>';
  var rows=[];
  rows.push(['Archetype',p.archetype?esc(p.archetype):unset,'']);
  var specs=(p.specializations||[]);
  rows.push(['Specializations',specs.length?('<span class="me-specs">'+specs.map(function(s){return '<span class="me-spec">'+esc(s)+'</span>';}).join('')+'</span>'):unset,'']);
  var loadout=p.cli_backend?esc(p.cli_backend):'';
  rows.push(['Loadout',loadout||unset,loadout?' mono':'']);
  var clanker=p.model?esc(p.model):'';
  rows.push(['Clanker',clanker||unset,clanker?' mono':'']);
  rows.push(['Sponsor',p.invited_by?esc(p.invited_by):unset,'']);
  var active='since '+esc(meYearMonth(p.registered_at)||'—');
  if(p.last_active){var ago=meTimeAgo(p.last_active);if(ago)active+=' · last op '+esc(ago);}
  rows.push(['Active',active,'']);
  var html='<div class="me-prows">';
  for(var i=0;i<rows.length;i++){
    html+='<div class="me-prow"><span class="k">'+rows[i][0]+'</span><span class="v'+rows[i][2]+'">'+rows[i][1]+'</span></div>';
  }
  return html+'</div>';
}

// meTestimonySection: the contributor's own words as a blockquote, or (when
// unset) a quiet single-link invite to the inline dossier form. Optional
// forever — no completion meter, no badge for filling it in.
function meTestimonySection(p){
  // A visitor sees the testimony as part of the record, but never the invitation
  // to edit it — that belongs to its owner alone.
  if(p.testimony){
    return '<div class="me-testimony">'+esc(p.testimony)+'<span class="attr">Testimony · in their own words</span></div>'
      +(ME_IS_OWNER?'<div class="me-dossier-invite"><a id="me-dossier-open">▸ Edit your dossier</a></div>':'');
  }
  if(!ME_IS_OWNER)return '';
  return '<div class="me-dossier-invite"><a id="me-dossier-open">▸ Complete your dossier</a></div>';
}

// meDossierForm: the small inline self-service editor. Every field optional,
// saved via POST /api/contribute/dossier (identity resolved server-side).
function meDossierForm(p){
  if(!ME_IS_OWNER)return ''; // the editor is the owner's alone
  var specs=(p.specializations||[]).join(', ');
  return '<div class="me-dossier-form" id="me-dossier-form">'
    +'<label>Equipped title <input id="me-df-title" maxlength="40" placeholder="e.g. WOLFHERDER" value="'+esc(p.equipped_title||'')+'"><span class="hint">Shown quoted above your name and on the rankings.</span></label>'
    +'<label>Archetype <input id="me-df-archetype" maxlength="40" placeholder="e.g. Community Steward" value="'+esc(p.archetype||'')+'"></label>'
    +'<label>Specializations <input id="me-df-specs" placeholder="docs, triage, ci (comma-separated, up to 8)" value="'+esc(specs)+'"></label>'
    +'<label>Testimony <textarea id="me-df-testimony" rows="2" maxlength="200" placeholder="Your own words — why you’re here (200 chars)">'+esc(p.testimony||'')+'</textarea></label>'
    +'<label>Credly name <input id="me-df-credly" maxlength="60" placeholder="your-credly-vanity-name" value="'+esc(p.credly_name||'')+'"><span class="hint">Links your public Credly badges as heraldry. Optional — your record stands either way.</span></label>'
    +'<div class="actions"><button type="button" class="me-dossier-save" id="me-df-save">Save dossier</button>'
    +'<button type="button" class="me-dossier-cancel" id="me-df-cancel">Cancel</button>'
    +'<span class="hint">Every field is optional.</span></div>'
  +'</div>';
}

// meWireCredlyLink wires the "Link it" invite to the dossier form. Both the
// form and the heraldry slot render this affordance, and the slot re-renders
// asynchronously, so the lookup happens at call time rather than being closed
// over by either caller.
function meWireCredlyLink(){
  var link=document.getElementById('me-heraldry-link');
  var form=document.getElementById('me-dossier-form');
  if(!link||!form)return;
  link.addEventListener('click',function(){
    form.classList.add('open');
    var f=document.getElementById('me-df-credly');
    if(f)f.focus();
  });
}

// wireMeDossier hooks up the open/cancel/save affordances of the inline form.
function wireMeDossier(p){
  var open=document.getElementById('me-dossier-open');
  var form=document.getElementById('me-dossier-form');
  if(open&&form)open.addEventListener('click',function(){form.classList.toggle('open');});
  var link2=document.getElementById('me-heraldry-link');
  if(link2)meWireCredlyLink();
  var cancel=document.getElementById('me-df-cancel');
  if(cancel&&form)cancel.addEventListener('click',function(){form.classList.remove('open');});
  var save=document.getElementById('me-df-save');
  if(!save)return;
  save.addEventListener('click',function(){
    save.disabled=true;
    var val=function(id){var el=document.getElementById(id);return el?el.value:'';};
    var specs=val('me-df-specs').split(',').map(function(s){return s.trim();}).filter(function(s){return !!s;});
    var body={
      equipped_title:val('me-df-title'),
      archetype:val('me-df-archetype'),
      specializations:specs,
      testimony:val('me-df-testimony'),
      credly_name:val('me-df-credly')
    };
    fetch('/api/contribute/dossier',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
      .then(function(r){return r.json().then(function(j){return {ok:r.ok,j:j};});})
      .then(function(res){
        save.disabled=false;
        if(!res.ok){toast((res.j&&res.j.error)||'Could not save dossier',false);return;}
        toast('Dossier saved',true);
        loadMeCard();
      }).catch(function(){save.disabled=false;toast('Could not save dossier',false);});
  });
}

// mePathProgress renders the req progress bars toward the nearest
// not-yet-attained NUMERIC milestones (real thresholds vs real counts —
// nothing fabricated): the next tasks-shipped landmark and, if still pending,
// the next PR-task tier threshold. Requirements only fill, never drain; there
// is nothing here that decays. Maintainer-granted milestones (no number) get
// no bar — there is nothing honest to fill.
function mePathProgress(p){
  var reqs=[];
  var ms=p.milestones||[];
  for(var i=0;i<ms.length;i++){
    var m=ms[i];
    if(m.attained||!m.value)continue;
    if(m.id&&m.id.indexOf('tasks-')===0){reqs.push([m.label,(p.tasks_completed||0),m.value]);break;}
  }
  for(var j=0;j<ms.length;j++){
    var t=ms[j];
    if(t.attained||!t.value)continue;
    if(t.id==='tier-contributor'||t.id==='tier-trusted'){reqs.push([t.label,(p.tasks_with_pr||0),t.value]);break;}
  }
  if(!reqs.length)return '';
  var out='<div class="me-path-reqs">';
  for(var k=0;k<reqs.length;k++){
    var have=reqs[k][1],need=reqs[k][2];
    var pct=Math.min(100,Math.round(have/need*100));
    out+='<div class="me-path-req"><div class="req-top"><span class="req-name">'+esc(reqs[k][0])+'</span>'
      +'<span class="req-num">'+have+' / '+need+'</span></div>'
      +'<div class="bar"><i style="width:'+pct+'%%"></i></div></div>';
  }
  return out+'</div>';
}

// loadMeHeraldry lazily fetches + mounts the viewer's public Credly badges.
// The unlinked state is a quiet invite, never a nag; badges float free with a
// soft halo + plinth shadow (heraldic, not corporate — no boxes).
function loadMeHeraldry(username){
  var slot=document.getElementById('me-heraldry-slot');
  if(!slot)return;
  var unlinked='<div class="me-heraldry-note">Have a Credly profile? <a id="me-heraldry-link">Link it</a> to mount your heraldry here. Optional — your record stands either way.</div>';
  var wireLink=meWireCredlyLink;
  fetch('/api/leaderboard/contributor/'+encodeURIComponent(username)+'/heraldry')
    .then(function(r){return r.json();})
    .then(function(h){
      if(!h||!h.linked){slot.innerHTML=unlinked;wireLink();return;}
      var badges=h.badges||[];
      if(!badges.length){slot.innerHTML='<div class="me-heraldry-note">No public badges on the linked profile yet.</div>';return;}
      // Cap the hall at 8 so it reads curated, not exhaustive; the profile link
      // carries the rest.
      var cap=8,total=badges.length;
      var shown=badges.slice(0,cap);
      var out='<div class="me-heraldry">';
      for(var i=0;i<shown.length;i++){
        var b=shown[i];
        var yr=(b.issued_at||'').slice(0,4);
        var sub=[b.issuer_summary,yr].filter(function(x){return !!x;}).map(esc).join(' · ');
        var inner='<span class="shield"><img src="'+esc(b.image_url||'')+'" alt="" loading="lazy"></span>'
          +'<span class="plinth"></span>'
          +'<span class="ribbon">'+esc(b.name||'')+'</span>'
          +(sub?('<span class="a-sub">'+sub+'</span>'):'');
        out+=b.public_url
          ?('<a class="me-arms" href="'+esc(b.public_url)+'" target="_blank" rel="noopener noreferrer" title="Verify on Credly">'+inner+'</a>')
          :('<span class="me-arms">'+inner+'</span>');
      }
      out+='</div>';
      if(total>cap){
        var prof='https://www.credly.com/users/'+encodeURIComponent(h.credly_name||'');
        out+='<div class="me-heraldry-note" style="margin-top:10px">+'+(total-cap)+' more on <a href="'+esc(prof)+'" target="_blank" rel="noopener noreferrer">Credly</a></div>';
      }
      slot.innerHTML=out;
    }).catch(function(){slot.innerHTML=unlinked;wireLink();});
}

var currentFilter='all';
var lastWork=[];
document.querySelectorAll('.ops-filter').forEach(function(f){f.addEventListener('click',function(){
  document.querySelectorAll('.ops-filter').forEach(function(x){x.classList.remove('active');});
  f.classList.add('active');
  currentFilter=f.getAttribute('data-filter');
  renderWork(lastWork);
});});

// esc HTML-escapes a value for interpolation into markup. It escapes QUOTES as
// well as &<>, because the dossier and admin renderers interpolate values into
// attribute context (value="..." / href="..." / title="...") as often as into
// text. The previous textContent→innerHTML round-trip escaped only &<>, so a
// stored or upstream-fetched value containing a double quote could close the
// attribute and inject markup — reachable on the PUBLIC dossier permalink via a
// crafted Credly image/badge URL. Escaping quotes is safe in text context too:
// &quot; and &#39; render as " and ' there.
function esc(s){return (s==null?'':String(s))
  .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')
  .replace(/"/g,'&quot;').replace(/'/g,'&#39;');}

// ── Sparklines (#persistent-history) ───────────────────────────────────────────
// A dependency-free, CSP-safe inline-SVG trend renderer. Given an array of
// numbers it returns an <svg> polyline string sized w x h in the given colour.
// No external library, no canvas, no animation (static by nature — nothing to
// gate behind prefers-reduced-motion). Degrades gracefully: an empty or single-
// point array renders a flat baseline rather than a NaN path, so a brand-new hive
// with no history yet shows a calm flat line instead of a broken chart.
var SPARK_W=64;   // default sparkline width in px
var SPARK_H=18;   // default sparkline height in px
var SPARK_PAD=2;  // top/bottom padding so the stroke is not clipped at extremes
function sparkline(values,w,h,color){
  values=values||[];
  w=w||SPARK_W;h=h||SPARK_H;color=color||'#8b949e';
  var innerH=h-SPARK_PAD*2;
  if(innerH<1)innerH=1;
  var pts=[];
  // Flat baseline for empty / single-point series: a centred horizontal line.
  if(values.length<2){
    var y=SPARK_PAD+innerH/2;
    pts=[[0,y],[w,y]];
  }else{
    var min=values[0],max=values[0],i;
    for(i=1;i<values.length;i++){if(values[i]<min)min=values[i];if(values[i]>max)max=values[i];}
    var range=max-min;
    var stepX=w/(values.length-1);
    for(i=0;i<values.length;i++){
      var x=i*stepX;
      // Invert Y (SVG origin is top-left) and flatten a zero-range series to the
      // vertical centre so a constant value reads as a steady line, not a spike.
      var norm=range>0?(values[i]-min)/range:0.5;
      var yy=SPARK_PAD+(1-norm)*innerH;
      pts.push([x,yy]);
    }
  }
  var d='';
  for(var j=0;j<pts.length;j++){
    d+=(j===0?'M':'L')+pts[j][0].toFixed(1)+' '+pts[j][1].toFixed(1);
  }
  return '<span class="spark" aria-hidden="true"><svg width="'+w+'" height="'+h+'" viewBox="0 0 '+w+' '+h+'" preserveAspectRatio="none">'+
    '<path d="'+d+'" fill="none" stroke="'+color+'" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>'+
    '</svg></span>';
}
// setSpark injects a sparkline into the element with the given id, if present.
function setSpark(id,values,w,h,color){
  var el=document.getElementById(id);
  if(el)el.innerHTML=sparkline(values,w,h,color);
}
// ccMetrics caches the last /api/contribute/metrics payload so the leaderboard
// render (which runs independently of opsPoll) can read per-user history without
// its own fetch. Null until the first successful poll.
var ccMetrics=null;
// ccMetricsPoll fetches the persistent hourly series and paints the four Ops-tab
// sparklines. Called from opsPoll() on its existing cadence — hourly data does
// not need a fast dedicated timer, so every opsPoll tick is more than enough.
function ccMetricsPoll(){
  return fetch('/api/contribute/metrics').then(function(r){return r.json();}).then(function(d){
    ccMetrics=d||{};
    // (a) Ready-work queue header → queue-depth trend.
    setSpark('spark-queue',ccMetrics.queue_depth,SPARK_W,SPARK_H,'#388bfd');
    // (b) Tasks-completed / hour throughput.
    setSpark('spark-throughput',ccMetrics.tasks_done,SPARK_W,SPARK_H,'#3fb950');
    // (c) Connected-clanker fleet-size trend.
    setSpark('spark-fleet',ccMetrics.fleet_size,SPARK_W,SPARK_H,'#d29922');
    // (d) Your daily-quota usage trend — the viewer's own per-hour completions.
    if(ccMeUsername&&ccMetrics.per_user_done&&ccMetrics.per_user_done[ccMeUsername]){
      setSpark('spark-quota',ccMetrics.per_user_done[ccMeUsername],SPARK_W,SPARK_H,'#388bfd');
    }
    // Leaderboard hive-wide trend + per-row sparklines, if the tab is rendered.
    ccRenderLeaderboardSparklines();
  }).catch(function(e){console.error('metrics poll failed',e);});
}
// ccRenderLeaderboardSparklines paints the hive-wide total-tasks trend strip and
// each per-contributor row sparkline from the cached metrics. Safe to call any
// time — it no-ops when the leaderboard is not on screen or metrics are absent.
function ccRenderLeaderboardSparklines(){
  if(!ccMetrics)return;
  var trend=document.getElementById('spark-lb-trend');
  if(trend)trend.innerHTML=sparkline(ccMetrics.tasks_done,120,20,'#3fb950');
  var pud=ccMetrics.per_user_done||{};
  var rows=document.querySelectorAll('.lb-spark[data-user]');
  for(var i=0;i<rows.length;i++){
    var u=rows[i].getAttribute('data-user');
    var series=(u&&pud[u])?pud[u]:[];
    rows[i].innerHTML=sparkline(series,60,16,'#8b949e');
  }
}

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
    'title="Open on GitHub" data-stop-prop="1">'+
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
var adminGrantableAgentRoles=[];
var adminAssignableAgentRoles=['outreach','quality','scanner'];
var privilegedAgentRoles={};
['ci-maintainer','sec-check','architect'].forEach(function(r){privilegedAgentRoles[r]=true;});

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

// _wireCooldownInfo toggles the cooldown explainer popover (#2649 companion). The
// ⓘ button flips the popover's [hidden] + aria-expanded; a click anywhere else or
// Escape closes it. Null-guarded so a missing element never throws (matching the
// confirm-modal wiring above).
function _wireCooldownInfo(){
  var btn=document.getElementById('cooldown-info-btn');
  var pop=document.getElementById('cooldown-info-pop');
  if(!btn||!pop)return;
  function place(){if(!pop.hidden)ccPlaceFixedPopover(btn,pop,{fallbackWidth:300,boundary:btn.closest('.ops-card')});}
  function close(){pop.hidden=true;btn.setAttribute('aria-expanded','false');}
  btn.addEventListener('click',function(e){
    e.stopPropagation();
    var open=pop.hidden;
    if(open){pop.hidden=false;btn.setAttribute('aria-expanded','true');place();}
    else close();
  });
  pop.addEventListener('click',function(e){e.stopPropagation();});
  window.addEventListener('resize',place);
  window.addEventListener('scroll',place,true);
  document.addEventListener('click',function(e){if(!pop.hidden&&e.target!==btn&&!pop.contains(e.target))close();});
  document.addEventListener('keydown',function(e){if(e.key==='Escape')close();});
}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',_wireCooldownInfo);else _wireCooldownInfo();

// _wireAffinityInfo toggles the label-interests explainer popover (#2637), mirroring
// _wireCooldownInfo exactly: the ⓘ button flips [hidden] + aria-expanded; a click
// elsewhere or Escape closes it. Null-guarded so a missing element never throws.
function _wireAffinityInfo(){
  var btn=document.getElementById('affinity-info-btn');
  var pop=document.getElementById('affinity-info-pop');
  if(!btn||!pop)return;
  function place(){if(!pop.hidden)ccPlaceFixedPopover(btn,pop,{fallbackWidth:300,boundary:btn.closest('.ops-card')});}
  function close(){pop.hidden=true;btn.setAttribute('aria-expanded','false');}
  btn.addEventListener('click',function(e){
    e.stopPropagation();
    var open=pop.hidden;
    if(open){pop.hidden=false;btn.setAttribute('aria-expanded','true');place();}
    else close();
  });
  pop.addEventListener('click',function(e){e.stopPropagation();});
  window.addEventListener('resize',place);
  window.addEventListener('scroll',place,true);
  document.addEventListener('click',function(e){if(!pop.hidden&&e.target!==btn&&!pop.contains(e.target))close();});
  document.addEventListener('keydown',function(e){if(e.key==='Escape')close();});
}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',_wireAffinityInfo);else _wireAffinityInfo();

// ccPlaceFixedPopover places an already-visible popover/menu using viewport
// coordinates so it is not clipped by card/queue overflow. It prefers below the
// trigger, flips above when needed, and clamps inside the viewport plus an optional
// boundary element (for example the visible .cc-queue panel).
function ccPlaceFixedPopover(anchor,pop,opts){
  opts=opts||{};
  var edge=opts.edge||8,gap=opts.gap||8;
  pop.style.position='fixed';
  pop.style.left='0px';
  pop.style.top='0px';
  pop.style.right='auto';
  pop.style.bottom='auto';
  var b=anchor.getBoundingClientRect();
  var vw=document.documentElement.clientWidth||window.innerWidth||0;
  var vh=document.documentElement.clientHeight||window.innerHeight||0;
  var r=pop.getBoundingClientRect();
  var w=r.width||pop.offsetWidth||opts.fallbackWidth||320;
  var h=r.height||pop.offsetHeight||opts.fallbackHeight||0;
  var minTop=edge,maxBottom=vh-edge;
  if(opts.boundary&&opts.boundary.getBoundingClientRect){
    var cb=opts.boundary.getBoundingClientRect();
    minTop=Math.max(minTop,cb.top+edge);
    maxBottom=Math.min(maxBottom,cb.bottom-edge);
  }
  if(maxBottom<=minTop){minTop=edge;maxBottom=vh-edge;}
  var rawLeft=(opts.align==='right')?(b.right-w):b.left;
  var maxLeft=Math.max(edge,vw-w-edge);
  var left=Math.min(Math.max(rawLeft,edge),maxLeft);
  var below=maxBottom-b.bottom-gap;
  var above=b.top-gap-minTop;
  var top=(below>=h||below>=above)?(b.bottom+gap):(b.top-gap-h);
  var maxTop=Math.max(minTop,maxBottom-h);
  top=Math.min(Math.max(top,minTop),maxTop);
  pop.style.left=left+'px';
  pop.style.top=top+'px';
}

// _wireCustomCSSInfo toggles the compact custom-stylesheet help next to the
// Leaderboard/Profile style picker. The element is rendered with the Me card, so
// renderMeCard() calls this after replacing that fragment.
function _wireCustomCSSInfo(){
  var btn=document.getElementById('custom-css-info-btn');
  var pop=document.getElementById('custom-css-info-pop');
  if(!btn||!pop||btn.getAttribute('data-wired')==='1')return;
  btn.setAttribute('data-wired','1');
  function place(){
    if(pop.hidden)return;
    ccPlaceFixedPopover(btn,pop,{fallbackWidth:320});
  }
  function close(){pop.hidden=true;btn.setAttribute('aria-expanded','false');}
  btn.addEventListener('click',function(e){
    e.stopPropagation();
    var open=pop.hidden;
    if(open){pop.hidden=false;btn.setAttribute('aria-expanded','true');place();}
    else close();
  });
  pop.addEventListener('click',function(e){e.stopPropagation();});
  window.addEventListener('resize',place);
  window.addEventListener('scroll',place,true);
  document.addEventListener('click',function(){if(!pop.hidden)close();});
  document.addEventListener('keydown',function(e){if(e.key==='Escape')close();});
}

// ccRenderCooldownCount surfaces the current cooldown tally next to the Management
// tab's Task cooldown control (#2649): "M issues currently cooling down". It reads
// the ccCooldownCount stashed by opsPoll and writes into #admin-cooldown-count.
// Hidden entirely when 0 so the control stays clean.
function ccRenderCooldownCount(){
  var el=document.getElementById('admin-cooldown-count');
  if(!el)return;
  if(ccCooldownCount>0){
    el.textContent=ccCooldownCount+(ccCooldownCount===1?' issue currently cooling down':' issues currently cooling down');
    el.style.display='';
  }else{
    el.textContent='';
    el.style.display='none';
  }
}

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
  // Task cooldown: the GET resolves contribute_cooldown_enabled to a concrete
  // bool (unset -> true), and contribute_cooldown_hours to the EFFECTIVE period
  // (168 default surfaces when unset), so we render both directly. The period
  // input is disabled while cooldown is off.
  var cdOn=(adminHub.contribute_cooldown_enabled!==false);
  document.getElementById('admin-cooldown-switch').classList.toggle('on',cdOn);
  var cdHours=document.getElementById('admin-cooldown-hours');
  if(cdHours){
    if(document.activeElement!==cdHours)cdHours.value=adminHub.contribute_cooldown_hours||ADMIN_COOLDOWN_DEFAULT_HOURS;
    cdHours.disabled=!cdOn;
    cdHours.style.opacity=cdOn?'1':'0.5';
  }
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

// bindCooldownHoursInput persists the Task Cooldown PERIOD immediately on change
// (like the immediate toggles, not the deferred filter Save), through the same
// governor-hub PUT. The value is clamped client-side to [min,max]; the server
// clamps again. On success adminHub is updated and both surfaces re-rendered so
// the Governor Hub tab picks up the new value on its next poll.
var cooldownHoursBusy=false;
function bindCooldownHoursInput(){
  var inp=document.getElementById('admin-cooldown-hours');
  if(!inp)return;
  inp.addEventListener('change',function(){
    if(cooldownHoursBusy||!adminHub)return;
    var v=parseInt(inp.value,10);
    if(isNaN(v)||v<ADMIN_COOLDOWN_MIN_HOURS)v=ADMIN_COOLDOWN_MIN_HOURS;
    if(v>ADMIN_COOLDOWN_MAX_HOURS)v=ADMIN_COOLDOWN_MAX_HOURS;
    inp.value=v;
    cooldownHoursBusy=true;
    adminSaveHub({contribute_cooldown_hours:v},'Cooldown period set to '+v+'h').then(function(ok){
      cooldownHoursBusy=false;
      if(ok){adminHub.contribute_cooldown_hours=v;renderAdminControls();}
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
  if(!adminEnabled)return;
  var controlRole=sel.getAttribute('data-role');
  if(controlRole==='agent-role-add'){
    var addRole=sel.value;
    sel.value='';
    if(addRole)updateContributorAgentRoleGrants(sel.getAttribute('data-cid'),null,addRole);
    return;
  }
  if(controlRole==='agent-role'){
    var cidRole=sel.getAttribute('data-cid'),acting=sel.value||'none';
    fetch('/api/contributors/'+encodeURIComponent(cidRole)+'/agent-role',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({agent_role:acting})})
      .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
      .then(function(x){if(x.ok){toast(acting==='none'?'Acting as general work':'Acting as '+acting,true);opsPoll();}else{toast((x.d&&x.d.error)||'Failed to set acting role',false);opsPoll();}})
      .catch(function(){toast('Failed to set acting role',false);opsPoll();});
    return;
  }
  if(controlRole!=='tier')return;
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
  if(role==='agent-role-remove'){
    updateContributorAgentRoleGrants(b.getAttribute('data-cid'),b.getAttribute('data-agent-role'),null);
    return;
  }
  if(role!=='revoke'&&role!=='remove'&&role!=='requeue')return;
  var cid=b.getAttribute('data-cid'),user=b.getAttribute('data-user')||'this contributor';
  if(role==='requeue'){
    // Reassign (kubestellar/hive#2568 + follow-up): take the clanker off its in-flight
    // task and immediately hand it its next-priority item, so it keeps working instead of
    // idling. The released task goes back to the ready queue for someone else. Not
    // destructive to the contributor (no revoke/remove). The released task is booked for
    // the same short cooldown as an auto-release and briefly not re-offered to THIS same
    // clanker, so it moves to different work. Uses the existing
    // POST /api/contributors/{id}/requeue endpoint (owner/read-write only), whose handler
    // now performs the release + reassignment.
    adminConfirm('Reassign '+user,'Take '+user+' off their current task and hand them their next-priority item; that task goes back to the ready queue for someone else. The released task won&rsquo;t be re-offered to '+user+' for a short window. If nothing else is available the clanker is simply released and idle. This uses the existing POST /api/contributors/{id}/requeue endpoint.','Reassign',function(){
      // Let the operator attach an optional reason. It is recorded in the audit +
      // activity log and pushed to the still-connected worker on task_revoke.
      var reason=(window.prompt('Reason for reassigning this clanker (optional):','wedged: moving to different work')||'').trim();
      fetch('/api/contributors/'+encodeURIComponent(cid)+'/requeue',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({reason:reason})})
        .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
        .then(function(x){
          if(x.ok){
            var msg=(x.d&&x.d.reassigned)?('Reassigned '+user+' &rarr; '+x.d.assigned_repo+'#'+x.d.assigned_number):('Reassigned '+user+' (released; no other work available, now idle)');
            toast(msg,true);opsPoll();
          }else{toast((x.d&&x.d.error)||'Reassign failed',false);}
        })
        .catch(function(){toast('Reassign failed',false);});
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
  bindImmediateToggle('admin-cooldown-switch');
  bindCooldownHoursInput();
  try{
    // The Governor config GET is what carries the hub.contribute_* fields the
    // Governor Hub dialog edits (GET /api/config, by contrast, is a thin summary
    // without them). Reading the same source keeps this mirror in lockstep.
    var cr=await fetch('/api/config/governor');var cd=await cr.json();
    adminHub=(cd&&cd.hub)?cd.hub:{};
  }catch(e){adminHub={};}
  syncGrantableAgentRoles(null);
  renderAdminControls();
  // Resume-all (#queue-hold): wire the header button once, now that we know the viewer
  // is owner/read-write. Visibility is still driven by ccRenderResumeAll (count-gated).
  var resumeAllBtn=document.getElementById('queue-resume-all-btn');
  if(resumeAllBtn&&!resumeAllBtn._wired){resumeAllBtn._wired=true;resumeAllBtn.addEventListener('click',function(e){e.stopPropagation();ccResumeAll();});}
  // The ready-work queue may have rendered before this role check resolved (SSE
  // hello / poll fires immediately on tab open). Re-render now that adminEnabled is
  // true so the grab bars appear for this owner/read-write viewer.
  if(typeof ccRenderQueue==='function')ccRenderQueue();
}

// capabilityLine renders the client-declared runtime posture (#2547 DECLARE half)
// as a compact, READ-ONLY sub-line: "declares: podman &middot; linux/arm64 &middot;
// cli 1.2.3 &middot; proto 1.1 &middot; cred:app". It returns '' when the client
// declared nothing (an unversioned relay), so those rows look exactly as before.
// The literal "declares:" prefix and the title tooltip keep the SELF-REPORTED
// nature visible at the point of use: this data is advisory only and is never used
// to route, gate, or trust — a client can claim anything (see kubestellar/hive#2547
// risk section). Purely display; nothing here feeds a dispatch or permission
// decision.
function capabilityLine(caps){
  if(!caps)return '';
  var parts=[];
  if(caps.container_runtime)parts.push(esc(caps.container_runtime));
  var osArch=[caps.os,caps.arch].filter(Boolean).map(esc).join('/');
  if(osArch)parts.push(osArch);
  if(caps.agent_cli_version)parts.push('cli '+esc(caps.agent_cli_version));
  if(caps.relay_protocol_version)parts.push('proto '+esc(caps.relay_protocol_version));
  if(caps.credential_type)parts.push('cred:'+esc(caps.credential_type));
  if(!parts.length)return '';
  return '<div class="clanker-sub clanker-declares" title="Self-declared by the client. Advisory only — the hub records and shows it but never routes, gates, or trusts work on it.">declares: '+parts.join(' &middot; ')+'</div>';
}
// protocolLine (#2547 peer-compatibility criterion) renders the hub-vs-client
// contributor-protocol comparison the fleet snapshot derives. Until now the ops
// row showed the client's declared "proto 1.1" with NOTHING to compare it
// against, so an operator could not tell a current relay from one three minors
// behind — the issue's "the only way to learn that an old relay is talking to a
// new hub is to watch it misbehave", still true after both sides gained a version.
//
// It returns '' unless there is genuinely something to say: an exact match and an
// undeclared version (every relay predating the versioned handshake) both render
// nothing, so this adds no noise to a healthy fleet and no shaming of an old
// client that works fine. The mismatch flag is the server's own boolean, so the
// UI does not re-derive the verdict set.
//
// Advisory ONLY. A drifted or even majorly-incompatible peer is admitted and
// assigned work exactly as before — the title text says so explicitly, because a
// warning-coloured line an operator cannot act on invites them to invent a
// remedy (disabling the contributor) that the hub never asked for.
function protocolLine(p){
  if(!p||!p.mismatch)return '';
  var label={older:'older than this hub',newer:'newer than this hub',
    incompatible:'INCOMPATIBLE with this hub',malformed:'unparseable'}[p.verdict];
  if(!label)return '';
  var cls='clanker-proto'+(p.verdict==='incompatible'?' incompatible':'');
  var shown=p.peer?esc(p.peer):'(none)';
  return '<div class="'+cls+'" title="'+esc(p.detail||'')+' The hub does not gate on this — the client is served exactly as before.">'+
    'protocol: client '+shown+' &middot; hub '+esc(p.hub||'')+' &middot; '+label+'</div>';
}
// #2546: human-readable label for the machine reason a clanker is idle. Keeps the
// raw reason as a fallback so a new server-side reason still renders legibly.
function idleReasonLabel(r){
  var m={contribution_suspended:'contribution suspended',hub_not_ready:'hub not ready',
    no_matching_work:'no matching work',token_mint_failed:'token mint failed',
    tier_disabled:'tier disabled',concurrency_limit:'concurrency limit'};
  return m[r]||String(r).replace(/_/g,' ');
}
// clankerInterestsLine (#2677) renders one contributor's OWN label interests as
// a compact read-only chip line for the operator fleet view. Purely
// observability: an owner sees who prefers what, but this never offers a way
// to set/edit another contributor's interests — that stays contributor-owned
// via PUT /api/contribute/interests (see the "My label interests" editor
// above). Returns '' when the contributor has none declared, matching how
// capabilityLine() omits itself for an unversioned client.
function clankerInterestsLine(interests){
  if(!interests||!interests.length)return '';
  var chips=interests.map(function(l){return '<span class="clanker-interest-chip">'+esc(l)+'</span>';}).join('');
  return '<div class="clanker-interests" title="This contributor&rsquo;s own opt-in label interests. Read-only here &mdash; they set these themselves."><span class="clanker-interests-label">prefers:</span>'+chips+'</div>';
}
function syncGrantableAgentRoles(policy){
  var roles=(policy&&policy.agent_role_grantable_roles)||[];
  if((!roles||!roles.length)&&adminHub&&adminHub.contribute_delegatable_roles){
    roles=adminHub.contribute_delegatable_roles;
  }
  var seen={},out=[];
  (roles||[]).forEach(function(r){
    r=String(r||'').trim().toLowerCase();
    if(r&&r!=='supervisor'&&privilegedAgentRoles[r]&&!seen[r]){seen[r]=true;out.push(r);}
  });
  adminGrantableAgentRoles=out.sort();
}
function syncAssignableAgentRoles(policy){
  var roles=(policy&&policy.agent_role_assignable_roles)||[];
  if((!roles||!roles.length)&&adminHub&&adminHub.contribute_delegatable_roles){
    roles=['scanner','quality','outreach'].concat(adminHub.contribute_delegatable_roles||[]);
  }
  if(!roles||!roles.length)roles=['scanner','quality','outreach'];
  var seen={},out=[];
  (roles||[]).forEach(function(r){
    r=String(r||'').trim().toLowerCase();
    if(r&&r!=='supervisor'&&!seen[r]){seen[r]=true;out.push(r);}
  });
  adminAssignableAgentRoles=out.sort();
}
function clankerActingAsControl(c,cid){
  var current=String(c.role||'').trim().toLowerCase();
  var roles=adminAssignableAgentRoles.slice();
  if(current&&roles.indexOf(current)<0)roles.push(current);
  roles.sort();
  var opts='<option value="none"'+(!current?' selected':'')+'>none (general work)</option>'+
    roles.map(function(r){return '<option value="'+esc(r)+'"'+(r===current?' selected':'')+'>'+esc(r)+'</option>';}).join('');
  var tip=c.role_mismatch||'Owner assignment takes effect on the next task request and never rewrites the current in-flight task.';
  return '<select class="admin-act" title="'+esc(tip)+'" data-cid="'+cid+'" data-role="agent-role">'+opts+'</select>';
}
function clankerAgentRoleGrantControl(c,cid){
  var grants=(c.agent_role_grants||[]).map(function(r){return String(r||'').trim().toLowerCase();}).filter(Boolean);
  var seen={};grants=grants.filter(function(r){if(seen[r])return false;seen[r]=true;return true;}).sort();
  var chips=grants.length?grants.map(function(r){
    return '<span class="agent-role-chip">'+esc(r)+'<button type="button" aria-label="Remove '+esc(r)+' grant" title="Remove '+esc(r)+' grant" data-cid="'+cid+'" data-agent-role="'+esc(r)+'" data-role="agent-role-remove">&times;</button></span>';
  }).join(''):'<span class="clanker-sub">none</span>';
  var granted={};grants.forEach(function(r){granted[r]=true;});
  var addOpts=adminGrantableAgentRoles.filter(function(r){return !granted[r];}).map(function(r){return '<option value="'+esc(r)+'">'+esc(r)+'</option>';}).join('');
  var add=addOpts?('<select class="agent-role-add" title="Grant a privileged agent role" data-cid="'+cid+'" data-role="agent-role-add"><option value="">+ grant</option>'+addOpts+'</select>'):'';
  var tip='Defaults scanner, quality and outreach need no grant. Privileged roles require trusted+ tier, hive allow-listing and this per-contributor grant. Supervisor is never delegatable.';
  return '<div class="agent-role-grants"><span class="agent-role-grants__label">Agent roles</span><span class="info-affordance"><button type="button" class="info-btn" tabindex="-1" aria-label="How agent-role grants work" title="'+tip+'">&#9432;</button></span>'+chips+add+'</div>';
}
function updateContributorAgentRoleGrants(cid,removeRole,addRole){
  if(!cid)return;
  var row=null;
  document.querySelectorAll('[data-role-grants-cid]').forEach(function(el){if(el.getAttribute('data-role-grants-cid')===cid)row=el;});
  var grants=[];
  if(row){
    row.querySelectorAll('[data-agent-role]').forEach(function(b){var r=b.getAttribute('data-agent-role');if(r)grants.push(r);});
  }
  if(removeRole)grants=grants.filter(function(r){return r!==removeRole;});
  if(addRole&&grants.indexOf(addRole)<0)grants.push(addRole);
  fetch('/api/contributors/'+encodeURIComponent(cid)+'/agent-role-grants',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({agent_role_grants:grants})})
    .then(function(r){return r.json().then(function(d){return{ok:r.ok,d:d};});})
    .then(function(x){if(x.ok){toast('Agent-role grants updated',true);opsPoll();}else{toast((x.d&&x.d.error)||'Failed to update grants',false);}})
    .catch(function(){toast('Failed to update grants',false);});
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
  // #2637 owner roster: aggregate the fleet's label interests into the owner-facing
  // "which labels to target, and who" summary. Independent of the row render below so
  // a malformed row can't leave the roster stuck on "Loading…".
  try{ccRenderInterestRoster(list);}catch(e){console.error('interest roster render failed',e);}
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
    // #2547 (DECLARE half): the client-declared runtime posture, surfaced READ-ONLY
    // exactly like cli_backend/model/role above. It is a self-report and is NEVER
    // used to route or gate work — see capabilityLine() for the "self-declared" note
    // that keeps that visible at the point of use (per the issue's risk section).
    var capsLine=capabilityLine(c.capabilities);
    // #2547 (peer-compatibility): the declared protocol version above is only
    // meaningful next to the hub's own. Renders nothing when they agree or when
    // the client declared none, so this is silent for a healthy fleet.
    var protoLine=protocolLine(c.protocol);
    // #2677: this contributor's own label interests, read-only, so the operator
    // gets a fleet-wide view of who prefers what without cross-referencing each
    // profile separately (the data already travels in this same fleet snapshot).
    var interestsLine=clankerInterestsLine(c.label_interests);
    // #2534: owner/read-write get per-contributor admin actions wired to the
    // EXISTING endpoints — set trust tier / promote (PUT /api/contributors/{id}/trust),
    // revoke (POST .../revoke), remove (DELETE .../{id}). Hidden for read viewers.
    // The contributor id (not the username) keys those endpoints.
    var actions='';
    if(adminEnabled&&c.contributor_id){
      var cid=esc(c.contributor_id);
      var tier=c.trust_tier||'newcomer';
      var opts=['newcomer','contributor','trusted','merger','advisor'].map(function(t){
        return '<option value="'+t+'"'+(t===tier?' selected':'')+'>'+t+'</option>';
      }).join('');
      // Reassign (kubestellar/hive#2568 + follow-up) is only meaningful while the clanker
      // is HOLDING a task — an operator moves work they can see is wedged. Hidden when
      // idle. It reuses the existing requeue endpoint/role; its handler now releases the
      // task AND immediately reassigns this clanker its next-priority item. An ⓘ marker
      // (same info-btn affordance as the cooldown explainer) states the outcome on hover,
      // so the operator understands it WITHOUT opening the confirm dialog.
      var reassignInfo='Reassign takes this clanker off its current task and immediately hands it the next-priority item, so it keeps working. The released task goes back to the ready queue for another contributor, and isn&rsquo;t re-offered to this clanker for a short window.';
      var requeueBtn=c.current_task
        ?('<span class="info-affordance"><button type="button" class="admin-act" title="'+reassignInfo+'" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="requeue">Reassign</button>'+
          '<button type="button" class="info-btn" tabindex="-1" aria-label="What does Reassign do?" title="'+reassignInfo+'">&#9432;</button></span>')
        :'';
      actions='<div class="admin-actions" data-role-grants-cid="'+cid+'">'+
        '<select class="admin-act" title="Set trust tier (maintainer voucher)" data-cid="'+cid+'" data-role="tier">'+opts+'</select>'+
        '<label class="clanker-act-as">Acting as '+clankerActingAsControl(c,cid)+'</label>'+
        requeueBtn+
        '<button type="button" class="admin-act danger" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="revoke">Revoke</button>'+
        '<button type="button" class="admin-act danger" data-cid="'+cid+'" data-user="'+esc(user)+'" data-role="remove">Remove</button>'+
        clankerAgentRoleGrantControl(c,cid)+
        '</div>';
    }
    // #command-center: at-a-glance status pill (working / reviewing / idle) and a
    // stable data-clanker key so the travel animation can target this row. "review"
    // is inferred when the in-flight task carries a review-ish signal; otherwise a
    // task in flight is "working" and no task is "idle".
    var st=c.current_task?'working':'idle';
    if(c.current_task&&/review|lgtm|approve/i.test((c.current_task.kind||'')+' '+(c.current_task.title||'')))st='reviewing';
    var statusText=st;
    if(c.current_task&&c.role)statusText=st+' &middot; as '+esc(c.role);
    var statusPill='<span class="clanker-status '+st+'">'+statusText+'</span>';
    var key=(c.github_username||c.contributor_id||'').toLowerCase();
    // Enter pop-in for a clanker we haven't seen in the previous render (army framing).
    var isNew=key&&!ccKnownClankers[key];
    var rowCls='clanker-row'+(isNew?' cc-enter':'');
    var rowTitle=c.role_mismatch?(' title="'+esc(c.role_mismatch)+'"'):'';
    return '<div class="'+rowCls+'" data-clanker="'+esc(key)+'"'+rowTitle+'><span class="clanker-dot'+(c.stale?' stale':'')+'"></span>'+av+
      '<div class="clanker-main"><div class="clanker-user">'+esc(user)+statusPill+tierPill+'</div>'+
      '<div class="clanker-sub">'+(sub||'&mdash;')+'</div>'+task+capsLine+protoLine+interestsLine+'</div>'+
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
// ccRenderInterestRoster (#2637) builds the OWNER-facing aggregate: for each label
// any connected contributor subscribes to, show the label + how many contributors
// want it + who — so the owner can label matching issues to route work. Aggregated
// from the SAME fleet snapshot renderClankers gets (each clanker carries its own
// label_interests), so it updates every poll. Labels sort by descending count, ties
// alphabetical. A graceful empty state shows when NO connected clanker set interests
// (the explainer above still renders, so the owner learns the feature exists).
function ccRenderInterestRoster(list){
  var body=document.getElementById('label-affinity-body');
  if(!body)return;
  list=list||[];
  // label(lower) -> {label: display, who: [usernames]}. First-seen casing wins for
  // the display label; matching is case-insensitive so 'nvidia'/'NVIDIA' aggregate.
  var agg={};
  list.forEach(function(c){
    var who=c.github_username||c.contributor_id||'clanker';
    var interests=c.label_interests||[];
    for(var i=0;i<interests.length;i++){
      var raw=(interests[i]||'').trim();if(!raw)continue;
      var lk=raw.toLowerCase();
      if(!agg[lk])agg[lk]={label:raw,who:[]};
      if(agg[lk].who.indexOf(who)<0)agg[lk].who.push(who);
    }
  });
  var labels=Object.keys(agg);
  if(!labels.length){
    body.innerHTML='<div class="affinity-empty">No contributors have set label interests yet &mdash; when they do, you&rsquo;ll see which labels to target here.</div>';
    return;
  }
  // Sort by descending contributor count, then alphabetically for a stable order.
  labels.sort(function(a,b){
    var d=agg[b].who.length-agg[a].who.length;
    if(d!==0)return d;
    return agg[a].label<agg[b].label?-1:(agg[a].label>agg[b].label?1:0);
  });
  body.innerHTML=labels.map(function(lk){
    var e=agg[lk];
    var whoStr=e.who.map(esc).join(', ');
    return '<div class="affinity-row">'+
      '<span class="affinity-chip">'+esc(e.label)+'<span class="affinity-count">'+e.who.length+'</span></span>'+
      '<span class="affinity-who">'+whoStr+'</span>'+
    '</div>';
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
  // reload bridge — overwritten by the next poll, including to empty.
  ccOpsCacheWrite(OPS_CACHE_WORK_KEY,lastWork);
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
    ['Assignable agent roles',list(p.agent_role_assignable_roles)],
    ['Privileged agent-role grants',list(p.agent_role_grantable_roles)],
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
    syncGrantableAgentRoles(data&&data.policy);
    syncAssignableAgentRoles(data&&data.policy);
    // Each panel renders independently — one failing does not block the others.
    safeRender('clankers',function(){renderClankers((data&&data.clankers)||[]);});
    safeRender('work',function(){renderWork((data&&data.work)||[]);});
    safeRender('policy',function(){renderPolicy(data&&data.policy);});
    // Cooldown / in-flight tallies (#2649 companion): stash the read-only counts
    // from the fleet payload so the ready-queue header can annotate "N ready" with
    // "M in cooldown / K in flight", and the Management cooldown control can show
    // how many issues are currently cooling down. Coerced to a number; a missing
    // field stays 0. A re-render picks up the fresh values.
    ccCooldownCount=(data&&typeof data.cooldown_count==='number')?data.cooldown_count:0;
    ccInFlightCount=(data&&typeof data.in_flight_count==='number')?data.in_flight_count:0;
    ccHeldCount=(data&&typeof data.held_count==='number')?data.held_count:0;
    safeRender('queue-counts',function(){if(typeof ccRenderQueue==='function')ccRenderQueue();});
    safeRender('cooldown-count',function(){if(typeof ccRenderCooldownCount==='function')ccRenderCooldownCount();});
  }catch(e){
    // fetch/parse failed — log so the "Loading…" placeholders are diagnosable, and
    // fall through to reschedule so a transient failure self-heals on the next poll.
    console.error('opsPoll fetch failed',e);
  }
  // Persistent hourly sparklines (#persistent-history). Independent of the fleet
  // fetch above (its own try/catch inside ccMetricsPoll) so a metrics hiccup never
  // stalls the panels. Hourly data on the opsPoll cadence is plenty — no fast timer.
  ccMetricsPoll();
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
// ccKnownQueueKeys tracks the queue-item keys (repo#number) present in the PREVIOUS
// row render so the cc-popin enter animation only plays for GENUINELY-new arrivals —
// mirroring ccKnownClankers for the fleet list. Without it, every poll re-render
// replayed the enter animation on EVERY row and the whole queue "blinked". Rebuilt
// at the end of each row render from the items actually painted.
var ccKnownQueueKeys={};
// ccQueueRenderDeferred is set when a poll re-render was SKIPPED because a ⋯ menu /
// "Move to #" dialog was open (a rebuild would wipe the open menu mid-interaction).
// ccCloseQueueMenus replays one render when it flips true, so the queue catches up to
// the latest data the moment the operator closes the menu.
var ccQueueRenderDeferred=false;
var ccCompleteStreak={};   // username -> consecutive completes (achievement combos)
var ccLastAch=0;           // debounce achievement pops
var ccCooldownCount=0;     // issues still within cooldown (from fleet payload, #2649)
var ccInFlightCount=0;     // issues currently held by a live connection (fleet payload)
var ccHeldCount=0;         // issues manually PARKED by the operator (fleet payload, on-hold tally)

// ── Label-affinity (#2637): the viewer's own label interests ───────────────────
// ccInterests is this viewer's opt-in label list (normalised lower-case). Loaded
// from /api/contribute/queue's "interests" echo (and the dedicated interests GET)
// when the viewer has a contributor profile; null means "not a known contributor"
// (or not loaded yet) so we hide the editor. It is a SOFT signal: we re-tag the
// queue client-side so matches highlight/float even on the anonymous SSE snapshot,
// but we NEVER drop a row — a viewer with no interests sees the shared queue as-is.
var ccInterests=null;
// ccInterestSet mirrors ccInterests as a lookup set for O(1) per-label matching.
var ccInterestSet={};
function ccRebuildInterestSet(){
  ccInterestSet={};
  (ccInterests||[]).forEach(function(l){var n=(l||'').trim().toLowerCase();if(n)ccInterestSet[n]=true;});
}
// ccItemMatchesInterests tags one queue item with matches_interest by comparing its
// labels (exact, case-insensitive) against the viewer's interest set. Mirrors the
// server rule so the client view agrees with a personalised /api/contribute/queue.
function ccItemMatchesInterests(q){
  var labels=q.labels||[];
  for(var i=0;i<labels.length;i++){
    if(ccInterestSet[(labels[i]||'').trim().toLowerCase()])return true;
  }
  return false;
}
// ccApplyInterestsToQueue re-tags every item's matches_interest and STABLY floats
// matches to the front — mirroring the server's personalizeQueueByInterests so the
// view is identical whether the queue arrived via the personalised poll or the
// anonymous SSE snapshot. No-op (order untouched) when the viewer set no interests:
// the anti-starvation guarantee holds client-side too.
function ccApplyInterestsToQueue(){
  for(var i=0;i<ccQueue.length;i++){ccQueue[i].matches_interest=ccItemMatchesInterests(ccQueue[i]);}
  var keys=Object.keys(ccInterestSet);
  if(!keys.length)return; // nothing to promote; leave order exactly as-is
  // Stable partition: matches first, keeping each group's relative order.
  var matched=[],rest=[];
  for(var j=0;j<ccQueue.length;j++){(ccQueue[j].matches_interest?matched:rest).push(ccQueue[j]);}
  ccQueue=matched.concat(rest);
}

// ── My-label-interests editor (#2637) ──────────────────────────────────────────
// The editor is shown ONLY to a viewer with a contributor profile (ccInterests is
// a real array, even if empty). ccApplyInterestsFromResponse is called with the
// "interests" field the personalised /api/contribute/queue returns; a present array
// means "known contributor" → show + render the editor.
function ccApplyInterestsFromResponse(interests){
  if(!Array.isArray(interests))return; // anonymous / no profile: leave hidden
  ccInterests=interests.slice();
  ccRebuildInterestSet();
  ccRenderInterests();
}
function ccRenderInterests(){
  var wrap=document.getElementById('cc-interests');if(!wrap)return;
  if(ccInterests===null){wrap.style.display='none';return;}
  wrap.style.display='';
  var chips=document.getElementById('cc-interests-chips');
  if(chips){
    if(!ccInterests.length){
      chips.innerHTML='<span class="cc-interests-empty">No interests yet &mdash; add a label (like <code>nvidia</code>) to have matching issues surfaced first for you.</span>';
    }else{
      chips.innerHTML=ccInterests.map(function(l){
        return '<span class="cc-interest-chip">'+esc(l)+'<span class="cc-interest-x" data-label="'+esc(l)+'" title="Remove" role="button" aria-label="Remove '+esc(l)+'">&times;</span></span>';
      }).join('');
      var xs=chips.querySelectorAll('.cc-interest-x');
      for(var i=0;i<xs.length;i++){(function(x){x.addEventListener('click',function(){ccRemoveInterest(x.getAttribute('data-label'));});})(xs[i]);}
    }
  }
}
function ccAddInterestFromInput(){
  var inp=document.getElementById('cc-interests-input');if(!inp)return;
  var v=(inp.value||'').trim().toLowerCase();
  inp.value='';
  if(!v||ccInterests===null)return;
  if(ccInterests.indexOf(v)>=0)return; // already present
  var next=ccInterests.concat([v]);
  ccSaveInterests(next);
}
function ccRemoveInterest(label){
  if(ccInterests===null)return;
  var next=ccInterests.filter(function(l){return l!==label;});
  ccSaveInterests(next);
}
// ccSaveInterests PUTs the new set and, on success, adopts the server-sanitised
// result (the server is the authority on normalisation/dedupe/cap), then re-renders
// the editor AND the queue so highlights/order update immediately.
function ccSaveInterests(next){
  fetch('/api/contribute/interests',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({interests:next})})
    .then(function(r){return r.ok?r.json():null;})
    .then(function(d){
      if(d&&Array.isArray(d.interests)){ccInterests=d.interests.slice();}
      else{ccInterests=next;} // optimistic fallback if the body was unexpected
      ccRebuildInterestSet();
      ccRenderInterests();
      ccRenderQueue(); // re-tag + re-float with the new interests
    })
    .catch(function(){});
}
function ccInitInterestsEditor(){
  var btn=document.getElementById('cc-interests-add-btn');
  if(btn)btn.addEventListener('click',ccAddInterestFromInput);
  var inp=document.getElementById('cc-interests-input');
  if(inp)inp.addEventListener('keydown',function(e){if(e.key==='Enter'){e.preventDefault();ccAddInterestFromInput();}});
  // Seed interests from the personalised queue endpoint (which echoes them for a
  // known contributor). A dedicated GET is unnecessary — the queue we already poll
  // carries them — but this initial fetch guarantees the editor appears even before
  // the first SSE/poll render.
  fetch('/api/contribute/queue').then(function(r){return r.json();}).then(function(d){
    if(d)ccApplyInterestsFromResponse(d.interests);
  }).catch(function(){});
}

// ccQueueKey is the identity the operator's HOLD and REORDER controls persist,
// so it must be the server's canonical key (kubestellar/hive#4245). The server
// now sends it as q.key: "owner/repo#42" for GitHub-backed work (unchanged) and
// "owner/repo!ENG-123" for a string-keyed source. The fallback keeps rows from a
// pre-#4245 server working; without the q.key preference an external item
// produced "owner/repo#" — a key the server can never match, so holding or
// reordering it silently did nothing.
function ccQueueKey(q){return q.key||((q.repo||'')+'#'+(q.number||''));}

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
  // Item count badge, same style as "My work"'s #work-count — kept in sync on
  // every render path. Populated FIRST so it stays set even if the container is absent.
  // (The interest re-float below only reorders ccQueue, never changes its length.)
  var qc=document.getElementById('queue-count');
  // Header tally: "N ready" plus the read-only cooldown / in-flight segments from
  // the fleet payload (#2649 companion). A segment is OMITTED when its count is 0
  // so the header stays compact; " · " (a middot) separates present segments.
  if(qc){
    var segs=[ccQueue.length+' ready'];
    if(ccCooldownCount>0)segs.push(ccCooldownCount+' in cooldown');
    if(ccInFlightCount>0)segs.push(ccInFlightCount+' in flight');
    if(ccHeldCount>0)segs.push(ccHeldCount+' on hold');
    qc.textContent=segs.join(' · ');
  }
  // No-blink guard: if a per-row ⋯ menu / "Move to #" dialog is OPEN and this is a
  // POLL re-render (flip is falsy — NOT a drag/move, which the operator initiated),
  // SKIP the row-list rebuild. A full innerHTML rebuild would destroy the open menu
  // mid-interaction; the queue data has not meaningfully changed under the operator's
  // hands. We already updated the header tally above so counts stay live; we mark the
  // render DEFERRED so ccCloseQueueMenus replays it the moment the menu closes.
  if(!flip&&document.querySelector('.cc-q-menu.open')){ccQueueRenderDeferred=true;return;}
  // Resume-all affordance (#queue-hold): a single "Resume all (H)" button next to the
  // header, shown ONLY to an owner/read-write viewer (adminEnabled) and ONLY when at
  // least one issue is on hold. Kept in sync with the tally above on every render.
  ccRenderResumeAll();
  // Label-affinity (#2637): re-tag + float this viewer's interested items. No-op
  // when no interests are set (order preserved); skipped mid-drag so an operator
  // reorder is not fought by an interest re-float. See ccApplyInterestsToQueue.
  if(!flip){try{ccApplyInterestsToQueue();}catch(e){}}
  var el=document.getElementById('cc-queue');if(!el)return;
  // reload bridge — overwritten by the next poll, including to empty.
  ccOpsCacheWrite(OPS_CACHE_QUEUE_KEY,ccQueue);
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
  if(!ccQueue.length){el.innerHTML='<div class="ops-empty">No work waiting &mdash; the backlog is clear or everything is in flight.</div>';ccUpdateFilterNote(0,0);ccKnownQueueKeys={};return;}
  var shown=0,total=ccQueue.length;
  // Render over the FULL model, tagging each row with its TRUE position (i) so the
  // shown index and the move-to menu reflect the real queue position even while a
  // search filter hides other rows. Filtered-out rows are simply skipped from the
  // HTML — the model is untouched, so a subsequent action still targets the right
  // qkey in the full order. Drag-reorder is DISABLED while a filter is active (a
  // drag over a partial list would be ambiguous); the ⋯ menu is the filtered path.
  var filtering=!!ccQueueSearch;
  // nextQueueKeys collects the keys present in the FULL model this render (not just
  // the painted subset) so the NEXT render pops-in only genuinely-new arrivals and a
  // filter toggle never re-animates existing rows. Mirrors ccKnownClankers.
  var nextQueueKeys={};
  for(var qk=0;qk<ccQueue.length;qk++){nextQueueKeys[ccQueueKey(ccQueue[qk])]=true;}
  el.innerHTML=ccQueue.map(function(q,i){
    if(!ccQueueMatches(q))return '';
    shown++;
    // Enter pop-in ONLY for an item absent from the previous render — so existing
    // rows never re-animate on a poll (the "blink"). A drag/move FLIP re-render
    // (flip truthy) must not pop-in either: the glide is the animation there, so we
    // suppress the enter class whenever flip is set.
    var qkey=ccQueueKey(q);
    var isNewQ=!flip&&qkey&&!ccKnownQueueKeys[qkey];
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
    // Held (#queue-hold): the server tags a manually-parked issue with held:true. It
    // stays VISIBLE in the queue (never hidden) but rendered greyed with an "on hold"
    // badge so the operator can see and Resume it. The ⋯ menu shows Resume for it.
    var isHeld=!!q.held;
    var menu=adminEnabled?ccQueueMenuHTML(ccQueueKey(q),i,total,isHeld):'';
    // Label-affinity (#2637): the server sets matches_interest per VIEWER when one
    // of the issue's labels matches a label this contributor subscribed to. Tag the
    // row so CSS can highlight it; a small "for you" pill makes the reason explicit.
    var mine=!!q.matches_interest;
    var mineCls=mine?' cc-q-mine':'';
    var mineTag=mine?'<span class="cc-q-mine-tag" title="Matches one of your label interests">for you</span>':'';
    var heldCls=isHeld?' cc-q-held':'';
    // On-hold badge tooltip (#queue-hold-reason): when the operator attached a note,
    // show "On hold — <reason>"; otherwise fall back to the generic text. esc() guards
    // the operator-supplied reason so it can never inject markup into the title attr.
    var heldReason=(q.held_reason||'').toString();
    var heldTitle=heldReason?('On hold — '+heldReason):'On hold — parked by the operator; not offered until resumed';
    var heldTag=isHeld?'<span class="cc-q-held-tag" title="'+esc(heldTitle)+'">&#x23F8; on hold</span>':'';
    // PR→issue badge (#2612 part c): if the triage poll resolved a fixing PR for
    // this issue (open/merged), show a small link. Absent until ccTriagePoll runs,
    // and simply omitted when no PR is linked — never blocks the queue render.
    // PR links are a GitHub-only observer: an item with no issue number has no
    // GitHub issue for a PR to close, so it gets no badge rather than a lookup
    // on a fabricated key (kubestellar/hive#4245).
    var prBadge=q.number?ccPRBadgeHTML((q.repo||'')+'#'+q.number):'';
    var enterCls=isNewQ?' cc-q-enter':'';
    return '<div class="cc-q-item'+mineCls+heldCls+enterCls+'"'+(canDrag?' draggable="true"':'')+' data-qkey="'+esc(ccQueueKey(q))+'">'+grip+'<span class="cc-q-idx">'+(i+1)+'</span>'+
      '<div class="cc-q-body"><div class="cc-q-repo">'+ccIssueLinkHTML(q,ccQueueKey(q))+mineTag+heldTag+'</div>'+
      '<div class="cc-q-title" title="'+esc(q.title||'')+'">'+esc(q.title||'(untitled)')+'</div>'+labels+prBadge+'</div>'+next+menu+'</div>';
  }).join('');
  // Adopt the freshly-painted key set so the NEXT render only pops-in new arrivals.
  ccKnownQueueKeys=nextQueueKeys;
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
// ── Triage ladder (#2612 parts b + c) ─────────────────────────────────────────
// ccPRLinks maps "repo#number" -> {number,url,state} for issues whose fixing PR the
// triage endpoint resolved (open/merged). Populated by ccTriagePoll; consumed by
// the queue-row badge and the triage groups. Empty until the first poll lands, so
// the queue renders immediately without any PR data.
var ccPRLinks={};
// ccPRBadgeHTML returns the small "PR #NNN (open|merged)" link chip for an issue
// key, or '' when no PR is linked. esc()-guarded; opens the PR on GitHub.
function ccPRBadgeHTML(key){
  var pr=ccPRLinks[key];
  if(!pr||!pr.number)return '';
  var cls=(pr.state==='merged')?'pr-merged':'pr-open';
  var label='PR #'+pr.number+' ('+(pr.state==='merged'?'merged':'open')+')';
  var href=pr.url||'';
  return '<a class="cc-pr-badge '+cls+'" href="'+esc(href)+'" target="_blank" rel="noopener noreferrer" title="'+esc(label)+'">'+esc(label)+'</a>';
}
// CC_TRIAGE_LEVELS pins the ladder's render order + display labels client-side,
// mirroring triageLevelOrder/triageLevelLabel on the server so the chips/groups
// render in lifecycle order even if the JSON key order ever changed.
var CC_TRIAGE_LEVELS=[['triaging','Triaging'],['ready','Ready to implement'],['implementing','Implementing'],['reviewing','Reviewing'],['closed','Closed']];
// ccTriagePoll fetches the live triage snapshot and renders the ladder + groups,
// then refreshes ccPRLinks and re-renders the queue so PR badges appear on queue
// rows too. Best-effort: any failure leaves the "Loading…"/prior state and is
// retried on the next tab open; it never throws into the ops bootstrap.
function ccTriagePoll(){
  fetch('/api/contribute/triage',{headers:{'Accept':'application/json'}})
    .then(function(r){return r.ok?r.json():null;})
    .then(function(d){if(d)ccRenderTriage(d);})
    .catch(function(){/* degrade silently — the panel keeps its last state */});
}
// ccRenderTriage paints the ladder summary chips + the per-level issue groups, and
// harvests the PR links into ccPRLinks so the queue rows can show badges.
function ccRenderTriage(snap){
  var groups=(snap&&snap.groups)||[];
  // Index groups by level for ordered lookup.
  var byLevel={};for(var i=0;i<groups.length;i++){byLevel[groups[i].level]=groups[i];}
  // Total count badge.
  var totEl=document.getElementById('cc-triage-total');
  if(totEl)totEl.textContent=(snap&&snap.total!=null?snap.total:0)+' issues';
  // Ladder chips.
  var ladder=document.getElementById('cc-triage-ladder');
  if(ladder){
    ladder.innerHTML=CC_TRIAGE_LEVELS.map(function(lv){
      var g=byLevel[lv[0]]||{count:0};
      return '<span class="cc-triage-chip lv-'+lv[0]+'"><span class="cc-tl-dot"></span><span class="cc-tl-n">'+(g.count||0)+'</span><span class="cc-tl-lbl">'+esc(lv[1])+'</span></span>';
    }).join('');
  }
  // Per-level groups with their issue lists.
  var wrap=document.getElementById('cc-triage-groups');
  if(wrap){
    ccPRLinks={};
    wrap.innerHTML=CC_TRIAGE_LEVELS.map(function(lv){
      var g=byLevel[lv[0]]||{count:0,issues:[]};
      var issues=g.issues||[];
      var rows=issues.map(function(it){
        var key=(it.repo||'')+'#'+(it.number||'');
        if(it.pr&&it.pr.number){ccPRLinks[key]=it.pr;}
        var repoLbl=esc((it.repo||'')+'#'+(it.number||''));
        var link=it.url?('<a class="cc-issue-link" href="'+esc(it.url)+'" target="_blank" rel="noopener noreferrer">'+repoLbl+'</a>'):repoLbl;
        var badge=(it.pr&&it.pr.number)?ccPRBadgeHTML(key):'';
        return '<div class="cc-tg-item"><div class="cc-tg-body"><div class="cc-tg-repo">'+link+'</div><div class="cc-tg-title" title="'+esc(it.title||'')+'">'+esc(it.title||'(untitled)')+'</div></div>'+badge+'</div>';
      }).join('');
      var body=issues.length?rows:'<div class="cc-tg-empty">None right now.</div>';
      return '<div class="cc-tg lv-'+lv[0]+'"><div class="cc-tg-head"><span class="cc-tg-dot"></span>'+esc(lv[1])+'<span class="cc-tg-count">'+(g.count||0)+'</span></div>'+body+'</div>';
    }).join('');
  }
  // Now that ccPRLinks is populated, re-render the queue so its rows pick up the
  // PR badges too (the queue endpoint intentionally does not resolve PR links).
  try{if(typeof ccRenderQueue==='function')ccRenderQueue();}catch(e){}
}
// ccQueueMenuHTML renders the per-row ⋯ menu markup (owner/read-write only). pos is
// the row's ZERO-based position in the full queue; total is the queue length. The
// move-to-position input is pre-filled with the row's current 1-based position.
function ccQueueMenuHTML(key,pos,total,isHeld){
  var atTop=(pos===0);
  // A held item is at the bottom of the visible queue by construction (held rows
  // trail all offerable ones), and Move-to-bottom on it is a no-op anyway; the
  // atBottom guard below disables it whenever pos is the last slot.
  var atBottom=(pos===total-1);
  // Hold/Resume toggles the persistent operator hold. "Resume" is shown when the
  // item is already held (play glyph), "Hold" otherwise (pause glyph). data-act
  // carries the CURRENT held state so the click handler knows which way to flip.
  var holdLabel=isHeld?'Resume':'Hold';
  var holdIcon=isHeld?'&#x25B6;':'&#x23F8;'; // ▶ resume / ⏸ hold
  // Optional hold reason (#queue-hold-reason): an inline note field shown ONLY when the
  // item is NOT yet held (a reason is attached when parking). The Hold click reads this
  // input's value and passes it to ccToggleHold. Absent for a held item (Resume needs
  // no note). Uses a distinct id ('hr-'+key) so it never collides with the mover input.
  var reasonRow=isHeld?'':(
    '<div class="cc-q-holdreason">'+
      '<input type="text" id="hr-'+esc(key)+'" class="cc-q-holdreason-input" maxlength="200" placeholder="Optional hold reason&hellip;" data-qkey="'+esc(key)+'" aria-label="Optional hold reason" autocomplete="off">'+
    '</div>');
  return '<span class="cc-q-menu-wrap">'+
    '<button type="button" class="cc-q-menu-btn" aria-haspopup="true" aria-expanded="false" title="More actions" data-qkey="'+esc(key)+'">&#x22EF;</button>'+
    '<div class="cc-q-menu" role="menu">'+
      '<button type="button" class="cc-q-act" role="menuitem" data-act="top" data-qkey="'+esc(key)+'"'+(atTop?' disabled style="opacity:.5;cursor:default"':'')+'><span class="cc-q-menu-ic">&#x2B06;</span>Move to top</button>'+
      '<button type="button" class="cc-q-act" role="menuitem" data-act="bottom" data-qkey="'+esc(key)+'"'+(atBottom?' disabled style="opacity:.5;cursor:default"':'')+'><span class="cc-q-menu-ic">&#x2B07;</span>Move to bottom</button>'+
      '<div class="cc-q-menu-sep"></div>'+
      '<button type="button" class="cc-q-act" role="menuitem" data-act="hold" data-held="'+(isHeld?'1':'0')+'" data-qkey="'+esc(key)+'"><span class="cc-q-menu-ic">'+holdIcon+'</span>'+holdLabel+'</button>'+
      reasonRow+
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
  for(var i=0;i<open.length;i++){open[i].classList.remove('open');}
  var btns=document.querySelectorAll('.cc-q-menu-btn[aria-expanded=true]');
  for(var j=0;j<btns.length;j++)btns[j].setAttribute('aria-expanded','false');
  // No-blink guard companion: a poll re-render that arrived while a menu was open was
  // SKIPPED (ccQueueRenderDeferred). Now that no menu is open, catch the queue up to
  // the latest data with a single plain (non-flip) re-render.
  if(ccQueueRenderDeferred){ccQueueRenderDeferred=false;try{ccRenderQueue();}catch(e){}}
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
      if(!isOpen){
        // Position with viewport coordinates BEFORE it paints so the menu escapes
        // the scrolling .cc-queue overflow clip and flips/clamps when near the
        // viewport or visible queue-panel bottom.
        menu.classList.add('open');
        ccPlaceFixedPopover(btn,menu,{align:'right',gap:6,fallbackWidth:220,fallbackHeight:220,boundary:btn.closest('.cc-queue')});
        btn.setAttribute('aria-expanded','true');
      }
    });
  })(btns[i]);}
  var acts=root.querySelectorAll('.cc-q-act[data-act=top]');
  for(var a=0;a<acts.length;a++){(function(act){
    act.addEventListener('click',function(e){e.stopPropagation();if(act.disabled)return;ccCloseQueueMenus();ccMoveToTop(act.getAttribute('data-qkey'));});
  })(acts[a]);}
  var bots=root.querySelectorAll('.cc-q-act[data-act=bottom]');
  for(var b2=0;b2<bots.length;b2++){(function(act){
    act.addEventListener('click',function(e){e.stopPropagation();if(act.disabled)return;ccCloseQueueMenus();ccMoveToBottom(act.getAttribute('data-qkey'));});
  })(bots[b2]);}
  var holds=root.querySelectorAll('.cc-q-act[data-act=hold]');
  for(var h2=0;h2<holds.length;h2++){(function(act){
    act.addEventListener('click',function(e){
      e.stopPropagation();if(act.disabled)return;
      var key=act.getAttribute('data-qkey');
      var willHold=act.getAttribute('data-held')!=='1';
      // Read the optional inline reason (present only on the not-yet-held menu) BEFORE
      // closing the menu tears the input out of the DOM. Ignored on resume (willHold=false).
      var reason='';
      if(willHold){var ri=root.querySelector('#'+cssEscRaw('hr-'+key));if(ri)reason=ri.value||'';}
      ccCloseQueueMenus();
      ccToggleHold(key,willHold,reason);
    });
  })(holds[h2]);}
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
  return cssEscRaw('mv-'+id);
}
// cssEscRaw escapes an ALREADY-PREFIXED id (e.g. 'hr-owner/repo#1') for a
// querySelector '#'+... lookup. Same escape as cssEscId but without the baked-in
// 'mv-' prefix, so callers that use a different prefix (the hold-reason input's
// 'hr-') get a correct selector instead of a doubled 'mv-' that never matches.
function cssEscRaw(raw){
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
function ccMoveToBottom(key){ccMoveToPosition(key,ccQueue.length);}
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
// ccToggleHold parks (held=true) or resumes (held=false) one ready-work issue via
// the authenticated POST /api/contribute/queue/hold endpoint. A held issue is never
// offered until resumed — a persistent operator hold, distinct from cooldown. On
// success it re-fetches the queue so the held/offerable split (which the SERVER
// computes) is reflected: a newly-held row re-appears greyed at the bottom, a
// resumed row rejoins the offerable list. Owner/read-write only; a read viewer 403s
// here, but the Hold/Resume action is never rendered for them (adminEnabled gate).
function ccToggleHold(key,held,reason){
  if(!key)return;
  // reason is OPTIONAL and only meaningful on hold=true; the server ignores it on
  // resume. Trimmed here so a whitespace-only note is treated as "no reason".
  var body={key:key,held:!!held};
  if(held&&reason&&reason.trim())body.reason=reason.trim();
  fetch('/api/contribute/queue/hold',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
    .then(function(r){if(!r.ok)throw new Error('http '+r.status);return r.json();})
    .then(function(){
      // Re-hydrate from the server so held tagging + offer-eligibility are authoritative.
      return fetch('/api/contribute/queue').then(function(r){return r.json();});
    })
    .then(function(d){if(d&&d.queue){ccQueue=d.queue.slice();ccRenderQueue();}})
    .catch(function(){/* a read viewer would 403; the UI never shows the action to them */});
}
// ccRenderResumeAll shows/hides the header "Resume all" button. It is visible ONLY
// when the viewer is owner/read-write (adminEnabled) AND at least one issue is on
// hold (ccHeldCount>0); otherwise it is hidden. The label carries the live count so
// the operator sees how many resuming will clear. Called on every ccRenderQueue.
function ccRenderResumeAll(){
  var btn=document.getElementById('queue-resume-all-btn');if(!btn)return;
  if(adminEnabled&&ccHeldCount>0){
    btn.style.display='';
    btn.textContent='▶ Resume all ('+ccHeldCount+')'; // ▶ Resume all (N)
  }else{
    btn.style.display='none';
  }
}
// ccResumeAll bulk-clears the ENTIRE operator hold set via POST
// /api/contribute/queue/hold/clear, after a themed confirm (never native confirm).
// On success it re-fetches the queue so every previously-held row rejoins the
// offerable list. Owner/read-write only; a read viewer 403s, but the button is never
// shown to them (adminEnabled gate in ccRenderResumeAll).
function ccResumeAll(){
  if(ccHeldCount<=0)return;
  adminConfirm('Resume all held issues','Resume every issue currently on hold ('+ccHeldCount+') so they can be offered again. This clears the entire operator hold set at once. Individual holds can be re-applied afterwards from each row&rsquo;s ⋯ menu.','Resume all',function(){
    fetch('/api/contribute/queue/hold/clear',{method:'POST',headers:{'Content-Type':'application/json'}})
      .then(function(r){if(!r.ok)throw new Error('http '+r.status);return r.json();})
      .then(function(){
        return fetch('/api/contribute/queue').then(function(r){return r.json();});
      })
      .then(function(d){if(d&&d.queue){ccQueue=d.queue.slice();ccRenderQueue();}toast('Resumed all held issues',true);})
      .catch(function(){toast('Could not resume all held issues',false);});
  });
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

// ── Ops panel reload-bridge cache ───────────────────────────────────────────────
// On a page refresh the ready-work queue, opportunistic work, and my-work panels
// sit on their "Loading…" placeholders until the first poll/SSE frame returns,
// which reads as a flash of empty. To bridge that gap we mirror each panel's DATA
// (the arrays, never rendered HTML) into localStorage as it renders and hydrate
// from it on load. The cache is ONLY a reload bridge: the very next successful
// poll overwrites both the DOM and the cache — INCLUDING overwriting to a
// genuinely-empty result — so stale work can never persist. Fresh window is short
// (OPS_CACHE_TTL_MS) so a long-closed tab does not resurrect ancient state.
var OPS_CACHE_QUEUE_KEY='hive.ops.cache.queue';
var OPS_CACHE_WORK_KEY='hive.ops.cache.work';
var OPS_CACHE_OPP_KEY='hive.ops.cache.opp';
var OPS_CACHE_TTL_MS=5*60*1000; // 5 minutes: the cache is a reload bridge, not a store
// ccOpsCacheWrite stores an array under key with a timestamp. Guarded like the
// rail helpers: private mode / quota errors degrade silently. Called on every
// render, so an empty array is persisted too (the next poll's empty overwrites the
// previous non-empty cache — no phantom work).
function ccOpsCacheWrite(key,arr){
  try{localStorage.setItem(key,JSON.stringify({t:new Date().getTime(),d:arr||[]}));}catch(e){}
}
// ccOpsCacheRead returns the cached array if present and fresher than the TTL,
// else null. Any parse/storage error yields null (degrade to placeholders).
function ccOpsCacheRead(key){
  try{
    var raw=localStorage.getItem(key);if(!raw)return null;
    var o=JSON.parse(raw);
    if(!o||typeof o.t!=='number'||!o.d)return null;
    if((new Date().getTime()-o.t)>OPS_CACHE_TTL_MS)return null;
    return o.d;
  }catch(e){return null;}
}
// ccHydrateOpsFromCache paints the three panels from the reload-bridge cache
// BEFORE the first poll returns, so a refresh shows the previous content instead
// of an empty flash. It sets the same state vars the live path uses (ccQueue /
// lastWork / ccOppItems) then calls the existing render fns, so the next poll
// overwrites them cleanly (including to empty). Every step is guarded so a bad
// cache entry can never block the live wiring that follows in ccStart().
function ccHydrateOpsFromCache(){
  try{
    var q=ccOpsCacheRead(OPS_CACHE_QUEUE_KEY);
    if(q&&q.length){ccQueue=q.slice();ccRenderQueue();}
  }catch(e){}
  try{
    var w=ccOpsCacheRead(OPS_CACHE_WORK_KEY);
    if(w&&w.length){renderWork(w);}
  }catch(e){}
  try{
    var o=ccOpsCacheRead(OPS_CACHE_OPP_KEY);
    if(o&&o.length){ccOppItems=o.slice();ccRenderOpportunistic();}
  }catch(e){}
}
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
// ccFormatLoadout renders one contributor's loadout suffix -- the SHARED
// formatter for both Live Activity views (the Operations rail via ccNarrate and
// the Onboarding feed below), so the two cannot drift apart again.
//
//   via codex CLI with gpt-5.6-terra (high)   -- everything known
//   via codex CLI with gpt-5.6-terra          -- no effort
//   via codex CLI (high)                      -- effort but no model
//   via codex CLI                             -- backend only
//   (empty)                                   -- no backend: nothing to say
//
// Effort is rendered INDEPENDENTLY of model: a contributor can set
// AGENT_REASONING_EFFORT without AGENT_MODEL (codex then runs its default model
// at that effort), and nesting the effort inside the model branch would drop
// exactly the value this view exists to surface. Every branch is guarded, so a
// bare '()' or a dangling 'with' can never be emitted.
function ccFormatLoadout(e,cls){
  if(!e||!e.cli)return '';
  var m=e.model?esc(e.model):'';
  var ef=e.effort?esc(e.effort):'';
  var text='via '+esc(e.cli)+' CLI';
  if(m)text+=' with '+m;
  if(ef)text+=' ('+ef+')';
  return ' <span class="'+(cls||'feed-cli')+'">'+text+'</span>';
}
window.ccFormatLoadout=ccFormatLoadout;
window.esc=esc;

function ccNarrate(e){
  var icons={joined:'🟢',left:'⚪',"picked up":'🔧',completed:'✅',failed:'❌',promoted:'🎖️'};
  var ic=icons[e.action]||'⚡';
  var who='<span class="who">'+esc(e.username||'someone')+'</span>';
  var ref=e.task?' <span class="ref">'+esc(e.task)+'</span>':'';
  var pickedRef=e.task?' '+ccTaskRefLink(e.task):'';
  var loadout=ccFormatLoadout(e,'ref');
  var body;
  switch(e.action){
    case 'joined': body=who+' entered the hive'; break;
    case 'left': body=who+' left the hive'; break;
    case 'picked up': body=who+' grabbed'+pickedRef; break;
    case 'completed': body=who+' completed'+(e.task?' '+ccTaskRefLink(e.task):''); break;
    case 'failed': body=who+' hit a snag on'+(e.task?' '+ccTaskRefLink(e.task):''); break;
    case 'promoted': body=who+' was promoted to <b>'+esc(e.task||e.role||'contributor')+'</b>'; break;
    default: body=who+' '+esc(e.action)+ref;
  }
  return {ic:ic,body:body+loadout,ts:e.timestamp};
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
    if(!d)return;
    // Adopt the viewer's interests the personalised endpoint echoes (#2637) so the
    // editor + highlights stay current even without a separate fetch.
    ccApplyInterestsFromResponse(d.interests);
    if(d.queue){ccQueue=d.queue.slice();ccRenderQueue();}
  }).catch(function(){});
  ccQueuePollTimer=setTimeout(ccQueuePoll,6000);
}
function ccStopFallback(){if(ccQueuePollTimer){clearTimeout(ccQueuePollTimer);ccQueuePollTimer=null;}}
function ccStart(){
  if(ccStarted)return;ccStarted=true;
  // Paint the queue / opportunistic / my-work panels from the reload-bridge cache
  // FIRST, before any poll returns, so a refresh shows the previous content instead
  // of an empty flash. The next successful poll overwrites both DOM and cache
  // (including to empty), so this is purely a bridge across the reload gap.
  try{ccHydrateOpsFromCache();}catch(e){console.error('ops cache hydrate failed',e);}
  // Seed + poll the Live Activity rail from the RELIABLE polling endpoint (the same
  // source Onboarding uses) so the rail shows the backlog immediately and stays
  // current even when the SSE stream delivers nothing (hosted spokes). SSE, when it
  // works, layers live updates on top via the shared, deduped activity store.
  try{ccPollActivity();}catch(e){console.error('activity poll init failed',e);}
  // Wire the playlist-style search filter and start the (light) opportunistic-work
  // poll. Both are independent of the SSE lifecycle: a throw here must not block the
  // live queue stream, so each is guarded.
  try{ccInitQueueSearch();}catch(e){console.error('queue search init failed',e);}
  try{ccInitInterestsEditor();}catch(e){console.error('interests editor init failed',e);}
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
  // reload bridge — overwritten by the next poll, including to empty.
  ccOpsCacheWrite(OPS_CACHE_OPP_KEY,ccOppItems);
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
    // Your usage trend (#persistent-history): the viewer's own per-hour completions
    // over the last 7 days, hydrated by ccMetricsPoll once metrics + identity load.
    // Only on the end-of-queue variant (variant==='') so the id stays unique — the
    // Me-card renders the same quota widget with a different variant.
    ((variant||'')===''?'<div class="quota__sub" style="text-align:right;margin-top:2px"><span class="spark" id="spark-quota" title="Your completions per hour, last 7 days"></span></div>':'')+
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
window.addEventListener('resize',function(){ccCloseQueueMenus();});
window.addEventListener('scroll',function(){ccCloseQueueMenus();},true);
})();
</script>
<script>
let prevCount=0;
// ── Cross-block helper resolution ────────────────────────────────────────────
// esc() and ccFormatLoadout() are declared inside the OPERATIONS script block's
// IIFE, so they are not in scope here; they reach this block only through the
// window.* republish at the end of that IIFE. A BARE cross-IIFE reference is
// exactly the shape that "threw ReferenceError on every dossier render" (see the
// ccProjectName note at the top of that IIFE), and it fails closed: if that IIFE
// ever throws before the republish -- it has regressed that way three times
// (#2603/#2604/#2606) -- the reference throws, poll()'s catch swallows it, and
// this feed silently freezes on a 3s loop with nothing in the console.
// Resolve through window with REAL local fallbacks instead, so the worst case is
// a feed that loses the loadout suffix rather than one that stops updating. The
// esc fallback is a genuine escaper, never a pass-through: falling back to no
// escaping would turn a rendering failure into an injection bug.
const feedEsc=(typeof window!=='undefined'&&window.esc)||function(s){return (s==null?'':String(s))
  .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')
  .replace(/"/g,'&quot;').replace(/'/g,'&#39;');};
const feedLoadout=(typeof window!=='undefined'&&window.ccFormatLoadout)||function(){return '';};
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
const taskInfo=e.task?' <span class="feed-cli">'+feedEsc(e.task)+'</span>':'';
const role=e.role?' as <span class="feed-role">'+feedEsc(e.role)+'</span>':'';
const cliModel=feedLoadout(e,'feed-cli');
return '<div class="feed-entry"'+(i===0&&isNew?' style="background:rgba(63,185,80,.08)"':'')+'>'+
'<div class="feed-text">'+icon+' <b>'+feedEsc(e.username)+'</b> '+verb+taskInfo+role+cliModel+'</div>'+
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
<div style="margin-top:40px;padding:16px 0;border-top:1px solid var(--cc-border);font-size:.75rem;color:var(--cc-muted);display:flex;align-items:center;gap:8px">
  <span id="hive-version">loading...</span>
</div>
<script>
fetch('/api/version').then(function(r){return r.json()}).then(function(d){
  var el=document.getElementById('hive-version');
  var dot=d.behind?'\u{1F7E1}':'\u{1F7E2}';
  el.innerHTML=dot+' Hive v'+d.version+' ('+d.short+')' + (d.behind?' · <span style="color:var(--cc-amber)">update available</span>':' · up to date');
}).catch(function(){});
</script>
</body></html>`, "{{HIVE_BRANCH}}", upstreamBranch()), projectName, michromaFontFaceCSS, customStyleHeadHTML, projectName, len(profiles), tierBoxes.String(), hubURL, hubURLJS, projectNameJS, tierTableRows, customStyleNoticeHTML)
	applyDocumentScriptSrcElem(w, page.Bytes())
	w.Write(page.Bytes())
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
		// SECURITY: this endpoint is unauthenticated (username is self-asserted),
		// so it must NEVER reissue and return an existing contributor's token —
		// that was an account-takeover primitive (POST any known username with
		// force:true → receive their live token). Reissuing requires proving you
		// own the GitHub account, which POST /api/contribute/reissue-token does
		// (it validates a GitHub token). The legacy force:true flag is ignored.
		_ = req.Force
		jsonResponse(w, map[string]string{
			"contributor_id": existing.ContributorID,
			"message":        "Already registered — to rotate your token, POST /api/contribute/reissue-token with Authorization: Bearer <your GitHub token>",
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
		// A redeemed invite is the first real relationship between two
		// contributors, so it is also the first entry in both dossiers'
		// Collaborators zone. Recorded after the profile is saved below.
		defer recordCollaboration(username, inviter, collabHowInvite, time.Now())
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
// profile, and requires their trust tier to be trusted, merger, or advisor — a newcomer,
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
		jsonError(w, "Only trusted, merger, or advisor contributors can invite others. Keep shipping to earn trust.", http.StatusForbidden)
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
	AgentRoleGrantable   []string `json:"agent_role_grantable_roles,omitempty"`
	AgentRoleAssignable  []string `json:"agent_role_assignable_roles,omitempty"`
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
	p.AgentRoleGrantable = contributorAgentRoleGrantableRoles(s.deps.Config)
	p.AgentRoleAssignable = contributorAgentRoleAssignableRoles(s.deps.Config)
	return p
}

func contributorAgentRoleAssignableRoles(cfg *config.Config) []string {
	roles := []string{"scanner", "quality", "outreach"}
	if cfg != nil {
		set := cfg.Hub.ContributeDelegatableRoleSet()
		for role := range roleClaimNeedsGrant {
			role = normalizeAgentRole(role)
			if set[role] {
				roles = append(roles, role)
			}
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		role = normalizeAgentRole(role)
		if role == "" || role == "supervisor" || seen[role] {
			continue
		}
		seen[role] = true
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func contributorAgentRoleGrantableRoles(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	set := cfg.Hub.ContributeDelegatableRoleSet()
	roles := make([]string, 0, len(set))
	for role := range set {
		role = normalizeAgentRole(role)
		if role == "" || role == "supervisor" || !roleClaimNeedsGrant[role] {
			continue
		}
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// handleContributeFleet serves the read-only fleet/work/policy snapshot that the
// Management & Operations tab hydrates. GET only, no side effects: it surfaces
// the hub's live connection state and the already-configured admission policy.
func (s *Server) handleContributeFleet(w http.ResponseWriter, r *http.Request) {
	snap := FleetSnapshot{Clankers: []FleetClanker{}, Work: []FleetWorkItem{}}
	// cooldownCount = completed issues still within their cooldown window (held out
	// of the queue); inFlightCount = issues currently held by a live connection.
	// Both are read-only tallies the queue header / Management tab surface (#2649
	// configurable cooldown).
	// heldCount = operator-held issues still in the actionable universe (would be
	// offerable but for the manual hold); the queue header surfaces it as "H on hold".
	cooldownCount, inFlightCount, heldCount := 0, 0, 0
	if s.contributeHub != nil {
		snap = s.contributeHub.FleetSnapshot()
		cooldownCount, inFlightCount = s.contributeHub.CooldownCounts()
		heldCount = s.contributeHub.HeldCount()
	}
	jsonResponse(w, map[string]any{
		"clankers":        snap.Clankers,
		"work":            snap.Work,
		"policy":          s.buildContributeAdmissionPolicy(),
		"cooldown_count":  cooldownCount,
		"in_flight_count": inFlightCount,
		"held_count":      heldCount,
	})
}

// handleContributeQueue serves the read-only ready-work QUEUE — the admissible
// issues waiting to be picked off, derived from the SAME ActionableIssues set
// selectTask offers from (see ReadyQueue). GET only, public, no side effects. It
// is both a JSON fallback for browsers without EventSource and the same payload
// the SSE "hello" frame carries, so the queue renders even if the stream drops.
func (s *Server) handleContributeQueue(w http.ResponseWriter, r *http.Request) {
	queue := []ReadyQueueItem{}
	resp := map[string]any{}
	if s.contributeHub != nil {
		// One snapshot serves the queue and — only when the convergence toggle
		// is in shadow mode (#4246, default off) — the additive withheld /
		// coverage diagnostics from the SAME sweep. With the toggle off the
		// response is exactly the pre-diagnostics payload.
		diag := s.convergenceDiagnosticsEnabled()
		snap := s.contributeHub.admissionQueueSnapshot(readyQueueDefaultLimit, diag)
		queue = snap.queue
		if diag {
			resp["withheld"] = snap.withheld
			resp["admission_coverage"] = snap.coverage
		}
	}
	resp["queue"] = queue
	// Label-affinity (#2637): if we can identify the viewer server-side and they
	// have declared label interests, personalise THIS response — tag matching
	// issues and float them to the front for them. Soft signal only: nothing is
	// filtered out, so a viewer with no interests (or none identifiable) gets the
	// exact shared queue. Resolved from session / X-Hive-User (never a client
	// param), so a viewer only ever personalises with their OWN interests.
	if username := s.resolveViewerUsername(r); username != "" {
		if profile := findContributor(username); profile != nil {
			personalizeQueueByInterests(queue, profile.LabelInterests)
			// Echo the viewer's own interests so the Operations tab can render the
			// editor pre-filled without a second round-trip.
			resp["interests"] = profile.LabelInterests
		}
	}
	jsonResponse(w, resp)
}

// maxLabelInterests caps how many label interests one contributor may declare. It
// is generous (a contributor could reasonably follow many hardware/area labels)
// yet bounds a hostile payload so a single profile file cannot be bloated. A
// submission over the cap is truncated, not rejected, so the save still succeeds.
const maxLabelInterests = 64

// maxLabelInterestLen bounds a single label string. GitHub labels are short; this
// is well above any real label yet stops a pathological entry from bloating the
// stored profile. Over-length entries are dropped.
const maxLabelInterestLen = 128

// handleContributeInterests is the contributor-owned read/write for their OWN
// label interests (#2637). GET returns the caller's current interests; PUT
// replaces them. Identity is resolved server-side (session / X-Hive-User / owner
// token / Bearer gh-token) via resolveContributeCaller — NEVER from the body — so
// a contributor can only ever read or write THEIR OWN interests, and an anonymous
// caller gets 401. This is a self-service PREFERENCE, not an operator control, so
// it deliberately does NOT require write-tier; any registered contributor may set
// what work they want surfaced to them.
func (s *Server) handleContributeInterests(w http.ResponseWriter, r *http.Request) {
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to set your label interests.", http.StatusUnauthorized)
		return
	}
	profile := findContributor(username)
	if profile == nil {
		jsonError(w, "You need a contributor profile on this hive before you can set label interests.", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodGet {
		interests := profile.LabelInterests
		if interests == nil {
			interests = []string{}
		}
		jsonResponse(w, map[string]any{"interests": interests})
		return
	}

	// PUT: replace the caller's interests with the submitted (sanitised) set.
	var body struct {
		Interests []string `json:"interests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	cleaned := sanitizeLabelInterests(body.Interests)
	profile.LabelInterests = cleaned
	if err := saveContributorProfile(profile); err != nil {
		s.logger.Error("failed to save contributor label interests", "username", username, "error", err)
		jsonError(w, "could not save label interests", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor label interests updated", "username", username, "count", len(cleaned))
	jsonResponse(w, map[string]any{"interests": cleaned})
}

// sanitizeLabelInterests normalises a submitted interest list: trims/lower-cases
// each entry (so matching is predictable and case-insensitive), drops blanks and
// over-length entries, de-duplicates while preserving first-seen order, and caps
// the total. It never errors — a hostile or messy payload is cleaned into a safe
// stored set rather than rejected.
func sanitizeLabelInterests(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		n := normalizeLabelInterest(raw)
		if n == "" || len(n) > maxLabelInterestLen {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
		if len(out) >= maxLabelInterests {
			break
		}
	}
	return out
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
var limitsTierOrder = []string{"newcomer", "contributor", "trusted", "merger", "advisor"}

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

// maxQueueHoldKeys caps how many issues the operator may hold at once. Mirrors
// maxQueueOrderKeys: generous (the whole visible queue could conceivably be
// parked) yet bounds a hostile payload so the hold set can neither bloat hive.yaml
// nor slow the per-selectTask membership lookup.
const maxQueueHoldKeys = 512

// maxQueueHoldReasonLen bounds the OPTIONAL operator note stored with a hold so a
// stored reason can never balloon hive.yaml. A hold reason is a short annotation
// (why this issue is parked), not free-form prose, so a compact cap is plenty; an
// over-long note is truncated rather than rejected (the hold itself must still succeed).
const maxQueueHoldReasonLen = 200

// handleContributeQueueHold toggles the OPERATOR HOLD on one ready-work issue.
// A held issue is parked INDEFINITELY — never offered — until the operator Resumes
// it; this is DISTINCT from the time-based cooldown, which self-clears. It is a
// CONTROL, so owner/read-write ONLY, gated exactly like handleContributeQueueOrder
// via requireContributorWrite (a read/anon caller gets 403). The hold set lives in
// Config.Hub.ContributeQueueHold and persists through the SAME refreshAndPersist
// path as ContributeQueueOrder, so it survives restart. Body:
// {"key":"owner/repo#number","held":true|false} — held=true adds the key, false
// removes it. The key MUST be the canonical "owner/repo#number" form (validated
// against the same queueOrderKeyPattern) so it matches selectTask's exclusion key
// exactly (the #2648 class of silent miss).
func (s *Server) handleContributeQueueHold(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var body struct {
		Key  string `json:"key"`
		Held bool   `json:"held"`
		// Reason is an OPTIONAL short operator note explaining WHY the issue is being
		// parked, surfaced in the on-hold badge tooltip. Ignored when held=false (the
		// reason is dropped alongside the key on resume). Empty means "no note" — the
		// badge falls back to its generic text, so holding without a reason is unchanged.
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" || !queueOrderKeyPattern.MatchString(key) {
		jsonError(w, "invalid issue key", http.StatusBadRequest)
		return
	}
	// Bound the note so a stored reason can never balloon the persisted config.
	reason := strings.TrimSpace(body.Reason)
	if len(reason) > maxQueueHoldReasonLen {
		reason = reason[:maxQueueHoldReasonLen]
	}
	// Rebuild the hold set: drop the target key (and any malformed/duplicate
	// stragglers) first, then re-add it when held=true. This keeps the stored list
	// well-formed and unique regardless of prior state, and makes the toggle
	// idempotent (holding an already-held issue, or resuming an already-free one, is
	// a clean no-op that still persists the canonical set).
	seen := make(map[string]struct{})
	cleaned := make([]string, 0, len(s.deps.Config.Hub.ContributeQueueHold)+1)
	for _, k := range s.deps.Config.Hub.ContributeQueueHold {
		k = strings.TrimSpace(k)
		if k == "" || k == key || !queueOrderKeyPattern.MatchString(k) {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		cleaned = append(cleaned, k)
	}
	if body.Held {
		if len(cleaned) >= maxQueueHoldKeys {
			jsonError(w, "too many held issues", http.StatusBadRequest)
			return
		}
		cleaned = append(cleaned, key)
	}
	s.deps.Config.Hub.ContributeQueueHold = cleaned
	// Maintain the OPTIONAL parallel reason map (#queue-hold-reason). On hold=true with
	// a non-empty note, store it under the canonical key; on hold=false (resume) or an
	// empty note, drop any prior entry. Prune to the current held set so a reason can
	// never outlive its hold. Built fresh each write (nil-safe) so it stays well-formed.
	reasons := pruneQueueHoldReasons(s.deps.Config.Hub.ContributeQueueHoldReasons, cleaned)
	if body.Held && reason != "" {
		reasons[key] = reason
	} else {
		delete(reasons, key)
	}
	if len(reasons) == 0 {
		reasons = nil // omitempty: no reasons => field absent, snapshot unchanged
	}
	s.deps.Config.Hub.ContributeQueueHoldReasons = reasons
	s.auditFromRequest(r, "contribute_queue_hold", auditDetail("key", key, "held", strconv.FormatBool(body.Held)), "")
	s.refreshAndPersist()
	s.logger.Info("contribute queue hold updated", "key", key, "held", body.Held, "total_held", len(cleaned), "has_reason", body.Held && reason != "")
	jsonResponse(w, map[string]any{"ok": true, "key": key, "held": body.Held, "hold": cleaned, "reason": reason})
}

// handleContributeQueueHoldClear RESUMES ALL held issues in one call: it drops the
// entire operator hold set (ContributeQueueHold) and its parallel reason map, then
// persists through the SAME refreshAndPersist path as the single-issue hold endpoint.
// This is the bulk companion to handleContributeQueueHold — same owner/read-write
// gate (requireContributorWrite; a read/anon caller gets 403), same persistence.
// Idempotent: clearing an already-empty set is a clean no-op that still persists.
func (s *Server) handleContributeQueueHoldClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	cleared := len(s.deps.Config.Hub.ContributeQueueHold)
	s.deps.Config.Hub.ContributeQueueHold = nil
	s.deps.Config.Hub.ContributeQueueHoldReasons = nil
	s.auditFromRequest(r, "contribute_queue_hold_clear", auditDetail("cleared", strconv.Itoa(cleared)), "")
	s.refreshAndPersist()
	s.logger.Info("contribute queue hold cleared (resume all)", "cleared", cleared)
	jsonResponse(w, map[string]any{"ok": true, "cleared": cleared, "hold": []string{}})
}

// pruneQueueHoldReasons returns a fresh copy of src keeping ONLY entries whose key is
// present in keep (the current held set). It is the invariant that keeps the parallel
// reason map from leaking a note for an issue that is no longer held. nil-safe: a nil
// src yields an empty (non-nil) map ready to write into.
func pruneQueueHoldReasons(src map[string]string, keep []string) map[string]string {
	held := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		held[k] = struct{}{}
	}
	out := make(map[string]string, len(keep))
	for k, v := range src {
		if _, ok := held[k]; ok && strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

// ── Contributor management ─────────────────────────────────────────────────

// requireContributorWrite enforces owner/read-write authorization on the
// contributor mutation endpoints (trust/revoke/delete) that the Management &
// Operations tab surfaces as admin controls.
//
// These handlers live under the /api/contributors/... path. Each mutation handler
// must enforce the write boundary itself, because these routes are otherwise only
// as protected as the surrounding auth layer — and on a direct OpenShift Route or
// in-cluster (no hub nginx, no NetworkPolicy) there is NO auth layer in front.
//
// SECURITY (C5): FAIL CLOSED. An absent/empty X-Hive-Role is treated as NOT
// authorized (deny), not as owner. The previous code defaulted an absent header to
// "owner" for "local/dev, no hub nginx" convenience — but that same absence is
// exactly what an anonymous caller hitting the pod directly (bypassing the hub
// nginx that would otherwise inject the header) presents. Combined with the
// prefix-match bug that exempted /api/contributors/... from authentication, an
// unauthenticated caller could promote/revoke/delete/requeue contributors. Only an
// explicit owner/read-write role may mutate; everything else (absent, "read", or
// any unrecognized value) is rejected. UI hiding on the ops tab is UX; this is the
// security boundary.
func (s *Server) requireContributorWrite(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role != config.RoleOwner && role != config.RoleReadWrite {
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
	if !requireOwnerRole(w, r) {
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
	validTiers := map[string]bool{"newcomer": true, "contributor": true, "trusted": true, "merger": true, "advisor": true, "revoked": true}
	if !validTiers[req.Tier] {
		jsonError(w, "Invalid tier", http.StatusBadRequest)
		return
	}
	p.TrustTier = req.Tier
	// H2: admin path — adminOverride lets this change a terminal (revoked) tier and
	// wins the CAS reconcile against any concurrent stale WS save. The write is
	// server-authoritative.
	if err := saveContributorProfileCAS(p, true); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	// H2: if this change revokes access, fence any live WebSocket sessions the
	// contributor holds so an in-flight connection cannot keep working (or keep
	// saving a stale "contributor" profile) after the revoke.
	if req.Tier == "revoked" && s.contributeHub != nil {
		s.contributeHub.DisconnectContributor(p.ContributorID, "contribution access revoked")
	}
	s.logger.Info("contributor tier changed", "username", p.GitHubUsername, "tier", req.Tier)
	jsonResponse(w, map[string]any{"ok": true, "trust_tier": req.Tier})
}

func (s *Server) handleContributorAgentRoleGrants(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
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
		AgentRoleGrants []string `json:"agent_role_grants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	grantable := contributorAgentRoleGrantableRoles(nil)
	if s.deps != nil {
		grantable = contributorAgentRoleGrantableRoles(s.deps.Config)
	}
	allowed := make(map[string]bool, len(grantable))
	for _, role := range grantable {
		allowed[role] = true
	}
	grants := make([]string, 0, len(req.AgentRoleGrants))
	for _, role := range req.AgentRoleGrants {
		role = normalizeAgentRole(role)
		if role == "" {
			continue
		}
		if !allowed[role] {
			jsonError(w, fmt.Sprintf("agent role %q is not a grantable delegated privileged role", role), http.StatusBadRequest)
			return
		}
		grants = append(grants, role)
	}
	grants = normalizeUniqueAgentRoles(grants)
	assigned := effectiveAssignedAgentRole(p.AssignedAgentRole)
	if roleClaimNeedsGrant[assigned] && !hasAgentRoleGrant(&ContributorProfile{AgentRoleGrants: grants}, assigned) {
		grants = normalizeUniqueAgentRoles(append(grants, assigned))
	}
	p.AgentRoleGrants = grants
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	if s.contributeHub != nil {
		s.contributeHub.SetContributorAgentRoleGrants(p.ContributorID, grants)
	}
	s.logger.Info("contributor agent-role grants changed", "username", p.GitHubUsername, "grants", strings.Join(grants, ","))
	jsonResponse(w, map[string]any{"ok": true, "agent_role_grants": grants, "grantable_roles": grantable})
}

// handleContributorAgentRole assigns a contributor's agent role.
//
// OWNER-ONLY (issue #3011, High). This was gated on requireContributorWrite,
// which admits any caller whose role is not "read" — so a read-write
// contributor could assign privileged agent roles. Its sibling
// handleContributorAgentRoleGrants was moved to requireOwnerRole by F16; this
// half of #3011 was left behind. Role assignment is an operator action.
//
// The handler also used to PRE-POPULATE its probe profile with the very grant
// it was about to check:
//
//	probeProfile := *p
//	if roleClaimNeedsGrant[role] && !hasAgentRoleGrant(&probeProfile, role) {
//	    probeProfile.AgentRoleGrants = append(probeProfile.AgentRoleGrants, role)
//	}
//
// which made roleClaimAllowed's "requires an operator grant" check pass
// trivially, and then wrote that grant to the REAL profile — auto-granting
// sec-check/architect/ci-maintainer as a side effect of assigning them. That
// is removed: the probe is now the unmodified profile, so a role needing a
// grant is REFUSED unless the target already holds it. Grant issuance stays a
// separate explicit owner action (handleContributorAgentRoleGrants).
//
// Assignment is additionally checked against the server-side assignable
// allowlist, so a role outside contributorAgentRoleAssignableRoles cannot be
// assigned even when roleClaimAllowed would tolerate it.
func (s *Server) handleContributorAgentRole(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
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
		AgentRole string `json:"agent_role"`
		Role      string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	role := normalizeAgentRole(req.AgentRole)
	if role == "" {
		role = normalizeAgentRole(req.Role)
	}
	if role == "" || role == "none" {
		p.AssignedAgentRole = "none"
	} else {
		// Server-side allowlist: only roles the hive actually exposes as
		// assignable may be assigned (issue #3011 recommendation 3).
		var allowCfg *config.Config
		if s.deps != nil {
			allowCfg = s.deps.Config
		}
		assignable := false
		for _, candidate := range contributorAgentRoleAssignableRoles(allowCfg) {
			if candidate == role {
				assignable = true
				break
			}
		}
		if !assignable {
			jsonError(w, fmt.Sprintf("agent role %q is not assignable", role), http.StatusBadRequest)
			return
		}

		// Probe with the profile AS IT IS. Pre-seeding the grant here was the
		// #3011 bypass: it made the grant check below tautological.
		probe := &ContributorConnection{profile: p}
		if s.contributeHub == nil {
			s.contributeHub = NewContributeWSHub(s.logger, s)
		}
		if ok, reason := s.contributeHub.roleClaimAllowed(probe, role); !ok {
			jsonError(w, reason, http.StatusBadRequest)
			return
		}
		// Belt and braces: roleClaimAllowed returns early for the no-config
		// hubs used by unit tests and legacy deployments, so re-assert the
		// grant requirement here unconditionally. No grant is ever WRITTEN by
		// this handler — that is handleContributorAgentRoleGrants' job.
		if roleClaimNeedsGrant[role] && !hasAgentRoleGrant(p, role) {
			jsonError(w, fmt.Sprintf("agent role %q requires an operator grant", role), http.StatusBadRequest)
			return
		}
		p.AssignedAgentRole = role
	}
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	if s.contributeHub != nil {
		s.contributeHub.SetAssignedAgentRole(p.ContributorID, p.AssignedAgentRole, p.AgentRoleGrants)
	}
	s.logger.Info("contributor assigned agent role changed", "username", p.GitHubUsername, "assigned_role", p.AssignedAgentRole)
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	jsonResponse(w, map[string]any{
		"ok":                  true,
		"assigned_agent_role": p.AssignedAgentRole,
		"effective_role":      effectiveAssignedAgentRole(p.AssignedAgentRole),
		"agent_role_grants":   p.AgentRoleGrants,
		"assignable_roles":    contributorAgentRoleAssignableRoles(cfg),
	})
}

func normalizeUniqueAgentRoles(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, role := range in {
		role = normalizeAgentRole(role)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleContributorRevoke(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TrustTier = "revoked"
	// H2: admin path — adminOverride wins the CAS reconcile so this revoke cannot be
	// silently clobbered by a concurrent stale WS save, and the tier is now
	// server-authoritative and terminal for any later non-admin write.
	if err := saveContributorProfileCAS(p, true); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	// H2: fence live sessions — close any WebSocket connections this contributor
	// holds so an in-flight session cannot keep working, keep minting credentials, or
	// keep saving a stale non-revoked profile after the revoke lands.
	if s.contributeHub != nil {
		s.contributeHub.DisconnectContributor(p.ContributorID, "contribution access revoked")
	}
	s.logger.Info("contributor revoked", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true})
}

// handleContributorRequeue is the operator YANK action — the manual release of a
// wedged clanker's in-flight task, repurposed (kubestellar/hive#2568 + the yank
// follow-up) to ALSO immediately reassign that clanker its next-priority item so it
// keeps working instead of idling. It is a CONTROL, so it is owner/read-write ONLY,
// enforced server-side by requireContributorWrite (a read/anon caller gets 403),
// exactly like trust/revoke/remove.
//
// It still reuses the SAME release+cooldown machinery the automatic disconnect-release
// (#2356/#2435) and ready-abandon (#2545) paths use — see
// ContributeWSHub.RequeueContributorTask — so the release can NOT recreate the
// duplicate-assignment race #2492/#2557 closed: the released issue books the same short
// failure cooldown and is therefore not instantly re-handed to a stale worker, and the
// connection's assignment generation is BUMPED so a stale worker's later completion is
// fenced out (the Gate).
//
// The YANK addition: after that release, the hub immediately calls selectTask for the
// SAME clanker and hands it its next-priority item (honouring the operator-pinned → own
// work → label-affinity → fewer-failures → rest order), and the just-released issue is
// briefly self-excluded from THIS clanker (yankSelfExcludeSeconds) so it moves to
// genuinely DIFFERENT work — while the released issue is immediately offerable to every
// OTHER contributor. When nothing else is admissible the clanker is simply released +
// idle (the old requeue-only outcome, now the fallback). The operator may pass a REASON
// (JSON body {"reason":...} or ?reason=), recorded in the audit + activity log and
// pushed to the still-connected worker on task_revoke. A contributor with no in-flight
// task is a 404 (nothing to release/reassign).
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
	// #2568: accept an optional operator reason from a JSON body or query param. Both
	// are optional — an empty reason falls back to the hub's default recovery label —
	// so existing callers that POST no body keep working unchanged.
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if reason == "" && r.Body != nil {
		var body struct {
			Reason string `json:"reason"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			reason = strings.TrimSpace(body.Reason)
		}
	}
	// Key the live release+reassign by the registered ContributorID (what the ops tab
	// passes), matching how the hub tracks connections. GitHubUsername is only for logs.
	released, assigned := s.contributeHub.RequeueContributorTask(p.ContributorID, reason)
	if released == 0 {
		jsonError(w, "That contributor has no in-flight task to yank.", http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "contributor_requeue", auditDetail("username", p.GitHubUsername, "reason", reason), "")
	// Report whether the clanker was reassigned (and to what) so the ops tab can show
	// the clanker was moved to different work. reassigned==false means it was released
	// but nothing else was admissible right now — a legitimate "released, now idle" state.
	resp := map[string]any{"ok": true, "released": released, "reassigned": false}
	if assigned != nil && assigned.Type == "task_assign" {
		resp["reassigned"] = true
		resp["assigned_repo"] = assigned.Repo
		resp["assigned_number"] = assigned.Number
		resp["assigned_title"] = assigned.Title
	}
	s.logger.Info("contributor task yanked by operator", "username", p.GitHubUsername, "sessions_released", released, "reassigned", resp["reassigned"], "reason", reason)
	jsonResponse(w, resp)
}

func (s *Server) handleContributorDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
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
	// ActiveContributorNames optionally names the contributors present on this
	// hive. It is what makes an honest "Theaters of Operation" possible: without
	// it a dossier cannot tell the hives a person actually works on from the
	// hives that merely exist. Optional and backward-compatible — a hive that
	// does not report names is simply never listed as anyone's theatre, which is
	// the correct conservative default.
	ActiveContributorNames []string `json:"active_contributor_names,omitempty"`
	ActiveAgents           int      `json:"active_agents"`
	ActionableItems        int      `json:"actionable_items"`
	RegisteredAt           string   `json:"registered_at"`
	LastHeartbeat          string   `json:"last_heartbeat,omitempty"`
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
		ActiveContributors     int      `json:"active_contributors"`
		ActiveContributorNames []string `json:"active_contributor_names"`
		ActiveAgents           int      `json:"active_agents"`
		ActionableItems        int      `json:"actionable_items"`
	}
	const maxFedCount = 10000
	const maxFedNames = 200
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		if req.ActiveContributors >= 0 && req.ActiveContributors <= maxFedCount {
			found.ActiveContributors = req.ActiveContributors
		}
		if req.ActiveContributorNames != nil {
			// Bounded + sanitised: a heartbeat names who is present, it does not
			// get to write arbitrary strings into every dossier's Theaters zone.
			names := make([]string, 0, len(req.ActiveContributorNames))
			for _, n := range req.ActiveContributorNames {
				n = strings.TrimSpace(n)
				if !validGitHubUsername(n) {
					continue
				}
				names = append(names, n)
				if len(names) >= maxFedNames {
					break
				}
			}
			found.ActiveContributorNames = names
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

// handleHivesDelete removes an entry from the federation registry.
//
// OWNER-ONLY (audit F25, 2026-08-14). Deletion is the destructive end of the
// hive lifecycle: register/heartbeat are self-service actions a peer hive
// performs for ITSELF, but delete removes ANOTHER hive from the discovery
// list, so it is an administrative action on shared state rather than a
// contributor one. Un-gated, any authenticated write-tier session could
// unregister peers. Impact is bounded — the registry is a discovery list and
// handleHivesRegister can re-add entries — but the gate matches the owner-only
// convention the other destructive dashboard mutations follow (F14's
// handleContributorDelete, handleAgentDelete).
func (s *Server) handleHivesDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
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
	// EquippedTitle is the contributor's self-chosen dossier title (e.g.
	// "WOLFHERDER"); rendered as a small accent after the name. Optional.
	EquippedTitle string `json:"equipped_title,omitempty"`
	Active        bool   `json:"active,omitempty"`
	CurrentTask   string `json:"current_task,omitempty"`
	IsAgent       bool   `json:"is_agent,omitempty"`
	Emoji         string `json:"emoji,omitempty"`
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
			EquippedTitle:  p.EquippedTitle,
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
	case "merger":
		return "#f778ba"
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
	case "merger":
		return "rgba(247,120,186,0.2)", "#f778ba", "rgba(247,120,186,0.3)"
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
	target := "/contribute/leaderboard"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
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

// privateURLTestExemptHostPorts lets tests treat specific loopback host:port
// pairs (httptest servers, which always bind 127.0.0.1) as public, so SSRF
// behaviour can be exercised end-to-end without weakening the guard.
//
// It is EMPTY in production and only ever populated by test helpers, so real
// traffic sees the unmodified check. Entries are host:port, never a bare host,
// so an exemption cannot widen to all of loopback.
var privateURLTestExemptHostPorts map[string]struct{}

func isPrivateURL(ctx context.Context, rawURL string) bool {
	for _, scheme := range []string{"https://", "http://", "wss://", "ws://"} {
		if strings.HasPrefix(rawURL, scheme) {
			rawURL = strings.TrimPrefix(rawURL, scheme)
			break
		}
	}
	if len(privateURLTestExemptHostPorts) > 0 {
		hostPort := rawURL
		if idx := strings.IndexAny(hostPort, "/"); idx >= 0 {
			hostPort = hostPort[:idx]
		}
		if _, ok := privateURLTestExemptHostPorts[strings.ToLower(hostPort)]; ok {
			return false
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
//
// Authentication accepts BOTH the bearer scheme (hosted clients) and the legacy
// "token <pat>" scheme that `gh auth token` users and older hive CLIs send, so
// upgrading a hive never breaks existing scripts. Credentials in the query
// string (?token=) are NOT supported: query strings land in ingress and access
// logs.
//
// Authorization: every /api/v1 path except /api/v1/me is gated on the hive's
// authorized-users allowlist. The contributor data behind these reads
// (knowledge base, contributor roster, activity feed) is hive-private, so a
// merely-authenticated GitHub user must not be able to read it. /api/v1/me is
// exempt because it only ever returns the caller's own profile.
func (s *Server) handleAPIv1(w http.ResponseWriter, r *http.Request) {
	// Defense in depth: strip any client-supplied identity headers up front so no
	// downstream handler can ever observe a client-forged identity on this route.
	r.Header.Del("X-Hive-User")
	r.Header.Del("X-Hive-Role")
	r.Header.Del(ownerRoleVerifiedHeader)

	var token string
	if auth := strings.Fields(r.Header.Get("Authorization")); len(auth) == 2 {
		// Both schemes carry a GitHub PAT; "token" is kept for compatibility.
		if strings.EqualFold(auth[0], "Bearer") || strings.EqualFold(auth[0], "token") {
			token = auth[1]
		}
	}
	if token == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-token>"}`))
		return
	}

	username := validateGitHubToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-token>"}`))
		return
	}

	// Require allowlist authorization for every path except /api/v1/me, which is
	// self-scoped. Fail closed: an empty allowlist authorizes nobody.
	if !strings.HasPrefix(r.URL.Path, "/api/v1/me") {
		if _, ok := s.deps.Config.Dashboard.AuthorizedRole(username); !ok {
			jsonError(w, "forbidden: not authorized for this endpoint", http.StatusForbidden)
			return
		}
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
		if !strings.HasPrefix(subpath, "/prs/") || !strings.HasSuffix(subpath, "/queue-automerge") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"Unknown endpoint","available":["/api/v1/status","/api/v1/activity","/api/v1/contributors","/api/v1/knowledge","/api/v1/me","/api/v1/prs/{owner}/{repo}/{number}/queue-automerge"]}`))
			return
		}
		parts := strings.Split(strings.TrimPrefix(subpath, "/prs/"), "/")
		if len(parts) != 4 || parts[3] != "queue-automerge" {
			jsonError(w, "Unknown endpoint", http.StatusNotFound)
			return
		}
		// queue-automerge mutates merge state, so it must never be reachable via
		// GET (or any other safe method) — that would let a link or image tag
		// trigger a merge and bypass the write gate.
		if r.Method != http.MethodPost {
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		role, ok := s.deps.Config.Dashboard.AuthorizedRole(username)
		if !ok {
			jsonError(w, "merger or owner access required", http.StatusForbidden)
			return
		}
		// Identity and role are resolved server-side from the validated token and
		// hive allowlist. Overwrite any client-supplied headers before reusing the
		// dashboard queue handler and its repo, self-review, and exact-head guards.
		r.Header.Set("X-Hive-User", username)
		r.Header.Set("X-Hive-Role", role)
		r.Header.Del(ownerRoleVerifiedHeader)
		if isOwnerRole(role) {
			r.Header.Set(ownerRoleVerifiedHeader, "true")
		}
		r.SetPathValue("owner", parts[0])
		r.SetPathValue("repo", parts[1])
		r.SetPathValue("number", parts[2])
		s.handleQueuePRAutoMerge(w, r)
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
h2{margin-top:32px;margin-bottom:12px;color:var(--cc-accent);font-size:1.2rem}
.endpoint{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px;margin-bottom:12px}
.method{color:var(--cc-green);font-weight:bold;margin-right:8px}
.path{color:var(--cc-accent);font-family:monospace}
.desc{color:#8b949e;margin-top:4px;font-size:0.9rem}
pre{background:#0d1117;border:1px solid #30363d;border-radius:6px;padding:12px;margin-top:12px;overflow-x:auto;font-size:0.85rem;color:#e6edf3}
code{font-family:'SF Mono',monospace;font-size:0.85rem}
.token-box{background:#161b22;border:1px solid #f0883e;border-radius:8px;padding:16px;margin:16px 0}
.token-box h3{color:#f0883e;margin-bottom:8px}
a{color:var(--cc-accent)}
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
<div class="desc">Register as a contributor (returns your token once). To rotate a lost token, use the reissue-token endpoint below with your GitHub token — registration cannot reissue.</div>
<pre>curl -X POST -d '{"github_username":"you"}' %s/api/contribute/register</pre>
</div>

</body></html>`, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL)
}
