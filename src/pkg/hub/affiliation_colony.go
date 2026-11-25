package hub

// Hub Affiliation enrichment from hivecommons/colony: phase 1 of
// src/docs/design/colony-customer-data-backend.md.
//
// The daily top-repo refresh (top_repo.go) derives a user's Affiliation from
// their public GitHub footprint. This file adds two colony providers to that
// refresh:
//
//   - gitdm, on by default. The hub downloads the public cncf/gitdm
//     affiliation files (the same files for everyone, nothing about any
//     person is sent) and matches the user's public github.com login against
//     them locally. Emails are never used for matching on the hub, so no
//     value derived from a private-domain email enters the Affiliation.
//   - devstats, off by default. Each lookup sends the user's public
//     github.com login, and nothing else, to CNCF DevStats. It adds the
//     user's CNCF-wide contribution count, and an affiliation should a later
//     colony release return one.
//
// Precedence for the Affiliation shown on the hub, strongest first:
//
//  1. an organization the user set on the hub (Company, source "user");
//  2. an organization an admin set for them (Company, source "admin");
//  3. the company the person wrote on their own GitHub/GHE profile;
//  4. colony gitdm, then colony devstats (labelled colony:<provider>, with
//     colony's confidence);
//  5. the heuristic GitHub footprint sources (contributions, org membership,
//     top repo owner) and the GHE/IdP fallbacks.
//
// 1 and 2 are applied when the record is read (userTopRepoAssociation), so an
// edit shows at once and a refresh can never overwrite it. 3 to 5 are decided
// by the refresh (mergeColonyAffiliation).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/colony/devstats"
	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"
)

const (
	// colonyGitdmEnv switches the gitdm provider. On by default.
	colonyGitdmEnv = "HIVE_HUB_AFFILIATION_COLONY_GITDM"
	// colonyGitdmRefreshEnv is how long a loaded gitdm snapshot is used
	// before the files are revalidated (Go duration).
	colonyGitdmRefreshEnv = "HIVE_HUB_AFFILIATION_COLONY_GITDM_REFRESH"
	// colonyDevstatsEnv switches the devstats provider. Off by default.
	colonyDevstatsEnv = "HIVE_HUB_AFFILIATION_COLONY_DEVSTATS"
	// colonyDevstatsHourlyLimitEnv caps devstats lookups across all users in
	// one hour.
	colonyDevstatsHourlyLimitEnv = "HIVE_HUB_AFFILIATION_COLONY_DEVSTATS_HOURLY_LIMIT"
	// colonyTimeoutEnv bounds one user's colony lookups (Go duration). The
	// first gitdm load downloads every affiliation file, so it is kept apart
	// from the GitHub profile budget.
	colonyTimeoutEnv = "HIVE_HUB_AFFILIATION_COLONY_TIMEOUT"

	colonyGitdmDefault               = true
	colonyDevstatsDefault            = false
	colonyGitdmRefreshDefault        = 24 * time.Hour
	colonyGitdmRefreshMin            = time.Hour
	colonyDevstatsHourlyLimitDefault = 30
	colonyDevstatsHourlyLimitMax     = 120
	colonyDevstatsWindow             = time.Hour
	colonyTimeoutDefault             = 2 * time.Minute
	colonyTimeoutMax                 = 10 * time.Minute

	// colonyCacheDirName holds colony's ETag cache of the gitdm files, beside
	// the users directory on the hub PVC.
	colonyCacheDirName = "colony-cache"

	affiliationSourceColonyGitdm    = "colony:" + gitdm.SourceName
	affiliationSourceColonyDevstats = "colony:" + devstats.SourceName
	// affiliationSourceUser and affiliationSourceAdmin label an Affiliation
	// taken from the hub Company field, by who set it.
	affiliationSourceUser  = "user"
	affiliationSourceAdmin = "admin"
)

// colonyAffiliationConfig is the hub's colony settings, read per refresh so a
// change needs no restart.
type colonyAffiliationConfig struct {
	Gitdm               bool
	GitdmRefresh        time.Duration
	Devstats            bool
	DevstatsHourlyLimit int
	Timeout             time.Duration
}

func colonyAffiliationConfigFromEnv() colonyAffiliationConfig {
	return colonyAffiliationConfig{
		Gitdm:               envOnOff(colonyGitdmEnv, colonyGitdmDefault),
		GitdmRefresh:        envDuration(colonyGitdmRefreshEnv, colonyGitdmRefreshDefault, colonyGitdmRefreshMin, 0),
		Devstats:            envOnOff(colonyDevstatsEnv, colonyDevstatsDefault),
		DevstatsHourlyLimit: envIntBounded(colonyDevstatsHourlyLimitEnv, colonyDevstatsHourlyLimitDefault, 1, colonyDevstatsHourlyLimitMax),
		Timeout:             envDuration(colonyTimeoutEnv, colonyTimeoutDefault, time.Second, colonyTimeoutMax),
	}
}

// envOnOff reads 1/true/yes/on or 0/false/no/off; anything else is def.
func envOnOff(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// envDuration reads a Go duration. A missing, invalid or below-min value is
// def; a value above max (when max > 0) is max.
func envDuration(name string, def, minimum, maximum time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name)))
	if err != nil || d < minimum {
		return def
	}
	if maximum > 0 && d > maximum {
		return maximum
	}
	return d
}

// envIntBounded reads an integer. A missing, invalid or below-min value is
// def; a value above max is max.
func envIntBounded(name string, def, minimum, maximum int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n < minimum {
		return def
	}
	if n > maximum {
		return maximum
	}
	return n
}

// colonyLookup is the part of a colony provider the hub uses.
type colonyLookup interface {
	Lookup(ctx context.Context, q resolver.Query) (resolver.Result, error)
}

// colonyAffiliator owns the colony providers for the life of the hub, so the
// gitdm snapshot is held in memory and only revalidated on its refresh
// interval. It is safe for concurrent use; the gitdm provider serializes its
// own loads, so many refreshes at once share one download.
type colonyAffiliator struct {
	mu       sync.Mutex
	cacheDir string
	refresh  time.Duration
	gitdm    colonyLookup
	devstats colonyLookup
	// newGitdm and newDevstats build the providers; tests replace them.
	newGitdm    func(cacheDir string, refresh time.Duration) colonyLookup
	newDevstats func() colonyLookup
	// devstatsSpent holds the start times of devstats lookups inside the
	// last colonyDevstatsWindow.
	devstatsSpent []time.Time
}

func newColonyAffiliator() *colonyAffiliator {
	return &colonyAffiliator{
		newGitdm: func(cacheDir string, refresh time.Duration) colonyLookup {
			return gitdm.New(gitdm.Options{CacheDir: cacheDir, Refresh: refresh})
		},
		newDevstats: func() colonyLookup { return devstats.New(devstats.Options{}) },
	}
}

func (c *colonyAffiliator) gitdmProvider(refresh time.Duration) colonyLookup {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gitdm == nil || c.refresh != refresh {
		dir := c.cacheDir
		if dir == "" {
			dir = filepath.Join(filepath.Dir(saasUsersDir), colonyCacheDirName)
		}
		c.gitdm = c.newGitdm(dir, refresh)
		c.refresh = refresh
	}
	return c.gitdm
}

func (c *colonyAffiliator) devstatsProvider() colonyLookup {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.devstats == nil {
		c.devstats = c.newDevstats()
	}
	return c.devstats
}

// reserveDevstats spends one lookup from the hourly budget, or reports that
// it is used up.
func (c *colonyAffiliator) reserveDevstats(now time.Time, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.devstatsSpent[:0]
	for _, at := range c.devstatsSpent {
		if now.Sub(at) < colonyDevstatsWindow {
			kept = append(kept, at)
		}
	}
	c.devstatsSpent = kept
	if len(kept) >= limit {
		return false
	}
	c.devstatsSpent = append(c.devstatsSpent, now)
	return true
}

// colonyAffiliation is what colony found for one user.
type colonyAffiliation struct {
	Org        string
	Source     string
	Confidence string
	// CNCFContributions is the devstats all-time CNCF contribution count;
	// zero when devstats is off, found nothing, or was not asked.
	CNCFContributions int
}

// colonyLogin is the login colony may be asked about: a public github.com
// login only. A GHE login names a different account namespace, and gitdm and
// devstats only know github.com.
func colonyLogin(p topRepoProfile) string {
	if strings.TrimRight(strings.TrimSpace(p.APIBase), "/") != githubActivityDefaultAPIURL {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(p.Login), "@")
}

// lookup asks the enabled colony providers about login. Only the login is
// used: the query never carries an email. A provider error leaves that
// provider's facts out and is returned for logging; it never fails the
// refresh.
func (c *colonyAffiliator) lookup(ctx context.Context, login string, now time.Time, cfg colonyAffiliationConfig) (colonyAffiliation, error) {
	var out colonyAffiliation
	if c == nil || login == "" {
		return out, nil
	}
	q := resolver.Query{GitHubLogin: login}
	var firstErr error
	if cfg.Gitdm {
		res, err := c.gitdmProvider(cfg.GitdmRefresh).Lookup(ctx, q)
		if err != nil {
			firstErr = err
		} else if a, ok := currentColonyAffiliation(res, now); ok {
			out.Org, out.Source, out.Confidence = a.Org, affiliationSourceColonyGitdm, string(a.Confidence)
		}
	}
	if cfg.Devstats && c.reserveDevstats(now, cfg.DevstatsHourlyLimit) {
		res, err := c.devstatsProvider().Lookup(ctx, q)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			if a, ok := currentColonyAffiliation(res, now); ok && out.Org == "" {
				out.Org, out.Source, out.Confidence = a.Org, affiliationSourceColonyDevstats, string(a.Confidence)
			}
			for _, act := range res.Activity {
				if act.Project == devstats.AllCNCF && act.Period == devstats.AllTime {
					out.CNCFContributions = act.Contributions
				}
			}
		}
	}
	return out, firstErr
}

// currentColonyAffiliation is the first affiliation in res that covers now,
// in colony's precedence order.
func currentColonyAffiliation(res resolver.Result, now time.Time) (resolver.Affiliation, bool) {
	for _, a := range res.CurrentAffiliations(now) {
		if org := truncateRunes(strings.TrimSpace(a.Org), maxContactCompanyLen); org != "" {
			a.Org = org
			return a, true
		}
	}
	return resolver.Affiliation{}, false
}

// colonyOutranksAffiliationSource reports whether a colony affiliation should
// replace one the GitHub footprint chose. The company a person wrote on their
// own profile is their own statement and stays; every other footprint source
// is a heuristic that a curated colony match beats.
func colonyOutranksAffiliationSource(source string) bool {
	return source != affiliationSourceCompany
}

// mergeColonyAffiliation folds a colony answer into the footprint result. It
// returns the association to save and whether there is anything to save: a
// colony match turns a "no data" footprint into a saveable one, while a
// transient footprint error is still reported so the old cache is kept.
func mergeColonyAffiliation(assoc topRepoAssociation, footprintErr error, col colonyAffiliation) (topRepoAssociation, error) {
	if footprintErr != nil && !isTopRepoNoData(footprintErr) {
		return assoc, footprintErr
	}
	if col.CNCFContributions > 0 {
		assoc.CNCFContributions = col.CNCFContributions
	}
	if col.Org != "" && (assoc.Affiliation == "" || colonyOutranksAffiliationSource(assoc.AffiliationSource)) {
		assoc.Affiliation = col.Org
		assoc.AffiliationSource = col.Source
		assoc.AffiliationConfidence = col.Confidence
	}
	if footprintErr != nil && assoc.Affiliation == "" && assoc.CNCFContributions == 0 {
		return assoc, footprintErr
	}
	return assoc, nil
}

func isTopRepoNoData(err error) bool {
	return errors.Is(err, errTopRepoNoData)
}

// hubSetAffiliation is the organization a person or an admin set on the hub
// record, which outranks anything derived. An explicit clear (a source with
// no value) is not a value and leaves the derived Affiliation in place on
// this admin-only view; the spoke-bound organization treats a clear as a
// stop (customerPublicOrganization).
func hubSetAffiliation(u *SaaSUser) (string, string, bool) {
	if u == nil {
		return "", "", false
	}
	v := truncateRunes(strings.TrimSpace(u.Company), maxContactCompanyLen)
	if v == "" {
		return "", "", false
	}
	switch u.CompanySource {
	case countrySourceUser:
		return v, affiliationSourceUser, true
	case countrySourceAdmin:
		return v, affiliationSourceAdmin, true
	}
	return "", "", false
}

// colonyAffiliationLookup runs the colony step of a refresh for u's profile
// under its own timeout. Errors are logged and swallowed.
func (s *HubServer) colonyAffiliationLookup(p topRepoProfile, now time.Time) colonyAffiliation {
	if s == nil || s.colonyAffiliation == nil {
		return colonyAffiliation{}
	}
	login := colonyLogin(p)
	if login == "" {
		return colonyAffiliation{}
	}
	cfg := colonyAffiliationConfigFromEnv()
	if !cfg.Gitdm && !cfg.Devstats {
		return colonyAffiliation{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	col, err := s.colonyAffiliation.lookup(ctx, login, now, cfg)
	if err != nil && s.logger != nil {
		// The login is not logged: the refresh's own debug line names the
		// user by hub id.
		s.logger.Debug("colony affiliation lookup failed", "error", err)
	}
	return col
}
