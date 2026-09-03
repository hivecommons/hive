// Hive provisioning: provision request persistence and handlers,
// placeholder pool selection, spoke project config adoption, and
// hive assignment.
package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// provisionWG tracks async hive-provisioning goroutines so tests (which swap
// the package-level saas*Dir path variables) can wait for them to drain
// before mutating shared state.
var provisionWG sync.WaitGroup

var provisionRequestsDir = "/data/saas/provision-requests"

const (
	provisionStatusPending  = "pending"
	provisionStatusApproved = "approved"
	provisionStatusDenied   = "denied"
)

const maxProvisionRequestBodyBytes = 4 * 1024

type ProvisionRequest struct {
	Username string `json:"username"`
	// UserID is the human-facing identifier admins should use when reviewing
	// the request. Username remains the stable auth key/native provider subject
	// (for example "ibmid:695000VVZ9"); UserID captures the meaningful login or
	// identity available at request time so the review queue does not headline
	// opaque SSO subjects. Empty on older records; enrichProvisionRequests fills
	// it from the user record when possible, and the UI falls back to Username.
	UserID       string `json:"user_id,omitempty"`
	UserIDSource string `json:"user_id_source,omitempty"`
	// GitHubHost is the GitHub instance the org lives on — empty means public
	// github.com, otherwise a GitHub Enterprise host (github.ibm.com,
	// github.cisco.com, …). Captured so an admin can see which instance a
	// request targets before deciding where to place the hive.
	GitHubHost  string `json:"github_host,omitempty"`
	Org         string `json:"org"`
	Repos       string `json:"repos"`
	PrimaryRepo string `json:"primary_repo"`
	ACMMLevel   int    `json:"acmm_level"`
	AuthMethod  string `json:"auth_method"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`

	// Who is asking, as a person rather than as a GitHub login. Both reuse the
	// SaaSUser contact fields of the same name and the same caps
	// (maxContactNameLen / maxContactSlackIDLen) — deliberately NOT new keys.
	// A second Slack key would split one fact across two names, with the
	// request form writing one and the admin users panel reading the other.
	//
	// These are captured here and copied onto the SaaSUser record on approval
	// (handleApproveProvision); asking and then dropping the answer would be
	// worse than not asking. Both are omitempty so requests filed before these
	// fields existed round-trip unchanged.
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`

	// Country is the requester's OPTIONAL self-declared ISO 3166-1 alpha-2
	// code, picked from the wizard's dropdown. Like the two fields above it
	// reuses the SaaSUser key of the same name and is copied onto the user
	// record on approval (applyRequestContactToUser) — asking and then dropping
	// the answer would be worse than not asking.
	//
	// This is the AUTHORITATIVE source of a user's country: they chose it about
	// themselves. The Accept-Language inference on the login path is only a
	// fallback for records that never got one. omitempty so requests filed
	// before this field existed round-trip unchanged.
	Country string `json:"country,omitempty"`

	// Decision audit. Previously a request only carried its final Status, so
	// once it left "pending" there was no record of WHO decided, WHEN, or —
	// for an approval — which hive the requester actually got. That made the
	// history unauditable: an approved request and a denied one looked equally
	// anonymous. Empty on records decided before these fields existed.
	DecidedBy string `json:"decided_by,omitempty"`
	// DecidedByName is the display-only label for DecidedBy, resolved on read.
	DecidedByName string `json:"decided_by_name,omitempty"`
	DecidedAt     string `json:"decided_at,omitempty"`
	AssignedHive  string `json:"assigned_hive,omitempty"`
	// DenyReason is the optional free-text explanation shown back to the
	// requester when a request is turned down.
	DenyReason string `json:"deny_reason,omitempty"`

	// --- Derived, never persisted ---
	// These are filled in on read (see enrichProvisionRequests) so the admin
	// Past Requests table can show the requester's role on the hive they were
	// given, plus the rest of their fleet, without one API call per row. They
	// are omitempty so records written before they existed stay valid and so a
	// round-trip through saveProvisionRequest never bakes stale roles onto disk.
	//
	// AssignedRole is the requester's role on AssignedHive, read from their
	// SaaSUser.Hives map — the authoritative grant (see accessForHive). Empty
	// when the request was denied, when no hive was assigned, or when the grant
	// was since revoked.
	AssignedRole string `json:"assigned_role,omitempty"`
	// OtherHives is every OTHER hive the requester can sign in to, with their
	// role on each — the person's footprint beyond this one request. Sorted
	// owners-first then by hive ID for a stable render.
	OtherHives []UserHiveRole `json:"other_hives,omitempty"`
}

// roleOwner is the role string stored in SaaSUser.Hives for the hive's owner.
// Named so the owners-first sort below does not repeat a bare literal.
const roleOwner = "owner"

// UserHiveRole is one hive a user can sign in to and the role they hold on it.
// The mirror image of HiveAccessEntry: that answers "who is on this hive", this
// answers "which hives is this user on".
type UserHiveRole struct {
	HiveID string `json:"hive_id"`
	Role   string `json:"role"`
}

// hivesForUser returns every hive the named user can sign in to, with their
// role on each, optionally excluding one hive ID (the one already shown in its
// own column). Access comes from SaaSUser.Hives — the authoritative grant that
// handleApproveProvision / handleAssignHive write — not from hive.Owner, which
// can name someone whose grant was revoked.
//
// users is passed in rather than read here so a caller enriching many rows can
// read the roster ONCE: listAllSaaSUsers hits the filesystem per user record.
func hivesForUser(username string, excludeHiveID string, users []SaaSUser) []UserHiveRole {
	if username == "" {
		return nil
	}
	out := make([]UserHiveRole, 0)
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		for id, role := range u.Hives {
			if id == "" || id == excludeHiveID {
				continue
			}
			out = append(out, UserHiveRole{HiveID: id, Role: role})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Owned hives first — the useful line to read when scanning someone's
		// footprint — then alphabetical by hive ID for a stable order.
		if (out[i].Role == roleOwner) != (out[j].Role == roleOwner) {
			return out[i].Role == roleOwner
		}
		return out[i].HiveID < out[j].HiveID
	})
	if len(out) == 0 {
		return nil // omitempty: no cell rather than an empty array
	}
	return out
}

// roleForUserOnHive returns the named user's role on the named hive, or "" if
// they hold no grant on it. Same roster-passed-in contract as hivesForUser.
func roleForUserOnHive(username, hiveID string, users []SaaSUser) string {
	if username == "" || hiveID == "" {
		return ""
	}
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		if role, ok := u.Hives[hiveID]; ok {
			return role
		}
	}
	return ""
}

// provisionRequestUserIdentity chooses the operator-facing identifier for a
// request, plus its source so the UI only links identifiers known to be GitHub
// logins.
// The raw request Username is the auth key and can be an opaque provider subject
// ("ibmid:…"). Prefer a linked GitHub/GHE login when the user has one, then a
// recognizable GitHub login, then email/name claims, and fall back to the auth
// key only when no friendlier identity exists.
func provisionRequestUserIdentity(username string, u *SaaSUser, requestFullName string) (id, source string) {
	if u != nil {
		if id := strings.TrimSpace(u.LinkedGitHubLogin); id != "" {
			return id, "github"
		}
		if provider, subject := splitIdentityKey(strings.TrimSpace(u.GitHubUsername)); provider == "" || normalizeIdentityProvider(provider) == legacyProvider {
			if subject != "" {
				return subject, "github"
			}
		}
		if id := strings.TrimSpace(u.Email); id != "" {
			return id, "email"
		}
		if id := strings.TrimSpace(u.DisplayName); id != "" {
			return id, "name"
		}
		if id := strings.TrimSpace(u.FullName); id != "" {
			return id, "name"
		}
	}
	if id := strings.TrimSpace(requestFullName); id != "" {
		return id, "name"
	}
	if provider, subject := splitIdentityKey(strings.TrimSpace(username)); normalizeIdentityProvider(provider) == legacyProvider && subject != "" {
		return subject, "github"
	}
	return strings.TrimSpace(username), "native"
}

func provisionRequestUserID(username string, u *SaaSUser, requestFullName string) string {
	id, _ := provisionRequestUserIdentity(username, u, requestFullName)
	return id
}

func provisionRequestUserFromRoster(username string, users []SaaSUser) *SaaSUser {
	for i := range users {
		if strings.EqualFold(users[i].GitHubUsername, username) {
			return &users[i]
		}
	}
	return nil
}

// enrichProvisionRequests fills in the derived AssignedRole / OtherHives fields
// on every request in place.
//
// ADMIN-ONLY: this exposes other people's hive memberships, so it must only be
// called on a payload already gated behind requireAdmin (or the isAdmin branch
// of the dashboard handler). Do not call it on a per-user response.
//
// The roster is read once here — O(1) filesystem sweeps for the whole table
// rather than O(rows) — because listAllSaaSUsers walks and unmarshals every
// user record on disk.
func enrichProvisionRequests(requests []ProvisionRequest) []ProvisionRequest {
	if len(requests) == 0 {
		return requests
	}
	users := listAllSaaSUsers()
	label := (&HubServer{}).identityLabeler()
	for i := range requests {
		if requests[i].UserID == "" {
			requests[i].UserID, requests[i].UserIDSource = provisionRequestUserIdentity(requests[i].Username, provisionRequestUserFromRoster(requests[i].Username, users), requests[i].FullName)
		} else if requests[i].UserIDSource == "" {
			if requests[i].UserID == requests[i].Username {
				requests[i].UserIDSource = "native"
			}
		}
		if l := label(requests[i].DecidedBy); l != requests[i].DecidedBy {
			requests[i].DecidedByName = l
		}
		requests[i].AssignedRole = roleForUserOnHive(requests[i].Username, requests[i].AssignedHive, users)
		requests[i].OtherHives = hivesForUser(requests[i].Username, requests[i].AssignedHive, users)
	}
	return requests
}

// applyRequestContactToUser carries the requester's contact details from an
// approved provision request onto their SaaSUser record.
//
// This is what makes asking for them worth anything. The admin users panel and
// the Slack sender both read SaaSUser, NOT ProvisionRequest — a value that
// stops at the request file is invisible to every consumer of it. Slack
// messaging shipped with no user having a slack_id, settable only by hand one
// user at a time; approval is the natural point of capture.
//
// It only ever FILLS A BLANK. An admin who has already curated these fields (or
// a user who corrected them later) outranks whatever was typed into a request
// form, and a re-approval must not silently revert that.
//
// Nil-safe on both sides: it runs on the approval path beside other work that
// can legitimately leave either side absent, and must not panic there.
func applyRequestContactToUser(user *SaaSUser, pr *ProvisionRequest) {
	if user == nil || pr == nil {
		return
	}
	if user.FullName == "" && pr.FullName != "" {
		user.FullName = truncateRunes(strings.TrimSpace(pr.FullName), maxContactNameLen)
	}
	if user.SlackID == "" && pr.SlackID != "" {
		user.SlackID = truncateRunes(strings.TrimSpace(pr.SlackID), maxContactSlackIDLen)
	}
	// The explicit pick outranks anything Accept-Language inferred at login, so
	// unlike the two fields above this one overwrites a value already on the
	// record — but only when the request actually carries a choice. Re-normalize
	// rather than trusting the stored request: it may predate the validation.
	if code := normalizeCountryCode(pr.Country); code != "" {
		// The wizard pick is a deliberate statement BY THE USER, so it carries
		// the same provenance the self-service endpoint stamps. Without this,
		// an approval would leave the record looking "inferred", and the
		// priority rule would hold only by the accident of the value being
		// non-empty.
		setUserCountry(user, code, countrySourceUser)
	}
}

func loadProvisionRequest(username string) *ProvisionRequest {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	path := filepath.Join(provisionRequestsDir, username+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pr ProvisionRequest
	if json.Unmarshal(data, &pr) != nil {
		return nil
	}
	return &pr
}

func saveProvisionRequest(pr *ProvisionRequest) error {
	// Same traversal guard as loadProvisionRequest/deleteProvisionRequest (and
	// SaveUser): the username becomes a filename, so a value carrying "..", "/"
	// or "\" must never reach filepath.Join. Auth'd usernames cannot normally
	// contain these (makeCanonical rejects them), but the write path fails
	// closed rather than trusting every future caller to have checked.
	if strings.Contains(pr.Username, "..") || strings.Contains(pr.Username, "/") || strings.Contains(pr.Username, "\\") {
		return fmt.Errorf("invalid username for provision request: %q", pr.Username)
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(provisionRequestsDir, 0o755)
	data, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(provisionRequestsDir, pr.Username+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func deleteProvisionRequest(username string) {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return
	}
	if err := os.Remove(filepath.Join(provisionRequestsDir, username+".json")); err != nil && !os.IsNotExist(err) {
		slog.Warn("deleteProvisionRequest: remove failed", "user", username, "error", err)
	}
}

func listProvisionRequests() []ProvisionRequest {
	// Best-effort: a failed mkdir surfaces via the ReadDir error below.
	_ = os.MkdirAll(provisionRequestsDir, 0o755)
	entries, err := os.ReadDir(provisionRequestsDir)
	if err != nil {
		return nil
	}
	var result []ProvisionRequest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		uname := strings.TrimSuffix(e.Name(), ".json")
		pr := loadProvisionRequest(uname)
		// Return decided requests too, not just pending ones — the admin view
		// splits them into a pending action queue and a decision-history table,
		// and filtering here made the history permanently empty.
		if pr != nil {
			result = append(result, *pr)
		}
	}
	return result
}

func (s *HubServer) handleRequestProvision(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	existing := loadProvisionRequest(username)
	if existing != nil && existing.Status == provisionStatusPending {
		http.Error(w, `{"error":"you already have a pending provision request"}`, http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProvisionRequestBodyBytes)
	var body struct {
		Org         string `json:"org"`
		GitHubHost  string `json:"github_host"`
		Repos       string `json:"repos"`
		PrimaryRepo string `json:"primary_repo"`
		ACMMLevel   int    `json:"acmm_level"`
		AuthMethod  string `json:"auth_method"`
		FullName    string `json:"full_name"`
		SlackID     string `json:"slack_id"`
		Country     string `json:"country"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Org == "" || body.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	// Contact details for the person asking.
	//
	// No charset validation on purpose: isValidName() is for identifiers
	// (org/repo/host names) and would reject spaces, apostrophes and every
	// non-ASCII script. These are display strings — trimmed, rune-capped and
	// escaped at every render site, exactly as the admin contact editor
	// (handleAdminUpdateUser) already treats these same two fields. We also do
	// NOT try to detect a "real" name: any pattern for that (two words,
	// capitalised, Latin script) is wrong for a large fraction of the world's
	// names, and a determined user types junk regardless. Human review is the
	// check, not a regex.
	body.FullName = truncateRunes(strings.TrimSpace(body.FullName), maxContactNameLen)
	body.SlackID = truncateRunes(strings.TrimSpace(body.SlackID), maxContactSlackIDLen)
	// Country is OPTIONAL and normalized rather than rejected: a malformed or
	// absent code stores "", which renders no flag at all. Validating here (the
	// last point before the PVC) means the render sites can trust that a stored
	// country is two uppercase letters, and a client that never sends the field
	// behaves exactly as before this shipped.
	body.Country = normalizeCountryCode(body.Country)
	// Accept a pasted org/repo URL, not just a bare name. Users read
	// "GitHub Organization" and paste the org's URL; the old validator rejected
	// ":" and "/" and returned a bare "invalid org name" that explained nothing.
	// The host is kept so the admin can see whether a request is for github.com
	// or a GitHub Enterprise instance before placing the hive.
	ghHost, orgName := normalizeOrgRef(body.Org)
	body.Org = orgName
	if ghHost != "" {
		body.GitHubHost = ghHost
	}
	// The forge is REQUIRED. A bare org name ("z-innersource") does not say
	// whether the org lives on github.com or on a GitHub Enterprise instance,
	// and the hub cannot guess: the wrong choice provisions the hive against
	// the wrong GitHub and points the App-install link at a forge the org
	// admin never sees. Normalize FIRST — the field accepts
	// "https://github.ibm.com/" and isValidName rejects ":" and "/", so
	// validating the raw value would reject a form the form itself advertises.
	forgeHost, ok := normalizeForgeHost(body.GitHubHost)
	if !ok {
		http.Error(w, `{"error":"a GitHub forge is required — enter the host your org lives on (e.g. github.com or github.ibm.com); without it we cannot tell which GitHub to provision against"}`, http.StatusBadRequest)
		return
	}
	body.GitHubHost = forgeHost
	// Single-host-per-spoke: every repo (and the primary) must be on the same
	// GitHub host as the org. Check BEFORE normalizeRepoRef strips the host off
	// each pasted repo. Reject a mixed request up front with a clear message —
	// a spoke that mixed github.com and a GHE instance would silently fail to
	// authenticate against half its repos, the onboarding footgun this removes.
	if err := validateSingleRepoHost(body.GitHubHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	{
		var cleaned []string
		for _, repo := range strings.Split(body.Repos, ",") {
			if r := normalizeRepoRef(repo); r != "" {
				cleaned = append(cleaned, r)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
	}
	if !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// Defence in depth: normalizeForgeHost above already guaranteed this, but
	// keep the check so a future edit that reorders the handler cannot let an
	// unvalidated host reach the stored request.
	if !isValidName(body.GitHubHost) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	for _, repo := range strings.Split(body.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(repo)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}

	// Name is REQUIRED; Slack ID is optional.
	//
	// The name exists to map a GitHub login to a person an operator can talk
	// to, which only works if it is actually populated — a field that is
	// usually blank cannot be relied on and stops being read. Requesting a hive
	// is a deliberate, one-per-user action already reviewed by a human, so one
	// short field is negligible friction at the moment the requester is most
	// motivated to answer.
	//
	// Checked AFTER the org/forge/repo validation above so the more specific
	// "which GitHub is this org on?" errors keep their precedence — a request
	// missing both should be told about the forge, not sent round the loop one
	// field at a time.
	//
	// This does tighten an existing endpoint: a caller that posted no full_name
	// now gets a 400 where it used to get a 200. That is intended — the whole
	// point is that the field is reliably present.
	//
	// The get-started wizard (static/get-started.html) is now the only in-tree
	// caller, but do not read that as "it always was". When this check landed
	// (#2369) this comment claimed the wizard was the only caller and it was
	// simply wrong: the hub dashboard had its own Request-a-Hive modal posting
	// here, added later than the wizard, reachable by every logged-in user, and
	// it went un-updated — so the button 400'd with no field on screen that
	// could satisfy it. The modal has since been removed deliberately (the
	// wizard is the single supported request path), which is what makes this
	// sentence true today rather than aspirational.
	//
	// The modal was invisible to CI because it was inline JS inside the
	// dashboardHTML raw string with no test naming any of its symbols. Before
	// adding another required field here, re-run the caller audit rather than
	// trusting this comment: TestRequestProvisionInTreeCallersSendRequiredFields
	// greps the embedded JS and the static wizard for callers of this endpoint
	// and fails on one that omits a required field.
	if body.FullName == "" {
		http.Error(w, `{"error":"your name is required — we use it to know who the request is from"}`, http.StatusBadRequest)
		return
	}

	// A hive is never REQUESTED above L3 Quality-Gated. L4-L6 are real levels, but
	// they are reached after provisioning, from the hive's own dashboard, once
	// the project has the coverage and CI history to justify them. The
	// get-started wizard only offers L1-L3, but the wizard is one client: clamp
	// here so a crafted request cannot provision straight into auto-merge.
	// Admin paths (assign/provision) keep the full 0..6 range on purpose — an
	// operator setting a level deliberately is not the case being guarded.
	acmm := body.ACMMLevel
	if acmm < minRequestACMMLevel || acmm > maxRequestACMMLevel {
		acmm = minRequestACMMLevel
	}

	primaryRepo := body.PrimaryRepo
	if primaryRepo == "" {
		repos := strings.Split(body.Repos, ",")
		if len(repos) > 0 {
			primaryRepo = strings.TrimSpace(repos[0])
		}
	}

	userID, userIDSource := provisionRequestUserIdentity(username, loadSaaSUser(username), body.FullName)
	pr := &ProvisionRequest{
		Username:     username,
		UserID:       userID,
		UserIDSource: userIDSource,
		GitHubHost:   body.GitHubHost,
		Org:          body.Org,
		Repos:        body.Repos,
		PrimaryRepo:  primaryRepo,
		ACMMLevel:    acmm,
		AuthMethod:   body.AuthMethod,
		FullName:     body.FullName,
		SlackID:      body.SlackID,
		Country:      body.Country,
		RequestedAt:  time.Now().UTC().Format(time.RFC3339),
		Status:       provisionStatusPending,
	}
	if err := saveProvisionRequest(pr); err != nil {
		http.Error(w, `{"error":"failed to save provision request"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: provision request created", "user", username, "org", body.Org, "repos", body.Repos)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": provisionStatusPending})
}

// ApproveProvisionRequest is the OPTIONAL body of PUT
// /api/saas/approve-provision/{username}. HiveID lets the admin pick the exact
// available placeholder to assign (from the approve-picker modal); an empty or
// absent HiveID preserves the historical auto-pick behavior.
type ApproveProvisionRequest struct {
	HiveID string `json:"hive_id"`
	// GitHubHost is the admin's explicit choice of GitHub instance for this
	// hive, overriding the one on the provision request. "public" forces public
	// github.com even on a GitHub Enterprise cluster; empty means "use the
	// request's host, else the cluster default".
	GitHubHost string `json:"github_host,omitempty"`
}

func (s *HubServer) handleApproveProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Optionally the admin picks the EXACT placeholder to assign (from the
	// approve-picker modal) instead of letting the hub auto-pick. The body is
	// tolerated as absent/empty — an empty hive_id preserves the historical
	// auto-pick behavior. The body is tiny (a single id), so cap it small.
	const maxApproveRequestBodyBytes = 1 * 1024
	var approveBody ApproveProvisionRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxApproveRequestBodyBytes)
		// An empty body is valid (auto-pick); ignore EOF/empty decode errors and
		// fall through to auto-pick. Any non-empty malformed body is rejected.
		if err := json.NewDecoder(r.Body).Decode(&approveBody); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}

	// Approving now ASSIGNS an available placeholder instead of just bumping
	// quota for a manual provision. Pick the pool from the request's auth_method
	// (private → the heartbeat-only cluster / GPU pool, otherwise → the hub-reachable cluster / public pool). If no
	// placeholder is available in that pool, tell the admin to provision more.
	pool := poolClusterForAuthMethod(pr.AuthMethod)
	hiveID := strings.TrimSpace(approveBody.HiveID)
	if hiveID != "" {
		// Admin chose a specific placeholder — validate it is an available
		// placeholder (same check the assign path uses) before using it. The
		// full status recheck under loadSaaSHive below still guards the race.
		if sel := loadSaaSHive(hiveID); sel == nil || sel.Status != statusAvailable || !isHubAdmin(sel.Owner) {
			http.Error(w, `{"error":"selected placeholder is not available"}`, http.StatusConflict)
			return
		}
	} else {
		hiveID = findAvailablePlaceholder(pool)
	}
	if hiveID == "" {
		http.Error(w, fmt.Sprintf(`{"error":"no available placeholder hive in pool %q — provision more placeholders"}`, pool), http.StatusConflict)
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil || h.Status != statusAvailable {
		// Raced with another assignment between selection and load.
		http.Error(w, `{"error":"selected placeholder became unavailable — retry"}`, http.StatusConflict)
		return
	}

	// Reuse the request's org/repos/primary_repo/acmm as the assignment inputs.
	var repos []string
	for _, repo := range strings.Split(pr.Repos, ",") {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	primaryRepo := pr.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := pr.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		acmm = defaultAssignACMMLevel
	}

	// Rewrite the placeholder's meta.json to the requesting user's real project.
	// Flipping status to statusAssigned makes it show under the new owner in My
	// Hives AND marks it as no-longer-available for the fleet counters; the
	// project config reaches the spoke via the heartbeat channel
	// (projectConfigForHiveID).
	h.Owner = targetUsername
	h.Org = pr.Org
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	h.ACMMLevel = acmm
	// Record the level as REQUESTED, not merely as current. ACMMLevel is
	// overwritten the moment the spoke reports the level it was minted at, so it
	// cannot be the source of truth for "what did the owner ask for". Keeping the
	// requested value in its own field is what lets the delivery reconcile below
	// remain correct — and idempotent — across any number of heartbeats.
	h.RequestedACMMLevel = acmm
	// Re-arm the level handshake for this claim. A placeholder is reused across
	// assignments, so a flag left true by a previous tenancy would suppress
	// delivery for the new owner.
	h.ACMMDelivered = false
	// Re-arm the org/repos claim handshake too (#2372). ClaimDelivered gates
	// both halves of adoptSpokeProjectConfig: the org/repos PUSH fires only
	// while !ClaimDelivered, and the spoke's report is ADOPTED only once it is
	// true. A RECYCLED placeholder (previously claimed, returned to the pool)
	// carries the prior tenant's ClaimDelivered=true, so without this reset the
	// hub never pushes the new owner's org/repos AND adopts the spoke's stale
	// self-report — hub and spoke silently agree on the PREVIOUS tenant's
	// project. The heartbeat is the only hub->spoke write channel, so the claim
	// must re-arm on (re)assignment for exactly the same reason ACMMDelivered
	// does above. (The assign path — handleAssignHive — already does this.)
	h.ClaimDelivered = false
	h.Status = statusAssigned
	// Stamp when this claim began so the self-heal sweep can age it out if the
	// spoke never reports the project back (ClaimDelivered stuck false).
	h.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	h.Error = ""
	// Preserve the placeholder's real cluster before ANY cluster-derived
	// resolution below (host backfill uses s.clusterForHive(h), which silently
	// returns the hub-reachable cluster when ClusterID is blank). The placeholder was picked
	// from `pool`, so a blank cluster_id here can only mean the placeholder was
	// created without one — stamp the pool it came from rather than leaving a
	// blank that later resolves to the wrong (default) cluster's App/host.
	s.ensureClusterIDForClaim(h, pool)
	// The requester already told us which GitHub their org lives on (parsed
	// from the org URL they pasted, or picked explicitly), so honour it. Before
	// this, approve dropped github_host entirely — only the manual assign path
	// ever set it — and a GHE request approved through this path produced a
	// hive with a blank host talking to api.github.com. An override the admin
	// chose in the approve modal arrives on the request body below and wins.
	if host := strings.TrimSpace(approveBody.GitHubHost); host != "" {
		if !isValidName(host) && !strings.EqualFold(host, githubHostPublic) {
			http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
			return
		}
		// An explicit "public" choice means public github.com. Record it as a
		// blank host so forgeAPIURLForHost pushes nothing and the spoke keeps its
		// own api.github.com default — and so the cluster backfill below, which
		// only ever fills a blank, does not silently re-GHE it.
		if strings.EqualFold(host, githubHostPublic) {
			// Record public github.com EXPLICITLY. This used to store a blank
			// host plus the "public" sentinel, because a blank was the only way
			// to stop backfillGitHubHostFromCluster re-GHE-ing the hive on the
			// next line. Storing the real host achieves the same thing — that
			// backfill only ever fills a value that is EMPTY — without leaving
			// an absent field that means "public" by implication.
			//
			// An absent field is what hid the 2026-07-31 incident and what left
			// 25 of 50 hub records with no github_host at all. github_host is
			// the single stored input for a hive's identity (#2386); it should
			// never be the one field we deliberately leave blank.
			h.GitHubHost = publicForgeHost
		} else {
			h.GitHubHost = host
		}
	} else if pr.GitHubHost != "" {
		// The request itself named a host. Honour a "public" sentinel here the SAME
		// way the admin override does: the self-service onboarding form now sends
		// "public" for an explicit github.com choice (never a blank), so a
		// github.com request must be pinned public — NOT stored as the literal host
		// "public", and NOT left blank for the cluster backfill below to re-GHE.
		if strings.EqualFold(pr.GitHubHost, githubHostPublic) {
			// Same as the admin-override branch above: store the real host, not
			// a blank plus the sentinel. The literal string "public" must never
			// be stored as a host — it is a request-time marker, not a hostname,
			// and a hive naming it resolves to no forge at all.
			h.GitHubHost = publicForgeHost
		} else {
			h.GitHubHost = pr.GitHubHost
		}
	}
	// Same cluster backfill the manual assign path does: when neither the admin
	// nor the request named a host, inherit the cluster's GHE default rather
	// than leaving a blank that pushes an empty API URL forever.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to assign placeholder hive"}`, http.StatusInternalServerError)
		return
	}

	// Entry point 2/3 for namespace identity: this is the moment a placeholder
	// gets a real owner/org — the hive's identity is known here for the first
	// time, so the namespace's labels/annotations must be (re)written. That
	// stamp is kubectl against the hive's cluster, so it runs in the BACKGROUND
	// via kickClaimClusterWorkAsync below — inline it held the approve dialog's
	// fetch behind up to 2×15s of dial timeouts against an unreachable cluster
	// (the same request-path disease #2730 cured on the heartbeat path).

	// Ensure the user record exists, grant them owner access, and count this
	// owned hive against a quota.
	//
	// Granting Hives[hiveID] is what actually puts the requester on the hive's
	// permissions: handleAccessList builds the access list by scanning every
	// user record for Hives[hiveID], NOT from h.Owner. Without this the
	// assignment set h.Owner correctly but the new owner never appeared under
	// Manage Access — only the admin who provisioned the placeholder did — and
	// on a heartbeat-only cluster (the heartbeat-only cluster) that stale list is what gets
	// delivered to the spoke.
	user := loadSaaSUser(targetUsername)
	if user == nil {
		user = ensureSaaSUser(targetUsername)
	}
	if user.Hives == nil {
		user.Hives = map[string]string{}
	}
	user.Hives[hiveID] = "owner"
	user.SaaSQuota++
	applyRequestContactToUser(user, pr)
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("assigned placeholder but failed to grant owner access", "user", targetUsername, "hive", hiveID, "error", err)
	}

	// Mark the request fulfilled, recording who approved it and which hive the
	// requester actually received — that pairing is the whole point of the
	// history table, and it is unrecoverable after the fact if not stored now.
	pr.Status = provisionStatusApproved
	pr.DecidedBy = approver
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.AssignedHive = hiveID
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("assigned placeholder but failed to update provision request", "user", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request approved via placeholder assignment",
		"target", targetUsername, "by", approver, "org", pr.Org, "repos", pr.Repos,
		"hive_id", hiveID, "cluster", clusterIDForHive(h))

	// Everything the response depends on is persisted above — all fast local
	// ops. The cluster-facing side effects (namespace identity stamp; vanity
	// mint, which this path previously left to the heartbeat repair) run in the
	// background so the approve dialog gets its ack immediately; the claim
	// itself reaches the spoke over the heartbeat channel regardless.
	s.kickClaimClusterWorkAsync(hiveID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"status":  provisionStatusApproved,
		"hive_id": hiveID,
	})
}

func (s *HubServer) handleDenyProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Retain the record instead of deleting it. Deleting made a denial
	// indistinguishable from a request that was never made — the history table
	// could only ever show approvals, and an admin had no way to answer "did we
	// already turn this person down, and why?". The retained record is what
	// makes a denial re-requestable-but-accountable.
	const maxDenyRequestBodyBytes = 1 * 1024
	var denyBody struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxDenyRequestBodyBytes)
		_ = json.NewDecoder(r.Body).Decode(&denyBody) // absent body is fine
	}
	pr.Status = provisionStatusDenied
	pr.DecidedBy = denier
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.DenyReason = strings.TrimSpace(denyBody.Reason)
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("failed to record provision denial", "target", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request denied", "target", targetUsername, "by", denier)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// authMethodPrivate is the request auth_method value that routes a provision
// request to the private/GPU placeholder pool (the heartbeat-only cluster). Any other value (the
// default) routes to the public pool (the hub-reachable cluster).
const authMethodPrivate = "private"

// poolClusterForAuthMethod maps a provision request's auth_method to the
// placeholder pool it should draw from: private methods → the GPU pool
// (the heartbeat-only cluster); everything else → the public pool (the hub-reachable cluster).
func poolClusterForAuthMethod(authMethod string) string {
	if strings.EqualFold(authMethod, authMethodPrivate) || strings.EqualFold(authMethod, gpuClusterID) {
		return gpuClusterID
	}
	return defaultClusterID
}

// clusterIDForHive returns the effective cluster ID of a SaaS hive, treating an
// empty cluster_id as the default (the hub-reachable cluster) — matching clusterForHive's own
// fallback so pool matching agrees with cluster resolution.
func clusterIDForHive(h *SaaSHive) string {
	if h.ClusterID == "" {
		return defaultClusterID
	}
	return h.ClusterID
}

// findAvailablePlaceholder returns the ID of an available placeholder hive in
// the given pool (cluster), or "" if none exists. A placeholder is a SaaS hive
// owned by the hub admin, sitting at statusAvailable, on the target cluster.
func findAvailablePlaceholder(clusterID string) string {
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if !isHubAdmin(h.Owner) {
			continue
		}
		if clusterIDForHive(&h) != clusterID {
			continue
		}
		return h.ID
	}
	return ""
}

// AvailablePlaceholder is one row of the approve-picker dropdown: an available
// placeholder hive the admin can assign a provision request to.
type AvailablePlaceholder struct {
	ID          string `json:"id"`
	ClusterID   string `json:"cluster_id"`
	ProjectName string `json:"project_name"`
}

// listAvailablePlaceholders returns every available placeholder (admin-owned,
// statusAvailable), optionally filtered to a single pool (cluster). An empty
// pool returns placeholders across all pools. It mirrors findAvailablePlaceholder's
// availability predicate so the picker and the assign path agree on what's usable.
func listAvailablePlaceholders(pool string) []AvailablePlaceholder {
	var result []AvailablePlaceholder
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if !isHubAdmin(h.Owner) {
			continue
		}
		cluster := clusterIDForHive(&h)
		if pool != "" && cluster != pool {
			continue
		}
		result = append(result, AvailablePlaceholder{
			ID:          h.ID,
			ClusterID:   cluster,
			ProjectName: h.ProjectName,
		})
	}
	return result
}

// handleAvailablePlaceholders (admin-only) returns the available placeholders
// the approve-picker modal populates its dropdown from. An optional ?pool=
// filters to a single cluster; the default is all available placeholders.
func (s *HubServer) handleAvailablePlaceholders(w http.ResponseWriter, r *http.Request) {
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	placeholders := listAvailablePlaceholders(pool)
	if placeholders == nil {
		placeholders = []AvailablePlaceholder{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"placeholders": placeholders})
}

// projectConfigForHiveID returns the claimed project's real org/repos/ACMM for
// delivery to the spoke in its heartbeat response, or nil when no reconcile is
// needed. It mirrors authorizedUsersForHiveID: nil when the hive has no SaaS
// record, is still an unclaimed placeholder (statusAvailable), or the spoke
// already reports the recorded project. The spoke's currently-reported values
// (curOrg/curRepos/curPrimary/curACMM) let the hub stop sending once matched.
// adoptSpokeProjectConfig makes the hub's meta.json track what a CLAIMED hive's
// spoke reports for its OPERATOR-CONTROLLED runtime settings: org, repos,
// primary_repo, and ACMM level. Once a placeholder is claimed, the spoke
// dashboard is the source of truth for these — an operator can re-point repos or
// change the level there. Without adopting them, the reconcile in
// projectConfigForHiveID keeps re-pushing meta's old values and silently reverts
// the operator's edit every heartbeat (the spyre / Joe Runde bug — originally
// just ACMM, but org/repos have the identical failure mode).
//
// Guardrails:
//   - No-op for a hive with no SaaS record or an unclaimed placeholder
//     (statusAvailable) — assign owns those until claimed.
//   - Never adopt EMPTY/zero values: a spoke reporting empty org/repos or level 0
//     (e.g. mid-boot, before its config loads) must not wipe/downgrade meta.
//   - Only writes when something actually changed.
func (s *HubServer) adoptSpokeProjectConfig(hiveID, org string, repos []string, primary string, level int) {
	h := loadSaaSHive(hiveID)
	if h == nil || h.Status == statusAvailable {
		return // no record, or an unclaimed placeholder — assign controls it
	}
	changed := false
	prevLevel := h.ACMMLevel

	// ACMM runs its OWN delivery handshake (ACMMDelivered), not the org/repos
	// one (ClaimDelivered).
	//
	// #2061 made ACMM "operator-owned from the start — always adopt", which fixed
	// the spyre revert but left a gap on the ASSIGN path: a freshly-claimed
	// placeholder keeps reporting the level it was MINTED at, and adopting that
	// pre-delivery report overwrote the level the requester asked for. #2333
	// closed that by gating on ClaimDelivered — correct for hives claimed after
	// it shipped, but a no-op for every hive claimed BEFORE, whose
	// ClaimDelivered was already true from the old org/repos-only rule. Those
	// hives adopt the stale report on the very first beat after upgrade and the
	// push stays disabled, which is why the live oke-11 hive is still L2 against
	// an approved L3 request.
	//
	// The dedicated flag defaults to false on those hives, so the level is
	// delivered exactly once, then ownership passes to the spoke as #2061
	// intended.

	// Backfill the requested level for hives assigned before the field existed.
	// Without this the reconcile has no target on exactly the hives that need
	// it. ACMMLevel is the best available record of the assignment at this
	// point, and using it is safe: if the spoke already agrees, the delivery is
	// a no-op that simply marks itself done.
	if h.RequestedACMMLevel == 0 && h.ACMMLevel > 0 {
		h.RequestedACMMLevel = h.ACMMLevel
		changed = true
	}

	// Delivery is complete once the spoke reports the level the hub asked for.
	// A level-0 report means "too old / mid-boot to say", never a mismatch.
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 && level == h.RequestedACMMLevel {
		h.ACMMDelivered = true
		changed = true
		s.logger.Info("acmm level delivered to spoke",
			"hive_id", hiveID, "acmm_level", level)
	}

	switch {
	case level <= 0:
		// Nothing reported — say nothing, change nothing.
	case !h.ACMMDelivered && h.RequestedACMMLevel > 0:
		// Pre-delivery the REQUESTED level is authoritative. Hold meta at it so
		// the stale spoke report cannot overwrite the target the push below is
		// still working toward — the exact loop that lost the L3.
		if h.ACMMLevel != h.RequestedACMMLevel {
			h.ACMMLevel = h.RequestedACMMLevel
			changed = true
		}
		if level != h.RequestedACMMLevel {
			s.logger.Info("acmm level not yet applied on spoke; holding requested level",
				"hive_id", hiveID,
				"spoke_reports", level,
				"requested", h.RequestedACMMLevel)
		}
	case level != h.ACMMLevel:
		// Post-delivery the spoke's dashboard owns the level: adopt operator
		// edits, and keep the requested level in step so a later re-delivery
		// never reverts the operator's choice.
		h.ACMMLevel = level
		h.RequestedACMMLevel = level
		changed = true
	}

	// Org/repos: while the claim hasn't been delivered yet, the hub is still
	// PUSHING them down and the spoke may report its OLD placeholder project —
	// do NOT adopt that (it would clobber the real claim). Mark the claim
	// delivered once the spoke reports the matching org/repos, and only AFTER
	// delivery treat the spoke as the source of truth for repo edits.
	orgMatches := org != "" && strings.EqualFold(org, h.Org)
	reposMatch := len(repos) > 0 && sameStringSliceFold(repos, h.Repos)
	primaryMatches := primary == "" || strings.EqualFold(primary, h.PrimaryRepo)
	// ACMM is NO LONGER part of this condition. #2333 added it here so the claim
	// could not be declared delivered while the level was outstanding, but that
	// coupled two independent deliveries: a hive whose level lagged would also
	// have its org/repos pushed forever, and — worse — the level had no way to
	// re-arm on a hive whose ClaimDelivered was already true. ACMMDelivered
	// above now tracks the level on its own, so this returns to being purely
	// about the project payload.
	if !h.ClaimDelivered {
		if orgMatches && reposMatch && primaryMatches {
			h.ClaimDelivered = true
			changed = true
		}
	} else {
		if org != "" && !strings.EqualFold(org, h.Org) {
			h.Org = org
			changed = true
		}
		if len(repos) > 0 && !sameStringSliceFold(repos, h.Repos) {
			h.Repos = repos
			changed = true
		}
		if primary != "" && !strings.EqualFold(primary, h.PrimaryRepo) {
			h.PrimaryRepo = primary
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("failed to persist spoke-reported project config to meta",
			"hive_id", hiveID, "error", err)
		return
	}
	// Keep the in-memory registry consistent so the UI and the next
	// projectConfigForHiveID comparison see the adopted values immediately.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].Org = h.Org
			s.registry.Hives[i].Repos = h.Repos
			s.registry.Hives[i].PrimaryRepo = h.PrimaryRepo
			s.registry.Hives[i].ACMMLevel = h.ACMMLevel
			break
		}
	}
	s.mu.Unlock()
	s.logger.Info("adopted dashboard-set project config from spoke heartbeat",
		"hive_id", hiveID, "org", h.Org, "primary_repo", h.PrimaryRepo,
		"acmm_was", prevLevel, "acmm_now", h.ACMMLevel)
}

// claimedVanityURL returns the vanity dashboard URL the hub should SHOW and LINK
// for a hive (My Hives, the SSO /open handoff, config proxy), or "" to fall back
// to the spoke-reported placeholder host.
//
// The rule is deliberately narrow so it never resurrects the 503 bug:
//   - Unclaimed placeholder (statusAvailable): "" — it has no project yet, so its
//     placeholder host is the only correct URL. Leave it.
//   - Claimed hive with a non-empty meta VanityURL: return it. A vanity URL is
//     only non-empty because it was VALIDATED as servable at provision/assign
//     time (addVanityHostToIngress succeeded, or a cluster wildcard/OpenShift
//     route already serves it, e.g. the heartbeat-only cluster's hosted OpenShift-route host).
//     Trusting it here means the hub shows/links the friendly host the instant a
//     hive is claimed, instead of waiting for the spoke to adopt+report it back.
//   - Claimed hive with an empty VanityURL: "" — never mint an unvalidated host
//     here; the placeholder still works.
func claimedVanityURL(h *SaaSHive) string {
	if h == nil || h.Status == statusAvailable {
		return ""
	}
	return h.VanityURL
}

// placeholderHostURL builds the "<hiveID>.<domain>" placeholder URL for a hive
// that has not yet reported a dashboard URL, using the domain of the cluster
// the hive actually lives on.
//
// The domain MUST come from the hive's own cluster rather than the hub's
// hardcoded hive.kubestellar.io. That constant is the wildcard fronting the
// HUB's router, so using it for a spoke on any other cluster produces a name
// that resolves to the hub and 503s — the exact defect this path exhibited on
// the OpenShift pool. Deriving it per-cluster is also what keeps this correct
// for clusters added in future, without naming any of them here.
//
// Returns "" when the cluster (or its domain) is unknown, so the caller reports
// "no reachable dashboard URL yet" instead of inventing an unreachable host.
func (s *HubServer) placeholderHostURL(hiveID string) string {
	// A hive with no meta record yet (mid-provision, or a registry-only entry)
	// still resolves through clusterForHive, which falls back to the default
	// cluster. That is the correct answer for it: with nothing recorded about
	// where it lives, the hub's own pool is the only defensible guess, and it
	// is the pool such a hive is in fact provisioned into.
	cluster := s.clusterForHive(&SaaSHive{ID: hiveID})
	if h := loadSaaSHive(hiveID); h != nil {
		cluster = s.clusterForHive(h)
	}
	if cluster == nil || cluster.Domain == "" {
		return ""
	}
	return "https://" + hiveID + "." + strings.Trim(strings.TrimSpace(cluster.Domain), ".")
}

// curAPIURL is the GitHub API base URL the spoke reports it is CURRENTLY using
// (HeartbeatPayload.GitHubAPIURL). Empty means the spoke is too old to report
// it — treated as UNKNOWN, never as a mismatch.
func projectConfigForHiveID(hiveID, curOrg string, curRepos []string, curPrimary string, curACMM int, curURL, curAPIURL string) *HeartbeatProjectConfig {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil
	}
	// This reconcile exists ONLY to push a freshly-CLAIMED placeholder's project
	// down to its spoke. It must NEVER touch a pre-existing hive, whose meta.json
	// predates the claim feature and carries stale/empty fields (empty
	// primary_repo, acmm_level: 0) even though the spoke runs a real project at a
	// real ACMM. Reconciling from that stale record silently wiped org/repos and
	// DOWNGRADED live hives to L0. So we only reconcile a record that looks like a
	// genuine claim — a complete project (org + repos + primary_repo) AND a real
	// non-zero ACMM — and even then we never send a value that would blank/lower
	// what the spoke already has.
	if h.Status == statusAvailable { // still an unclaimed placeholder
		return nil
	}
	primary := h.PrimaryRepo
	if primary == "" && len(h.Repos) > 0 {
		primary = h.Repos[0]
	}
	claimComplete := h.Org != "" && len(h.Repos) > 0 && primary != "" && h.ACMMLevel > 0
	if !claimComplete {
		// Incomplete/stale record (a pre-claim hive) — leave the spoke's PROJECT
		// (org/repos/ACMM) alone; reconciling from a stale record wiped/downgraded
		// live hives. BUT the vanity URL is independent of project completeness: a
		// claimed hive can carry a validated meta vanity_url (set at provision,
		// e.g. a hive's hosted OpenShift-route on the heartbeat-only cluster) while its meta's
		// org/repos/ACMM are still stale/empty. Without pushing it, the spoke never
		// adopts the vanity URL and the hub keeps showing the raw placeholder host
		// forever (the placeholder-URL-persists bug). Push the URL alone — never
		// the stale project — until the spoke reports the vanity back.
		//
		// Safety: only a NON-EMPTY VanityURL is ever pushed. A vanity URL is only
		// non-empty because it was validated/served at provision or assign time
		// (addVanityHostToIngress succeeded, or a cluster wildcard route already
		// serves it), so this never pushes an unserved host and never reintroduces
		// the 503.
		//
		// A pending FORGE SWITCH is likewise independent of project
		// completeness — a hive can be moved between forges whether or not its
		// meta carries a complete claim, and the switch is precisely the
		// operator action that must not be silently dropped. Push the API URL
		// alone (never the stale project) until the spoke reports the requested
		// host back.
		if apiURL := pendingForgeAPIURL(h, curAPIURL); apiURL != "" {
			return &HeartbeatProjectConfig{GitHubAPIURL: apiURL}
		}
		if h.VanityURL != "" && curURL != h.VanityURL {
			return &HeartbeatProjectConfig{DashboardURL: h.VanityURL}
		}
		return nil
	}
	// What the hub still PUSHES to the spoke, and until when:
	//   - org/repos/primary_repo: pushed only until the claim is DELIVERED (the
	//     spoke first reports the assigned project). After delivery the spoke's
	//     dashboard owns them and the caller adopts operator edits instead.
	//   - vanity URL: pushed until the spoke first reports it back.
	//   - ACMM level: pushed on its OWN handshake (ACMMDelivered), until the
	//     spoke reports the requested level back. After that it is never pushed
	//     again — the spoke's dashboard owns it and the caller adopts operator
	//     edits, exactly as #2061 intended.
	//
	//     #2061 removed the ACMM push entirely because pushing it FOREVER
	//     reverted every dashboard level change (the spyre bug). Dropping it
	//     altogether meant a freshly-assigned placeholder was never told the
	//     level its owner requested. #2333 bounded the push by ClaimDelivered,
	//     which is right in principle but dead in practice for every hive
	//     claimed before it shipped: their ClaimDelivered was already true, so
	//     the push never fires. Bounding by the level's own flag is what makes
	//     the delivery reachable for those hives — and it is self-limiting, so
	//     re-running it is harmless.
	needClaimPush := !h.ClaimDelivered &&
		(!strings.EqualFold(curOrg, h.Org) ||
			!sameStringSliceFold(curRepos, h.Repos) ||
			!strings.EqualFold(curPrimary, primary))
	// Independent of the project claim: deliver the requested level until the
	// spoke confirms it. curACMM == 0 means the spoke did not report a level, so
	// there is nothing to correct yet.
	needACMMPush := !h.ACMMDelivered && h.RequestedACMMLevel > 0 && curACMM != h.RequestedACMMLevel
	// Vanity URL: the spoke always reports its dashboard URL, so observed is
	// known and an empty one is a real "I have none" rather than silence.
	needURLPush := needsPush(h.VanityURL, curURL, true)
	// A spoke reporting a repo that fails isValidRepoRef is wedged: the hub 400s
	// its every heartbeat ("invalid repo name"), /api/livez then fails on the
	// stale heartbeat and the kubelet crash-loops the pod. Push a corrected
	// project even when the claim was already delivered — otherwise the guard
	// above ("nothing left to push") leaves the hive broken forever, since a
	// wedged spoke can never report anything the hub will accept.
	needRepoRepair := false
	for _, r := range append(append([]string{}, curRepos...), curPrimary) {
		if r != "" && !isValidRepoRef(r) {
			needRepoRepair = true
			break
		}
	}
	// A hive whose GitHubHost was filled in AFTER its claim was delivered —
	// the retroactive repair, or an admin editing the host later — has
	// ClaimDelivered == true and a matching vanity URL, so every gate above is
	// false and the reconcile returns nil forever. The spoke would then keep
	// talking to api.github.com against a GitHub Enterprise org (the heartbeat-only cluster /
	// hosted-available-vllmd-01 failure). Push whenever we have a GHE API URL
	// to deliver and the spoke reports a DIFFERENT one.
	//
	// Deliberately conservative: an empty curAPIURL means the spoke is too old
	// to report its API URL, which is UNKNOWN, not a mismatch — pushing on it
	// would re-send on every beat with no read-back to ever stop it.
	wantAPIURL := forgeAPIURLForHost(h.Forge, h.GitHubHost)
	// The api_url is the field where unknown-vs-mismatch actually bites: a spoke
	// too old to report it sends "", which is NOT "I am on api.github.com". The
	// observedKnown argument carries that distinction explicitly instead of
	// hiding it in a curAPIURL != "" conjunct that a later edit could drop.
	needGHEAPIPush := needsPush(wantAPIURL, curAPIURL, curAPIURL != "")
	// A pending FORGE SWITCH pushes on its own handshake. It cannot ride
	// needGHEAPIPush: that gate is deliberately conservative about an empty
	// curAPIURL (unknown, not a mismatch) and — more importantly — it can never
	// deliver a switch TO public github.com, whose wantAPIURL is "" by
	// definition. An operator moving a hive back to github.com must be able to,
	// so the switch carries its own target and its own read-back.
	forgeAPIURL := pendingForgeAPIURL(h, curAPIURL)
	needForgePush := forgeAPIURL != ""
	if !needClaimPush && !needURLPush && !needRepoRepair && !needGHEAPIPush && !needACMMPush && !needForgePush {
		return nil // nothing left to push
	}
	// Sanitize before pushing. A repo pasted as a URL
	// ("github.ibm.com/enricom-ibm/jackrabbit") has two slashes, which
	// isValidRepoRef rejects — so the hub 400s the spoke's every heartbeat
	// ("invalid repo name"), /api/livez then fails on the stale heartbeat, and
	// the kubelet restarts the pod in a loop. Normalizing here repairs an
	// already-broken hive over the heartbeat, which is the only channel that
	// reaches a firewalled cluster (the heartbeat-only cluster).
	pushRepos := make([]string, 0, len(h.Repos))
	for _, r := range h.Repos {
		if rr := sanitizeRepoEntry(r); rr != "" {
			pushRepos = append(pushRepos, rr)
		}
	}
	if len(pushRepos) == 0 {
		pushRepos = h.Repos
	}
	pushPrimary := sanitizeRepoEntry(primary)
	if pushPrimary == "" {
		pushPrimary = primary
	}

	// Pre-delivery the REQUESTED level is what goes down the wire. The adopt path
	// also holds h.ACMMLevel at that value, so the two normally agree — but
	// stating it here means a push cannot deliver a stale level if this function
	// ever runs against a record the adopt path has not touched yet.
	pushACMM := h.ACMMLevel
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 {
		pushACMM = h.RequestedACMMLevel
	}

	return &HeartbeatProjectConfig{
		Org:          h.Org,
		Repos:        pushRepos,
		PrimaryRepo:  pushPrimary,
		ACMMLevel:    pushACMM,
		DashboardURL: h.VanityURL,
		// IssueFilter rides the claim push only when the record carries one.
		// nil (the ordinary case) tells the spoke "keep your own filter" — the
		// echo of this struct on later beats must never blank an operator's
		// locally configured project.issue_filter.
		IssueFilter: h.IssueFilter,
		// Point a GHE hive at its enterprise API. jjs-world
		// (hosted-open-source-osscar) is the working reference: a bare
		// primary_repo plus github.api_url = https://<host>/api/v3. Empty host
		// pushes nothing, so a github.com hive keeps the spoke's own default.
		// A pending forge switch wins: forgeAPIURL is the host the operator
		// asked for and is only non-empty while that delivery is outstanding.
		// Once the spoke reports the requested host, ForgeDelivered latches and
		// this falls back to the ordinary host-derived value — which by then
		// derives from the SAME host, so the two agree and nothing flaps.
		GitHubAPIURL: func() string {
			if forgeAPIURL != "" {
				return forgeAPIURL
			}
			return forgeAPIURLForHost(h.Forge, h.GitHubHost)
		}(),
		// AIAuthor is deliberately left empty here. Provisioning state never
		// knows the agents' GitHub account — the spoke owns it — and the spoke
		// treats an empty author as "leave mine alone". Setting it from this
		// struct would reintroduce the blanking bug.
	}
}

// sameStringSliceFold reports whether two string slices contain the same
// entries in the same order, case-insensitively (org/repo names are compared
// case-insensitively throughout the hub).
func sameStringSliceFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// AssignHiveRequest is the body of POST /api/saas/hives/{id}/assign. It carries
// the real project the placeholder is being claimed for, plus optional GitHub
// App credentials to deliver to the spoke via the heartbeat channel.
type AssignHiveRequest struct {
	Owner string `json:"owner"`
	Org   string `json:"org"`
	// GitHubHost is the GitHub instance the org lives on ("" = public
	// github.com, otherwise a GHE host). Parsed from a pasted org URL when the
	// caller does not send it explicitly.
	GitHubHost     string `json:"github_host,omitempty"`
	Repos          string `json:"repos"`
	PrimaryRepo    string `json:"primary_repo"`
	ProjectName    string `json:"project_name"`
	ACMMLevel      int    `json:"acmm_level"`
	IsPublic       bool   `json:"is_public"`
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey  string `json:"app_private_key"`
}

// handleAssignHive assigns an available placeholder hive to a real owner/project
// (admin-only). It rewrites the hive's meta.json to the real project and clears
// its "available" status, then delivers the new project config — and any GitHub
// App creds — to the spoke via the heartbeat response. This works uniformly for
// both reachable (the hub-reachable cluster) and heartbeat-only (the heartbeat-only cluster) clusters: NO hub→spoke
// push or kubectl is used, so a heartbeat-only-cluster claim is delivered entirely by heartbeat.
func (s *HubServer) handleAssignHive(w http.ResponseWriter, r *http.Request) {
	if !isHubAdmin(s.getAuthUser(r)) {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}
	hiveID := r.PathValue("id")

	// A GitHub App private key PEM can be a few KB, so allow more headroom than
	// the provision-request body limit.
	const maxAssignRequestBodyBytes = 16 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxAssignRequestBodyBytes)
	var body AssignHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Status != statusAvailable {
		http.Error(w, `{"error":"hive is not an available placeholder"}`, http.StatusConflict)
		return
	}

	// Validate the claimed project inputs (reuse the shared validators).
	if body.Owner == "" || !isValidName(body.Owner) {
		http.Error(w, `{"error":"invalid owner"}`, http.StatusBadRequest)
		return
	}
	// Accept a pasted org/repo URL here too — an admin assigning a hive reaches
	// for the same paste the requester did. normalizeOrgRef returns a non-empty
	// host with an EMPTY org when the field held only a forge host
	// ("github.ibm.com") and no org — clear body.Org in that case so the
	// isValidName check below rejects it, instead of the raw hostname (which
	// isValidName accepts, dots and all) silently becoming the org. That
	// host-as-org bug produced the two broken github.ibm.com claims on the heartbeat-only cluster.
	if h, o := normalizeOrgRef(body.Org); h != "" {
		body.Org = o // may be "" for a bare-host paste — rejected just below
		if body.GitHubHost == "" {
			body.GitHubHost = h
		}
	}
	if body.Org == "" || !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// "public" is the sentinel that forces public github.com even on a GHE
	// cluster; it is not a hostname, so exempt it from the hostname validator.
	if body.GitHubHost != "" && !isValidName(body.GitHubHost) && !strings.EqualFold(body.GitHubHost, githubHostPublic) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	if body.Repos == "" {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	// Single-host-per-spoke (assign path — mirrors the request path). Every repo
	// and the primary must share the spoke's host, checked on the raw pasted
	// values before normalizeRepoRef strips the host. The "public" sentinel means
	// github.com, so pass "" to the validator for it.
	{
		spokeHost := body.GitHubHost
		if strings.EqualFold(spokeHost, githubHostPublic) {
			spokeHost = ""
		}
		if err := validateSingleRepoHost(spokeHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
	}
	{
		var cleaned []string
		for _, r := range strings.Split(body.Repos, ",") {
			if rr := normalizeRepoRef(r); rr != "" {
				cleaned = append(cleaned, rr)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		if body.PrimaryRepo != "" {
			body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
		}
	}
	var repos []string
	for _, repo := range strings.Split(body.Repos, ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if !isValidRepoRef(repo) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	primaryRepo := strings.TrimSpace(body.PrimaryRepo)
	if primaryRepo == "" {
		primaryRepo = repos[0]
	} else if !isValidRepoRef(primaryRepo) {
		http.Error(w, `{"error":"invalid primary repo"}`, http.StatusBadRequest)
		return
	}
	forgeHost := body.GitHubHost
	if strings.EqualFold(forgeHost, githubHostPublic) || forgeHost == "" {
		forgeHost = "github.com"
	}
	if issue := config.ValidateProjectRepoTargets(body.Org, repos, primaryRepo, forgeHost); issue != nil {
		writeJSONError(w, http.StatusBadRequest, issue.Message)
		return
	}

	acmm := body.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		http.Error(w, `{"error":"acmm_level must be between 0 and 6"}`, http.StatusBadRequest)
		return
	}

	// Rewrite the placeholder's meta.json to the real project. Clearing status
	// (and any stale error) alone makes it show under the new owner in My Hives.
	h.Owner = body.Owner
	h.Org = body.Org
	// Preserve the placeholder's real cluster before the host backfill below,
	// which resolves via s.clusterForHive(h) and would fall back to the hub-reachable cluster on
	// a blank cluster_id. The admin assigns a specific placeholder, so its own
	// ClusterID is authoritative; only a placeholder created without one lands
	// on the default here (never a silent blank that mis-routes to the hub-reachable cluster).
	s.ensureClusterIDForClaim(h, "")
	// Record the GHE host (if any) so the heartbeat can point this spoke at the
	// right GitHub API. Never blank an existing value with an empty one.
	//
	// "public" is an explicit choice of public github.com on a cluster whose
	// defaults point at GHE. Record it as a blank host (so forgeAPIURLForHost
	// pushes nothing and the spoke keeps api.github.com) PLUS the
	// GitHubBaseURL sentinel, which is what makes effectiveGitHubBaseURL
	// resolve to "" and therefore makes the cluster backfill below decline to
	// re-GHE the hive. Without the sentinel a blank host would simply be
	// refilled from the cluster on the very next line.
	if strings.EqualFold(body.GitHubHost, githubHostPublic) {
		// Explicit public github.com, stored as the real host rather than a
		// blank + sentinel. The cluster backfill below only fills an EMPTY
		// host, so a stated value blocks it just as the blank did — and does
		// not leave a field whose absence has to be interpreted.
		h.GitHubHost = publicForgeHost
	} else if body.GitHubHost != "" {
		h.GitHubHost = body.GitHubHost
	}
	// Backfill the host from the hive's cluster when neither the request nor
	// the placeholder carries one. Placeholders provisioned BEFORE their
	// cluster gained github_base_url/github_api_url have GitHubHost == "", and
	// nothing else ever fills it in: projectConfigForHiveID pushes
	// forgeAPIURLForHost(h.Forge, h.GitHubHost), which is empty for those hives, so the
	// spoke keeps api.github.com and the public app_id even though the cluster
	// is a GHE cluster (observed on the heartbeat-only cluster: hosted-available-vllmd-01 has
	// base_url: "" / api_url: "" against a github.ibm.com cluster). The hive's
	// own value always wins; this only fills a blank.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	if body.ProjectName != "" {
		h.ProjectName = body.ProjectName
	}
	h.ACMMLevel = acmm
	// Same as the approve-provision path: the admin-assigned level is what the
	// hub must deliver, and it needs its own field because the spoke will
	// overwrite ACMMLevel with whatever it is currently running.
	h.RequestedACMMLevel = acmm
	h.ACMMDelivered = false
	h.IsPublic = body.IsPublic
	h.Status = statusAssigned
	// Stamp when this claim began so the self-heal sweep can age it out if the
	// spoke never reports the project back (ClaimDelivered stuck false).
	h.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	h.Error = ""
	// A (re)assignment is a new claim payload: reset delivery so the hub pushes
	// this project to the spoke until it reports the new org/repos back, before
	// letting the spoke's dashboard own them.
	h.ClaimDelivered = false
	// The vanity URL is NOT minted here anymore. It used to be derived and made
	// servable inline (makeVanityHostServable → kubectl against the hive's
	// cluster) between this save and the HTTP response, which held the admin's
	// assign dialog hostage for ~a minute whenever the cluster was slow or
	// unreachable (each kubectl call eats a ~45s TCP dial timeout on the
	// heartbeat-only cluster pool — the same disease #2730 cured on the
	// heartbeat path). The mint — same host preference (name-bearing Option B
	// host, org/repo fallback), same servability seam, same "never adopt an
	// unservable host" rule — now runs in the background via
	// kickClaimClusterWorkAsync below, after the response is written. Nothing
	// about the response depends on it: the claim reaches the spoke over the
	// heartbeat channel regardless, and a failed mint is retried by the
	// heartbeat-kicked repair exactly as before.
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive assignment"}`, http.StatusInternalServerError)
		return
	}

	// Grant the assignee owner access. handleAccessList builds a hive's access
	// list by scanning every user record for Hives[hiveID], NOT from h.Owner —
	// so without this the assignment set h.Owner correctly while Manage Access
	// still showed only the admin who provisioned the placeholder. On a
	// heartbeat-only cluster (the heartbeat-only cluster) that stale list is what reaches the spoke.
	assignee := loadSaaSUser(body.Owner)
	if assignee == nil {
		assignee = ensureSaaSUser(body.Owner)
	}
	if assignee.Hives == nil {
		assignee.Hives = map[string]string{}
	}
	if assignee.Hives[hiveID] != "owner" {
		assignee.Hives[hiveID] = "owner"
		assignee.SaaSQuota++
		if err := saveSaaSUser(assignee); err != nil {
			s.logger.Warn("assigned hive but failed to grant owner access", "user", body.Owner, "hive", hiveID, "error", err)
		}
	}

	// Deliver GitHub App creds (if supplied) via the SAME heartbeat channel the
	// webhook path uses — storePendingGitHubAppConfig queues them for the next
	// heartbeat response (consumePendingGitHubAppConfig in handleHeartbeat). We
	// deliberately do NOT call pushGitHubConfigToSpoke here: it requires a
	// reachable dashboardURL and would fail for the heartbeat-only cluster. The heartbeat path
	// covers both clusters uniformly.
	appDelivered := false
	if body.AppID != "" && body.InstallationID != "" && strings.TrimSpace(body.AppPrivateKey) != "" {
		appID, err1 := strconv.ParseInt(strings.TrimSpace(body.AppID), 10, 64)
		installID, err2 := strconv.ParseInt(strings.TrimSpace(body.InstallationID), 10, 64)
		if err1 != nil || err2 != nil {
			http.Error(w, `{"error":"app_id and installation_id must be numeric"}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(body.AppPrivateKey), "-----BEGIN") {
			http.Error(w, `{"error":"app_private_key must be a PEM private key"}`, http.StatusBadRequest)
			return
		}
		s.storePendingGitHubAppConfig(hiveID, &HeartbeatGitHubAppConfig{
			AppID:          appID,
			InstallationID: installID,
			PrivateKey:     strings.TrimSpace(body.AppPrivateKey),
		})
		appDelivered = true
	}

	// NO CREDS PASTED: derive the identity from the forge we already know.
	//
	// The three-way AND above is an ADMIN OVERRIDE, not the normal path — it
	// fires only when someone hand-carries an app_id, an installation_id and a
	// PEM into the dialog. Every other assignment fell through it silently and
	// left the hive on config.PlaceholderAppID, even though h.GitHubHost was
	// resolved a hundred lines earlier and the hub holds that forge's App key.
	// Deriving here is what makes "assign knows the forge, so assign sets the
	// forge identity" true in code rather than only in intent.
	if !appDelivered {
		if appCfg := s.assignTimeAppIdentity(h); appCfg != nil {
			s.storePendingGitHubAppConfig(hiveID, appCfg)
			appDelivered = true
			s.logger.Info("assign: derived github app identity from the hive's forge",
				"hive_id", hiveID,
				"forge", h.GitHubHost,
				"app_id", appCfg.AppID,
				"app_slug", appCfg.AppSlug,
				"api_url", appCfg.APIURL,
				"key_delivered", appCfg.PrivateKey != "",
			)
		} else {
			s.logger.Warn("assign: no github app identity for this hive's forge — spoke keeps the placeholder app_id and starts in dashboard-only mode",
				"hive_id", hiveID,
				"forge", h.GitHubHost,
				"cluster", clusterIDForHive(h),
				"remedy", "name an App for this forge in clusters.json, or supply app_id/installation_id/app_private_key on the assign request",
			)
		}
	}

	// The project config itself is delivered by handleHeartbeat via
	// projectConfigForHiveID on the next beat — it keeps sending until the spoke
	// reports the matching project. No hub→spoke push or kubectl is needed, so
	// this works for the heartbeat-only cluster pool as well as the hub-reachable cluster.

	s.logger.Info("audit: placeholder hive assigned",
		"hive_id", hiveID,
		"owner", h.Owner,
		"org", h.Org,
		"primary_repo", h.PrimaryRepo,
		"acmm_level", h.ACMMLevel,
		"cluster", clusterIDForHive(h),
		"app_creds_delivered", appDelivered,
	)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("hive assigned to %s (%s, ACMM %d)", h.Owner, repoDisplayLine(h.Org, h.PrimaryRepo), h.ACMMLevel),
		s.getAuthUser(r))

	// Everything the response depends on is persisted above (meta.json, owner
	// grant, pending App creds, audit/timeline) — all fast local ops. The
	// cluster-facing work (namespace identity stamp + vanity-host mint, both
	// kubectl against the hive's cluster) runs in the background so the assign
	// dialog gets its ack immediately; the row's "claim pending" indicators
	// track actual delivery, which happens over the heartbeat channel anyway.
	s.kickClaimClusterWorkAsync(hiveID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "assigned",
		"id":           hiveID,
		"owner":        h.Owner,
		"org":          h.Org,
		"primary_repo": h.PrimaryRepo,
		"acmm_level":   h.ACMMLevel,
	})
}
