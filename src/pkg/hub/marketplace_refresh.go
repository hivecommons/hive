package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest/corpus"
)

// RFC #17620 phase 7: the marketplace's ingest refresh.
//
// Every run first seeds the curated models (marketplace_curated.go), then
// walks the CNCF survey corpus (pkg/workflowmodel/ingest/corpus, including
// its operator-added extra.json) plus HIVE_HUB_MARKETPLACE_EXTRA_REPOS, fetches each default branch, hashes the
// pipeline-file set and compares it with the last recorded run for that
// repository. Unchanged → no-op. Changed → Lifecycle → Compile → bundle →
// publish as `ingested-YYYYMMDD.N`, tier ingested-unreviewed. Curated
// versions are never touched; the index flags them newerIngestAvailable.

const (
	// hubMarketplaceExtraReposEnv is a comma-separated list of extra
	// owner/repo entries to ingest alongside the corpus.
	hubMarketplaceExtraReposEnv = "HIVE_HUB_MARKETPLACE_EXTRA_REPOS"
	// hubMarketplaceScheduleEnv is the daily refresh time as HH:MM (UTC).
	// Empty disables the nightly run; on-demand refresh still works.
	hubMarketplaceScheduleEnv     = "HIVE_HUB_MARKETPLACE_REFRESH_SCHEDULE"
	hubMarketplaceDefaultSchedule = "03:00"
	// hubMarketplaceDisableEnv turns the whole catalogue off (routes 404).
	hubMarketplaceDisableEnv = "HIVE_HUB_MARKETPLACE_DISABLED"

	marketplaceFetchTimeout = 2 * time.Minute
	// marketplaceRateFloor is the X-RateLimit-Remaining below which a run
	// waits for the reset (bounded by marketplaceRateMaxWait) before
	// continuing.
	marketplaceRateFloor   = 50
	marketplaceRateMaxWait = 10 * time.Minute
	marketplaceSeedDelay   = 30 * time.Second
	// marketplaceStaleAfter is how old a repo's refresh record may be
	// before the startup pass re-ingests it: the nightly interval, so a
	// hub that restarts repeatedly only re-crawls what the nightly missed.
	marketplaceStaleAfter = 24 * time.Hour
	// marketplaceRetryErrorAfter is how soon the startup pass retries a
	// repository whose last record is an error.
	marketplaceRetryErrorAfter = time.Hour
	// marketplaceProgressEvery is how many repos between progress log lines.
	marketplaceProgressEvery = 25
	// marketplaceFullRefreshInterval bounds on-demand full runs from
	// non-admin callers (spokes): one per interval across all callers.
	marketplaceFullRefreshInterval = time.Hour
	// marketplaceRepoRefreshBurst / Window bound on-demand single-repo
	// runs from non-admin callers.
	marketplaceRepoRefreshBurst  = 10
	marketplaceRepoRefreshWindow = time.Minute
)

// marketplaceRepoPause keeps the crawl polite: one repo at a time with a
// short gap, so a 300-repo corpus takes a few minutes and never bursts
// GitHub's secondary limits. A var so tests can shorten it.
var marketplaceRepoPause = 500 * time.Millisecond

// marketplaceTreeFetcher fetches a repository's default branch and returns
// its pipeline files (ingest.ReadTree shape) plus the commit SHA. Tests
// supply a fake; production uses the GitHub tarball endpoint.
type marketplaceTreeFetcher func(ctx context.Context, repo string) (files map[string][]byte, sha string, err error)

// marketplaceRefreshResult summarises one run.
type marketplaceRefreshResult struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Repos      int       `json:"repos"`
	Unchanged  int       `json:"unchanged"`
	Published  []string  `json:"published"`
	// Curated lists the curated name@version keys this run seeded (empty
	// once they are all present; curated entries are immutable).
	Curated []string `json:"curated,omitempty"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors"`
	Trigger string   `json:"trigger"`
}

// marketplaceRefresher owns the run lock and the last-run summary.
type marketplaceRefresher struct {
	mu      sync.Mutex
	running bool
	last    *marketplaceRefreshResult
	fetch   marketplaceTreeFetcher
	// now is injectable for deterministic version names in tests.
	now func() time.Time
	// bgCtx bounds on-demand runs started from a request; set by
	// StartMarketplaceRefresh so they stop with the other pollers.
	bgCtx context.Context
	// lastFullRequest and repoRequests back the on-demand rate limits.
	lastFullRequest time.Time
	repoRequests    []time.Time
}

// allowFullRefresh reports whether a non-admin full refresh may start now
// and, when not, how long until one may. Callers hold no lock.
func (rf *marketplaceRefresher) allowFullRefresh(now time.Time) (bool, time.Duration) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if wait := marketplaceFullRefreshInterval - now.Sub(rf.lastFullRequest); !rf.lastFullRequest.IsZero() && wait > 0 {
		return false, wait
	}
	rf.lastFullRequest = now
	return true, 0
}

// allowRepoRefresh is the sliding-window limit for non-admin ?repo= runs.
func (rf *marketplaceRefresher) allowRepoRefresh(now time.Time) (bool, time.Duration) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	kept := rf.repoRequests[:0]
	for _, t := range rf.repoRequests {
		if now.Sub(t) < marketplaceRepoRefreshWindow {
			kept = append(kept, t)
		}
	}
	rf.repoRequests = kept
	if len(kept) >= marketplaceRepoRefreshBurst {
		return false, marketplaceRepoRefreshWindow - now.Sub(kept[0])
	}
	rf.repoRequests = append(kept, now)
	return true, 0
}

// marketplaceBackgroundContext is the pollers' context when running, else
// context.Background().
func (s *HubServer) marketplaceBackgroundContext() context.Context {
	rf := s.refresher()
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.bgCtx != nil {
		return rf.bgCtx
	}
	return context.Background()
}

func (s *HubServer) marketplace() *marketplaceStore {
	s.marketplaceOnce.Do(func() {
		if s.marketplaceStore == nil {
			s.marketplaceStore = newMarketplaceStore(hubMarketplaceDir)
		}
		if s.marketplaceRefresher == nil {
			s.marketplaceRefresher = &marketplaceRefresher{fetch: s.marketplaceGitHubFetcher, now: time.Now}
		}
		if s.marketplaceRefresher.last == nil {
			s.marketplaceRefresher.last = s.marketplaceStore.loadLastRefresh()
		}
	})
	return s.marketplaceStore
}

func (s *HubServer) refresher() *marketplaceRefresher {
	s.marketplace()
	return s.marketplaceRefresher
}

// marketplaceRepos is the corpus plus HIVE_HUB_MARKETPLACE_EXTRA_REPOS,
// de-duplicated and sorted.
func marketplaceRepos() []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || !strings.Contains(r, "/") || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}
	for _, r := range corpus.Repos() {
		add(r)
	}
	for _, r := range strings.Split(os.Getenv(hubMarketplaceExtraReposEnv), ",") {
		add(r)
	}
	sort.Strings(out)
	return out
}

// marketplaceTracksRepo reports whether repo (already lower-cased and
// trimmed) is one the hub ingests on its own: a corpus entry or an
// HIVE_HUB_MARKETPLACE_EXTRA_REPOS entry.
func marketplaceTracksRepo(repo string) bool {
	for _, r := range marketplaceRepos() {
		if r == repo {
			return true
		}
	}
	return false
}

// marketplaceDisabled reports HIVE_HUB_MARKETPLACE_DISABLED.
func marketplaceDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(hubMarketplaceDisableEnv)))
	return v == "1" || v == "true" || v == "yes"
}

// Run ingests repos (all when nil) and returns the summary. A second
// caller while a run is in flight gets errMarketplaceRefreshBusy.
var errMarketplaceRefreshBusy = errors.New("marketplace: refresh already running")

func (s *HubServer) runMarketplaceRefresh(ctx context.Context, repos []string, trigger string) (*marketplaceRefreshResult, error) {
	st := s.marketplace()
	rf := s.refresher()
	rf.mu.Lock()
	if rf.running {
		rf.mu.Unlock()
		return nil, errMarketplaceRefreshBusy
	}
	rf.running = true
	rf.mu.Unlock()
	defer func() {
		rf.mu.Lock()
		rf.running = false
		rf.mu.Unlock()
	}()

	if repos == nil {
		repos = marketplaceRepos()
	}
	res := &marketplaceRefreshResult{StartedAt: rf.now().UTC(), Trigger: trigger, Published: []string{}, Errors: []string{}}
	s.logger.Info("marketplace refresh started", "trigger", trigger, "repos", len(repos))
	seeded, seedErrs := s.seedCuratedModels(st, rf.now())
	res.Curated = seeded
	for _, err := range seedErrs {
		res.Errors = append(res.Errors, "curated: "+err.Error())
	}
	for i, repo := range repos {
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "cancelled: "+ctx.Err().Error())
			break
		}
		if i > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(marketplaceRepoPause):
			}
		}
		res.Repos++
		outcome, err := s.refreshOneRepo(ctx, st, rf, repo)
		switch {
		case err != nil:
			res.Errors = append(res.Errors, repo+": "+err.Error())
			s.logger.Warn("marketplace refresh: repo failed", "repo", repo, "error", err)
		case outcome == "":
			res.Unchanged++
		case outcome == "skipped":
			res.Skipped++
		default:
			res.Published = append(res.Published, outcome)
		}
		if res.Repos%marketplaceProgressEvery == 0 && res.Repos < len(repos) {
			s.logger.Info("marketplace refresh progress", "trigger", trigger, "done", res.Repos, "total", len(repos), "published", len(res.Published), "errors", len(res.Errors))
		}
	}
	res.FinishedAt = rf.now().UTC()
	rf.mu.Lock()
	rf.last = res
	rf.mu.Unlock()
	if err := st.saveLastRefresh(res); err != nil {
		s.logger.Warn("marketplace refresh: could not persist summary", "error", err)
	}
	s.logger.Info("marketplace refresh finished", "trigger", trigger, "repos", res.Repos, "published", len(res.Published), "unchanged", res.Unchanged, "skipped", res.Skipped, "errors", len(res.Errors))
	return res, nil
}

// refreshOneRepo returns the published name@version, "" when unchanged, or
// "skipped" when the repository has no pipelines or does not compile.
func (s *HubServer) refreshOneRepo(ctx context.Context, st *marketplaceStore, rf *marketplaceRefresher, repo string) (string, error) {
	fctx, cancel := context.WithTimeout(ctx, marketplaceFetchTimeout)
	defer cancel()
	files, sha, err := rf.fetch(fctx, repo)
	if err != nil {
		_ = st.saveRefreshRecord(marketplaceRefreshRecord{Repo: repo, CheckedAt: rf.now().UTC(), Error: err.Error()})
		return "", err
	}
	hash := marketplaceFilesHash(files)
	rec := marketplaceRefreshRecord{Repo: repo, SourceSHA: sha, FilesHash: hash, Files: len(files), CheckedAt: rf.now().UTC()}
	if prev, ok := st.refreshRecord(repo); ok && prev.Format == marketplaceRecordFormat && prev.FilesHash == hash && prev.Error == "" {
		rec.Published = prev.Published
		_ = st.saveRefreshRecord(rec)
		return "", nil
	}
	if len(files) == 0 {
		_ = st.saveRefreshRecord(rec)
		return "skipped", nil
	}
	m := ingest.Lifecycle(workflowmodel.Scope{Forge: "github.com", Repository: repo}, files)
	if len(m.Spec.Pipelines) == 0 {
		_ = st.saveRefreshRecord(rec)
		return "skipped", nil
	}
	version, err := marketplaceNextIngestVersion(st, m.Metadata.Name, rf.now())
	if err != nil {
		return "", err
	}
	m.Spec.Meta.Version = version
	m.Spec.Meta.Provenance = workflowmodel.ProvenanceIngestedUnreviewed
	if m.Metadata.Annotations == nil {
		m.Metadata.Annotations = map[string]string{}
	}
	m.Metadata.Annotations["workflow.hivecommons.dev/source-sha"] = sha
	b, err := bundle.Export(m, "marketplace-refresh", rf.now())
	if err != nil {
		rec.Error = "does not compile: " + err.Error()
		_ = st.saveRefreshRecord(rec)
		return "skipped", nil
	}
	b.Tier = bundle.TierIngestedUnreviewed
	entry, created, err := st.Publish(b, "marketplace-refresh", rf.now(), sha)
	if err != nil {
		return "", err
	}
	rec.Published = entry.Key()
	if err := st.saveRefreshRecord(rec); err != nil {
		return "", err
	}
	if !created {
		return "", nil
	}
	return entry.Key(), nil
}

// marketplaceNextIngestVersion yields `ingested-YYYYMMDD.N`, N being one
// past the highest suffix already published for that name and day.
func marketplaceNextIngestVersion(st *marketplaceStore, name string, now time.Time) (string, error) {
	prefix := "ingested-" + now.UTC().Format("20060102") + "."
	versions, err := st.Versions(name)
	if err != nil {
		return "", err
	}
	max := 0
	for _, e := range versions {
		if strings.HasPrefix(e.Version, prefix) {
			if n, err := strconv.Atoi(strings.TrimPrefix(e.Version, prefix)); err == nil && n > max {
				max = n
			}
		}
	}
	return prefix + strconv.Itoa(max+1), nil
}

// marketplaceGitHubFetcher downloads the default-branch tarball through the
// hub's GitHub identity and returns the pipeline files + commit SHA.
func (s *HubServer) marketplaceGitHubFetcher(ctx context.Context, repo string) (map[string][]byte, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, "", fmt.Errorf("repo %q must be owner/name", repo)
	}
	client := hubGitHubHTTPClient()
	base := strings.TrimRight(githubAPIBase, "/")
	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := marketplaceGitHubJSON(ctx, client, base+"/repos/"+owner+"/"+name, &meta); err != nil {
		return nil, "", err
	}
	if meta.DefaultBranch == "" {
		return nil, "", errors.New("no default branch")
	}
	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := marketplaceGitHubJSON(ctx, client, base+"/repos/"+owner+"/"+name+"/branches/"+meta.DefaultBranch, &branch); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+owner+"/"+name+"/tarball/"+meta.DefaultBranch, nil)
	if err != nil {
		return nil, "", err
	}
	authGitHubRequest(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	marketplaceHonourRateLimit(ctx, resp.StatusCode, resp.Header)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("tarball: %s", resp.Status)
	}
	files, err := ingest.ReadTarball(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return files, branch.Commit.SHA, nil
}

// marketplaceGitHubJSON GETs a GitHub API resource, honouring rate-limit
// headers: when the remaining budget is under marketplaceRateFloor, or the
// reply is a 403/429 carrying Retry-After (secondary limit), it sleeps
// (bounded) before returning so the next repo does not hit the same wall.
func marketplaceGitHubJSON(ctx context.Context, client *http.Client, url string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	marketplaceHonourRateLimit(ctx, resp.StatusCode, resp.Header)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// marketplaceSleep is swapped in tests.
var marketplaceSleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func marketplaceHonourRateLimit(ctx context.Context, status int, h http.Header) {
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		// Secondary rate limits answer 403/429 with Retry-After seconds and
		// a non-zero X-RateLimit-Remaining, so the floor check below would
		// not catch them.
		if secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && secs > 0 {
			wait := time.Duration(secs) * time.Second
			if wait > marketplaceRateMaxWait {
				wait = marketplaceRateMaxWait
			}
			marketplaceSleep(ctx, wait)
			return
		}
	}
	rem, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	if err != nil || rem >= marketplaceRateFloor {
		return
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64)
	if err != nil {
		return
	}
	wait := time.Until(time.Unix(reset, 0))
	if wait <= 0 {
		return
	}
	if wait > marketplaceRateMaxWait {
		wait = marketplaceRateMaxWait
	}
	marketplaceSleep(ctx, wait)
}

// marketplaceNextRun returns the next occurrence of the HH:MM UTC schedule
// after now, or false when the schedule is empty/invalid.
func marketplaceNextRun(schedule string, now time.Time) (time.Time, bool) {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("15:04", schedule)
	if err != nil {
		return time.Time{}, false
	}
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next, true
}

// marketplaceSchedule is the configured HH:MM, defaulting to 03:00 UTC when
// the env var is unset; an explicitly empty value disables the nightly run.
func marketplaceSchedule() string {
	v, ok := os.LookupEnv(hubMarketplaceScheduleEnv)
	if !ok {
		return hubMarketplaceDefaultSchedule
	}
	return strings.TrimSpace(v)
}

// StartMarketplaceRefresh runs the nightly ingest until ctx is cancelled.
// At start (after marketplaceSeedDelay) it ingests every corpus repository
// that is due — no refresh record, a record older than marketplaceStaleAfter
// or in an older format, or an error older than marketplaceRetryErrorAfter —
// so a seed interrupted by a hub restart resumes where it stopped instead of
// waiting for the nightly. Started from StartBackgroundPollers.
func (s *HubServer) StartMarketplaceRefresh(ctx context.Context) {
	if marketplaceDisabled() {
		s.logger.Info("marketplace disabled", "env", hubMarketplaceDisableEnv)
		return
	}
	rf := s.refresher()
	rf.mu.Lock()
	rf.bgCtx = ctx
	rf.mu.Unlock()
	schedule := marketplaceSchedule()
	s.logger.Info("marketplace refresh started", "schedule_utc", schedule, "corpus_repos", len(marketplaceRepos()))
	// Curated models need no GitHub token and no delay: seed them first so
	// the catalogue is never empty while the ingest crawl is pending.
	s.seedCuratedModels(s.marketplace(), rf.now())
	if due := s.marketplaceDueRepos(); len(due) > 0 {
		s.logger.Warn("marketplace catalogue has repositories due for ingest: resuming in the background", "due", len(due), "delay", marketplaceSeedDelay.String())
		marketplaceSleep(ctx, marketplaceSeedDelay)
		if ctx.Err() == nil {
			if _, err := s.runMarketplaceRefresh(ctx, due, "seed"); err != nil {
				s.logger.Warn("marketplace seed did not run", "error", err)
			}
		}
	}
	for {
		next, ok := marketplaceNextRun(schedule, time.Now())
		if !ok {
			s.logger.Info("marketplace nightly refresh disabled (empty or invalid schedule)", "schedule", schedule)
			<-ctx.Done()
			return
		}
		marketplaceSleep(ctx, time.Until(next))
		if ctx.Err() != nil {
			return
		}
		if _, err := s.runMarketplaceRefresh(ctx, nil, "nightly"); err != nil {
			s.logger.Warn("marketplace nightly refresh skipped", "error", err)
		}
	}
}

// marketplaceRepoDue reports whether a repository needs ingesting at
// startup given its last record (ok=false when there is none).
func marketplaceRepoDue(rec marketplaceRefreshRecord, ok bool, now time.Time) bool {
	switch {
	case !ok:
		return true
	case rec.Format != marketplaceRecordFormat:
		return true
	case rec.Error != "":
		return now.Sub(rec.CheckedAt) >= marketplaceRetryErrorAfter
	default:
		return now.Sub(rec.CheckedAt) >= marketplaceStaleAfter
	}
}

// marketplaceDueRepos is the sorted subset of marketplaceRepos the startup
// pass should ingest, or nil when the hub has no GitHub token (nothing can
// be fetched, so nothing is due).
func (s *HubServer) marketplaceDueRepos() []string {
	if hubGitHubAuthTokenForRequest(context.Background()) == "" {
		return nil
	}
	st := s.marketplace()
	now := s.refresher().now().UTC()
	var due []string
	for _, repo := range marketplaceRepos() {
		rec, ok := st.refreshRecord(repo)
		if marketplaceRepoDue(rec, ok, now) {
			due = append(due, repo)
		}
	}
	return due
}

// marketplacePendingRepos counts corpus repositories with no refresh record.
func (s *HubServer) marketplacePendingRepos() []string {
	st := s.marketplace()
	var pending []string
	for _, repo := range marketplaceRepos() {
		if _, ok := st.refreshRecord(repo); !ok {
			pending = append(pending, repo)
		}
	}
	return pending
}
