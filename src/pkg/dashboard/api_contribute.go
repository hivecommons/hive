package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
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

func contributorEligibleForTrusted(p *ContributorProfile) bool {
	return p != nil && p.TrustTier == "contributor" && p.TasksWithPR >= contributorTrustedAt
}

func contributorTrustedEligibilityCrossed(p *ContributorProfile, previousTasksWithPR int) bool {
	return contributorEligibleForTrusted(p) && previousTasksWithPR < contributorTrustedAt
}

func logTrustedEligibilityIfCrossed(logger *slog.Logger, p *ContributorProfile, previousTasksWithPR int) bool {
	if !contributorTrustedEligibilityCrossed(p, previousTasksWithPR) {
		return false
	}
	if logger != nil {
		logger.Info(fmt.Sprintf("%s eligible for trusted tier (needs operator grant)", p.GitHubUsername))
	}
	return true
}

// inviteTrustTiers are the trust tiers permitted to mint an invite link. Only a
// trusted, merger, or advisor contributor may invite; a newcomer/contributor/anonymous
// viewer may not. Enforced server-side (handleContributeInvite) — UI hiding is
// UX only.
var inviteTrustTiers = map[string]bool{"trusted": true, "merger": true, "advisor": true}

// inviteSigningSecret returns the HMAC key used to sign/verify invite tokens.
//
// Resolution order, most to least identity-bound:
//
//  1. hub.SpokeInviteKey() — the PER-HIVE invite key, either hub-injected as
//
// mintInviteToken builds an opaque, HMAC-signed invite token that carries the
// inviter's GitHub username and an expiry. Format: base64url(inviter) "." expiry
// "." base64url(hmac). The signature covers "inviter|expiry", so neither field
// can be tampered with without invalidating the token.
// verifyInviteToken checks an invite token's signature and expiry and returns
// the inviter username. An empty string means the token is invalid, tampered,
// or expired — the caller must treat that as "no attribution" (a plain
// self-registration), never as an error.
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
	s.mux.HandleFunc("PUT /api/contribute/announcement", s.handleContributeAnnouncement)
	s.mux.HandleFunc("POST /api/contribute/announcement/dismiss", s.handleContributeAnnouncementDismiss)
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
	s.mux.HandleFunc("GET /api/contribute/run-stats/models", s.handleContributeRunStatsByModel)
	s.mux.HandleFunc("GET /api/contribute/wall", s.handleContributeWall)
	s.mux.HandleFunc("POST /api/contribute/wall", s.handleContributeWall)
	s.mux.HandleFunc("DELETE /api/contribute/wall/{id}", s.handleContributeWallDelete)
	s.mux.HandleFunc("POST /api/contribute/wall/{id}/hide", s.handleContributeWallHide)
	s.mux.HandleFunc("POST /api/contribute/wall/{id}/flag", s.handleContributeWallFlag)
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
	s.mux.HandleFunc("POST /api/contributors/{id}/wall-mute", s.handleContributorWallMute)
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
	// Sign-in return trampoline for the public /contribute pages on a
	// hub-proxied spoke (#7453). Deliberately NOT a public path: on a hosted
	// spoke nginx gates it, an anonymous visitor is bounced through the hub
	// login and comes back here with a session, and the handler sends them on
	// to the tab they were on. Tail-registered for the same file:line reason.
	s.mux.HandleFunc("GET /auth/return", s.handleAuthReturn)
}

// handleAuthReturn bounces a freshly signed-in visitor back to the same-origin
// path in ?to= (#7453). The Operations tab's "Sign in with GitHub" link used to
// point at "/" — the dashboard root, the only gated page a hosted spoke has —
// so a visitor who signed in landed on the dashboard and had to find their way
// back to /contribute/operations by hand. Linking to /contribute/operations
// itself would not work: /contribute is public, so the ingress never asks for
// a login there.
//
// Same-origin paths only: "to" must start with a single "/" (never "//" or a
// scheme), or the visitor is sent to /contribute. Nothing else is trusted from
// the query.
func (s *Server) handleAuthReturn(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, safeReturnPath(r.URL.Query().Get("to")), http.StatusFound)
}

// authReturnDefault is where /auth/return sends a visitor whose ?to= is
// missing or not a same-origin path.
const authReturnDefault = "/contribute"

// safeReturnPath accepts only a same-origin absolute path: a leading "/",
// not "//" (protocol-relative) or "/\", no scheme, no host, no
// control characters.
func safeReturnPath(to string) string {
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.HasPrefix(to, "/\\") {
		return authReturnDefault
	}
	if strings.ContainsAny(to, "\r\n\x00") {
		return authReturnDefault
	}
	// A string that starts with a single "/" has no scheme and no authority,
	// so the prefix checks above are the whole same-origin argument; no URL
	// parsing is needed (and none is done, to keep this file's route
	// registrations — cited by line in api-reference.md — where they are).
	return to
}

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
		"announcement":    s.activeContributeAnnouncement(),
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

// handleContributeActivity serves the recent contributor activity feed.
//
// Query params:
//
//	limit — how many of the MOST RECENT events to return. Omitted, non-numeric
//	        or <= 0 returns everything retained.
//
// `limit` used to be accepted and silently ignored: every request returned the
// whole ring whether it asked for 5 or for 300 (kubestellar/hive#5704).
//
// The response reports `retained` and `capacity` alongside the events because
// this feed is a BOUNDED RING and truncating it invisibly produced confidently
// wrong analysis rather than an obvious error — a reader counted one
// contributor's events over what they believed was a full day while actually
// reading a 50-event window shared with every other contributor on the hive.
// Two integers let a caller tell the three cases apart, which no amount of
// looking at the array alone can:
//
//	len(activity) < retained  — this response was cut short by `limit`
//	retained == capacity      — the ring is full, so older events have been
//	                            evicted; this is NOT the hive's full history
//	retained < capacity       — the hive really has produced only this many
//
// Deliberately NOT done here: growing the ring. `capacity` is the honest
// ceiling, and a request above it clamps visibly rather than pretending to
// serve history the hub never kept.
func (s *Server) handleContributeActivity(w http.ResponseWriter, r *http.Request) {
	limit := 0 // 0 = everything retained
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if s.contributeHub == nil {
		jsonResponse(w, map[string]any{
			"activity": []ActivityEntry{},
			"retained": 0,
			"capacity": maxActivityEntries,
		})
		return
	}
	all := s.contributeHub.RecentActivity()
	jsonResponse(w, map[string]any{
		"activity": limitActivity(all, limit),
		"retained": len(all),
		"capacity": maxActivityEntries,
	})
}

// ContributeAdmissionPolicy is a read-only summary of the merge/automation
// posture and the contributor admission filters that ALREADY exist server-side.
// It is surfaced to the Management & Operations tab so an operator can read what
// is configured; it adds no controls and changes nothing.
type ContributeAdmissionPolicy struct {
	Suspended            bool                                   `json:"suspended"`
	TitlesMode           string                                 `json:"titles_mode,omitempty"`
	AuthorsMode          string                                 `json:"authors_mode,omitempty"`
	LabelsMode           string                                 `json:"labels_mode,omitempty"`
	DenyTitles           []string                               `json:"deny_titles,omitempty"`
	DenyAuthors          []string                               `json:"deny_authors,omitempty"`
	DenyLabels           []string                               `json:"deny_labels,omitempty"`
	AllowLabels          []string                               `json:"allow_labels,omitempty"`
	AllowModels          []string                               `json:"allow_models,omitempty"`
	RepoFilters          map[string]config.ContributeRepoFilter `json:"repo_filters,omitempty"`
	RejectUnknownModels  bool                                   `json:"reject_unknown_models"`
	SkipAssignedToOthers bool                                   `json:"skip_assigned_to_others"`
	DisabledTiers        []string                               `json:"disabled_tiers,omitempty"`
	DisabledRepos        []string                               `json:"disabled_repos,omitempty"`
	AgentRoleGrantable   []string                               `json:"agent_role_grantable_roles,omitempty"`
	AgentRoleAssignable  []string                               `json:"agent_role_assignable_roles,omitempty"`
	AutoPromoteAt        int                                    `json:"auto_promote_at"`
	TrustedAt            int                                    `json:"trusted_at"`
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
	p.RepoFilters = h.ContributeRepoFilters
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
// how many of those produced a pull request (all-time and, since #7894, in the
// last 24 hours), and how many failed.
//
// Every number here already existed and was already load-bearing — TasksWithPR
// is the auto-promotion currency — but nothing on the Operations page ever
// showed it back to the person who earned it, so "tasks completed" could not be
// told apart from "pull requests shipped". This endpoint changes no schema; it
// reads the persisted profile and the hourly rings.
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
	store := s.contributeMetricsStore()
	recent, covered, known := store.userRecent(seriesKey, recentWindowBuckets)
	prRecent, prCovered, prKnown := store.userRecentPR(seriesKey, recentWindowBuckets)

	resp := map[string]any{
		"github_username":      profile.GitHubUsername,
		"trust_tier":           profile.TrustTier,
		"eligible_for_trusted": contributorEligibleForTrusted(profile),
		// Same field names as ContributorProfile so a reader of one payload can
		// read the other without a translation table.
		"total_tasks_completed":         profile.TasksCompleted,
		"total_tasks_completed_with_pr": profile.TasksWithPR,
		"total_tasks_failed":            profile.TasksFailed,
		"announcement_dismissed_id":     profile.DismissedContributeAnnouncementID,
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
		// prs_produced_24h is the trailing 24 buckets of this contributor's own
		// PR-producing completions (#7894), the per-hour counterpart of
		// total_tasks_completed_with_pr. It gets its own availability/coverage
		// pair rather than borrowing the completion series': the PR ring is
		// younger, so a spoke upgraded this morning has a day of completion
		// history and a few hours of PR history (none at all until its first
		// post-upgrade rollup), and the tile must say so rather than print a
		// zero labelled "24h".
		"prs_produced_24h": prRecent,
		"pr_window_hours_covered": func() int {
			if !prKnown {
				return 0
			}
			return prCovered
		}(),
		"pr_history_available": prKnown,
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

const maxUsernameLength = 39

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

func (s *Server) contributorsDirOrDefault() string {
	if s != nil && s.contributorsDir != "" {
		return s.contributorsDir
	}
	if v := os.Getenv("HIVE_CONTRIBUTORS_DIR"); v != "" {
		return v
	}
	if s != nil && s.deps != nil {
		return contributorsDirFromConfig(s.deps.Config)
	}
	return getContributorsDir()
}

func (s *Server) SetContributorsDir(dir string) {
	if s != nil {
		s.contributorsDir = dir
	}
}

func contributorsDirFromConfig(cfg *config.Config) string {
	if cfg == nil {
		return defaultContributorsDir
	}
	for _, dir := range []string{
		cfg.Data.AgentsDir,
		cfg.Data.MetricsDir,
		cfg.Data.LogsDir,
		cfg.Data.ClaudeSessionsDir,
		cfg.Data.CopilotSessionsDir,
		cfg.Data.BobSessionsDir,
	} {
		root := dataRootFromDir(dir)
		if root != "" {
			return filepath.Join(root, "contributors")
		}
	}
	return defaultContributorsDir
}

func dataRootFromDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	clean := filepath.Clean(dir)
	if base := filepath.Base(clean); base == "contributors" {
		return filepath.Dir(clean)
	}
	parent := filepath.Dir(clean)
	if parent == "." || parent == string(filepath.Separator) {
		return ""
	}
	return parent
}

// limitActivity returns at most limit entries taken from the END of the feed —
// the most RECENT ones — preserving the oldest-first order callers already rely
// on (the dashboard's own field-log consumer does `acts.slice(-6).reverse()`).
//
// limit <= 0 means "everything retained", which is what every caller received
// before the parameter was honoured, so an omitted or unparseable limit changes
// nothing for them.
func limitActivity(entries []ActivityEntry, limit int) []ActivityEntry {
	if limit <= 0 || len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
}
