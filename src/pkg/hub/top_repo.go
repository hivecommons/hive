package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var errTopRepoNoData = errors.New("top repo unavailable")

const (
	topRepoRefreshInterval  = 24 * time.Hour
	topRepoHTTPTimeout      = 10 * time.Second
	topRepoEventsPages      = 3
	topRepoEventsPerPage    = 100
	topRepoGraphQLRepos     = 25
	topRepoActivityWindow   = 90 * 24 * time.Hour
	topRepoActivityActive   = 5
	topRepoActivityProlific = 50

	affiliationSourceCompany       = "company_field"
	affiliationSourceOrgMembership = "org_membership"
	affiliationSourceContributions = "contributions"
	affiliationSourceGHE           = "ghe"
	affiliationSourceIDP           = "idp"

	publicActivityQuiet    = "quiet"
	publicActivityActive   = "active"
	publicActivityProlific = "prolific"
)

type topRepoAssociation struct {
	TopRepo           string
	TopRepoURL        string
	Affiliation       string
	AffiliationSource string
	ProfileBio        string
	ProfileLocation   string
	PublicActivity    string
}

type topRepoResolver struct {
	client *http.Client
	clock  func() time.Time
}

func userTopRepoAssociation(u *SaaSUser, _ []RegistryEntry) topRepoAssociation {
	if u == nil {
		return topRepoAssociation{}
	}
	assoc := topRepoAssociation{
		TopRepo:           strings.TrimSpace(u.TopRepo),
		TopRepoURL:        strings.TrimSpace(u.TopRepoURL),
		Affiliation:       strings.TrimSpace(u.Affiliation),
		AffiliationSource: strings.TrimSpace(u.AffiliationSource),
		ProfileBio:        strings.TrimSpace(u.ProfileBio),
		ProfileLocation:   strings.TrimSpace(u.ProfileLocation),
		PublicActivity:    strings.TrimSpace(u.PublicActivity),
	}
	if assoc.Affiliation == "" && assoc.TopRepo != "" {
		assoc.Affiliation = repoOwner(assoc.TopRepo)
		assoc.AffiliationSource = affiliationSourceContributions
	}
	return assoc
}

func (s *HubServer) queueTopRepoRefresh(u *SaaSUser, now time.Time) {
	if s == nil || u == nil || !s.topRepoRefreshEnabled || s.authProviders == nil || s.authProviders.Count() == 0 || !topRepoRefreshDue(u, now) {
		return
	}
	profile := s.topRepoProfile(u)
	if profile.Login == "" && profile.IDPFallback == "" {
		return
	}
	key := userCanonicalID(u)
	if key == "" {
		key = u.GitHubUsername
	}
	if key == "" {
		return
	}
	if _, loaded := s.topRepoRefreshInFlight.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		defer s.topRepoRefreshInFlight.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), topRepoHTTPTimeout*time.Duration(topRepoEventsPages+2))
		defer cancel()
		fresh := loadSaaSUser(key)
		if fresh == nil && key != u.GitHubUsername {
			fresh = loadSaaSUser(u.GitHubUsername)
		}
		if fresh == nil {
			return
		}
		if err := s.refreshUserTopRepo(ctx, fresh, time.Now().UTC()); err != nil && s.logger != nil {
			s.logger.Debug("affiliation refresh skipped", "user", key, "error", err)
		}
	}()
}

func topRepoRefreshDue(u *SaaSUser, now time.Time) bool {
	if u == nil {
		return false
	}
	if strings.TrimSpace(u.TopRepo) != "" && (strings.TrimSpace(u.Affiliation) == "" || strings.TrimSpace(u.AffiliationSource) == "") {
		return true
	}
	stamp := strings.TrimSpace(u.TopRepoUpdatedAt)
	if stamp == "" {
		return true
	}
	updated, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return true
	}
	return !now.Before(updated.Add(topRepoRefreshInterval))
}

func (s *HubServer) refreshUserTopRepo(ctx context.Context, u *SaaSUser, now time.Time) error {
	resolver := topRepoResolver{client: &http.Client{Timeout: topRepoHTTPTimeout}}
	assoc, err := resolver.resolve(ctx, s.topRepoProfile(u))
	if err != nil {
		if errors.Is(err, errTopRepoNoData) {
			_ = saveUserTopRepoCache(u, topRepoAssociation{}, now)
		}
		return err
	}
	return saveUserTopRepoCache(u, assoc, now)
}

func saveUserTopRepoCache(u *SaaSUser, assoc topRepoAssociation, now time.Time) error {
	if u == nil {
		return fmt.Errorf("nil user")
	}
	latest := loadSaaSUser(userCanonicalID(u))
	if latest == nil && userCanonicalID(u) != u.GitHubUsername {
		latest = loadSaaSUser(u.GitHubUsername)
	}
	if latest == nil {
		latest = u
	}
	latest.TopRepo = assoc.TopRepo
	latest.TopRepoURL = assoc.TopRepoURL
	latest.Affiliation = assoc.Affiliation
	latest.AffiliationSource = assoc.AffiliationSource
	latest.ProfileBio = assoc.ProfileBio
	latest.ProfileLocation = assoc.ProfileLocation
	latest.PublicActivity = assoc.PublicActivity
	latest.TopRepoUpdatedAt = now.UTC().Format(time.RFC3339)
	return saveSaaSUser(latest)
}

type topRepoProfile struct {
	Login       string
	APIBase     string
	WebBase     string
	GraphQLURL  string
	Token       string
	AllowEvents bool
	IDPFallback string
	GHEFallback string
}

func (s *HubServer) topRepoProfile(u *SaaSUser) topRepoProfile {
	login := topRepoProfileLogin(u)
	provider := userProvider(u)
	if provider == legacyProvider {
		if login == "" {
			return topRepoProfile{}
		}
		token := decryptStoredGitHubToken(u)
		if token != "" && !looksLikeGitHubBearer(token) {
			return topRepoProfile{}
		}
		if token == "" {
			token = hubGitHubToken()
		}
		return topRepoProfile{Login: login, APIBase: githubActivityDefaultAPIURL, WebBase: "https://github.com", GraphQLURL: "https://api.github.com/graphql", Token: token, AllowEvents: true}
	}
	fallback := idpAffiliationFallback(provider)
	if provider != "ibmid" {
		return topRepoProfile{IDPFallback: fallback}
	}
	apiBase, webBase, token, gheFallback := s.gheTopRepoEndpoint()
	if gheFallback == "" {
		gheFallback = fallback
	}
	if login == "" || apiBase == "" || token == "" {
		return topRepoProfile{IDPFallback: fallback, GHEFallback: gheFallback}
	}
	return topRepoProfile{Login: login, APIBase: apiBase, WebBase: webBase, GraphQLURL: gheGraphQLURL(apiBase), Token: token, AllowEvents: true, IDPFallback: fallback, GHEFallback: gheFallback}
}

func topRepoProfileLogin(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if login := strings.TrimSpace(u.LinkedGitHubLogin); login != "" {
		return login
	}
	provider, subject, ok := parseCanonical(userCanonicalID(u))
	if ok && provider == legacyProvider {
		return strings.TrimSpace(subject)
	}
	return ""
}

func decryptStoredGitHubToken(u *SaaSUser) string {
	if u == nil || strings.TrimSpace(u.EncryptedToken) == "" {
		return ""
	}
	token, err := decryptToken(u.EncryptedToken)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

func looksLikeGitHubBearer(token string) bool {
	token = strings.TrimSpace(token)
	return strings.HasPrefix(token, "gh") || strings.HasPrefix(token, "github_pat_")
}

func idpAffiliationFallback(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ibmid":
		return "IBM"
	default:
		return ""
	}
}

func (s *HubServer) gheTopRepoEndpoint() (apiBase, webBase, token, fallback string) {
	if s == nil {
		return "", "", "", ""
	}
	// GHE profile APIs generally require authentication. The hub currently only
	// has a reusable hub token (HIVE_HUB_GITHUB_TOKEN / dashboard config token) in
	// this process; GitHub App keys are installation-scoped and are not a user
	// profile credential, so we deliberately do not mint App tokens here.
	token = strings.TrimSpace(s.envGitHubToken)
	if token == "" {
		token = hubGitHubToken()
	}
	for _, c := range s.clusters {
		if host := forgeHostKey(c.GitHubBaseURL); host != publicForgeHost && host != "" {
			api := strings.TrimSpace(c.GitHubAPIURL)
			if api == "" {
				api = "https://" + host + "/api/v3"
			}
			base := strings.TrimSpace(c.GitHubBaseURL)
			if base == "" {
				base = "https://" + host
			}
			return strings.TrimRight(api, "/"), strings.TrimRight(base, "/"), token, gheHostAffiliation(host)
		}
		for host, id := range forgesForCluster(&c) {
			host = forgeHostKey(host)
			if host == publicForgeHost || host == "" {
				continue
			}
			api := strings.TrimSpace(id.APIURL)
			if api == "" {
				api = "https://" + host + "/api/v3"
			}
			base := strings.TrimSpace(id.BaseURL)
			if base == "" {
				base = "https://" + host
			}
			return strings.TrimRight(api, "/"), strings.TrimRight(base, "/"), token, gheHostAffiliation(host)
		}
	}
	return "", "", token, ""
}

func gheHostAffiliation(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, "ibm") {
		return "IBM"
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 2 && parts[0] == "github" {
		return strings.ToUpper(parts[1][:1]) + parts[1][1:]
	}
	return ""
}

func gheGraphQLURL(apiBase string) string {
	u, err := url.Parse(strings.TrimRight(apiBase, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/api/graphql"
}

func (r topRepoResolver) resolve(ctx context.Context, p topRepoProfile) (topRepoAssociation, error) {
	if strings.TrimSpace(p.Login) == "" || strings.TrimSpace(p.APIBase) == "" {
		if p.IDPFallback != "" {
			return topRepoAssociation{Affiliation: p.IDPFallback, AffiliationSource: affiliationSourceIDP}, nil
		}
		if p.GHEFallback != "" {
			return topRepoAssociation{Affiliation: p.GHEFallback, AffiliationSource: affiliationSourceGHE}, nil
		}
		return topRepoAssociation{}, errTopRepoNoData
	}

	profile, err := r.fetchUserProfile(ctx, p)
	if err != nil {
		return topRepoAssociation{}, err
	}
	orgs, orgsErr := r.fetchUserOrgs(ctx, p)
	if orgsErr != nil && p.GHEFallback == "" {
		orgs = nil
	}
	events, eventsErr := r.resolveEventsFootprint(ctx, p)
	contrib, err := r.resolveContributions(ctx, p, events, eventsErr)

	assoc := topRepoAssociation{
		TopRepo:         contrib.TopRepo,
		TopRepoURL:      contrib.TopRepoURL,
		ProfileBio:      strings.TrimSpace(profile.Bio),
		ProfileLocation: strings.TrimSpace(profile.Location),
		PublicActivity:  events.ActivityBucket,
	}
	// Affiliation ranking is intentionally different from a raw "top repo" trophy:
	// 1. profile company (normalised from values such as "@ibm"); profile bio and
	//    location are cached only as context for the admin tooltip, not parsed into
	//    unverifiable claims;
	// 2. strongest contributed-to repo owner/org from the public GitHub/GHE
	//    footprint, excluding the user's personal namespace;
	// 3. sole/first public org membership;
	// 4. owner of the top repo, even when that owner is the user (independent-style fallback).
	if company := normalizeCompanyAffiliation(profile.Company); company != "" {
		assoc.Affiliation = company
		assoc.AffiliationSource = affiliationSourceCompany
		return assoc, nil
	}
	if org := contrib.TopOrgExcludingLogin(p.Login); org != "" {
		assoc.Affiliation = org
		assoc.AffiliationSource = affiliationSourceContributions
		return assoc, nil
	}
	if len(orgs) > 0 {
		assoc.Affiliation = orgs[0]
		assoc.AffiliationSource = affiliationSourceOrgMembership
		return assoc, nil
	}
	if owner := repoOwner(contrib.TopRepo); owner != "" {
		assoc.Affiliation = owner
		assoc.AffiliationSource = affiliationSourceContributions
		return assoc, nil
	}
	if p.GHEFallback != "" {
		assoc.Affiliation = p.GHEFallback
		assoc.AffiliationSource = affiliationSourceGHE
		return assoc, nil
	}
	if p.IDPFallback != "" {
		assoc.Affiliation = p.IDPFallback
		assoc.AffiliationSource = affiliationSourceIDP
		return assoc, nil
	}
	if err != nil && !errors.Is(err, errTopRepoNoData) {
		return topRepoAssociation{}, err
	}
	if assoc.ProfileBio != "" || assoc.ProfileLocation != "" || assoc.PublicActivity != "" {
		return assoc, nil
	}
	return topRepoAssociation{}, errTopRepoNoData
}

type topRepoUserProfile struct {
	Company  string `json:"company"`
	Bio      string `json:"bio"`
	Location string `json:"location"`
}

func (r topRepoResolver) fetchUserProfile(ctx context.Context, p topRepoProfile) (topRepoUserProfile, error) {
	var profile topRepoUserProfile
	req, err := topRepoRequest(ctx, http.MethodGet, strings.TrimRight(p.APIBase, "/")+"/users/"+url.PathEscape(p.Login), nil, p)
	if err != nil {
		return profile, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return profile, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return profile, topRepoHTTPError{status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNotFound {
		return profile, errTopRepoNoData
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return profile, fmt.Errorf("user profile status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return profile, err
	}
	return profile, nil
}

func (r topRepoResolver) fetchUserOrgs(ctx context.Context, p topRepoProfile) ([]string, error) {
	req, err := topRepoRequest(ctx, http.MethodGet, strings.TrimRight(p.APIBase, "/")+"/users/"+url.PathEscape(p.Login)+"/orgs?per_page=100", nil, p)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, topRepoHTTPError{status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("orgs status %d", resp.StatusCode)
	}
	var out []struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	orgs := make([]string, 0, len(out))
	seen := map[string]bool{}
	for _, org := range out {
		login := strings.TrimSpace(org.Login)
		key := strings.ToLower(login)
		if login != "" && !seen[key] {
			seen[key] = true
			orgs = append(orgs, login)
		}
	}
	return orgs, nil
}

func topRepoRequest(ctx context.Context, method, target string, body io.Reader, p topRepoProfile) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	} else if strings.TrimRight(p.APIBase, "/") == githubActivityDefaultAPIURL {
		authGitHubRequest(req)
	}
	return req, nil
}

func (r topRepoResolver) resolveContributions(ctx context.Context, p topRepoProfile, events topRepoEventsFootprint, eventsErr error) (topRepoContributionSummary, error) {
	if p.Token != "" && p.GraphQLURL != "" {
		if summary, err := r.resolveGraphQL(ctx, p); err == nil && summary.TopRepo != "" {
			return summary, nil
		} else if err != nil && topRepoRateLimited(err) {
			return topRepoContributionSummary{}, err
		}
	}
	if !p.AllowEvents {
		return topRepoContributionSummary{}, errTopRepoNoData
	}
	if eventsErr != nil {
		return topRepoContributionSummary{}, eventsErr
	}
	if len(events.Summary.repoScores) == 0 {
		return topRepoContributionSummary{}, errTopRepoNoData
	}
	return events.Summary.Finalize(p.WebBase), nil
}

type topRepoHTTPError struct{ status int }

func (e topRepoHTTPError) Error() string { return fmt.Sprintf("github api status %d", e.status) }

func topRepoRateLimited(err error) bool {
	if e, ok := err.(topRepoHTTPError); ok {
		return e.status == http.StatusForbidden || e.status == http.StatusTooManyRequests
	}
	return false
}

type topRepoContributionSummary struct {
	TopRepo     string
	TopRepoURL  string
	repoScores  map[string]int
	repoRecent  map[string]time.Time
	ownerScores map[string]int
	ownerRecent map[string]time.Time
}

type topRepoEventsFootprint struct {
	Summary        topRepoContributionSummary
	Events90d      int
	ActivityBucket string
}

func newTopRepoContributionSummary() topRepoContributionSummary {
	return topRepoContributionSummary{
		repoScores:  map[string]int{},
		repoRecent:  map[string]time.Time{},
		ownerScores: map[string]int{},
		ownerRecent: map[string]time.Time{},
	}
}

func (s *topRepoContributionSummary) Add(repo string, score int, when time.Time) {
	repo = strings.TrimSpace(repo)
	if repo == "" || score <= 0 {
		return
	}
	s.repoScores[repo] += score
	if when.After(s.repoRecent[repo]) {
		s.repoRecent[repo] = when
	}
	if owner := repoOwner(repo); owner != "" {
		s.ownerScores[owner] += score
		if when.After(s.ownerRecent[owner]) {
			s.ownerRecent[owner] = when
		}
	}
}

func (s topRepoContributionSummary) Finalize(webBase string) topRepoContributionSummary {
	if len(s.repoScores) == 0 {
		return s
	}
	repos := make([]string, 0, len(s.repoScores))
	for repo := range s.repoScores {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		return topRepoRankLess(repos[i], repos[j], s.repoScores, s.repoRecent)
	})
	s.TopRepo = repos[0]
	s.TopRepoURL = repoWebURL(webBase, s.TopRepo)
	return s
}

func (s topRepoContributionSummary) TopOrgExcludingLogin(login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	owners := make([]string, 0, len(s.ownerScores))
	for owner := range s.ownerScores {
		if strings.ToLower(owner) != login {
			owners = append(owners, owner)
		}
	}
	if len(owners) == 0 {
		return ""
	}
	sort.Slice(owners, func(i, j int) bool {
		return topRepoRankLess(owners[i], owners[j], s.ownerScores, s.ownerRecent)
	})
	return owners[0]
}

func topRepoRankLess(a, b string, scores map[string]int, recent map[string]time.Time) bool {
	if scores[a] != scores[b] {
		return scores[a] > scores[b]
	}
	if !recent[a].Equal(recent[b]) {
		return recent[a].After(recent[b])
	}
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

func (r topRepoResolver) resolveGraphQL(ctx context.Context, p topRepoProfile) (topRepoContributionSummary, error) {
	body := map[string]any{
		"query":     `query($login:String!,$max:Int!){ user(login:$login){ contributionsCollection { commitContributionsByRepository(maxRepositories:$max){ repository { nameWithOwner url } contributions(first:1, orderBy:{field:OCCURRED_AT,direction:DESC}) { totalCount nodes { occurredAt } } } } } }`,
		"variables": map[string]any{"login": p.Login, "max": topRepoGraphQLRepos},
	}
	payload, _ := json.Marshal(body)
	req, err := topRepoRequest(ctx, http.MethodPost, p.GraphQLURL, bytes.NewReader(payload), p)
	if err != nil {
		return topRepoContributionSummary{}, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return topRepoContributionSummary{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return topRepoContributionSummary{}, topRepoHTTPError{status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return topRepoContributionSummary{}, fmt.Errorf("graphql status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			User *struct {
				ContributionsCollection struct {
					CommitContributionsByRepository []struct {
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
							URL           string `json:"url"`
						} `json:"repository"`
						Contributions struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
								OccurredAt time.Time `json:"occurredAt"`
							} `json:"nodes"`
						} `json:"contributions"`
					} `json:"commitContributionsByRepository"`
				} `json:"contributionsCollection"`
			} `json:"user"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return topRepoContributionSummary{}, err
	}
	if len(out.Errors) > 0 || out.Data.User == nil || len(out.Data.User.ContributionsCollection.CommitContributionsByRepository) == 0 {
		return topRepoContributionSummary{}, errTopRepoNoData
	}
	summary := newTopRepoContributionSummary()
	urls := map[string]string{}
	for _, row := range out.Data.User.ContributionsCollection.CommitContributionsByRepository {
		repo := strings.TrimSpace(row.Repository.NameWithOwner)
		if repo == "" {
			continue
		}
		score := row.Contributions.TotalCount
		if score <= 0 {
			score = 1
		}
		var when time.Time
		if len(row.Contributions.Nodes) > 0 {
			when = row.Contributions.Nodes[0].OccurredAt
		}
		summary.Add(repo, score, when)
		if row.Repository.URL != "" {
			urls[repo] = row.Repository.URL
		}
	}
	if len(summary.repoScores) == 0 {
		return topRepoContributionSummary{}, errTopRepoNoData
	}
	summary = summary.Finalize(p.WebBase)
	if urls[summary.TopRepo] != "" {
		summary.TopRepoURL = urls[summary.TopRepo]
	}
	return summary, nil
}

func (r topRepoResolver) resolveEvents(ctx context.Context, p topRepoProfile) (topRepoContributionSummary, error) {
	events, err := r.resolveEventsFootprint(ctx, p)
	if err != nil {
		return topRepoContributionSummary{}, err
	}
	if len(events.Summary.repoScores) == 0 {
		return topRepoContributionSummary{}, errTopRepoNoData
	}
	return events.Summary.Finalize(p.WebBase), nil
}

func (r topRepoResolver) resolveEventsFootprint(ctx context.Context, p topRepoProfile) (topRepoEventsFootprint, error) {
	// Events are the anonymous/public fallback. We score contribution-shaped
	// events by repo and also aggregate the same score by repo owner/org, because
	// AFFILIATION is intended to identify who the person is associated with. A
	// user active across five kubestellar/* repos therefore ranks kubestellar
	// above any individual repo. The same public events page gives a cheap 90-day
	// activity hint for just-signed-up users with zero Hive activity.
	summary := newTopRepoContributionSummary()
	events90d := 0
	windowStart := r.now().Add(-topRepoActivityWindow)
	for page := 1; page <= topRepoEventsPages; page++ {
		events, err := r.fetchEventsPage(ctx, p, page)
		if err != nil {
			return topRepoEventsFootprint{}, err
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if !ev.CreatedAt.IsZero() && !ev.CreatedAt.Before(windowStart) {
				events90d++
			}
			summary.Add(ev.Repo.Name, topRepoEventWeight(ev), ev.CreatedAt)
		}
	}
	return topRepoEventsFootprint{Summary: summary, Events90d: events90d, ActivityBucket: publicActivityBucket(events90d)}, nil
}

type topRepoEvent struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Repo      struct {
		Name string `json:"name"`
	} `json:"repo"`
	Payload struct {
		Commits []struct{} `json:"commits"`
	} `json:"payload"`
}

func (r topRepoResolver) fetchEventsPage(ctx context.Context, p topRepoProfile, page int) ([]topRepoEvent, error) {
	base := strings.TrimRight(p.APIBase, "/")
	u := fmt.Sprintf("%s/users/%s/events/public?per_page=%d&page=%d", base, url.PathEscape(p.Login), topRepoEventsPerPage, page)
	req, err := topRepoRequest(ctx, http.MethodGet, u, nil, p)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, topRepoHTTPError{status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errTopRepoNoData
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("events status %d", resp.StatusCode)
	}
	var events []topRepoEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	return events, nil
}

func topRepoEventWeight(ev topRepoEvent) int {
	switch ev.Type {
	case "PushEvent":
		if len(ev.Payload.Commits) > 0 {
			return len(ev.Payload.Commits)
		}
		return 1
	case "PullRequestEvent", "IssuesEvent", "IssueCommentEvent":
		return 1
	default:
		return 0
	}
}

func (r topRepoResolver) httpClient() *http.Client {
	if r.client != nil {
		return r.client
	}
	return &http.Client{Timeout: topRepoHTTPTimeout}
}

func (r topRepoResolver) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now().UTC()
}

func publicActivityBucket(events90d int) string {
	switch {
	case events90d >= topRepoActivityProlific:
		return publicActivityProlific
	case events90d >= topRepoActivityActive:
		return publicActivityActive
	default:
		return publicActivityQuiet
	}
}

func normalizeCompanyAffiliation(company string) string {
	company = strings.TrimSpace(company)
	if company == "" {
		return ""
	}
	company = strings.TrimLeft(company, "@")
	company = strings.TrimSpace(company)
	if strings.Contains(company, ",") {
		company = strings.TrimSpace(strings.Split(company, ",")[0])
	}
	return strings.Trim(company, "/ ")
}

func repoOwner(repo string) string {
	parts := strings.Split(strings.Trim(repo, "/ "), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func repoWebURL(webBase, label string) string {
	label = strings.Trim(label, "/ ")
	if label == "" || !strings.Contains(label, "/") {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(webBase), "/")
	if base == "" {
		base = "https://github.com"
	}
	return base + "/" + label
}
