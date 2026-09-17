// Contributor profile store and invite/registration tokens, moved verbatim
// out of api_contribute.go (#7435). Same package — no call sites changed.

package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hub"
)

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
