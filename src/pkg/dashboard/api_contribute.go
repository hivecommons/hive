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

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub"
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
	// Version is the profile's optimistic-concurrency token (hivecommons/hive H2,
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
	if s.contributeHub != nil {
		s.contributeHub.Close()
	}
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
	// Read-only SELF stats (#6543): the signed-in contributor's own issues-worked
	// (24h + all-time), PRs produced, and failures. Self-service like interests /
	// dossier above — the identity is resolved SERVER-SIDE and there is no
	// username parameter, so this endpoint can only ever answer for its caller
	// (an anonymous caller gets 401, not someone else's numbers). GET only.
	s.mux.HandleFunc("GET /api/contribute/me", s.handleContributeMe)
	// Read-only per-backend RUN SCENARIOS: aggregates over the durable task-run
	// log (task_run_log.go) — scenario counts, sentinel-compliance share, and
	// duration percentiles per backend. Public like the other /api/contribute*
	// reads (aggregate counts only; no usernames, no reasons, no tokens).
	s.mux.HandleFunc("GET /api/contribute/run-stats", s.handleContributeRunStats)
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

	// Read-only PER-RUN task history for one contributor (#7317): the records
	// behind /api/contribute/run-stats' aggregates — outcome, failure kind,
	// reason, duration, scenario, per task. Registered at the tail of this
	// function on purpose: src/docs/api-reference.md cites every route by
	// file:line, so inserting one mid-list rewrites the citation of every route
	// below it and check-api-reference-citations.sh goes red for a change that
	// touched none of them. Public like the other /api/contribute* reads — see
	// handleContributeRuns for why that posture is inherited rather than chosen.
	s.mux.HandleFunc("GET /api/contribute/runs", s.handleContributeRuns)
	// Read-only HUB DECISIONS for one contributor (#7330): the refusals, fences
	// and ignored reports the hub makes ABOUT a contributor, which existed only
	// as slog lines on the hub's stdout. Owner/read-write ONLY — enforced
	// in-handler via hubDecisionViewer, because the /api/contribute prefix is
	// public in isPublicPath. Unlike a run's failure reason (already served
	// anonymously as last_failure on /api/contribute/fleet) these carry the
	// hub's internal protocol state, so the handler 403s rather than stripping.
	// Same tail-registration reasoning as the route above: api-reference.md
	// cites routes by file:line.
	s.mux.HandleFunc("GET /api/contribute/decisions", s.handleContributeDecisions)
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

func registrationTokenFromAuthorization(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Bearer ") {
		const bearerPrefixLen = 7 // len("Bearer ")
		return authz[bearerPrefixLen:]
	}
	if strings.HasPrefix(authz, "token ") {
		const tokenPrefixLen = 6 // len("token ")
		return authz[tokenPrefixLen:]
	}
	return ""
}

func contributorProfileFromRegistrationToken(token string) *ContributorProfile {
	if token == "" {
		return nil
	}
	tokenHash := sha256Hex(token)
	profiles := listContributorProfiles()
	for i := range profiles {
		if secureCompare(profiles[i].RegistrationToken, tokenHash) {
			return &profiles[i]
		}
	}
	return nil
}

func (s *Server) contributorProfileFromAuthorization(r *http.Request) *ContributorProfile {
	return contributorProfileFromRegistrationToken(registrationTokenFromAuthorization(r))
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
//
// The owner-token fallback is ONLY for a single-owner spoke, where the one
// person who can reach the dashboard is the person who logged in. It must not
// run on a hub-proxied spoke: there the hub's nginx injects X-Hive-User for
// every signed-in visitor, so a request WITHOUT that header is an anonymous
// one — and /api/contribute* is public, so anonymous requests do arrive.
// Falling through handed every such visitor the hub owner's identity, and the
// Operations "Your contribution" panel then showed the owner's totals (300
// worked / 3066 failed on the projectbluefin hive) to anyone who opened the
// page signed out. Same rule handleAuthToken already applies.
func (s *Server) resolveViewerUsername(r *http.Request) string {
	if sess := s.sessionFromRequest(r); sess != nil {
		return sess.Username
	}
	if hubUser := r.Header.Get("X-Hive-User"); hubUser != "" {
		return hubUser
	}
	// SECURITY (#7394, residual of #7362): ANY auth boundary makes an absent
	// session/X-Hive-User an anonymous caller — including a dashboard auth
	// token. /api/contribute* is public (isPublicPath), so on a token-protected
	// standalone spoke anonymous internet requests reach this code; the owner
	// fallback must not stand in for them. Same "genuinely open spoke"
	// predicate hubDecisionViewer and paneTailViewer use. The token-protected
	// owner still resolves via the device-flow session checked above.
	if s.directRouteAuthzEnabled() || s.hubProxied() || s.authToken != "" {
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
	token := registrationTokenFromAuthorization(r)
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
		_, _ = w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-personal-access-token>"}`))
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
	actionable, candidates := 0, 0
	if s.contributeHub != nil {
		active = s.contributeHub.ActiveCount()
		// "Actionable" is the work a contributor can actually be offered, not the
		// scanner's raw candidate population. Reuse the same admission sweep as the
		// queue/assignment projection so disabled repositories, holds, cooldowns,
		// dependency gates, in-flight work and configured filters cannot make this
		// endpoint advertise work that the relay will reject as no_matching_work.
		snap := s.contributeHub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldNone)
		actionable = snap.offerableTotal
		candidates = snap.candidateTotal
	} else {
		// Defensive fallback for partially constructed test/embedding servers. A
		// production route always has a contribute hub after registration.
		s.statusMu.RLock()
		if s.status != nil {
			for _, repo := range s.status.Repos {
				candidates += len(repo.ActionableIssues)
			}
		}
		s.statusMu.RUnlock()
	}
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
		// Additive compatibility field for callers that need scanner health or want
		// to explain why admission reduced the raw population. This is deliberately
		// not named actionable: candidates may still be disabled or filtered out.
		"candidate_items": candidates,
		"surface":         s.contributeSurface(),
		"api_version":     contributorProtocolVersion,
		"served_sha":      versionShort,
	})
}

// handleAPIv1Queue serves a paginated page of the offerable ready-work set
// (the same population /api/v1/status counts as actionable_items), so a
// downstream consumer can enumerate the full backlog instead of only the
// unpaginated /api/contribute/queue slice (hivecommons/hive#6537). Auth and
// the contributor allowlist are already enforced by handleAPIv1 before this
// is reached.
func (s *Server) handleAPIv1Queue(w http.ResponseWriter, r *http.Request) {
	limit := readyQueueDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			jsonError(w, "invalid limit: must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			jsonError(w, "invalid offset: must be a non-negative integer", http.StatusBadRequest)
			return
		}
		offset = n
	}
	items, total := []ReadyQueueItem{}, 0
	if s.contributeHub != nil {
		items, total = s.contributeHub.admissionQueueRange(limit, offset, withheldNone)
	}
	jsonResponse(w, map[string]any{
		"queue":    items,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": offset+len(items) < total,
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
	activity := s.contributeHub.RecentActivity()
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if limit, err := strconv.Atoi(raw); err == nil && limit >= 0 && limit < len(activity) {
			activity = activity[len(activity)-limit:]
		}
	}
	jsonResponse(w, map[string]any{"activity": activity})
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
	// #7317 item 3: the live pane tail is gated exactly like the stored one on
	// /api/contribute/runs — see paneTailViewer. The snapshot builds it for
	// every viewer (it is a bounded copy, cheap) and this boundary decides who
	// gets it, so the same rule applies whether the contributor is still
	// connected or long gone.
	paneVisible := s.paneTailViewer(r)
	if !paneVisible {
		for i := range snap.Clankers {
			snap.Clankers[i].PaneTail = nil
		}
	}
	jsonResponse(w, map[string]any{
		"clankers":          snap.Clankers,
		"work":              snap.Work,
		"policy":            s.buildContributeAdmissionPolicy(),
		"cooldown_count":    cooldownCount,
		"in_flight_count":   inFlightCount,
		"held_count":        heldCount,
		"pane_tail_visible": paneVisible,
	})
}

// handleContributeQueue serves the read-only ready-work QUEUE — the admissible
// issues waiting to be picked off, derived from the SAME ActionableIssues set
// selectTask offers from (see ReadyQueue). GET only, public, no side effects. It
// is both a JSON fallback for browsers without EventSource and the same payload
// the SSE "hello" frame carries, so the queue renders even if the stream drops.
func (s *Server) handleContributeQueue(w http.ResponseWriter, r *http.Request) {
	queue := []ReadyQueueItem{}
	resp := map[string]any{
		"queue_total": 0,
		"held_total":  0,
	}
	if s.contributeHub != nil {
		// One snapshot serves the queue and — only when the convergence toggle
		// is in shadow mode (#4246, default off) — the additive withheld /
		// coverage diagnostics from the SAME sweep. With the toggle off the
		// response is exactly the pre-diagnostics payload.
		//
		// #6902 adds an explicit opt-in on top: ?withheld=1 asks for the FULL
		// admission explanation (every gate, not only convergence). It is opt-in
		// precisely so the default payload every existing client already parses
		// stays byte-identical — a queue view that does not render the Withheld
		// section should not pay for it either.
		scope := s.contributeQueueWithheldScope(r)
		snap := s.contributeHub.admissionQueueSnapshot(readyQueueDefaultLimit, scope)
		queue = snap.queue
		resp["queue_total"] = snap.offerableTotal
		resp["held_total"] = snap.heldTotal
		if scope.collects() {
			resp["withheld"] = snap.withheld
			resp["admission_coverage"] = snap.coverage
		}
		if scope == withheldAll {
			// The count is of what the sweep RETAINED, which the limit bounds —
			// stated separately from the rendered rows so a truncated list does
			// not read as the whole population.
			resp["withheld_total"] = len(snap.withheld)
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
// handleContributeMe serves the signed-in contributor their OWN contribution
// numbers (#6543): issues worked in the last 24 hours, issues worked all-time,
// how many of those produced a pull request, and how many failed.
//
// Every number here already existed and was already load-bearing — TasksWithPR
// is the auto-promotion currency — but nothing on the Operations page ever
// showed it back to the person who earned it, so "tasks completed" could not be
// told apart from "pull requests shipped". This endpoint changes no schema and
// computes nothing new; it reads the persisted profile and the hourly ring.
//
// Identity is resolved server-side (resolveContributeCaller) and there is NO
// username parameter, so the response is always the caller's own record. That
// matters because /api/contribute* is a PUBLIC prefix (isPublicPath): the gate
// has to live in the handler.
func (s *Server) handleContributeMe(w http.ResponseWriter, r *http.Request) {
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to see your contribution stats.", http.StatusUnauthorized)
		return
	}
	profile := findContributor(username)
	if profile == nil {
		jsonError(w, "You need a contributor profile on this hive before it can show your stats.", http.StatusForbidden)
		return
	}

	// The metrics rings are keyed on the profile's STORED github_username, which
	// is the case GitHub first registered. A session can hand us a different case
	// (see findContributor), so key the lookup off the profile, never the caller
	// string, or the 24h figure silently reads as "no history".
	seriesKey := profile.GitHubUsername
	if seriesKey == "" {
		seriesKey = username
	}
	recent, covered, known := s.contributeMetricsStore().userRecent(seriesKey, recentWindowBuckets)

	resp := map[string]any{
		"github_username": profile.GitHubUsername,
		"trust_tier":      profile.TrustTier,
		// Same field names as ContributorProfile so a reader of one payload can
		// read the other without a translation table.
		"total_tasks_completed":         profile.TasksCompleted,
		"total_tasks_completed_with_pr": profile.TasksWithPR,
		"total_tasks_failed":            profile.TasksFailed,
		// tasks_completed_24h is the trailing 24 hourly buckets of this
		// contributor's own completion series. window_hours_covered says how much
		// history actually backed that sum, so a page can say "6h of history" on a
		// freshly-started spoke instead of labelling six hours as a day.
		"tasks_completed_24h": recent,
		"window_hours":        recentWindowBuckets,
		"window_hours_covered": func() int {
			if !known {
				return 0
			}
			return covered
		}(),
		// history_available distinguishes "no hourly series for you yet" (the spoke
		// has not rolled up since you registered) from a genuine zero. Without it a
		// brand-new contributor and an idle one look identical.
		"history_available": known,
	}
	jsonResponse(w, resp)
}

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

// paneTailViewer reports whether this request may see agent pane output —
// the pane_tail on a fleet row or a task-run record (#7317 item 3).
//
// Pane text is what the agent printed: more sensitive than the bounded reason
// string the same endpoints already serve anonymously, since it can carry
// repository paths, partial secrets the redactor did not recognize, or a
// half-typed credential prompt. So it follows the same line the per-clanker
// controls draw: owner and read-write see it, everyone else does not.
//
// The empty-role case mirrors requestRoleAllowsOwner, not requireContributorWrite:
// on a spoke with any auth boundary an absent X-Hive-Role is an anonymous caller
// and gets nothing, while on a genuinely open spoke (no token, no allowlist) the
// whole dashboard is already anonymous and hiding one field from its only
// operator would be theatre. Read-only decision, no side effects.
func (s *Server) paneTailViewer(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role == config.RoleOwner || role == config.RoleReadWrite {
		return true
	}
	if role == "" {
		return s.authToken == "" && !s.directRouteAuthzEnabled()
	}
	return false
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
			"3. Save the private key in the deployment's secrets directory (for example, /etc/hive/secrets/gh-app-key.pem, or ~/.config/hive/secrets/gh-app-key.pem for rootless Podman)",
			"4. Deploy: Docker — docker compose up -d; Podman — install the Quadlet units from src/deploy/quadlet/ per src/docs/podman-standalone-quadlet.md, then start hive-gateway.service with systemctl (systemctl --user for rootless)",
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
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
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
	defer closeHTTPBody(resp.Body)
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
		_, _ = w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-token>"}`))
		return
	}

	username := validateGitHubToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-token>"}`))
		return
	}

	// Require allowlist authorization for every path except /api/v1/me, which is
	// self-scoped. Fail closed: an empty allowlist authorizes nobody.
	if !strings.HasPrefix(r.URL.Path, "/api/v1/me") {
		role, ok := s.deps.Config.Dashboard.AuthorizedRole(username)
		if !ok {
			jsonError(w, "forbidden: not authorized for this endpoint", http.StatusForbidden)
			return
		}
		r.Header.Set("X-Hive-User", username)
		r.Header.Set("X-Hive-Role", role)
		if isOwnerRole(role) {
			r.Header.Set(ownerRoleVerifiedHeader, "true")
		}
	}

	subpath := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch subpath {
	case "/queue":
		// Paginated ready-work listing: supports ?limit=<int>&offset=<int>
		// Authentication and allowlist already enforced by handleAPIv1.
		s.handleAPIv1Queue(w, r)
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
		_, _ = w.Write([]byte(`{"error":"Not registered as a contributor. Run: just contribute-setup"}`))
	default:
		if !strings.HasPrefix(subpath, "/prs/") || !strings.HasSuffix(subpath, "/queue-automerge") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Unknown endpoint","available":["/api/v1/status","/api/v1/queue","/api/v1/activity","/api/v1/contributors","/api/v1/knowledge","/api/v1/me","/api/v1/prs/{owner}/{repo}/{number}/queue-automerge"]}`))
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
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
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
