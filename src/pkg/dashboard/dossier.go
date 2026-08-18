package dashboard

// Contributor dossier (v1) — self-service identity fields for the Leaderboard
// "Me" card (archetype / specializations / testimony / equipped title / Credly
// link / emblem seed) plus the HERALDRY endpoint that mirrors a contributor's
// OWN public Credly badges onto their dossier.
//
// Design rules (see the dossier spec):
//   - Every field is OPTIONAL and self-service; setting none is a first-class
//     state. There is no completion %, no nag, no decay.
//   - Identity for writes is resolved server-side (resolveContributeCaller),
//     exactly like the label-interests endpoint — a contributor can only ever
//     edit THEIR OWN dossier, and the identity is never taken from the body.
//   - All stored values are aggressively sanitised/bounded so a hostile payload
//     cannot bloat a profile file or smuggle markup (testimony is plain text,
//     HTML-escaped again on render).
//   - Heraldry involves NO secrets: Credly exposes every public profile as
//     JSON at credly.com/users/{vanity}/badges.json. The hub fetches it
//     server-side with a short timeout and a 6h in-memory cache; a private or
//     missing profile simply yields an empty, unlinked response.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bounds for the self-service dossier fields. Over-length submissions are
// truncated/dropped, never rejected — the save always succeeds with a safe set.
const (
	maxDossierArchetypeLen  = 40  // archetype, e.g. "Community Steward"
	maxDossierTitleLen      = 40  // equipped title, e.g. "WOLFHERDER"
	maxDossierTestimonyLen  = 200 // testimony — plain text, escaped on render
	maxDossierCredlyLen     = 60  // Credly vanity name, [a-z0-9-] only
	maxDossierEmblemSeedLen = 16  // generative-emblem seed string
	maxDossierSpecs         = 8   // specialization pills
)

// sanitizeDossierText trims, strips control characters, collapses inner
// whitespace runs, and truncates to max runes. Plain text only — any markup a
// caller submits is stored verbatim as text and HTML-escaped again at render.
func sanitizeDossierText(s string, max int) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		if r == ' ' {
			if lastSpace {
				continue
			}
			lastSpace = true
		} else {
			lastSpace = false
		}
		b.WriteRune(r)
	}
	out := []rune(strings.TrimSpace(b.String()))
	if len(out) > max {
		out = out[:max]
	}
	return string(out)
}

// sanitizeCredlyName keeps only [a-z0-9-] (lower-casing letters), matching the
// character set of Credly vanity URLs, and truncates to the bound. Anything
// else is dropped so the stored value is always safe to interpolate into the
// credly.com fetch URL.
func sanitizeCredlyName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(strings.ToLower(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= maxDossierCredlyLen {
			break
		}
	}
	return b.String()
}

// dossierUpdateRequest is the POST body for /api/contribute/dossier. Pointer
// fields make the update PARTIAL: an absent field leaves the stored value
// untouched, while an explicit empty string/list clears it — so the inline
// form can save one field at a time without wiping the rest.
type dossierUpdateRequest struct {
	Archetype       *string   `json:"archetype,omitempty"`
	Specializations *[]string `json:"specializations,omitempty"`
	Testimony       *string   `json:"testimony,omitempty"`
	EquippedTitle   *string   `json:"equipped_title,omitempty"`
	CredlyName      *string   `json:"credly_name,omitempty"`
	EmblemSeed      *string   `json:"emblem_seed,omitempty"`
}

// dossierFieldsResponse echoes the stored dossier back to the owner.
func dossierFieldsResponse(p *ContributorProfile) map[string]any {
	specs := p.Specializations
	if specs == nil {
		specs = []string{}
	}
	return map[string]any{
		"archetype":       p.Archetype,
		"specializations": specs,
		"testimony":       p.Testimony,
		"equipped_title":  p.EquippedTitle,
		"credly_name":     p.CredlyName,
		"emblem_seed":     p.EmblemSeed,
	}
}

// handleContributeDossier is the contributor-owned read/write for their OWN
// dossier fields. GET returns the caller's current dossier; POST partially
// updates it. Identity is resolved server-side (session / X-Hive-User / GitHub
// token via resolveContributeCaller — NEVER from the body). Like label
// interests, this is a self-service preference, not an operator control, so
// any registered contributor may set their own dossier.
func (s *Server) handleContributeDossier(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	username := s.resolveContributeCaller(r)
	if username == "" {
		jsonError(w, "Sign in with GitHub to edit your dossier.", http.StatusUnauthorized)
		return
	}
	profile := findContributor(username)
	if profile == nil {
		jsonError(w, "You need a contributor profile on this hive before you can edit your dossier.", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodGet {
		jsonResponse(w, dossierFieldsResponse(profile))
		return
	}

	var body dossierUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Archetype != nil {
		profile.Archetype = sanitizeDossierText(*body.Archetype, maxDossierArchetypeLen)
	}
	if body.Specializations != nil {
		specs := sanitizeLabelInterests(*body.Specializations)
		if len(specs) > maxDossierSpecs {
			specs = specs[:maxDossierSpecs]
		}
		profile.Specializations = specs
	}
	if body.Testimony != nil {
		profile.Testimony = sanitizeDossierText(*body.Testimony, maxDossierTestimonyLen)
	}
	if body.EquippedTitle != nil {
		profile.EquippedTitle = sanitizeDossierText(*body.EquippedTitle, maxDossierTitleLen)
	}
	if body.CredlyName != nil {
		profile.CredlyName = sanitizeCredlyName(*body.CredlyName)
	}
	if body.EmblemSeed != nil {
		profile.EmblemSeed = sanitizeDossierText(*body.EmblemSeed, maxDossierEmblemSeedLen)
	}
	if err := saveContributorProfile(profile); err != nil {
		s.logger.Error("failed to save contributor dossier", "username", username, "error", err)
		jsonError(w, "could not save dossier", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor dossier updated", "username", username)
	jsonResponse(w, dossierFieldsResponse(profile))
}

// ── Heraldry: the contributor's own public Credly badges ────────────────────

// HeraldryBadge is one trimmed public Credly badge — only the fields the
// dossier renders. No tokens, no emails: everything here is already public on
// the contributor's Credly profile page.
type HeraldryBadge struct {
	Name          string `json:"name"`
	IssuerSummary string `json:"issuer_summary,omitempty"`
	IssuedAt      string `json:"issued_at,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	PublicURL     string `json:"public_url,omitempty"`
}

const (
	heraldryCacheTTL = 6 * time.Hour
	// heraldryNegativeTTL bounds how long a FAILED fetch is remembered. A
	// transient network error must not pin the "unlinked" state for hours —
	// the linked invite would wrongly show for a contributor who did link.
	heraldryNegativeTTL  = 5 * time.Minute
	heraldryFetchTimeout = 5 * time.Second
	heraldryMaxBadges    = 24
)

const (
	dossierCacheMaxEntriesEnv = "HIVE_DOSSIER_CACHE_MAX_ENTRIES"
	// dossierCacheMaxEntriesDefault caps each public dossier cache. 512 entries
	// keeps normal contributor reuse hot while bounding username-spray memory.
	dossierCacheMaxEntriesDefault = 512
)

var dossierCacheMaxEntries = configuredDossierCacheMaxEntries()

func configuredDossierCacheMaxEntries() int {
	raw := strings.TrimSpace(os.Getenv(dossierCacheMaxEntriesEnv))
	if raw == "" {
		return dossierCacheMaxEntriesDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return dossierCacheMaxEntriesDefault
	}
	return n
}

// heraldryBaseURL is a var so tests can point the fetch at a local stub.
var heraldryBaseURL = "https://www.credly.com"

type heraldryCacheEntry struct {
	badges    []HeraldryBadge
	linked    bool
	fetchedAt time.Time
}

var (
	heraldryCacheMu sync.Mutex
	heraldryCache   = map[string]heraldryCacheEntry{}
	heraldryFlight  = newFetchFlight()
)

// ── Fetch de-duplication ────────────────────────────────────────────────────
//
// The dossier's upstream fetches (Credly, api.github.com) are reached from
// PUBLIC, unauthenticated endpoints, and both caches release their mutex before
// fetching. Without de-duplication, N concurrent requests for a cold or
// just-expired key each issue their own outbound call — and unauthenticated
// api.github.com allows only 60 requests/hour/IP, so a modest burst exhausts
// the budget, the failures land in the 5-minute negative cache, and the
// stampede simply re-arms. fetchFlight lets exactly one fetch per key run at a
// time; everyone else waits for it and then reads the cache it filled.
type fetchFlight struct {
	mu       sync.Mutex
	inFlight map[string]chan struct{}
}

func newFetchFlight() *fetchFlight {
	return &fetchFlight{inFlight: map[string]chan struct{}{}}
}

// do runs fn for key unless an identical call is already running, in which case
// it waits for that one instead. It returns once the cache for key is fresh.
func (f *fetchFlight) do(key string, fn func()) {
	f.mu.Lock()
	if ch, running := f.inFlight[key]; running {
		f.mu.Unlock()
		<-ch // the leader is fetching; its result lands in the cache
		return
	}
	ch := make(chan struct{})
	f.inFlight[key] = ch
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.inFlight, key)
		f.mu.Unlock()
		close(ch)
	}()
	fn()
}

// credlyBadgesPayload mirrors the slice of credly.com/users/{v}/badges.json we
// consume. Unknown fields are ignored by encoding/json.
type credlyBadgesPayload struct {
	Data []struct {
		ID            string `json:"id"`
		IssuedAt      string `json:"issued_at"`
		ImageURL      string `json:"image_url"`
		Public        bool   `json:"public"`
		BadgeTemplate struct {
			Name     string `json:"name"`
			ImageURL string `json:"image_url"`
		} `json:"badge_template"`
		Issuer struct {
			Summary string `json:"summary"`
		} `json:"issuer"`
	} `json:"data"`
}

// fetchCredlyBadges pulls + trims a contributor's public Credly badges. A short
// timeout and an explicit User-Agent (Credly 404s UA-less requests) keep this
// polite; any failure returns an error so the caller can degrade to unlinked.
func fetchCredlyBadges(credlyName string) ([]HeraldryBadge, error) {
	url := heraldryBaseURL + "/users/" + credlyName + "/badges.json"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hive-dashboard-heraldry")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: heraldryFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("credly returned %d", resp.StatusCode)
	}
	var payload credlyBadgesPayload
	// Bound how much of the upstream response we will parse, so a hostile or
	// misbehaving upstream cannot balloon memory.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]HeraldryBadge, 0, len(payload.Data))
	for _, b := range payload.Data {
		if !b.Public {
			continue // respect per-badge privacy set on Credly
		}
		name := b.BadgeTemplate.Name
		if name == "" {
			continue
		}
		img := b.ImageURL
		if img == "" {
			img = b.BadgeTemplate.ImageURL
		}
		badge := HeraldryBadge{
			Name:          name,
			IssuerSummary: b.Issuer.Summary,
			IssuedAt:      b.IssuedAt,
			ImageURL:      img,
		}
		if b.ID != "" {
			badge.PublicURL = "https://www.credly.com/badges/" + b.ID
		}
		out = append(out, badge)
		if len(out) >= heraldryMaxBadges {
			break
		}
	}
	return out, nil
}

func effectiveDossierCacheMaxEntries() int {
	if dossierCacheMaxEntries <= 0 {
		return dossierCacheMaxEntriesDefault
	}
	return dossierCacheMaxEntries
}

func heraldryEntryExpired(e heraldryCacheEntry, now time.Time) bool {
	ttl := heraldryCacheTTL
	if !e.linked {
		ttl = heraldryNegativeTTL
	}
	return now.Sub(e.fetchedAt) >= ttl
}

func pruneHeraldryCacheLocked(now time.Time) {
	for k, e := range heraldryCache {
		if heraldryEntryExpired(e, now) {
			delete(heraldryCache, k)
		}
	}
}

func enforceHeraldryCacheBoundLocked() {
	maxEntries := effectiveDossierCacheMaxEntries()
	for len(heraldryCache) > maxEntries {
		var oldestKey string
		var oldestFetchedAt time.Time
		for k, e := range heraldryCache {
			if oldestKey == "" || e.fetchedAt.Before(oldestFetchedAt) {
				oldestKey = k
				oldestFetchedAt = e.fetchedAt
			}
		}
		delete(heraldryCache, oldestKey)
	}
}

func storeHeraldryCache(credlyName string, entry heraldryCacheEntry) {
	heraldryCacheMu.Lock()
	defer heraldryCacheMu.Unlock()
	heraldryCache[credlyName] = entry
	pruneHeraldryCacheLocked(time.Now())
	enforceHeraldryCacheBoundLocked()
}

// heraldryCached returns a live cache entry for credlyName, if one exists and
// has not aged out.
func heraldryCached(credlyName string) ([]HeraldryBadge, bool, bool) {
	heraldryCacheMu.Lock()
	defer heraldryCacheMu.Unlock()
	e, ok := heraldryCache[credlyName]
	if !ok {
		return nil, false, false
	}
	if heraldryEntryExpired(e, time.Now()) {
		delete(heraldryCache, credlyName)
		return nil, false, false
	}
	return e.badges, e.linked, true
}

// heraldryFor returns the cached-or-fetched public badges for one credly name.
func heraldryFor(credlyName string) ([]HeraldryBadge, bool) {
	if badges, linked, ok := heraldryCached(credlyName); ok {
		return badges, linked
	}

	heraldryFlight.do(credlyName, func() {
		// Re-check: we may have waited behind a leader that just refreshed.
		if _, _, ok := heraldryCached(credlyName); ok {
			return
		}
		badges, err := fetchCredlyBadges(credlyName)
		if badges == nil {
			badges = []HeraldryBadge{}
		}
		storeHeraldryCache(credlyName, heraldryCacheEntry{badges: badges, linked: err == nil, fetchedAt: time.Now()})
	})

	if badges, linked, ok := heraldryCached(credlyName); ok {
		return badges, linked
	}
	return []HeraldryBadge{}, false
}

// ── GitHub public enrichment: service years + renown (followers) ────────────
//
// The dossier's Deeds grid shows two public GitHub facts — account age
// ("github service" years) and follower count ("renown"). Both come from the
// public, unauthenticated https://api.github.com/users/{username} endpoint,
// fetched server-side with the SAME polite posture as heraldry: short timeout,
// explicit User-Agent, and a 6h in-memory cache (failures are cached too, so a
// missing account cannot trigger a fetch per page view). On any failure the
// fields are simply omitted from the profile response — never fabricated.

const (
	githubUserCacheTTL     = 6 * time.Hour
	githubUserFetchTimeout = 5 * time.Second
)

// githubUserAPIBase is a var so tests can point the fetch at a local stub.
var githubUserAPIBase = "https://api.github.com"

type githubUserCacheEntry struct {
	serviceYears int
	followers    int
	ok           bool
	fetchedAt    time.Time
}

var (
	githubUserCacheMu sync.Mutex
	githubUserCache   = map[string]githubUserCacheEntry{}
	githubUserFlight  = newFetchFlight()
)

// githubPublicUserPayload mirrors the two public fields we consume.
type githubPublicUserPayload struct {
	CreatedAt string `json:"created_at"`
	Followers int    `json:"followers"`
}

// fetchGitHubPublicUser pulls the public user record and derives whole years
// of service from created_at. Any failure returns an error so the caller can
// degrade to "omit the fields".
func fetchGitHubPublicUser(username string) (serviceYears, followers int, err error) {
	req, err := http.NewRequest(http.MethodGet, githubUserAPIBase+"/users/"+username, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "hive-dashboard-dossier")
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: githubUserFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("github returned %d", resp.StatusCode)
	}
	var payload githubPublicUserPayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return 0, 0, err
	}
	created, err := time.Parse(time.RFC3339, payload.CreatedAt)
	if err != nil {
		return 0, 0, err
	}
	years := int(time.Since(created).Hours() / (24 * 365.25))
	if years < 0 {
		years = 0
	}
	return years, payload.Followers, nil
}

func githubUserEntryExpired(e githubUserCacheEntry, now time.Time) bool {
	ttl := githubUserCacheTTL
	if !e.ok {
		// A transient failure must not hide service-years/renown for hours.
		ttl = heraldryNegativeTTL
	}
	return now.Sub(e.fetchedAt) >= ttl
}

func pruneGitHubUserCacheLocked(now time.Time) {
	for k, e := range githubUserCache {
		if githubUserEntryExpired(e, now) {
			delete(githubUserCache, k)
		}
	}
}

func enforceGitHubUserCacheBoundLocked() {
	maxEntries := effectiveDossierCacheMaxEntries()
	for len(githubUserCache) > maxEntries {
		var oldestKey string
		var oldestFetchedAt time.Time
		for k, e := range githubUserCache {
			if oldestKey == "" || e.fetchedAt.Before(oldestFetchedAt) {
				oldestKey = k
				oldestFetchedAt = e.fetchedAt
			}
		}
		delete(githubUserCache, oldestKey)
	}
}

func storeGitHubUserCache(username string, entry githubUserCacheEntry) {
	githubUserCacheMu.Lock()
	defer githubUserCacheMu.Unlock()
	githubUserCache[username] = entry
	pruneGitHubUserCacheLocked(time.Now())
	enforceGitHubUserCacheBoundLocked()
}

// githubUserCached returns a live cache entry for username, if one exists and
// has not aged out.
func githubUserCached(username string) (int, int, bool, bool) {
	githubUserCacheMu.Lock()
	defer githubUserCacheMu.Unlock()
	e, ok := githubUserCache[username]
	if !ok {
		return 0, 0, false, false
	}
	if githubUserEntryExpired(e, time.Now()) {
		delete(githubUserCache, username)
		return 0, 0, false, false
	}
	return e.serviceYears, e.followers, e.ok, true
}

// githubPublicFor returns the cached-or-fetched public GitHub facts for one
// username: (serviceYears, followers, ok). ok=false means the fields should be
// omitted from the profile response.
func githubPublicFor(username string) (int, int, bool) {
	if years, followers, ok, fresh := githubUserCached(username); fresh {
		return years, followers, ok
	}

	githubUserFlight.do(username, func() {
		// Re-check: we may have waited behind a leader that just refreshed.
		if _, _, _, fresh := githubUserCached(username); fresh {
			return
		}
		years, followers, err := fetchGitHubPublicUser(username)
		storeGitHubUserCache(username, githubUserCacheEntry{
			serviceYears: years, followers: followers, ok: err == nil, fetchedAt: time.Now(),
		})
	})

	years, followers, ok, _ := githubUserCached(username)
	return years, followers, ok
}

// handleContributorHeraldry serves
// GET /api/leaderboard/contributor/{username}/heraldry —
// the trimmed public Credly badges for one contributor. Public read (the badge
// data is already public on credly.com); the hub is only a caching mirror. A
// contributor with no credly_name — or whose profile fails to fetch — gets
// {"badges":[],"linked":false}, which the client renders as the quiet invite.
func (s *Server) handleContributorHeraldry(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if !validGitHubUsername(username) {
		jsonError(w, "invalid username", http.StatusBadRequest)
		return
	}
	unlinked := map[string]any{"badges": []HeraldryBadge{}, "linked": false}
	profile := findContributor(username)
	if profile == nil || profile.CredlyName == "" {
		jsonResponse(w, unlinked)
		return
	}
	badges, linked := heraldryFor(profile.CredlyName)
	if !linked {
		jsonResponse(w, unlinked)
		return
	}
	jsonResponse(w, map[string]any{"badges": badges, "linked": true, "credly_name": profile.CredlyName})
}
