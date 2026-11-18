package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Review preflight (hivecommons/hive#19069): facts the hive gathers before the
// reviewer reads a PR, so a review can rest on the repository's own rules and
// on dependency data instead of on the diff alone. Hive-wide defaults live
// under review.preflight; a repository overrides them under
// repos.<repo>.review_preflight.

const (
	// ReviewPreflightOfflineEnv forces every network lookup of the evidence
	// builder off when set to "true", whatever the file says. It is the
	// switch for an air-gapped hive that cannot edit hive.yaml per host.
	ReviewPreflightOfflineEnv = "HIVE_REVIEW_PREFLIGHT_OFFLINE"

	// DefaultReviewPreflightOSVURL is the vulnerability database queried when
	// review.preflight.vulnerabilities is on.
	DefaultReviewPreflightOSVURL = "https://api.osv.dev"
	// DefaultReviewPreflightMinAgeDays is the release age below which a new
	// dependency version is called out as inside the supply-chain risk window.
	DefaultReviewPreflightMinAgeDays = 14
	// DefaultReviewPreflightRuleDocBudgetBytes bounds how many bytes of rule
	// documents one review is told to read in total.
	DefaultReviewPreflightRuleDocBudgetBytes = 60000
	// MinReviewPreflightRuleDocBudgetBytes is the smallest accepted budget.
	MinReviewPreflightRuleDocBudgetBytes = 1000
	// DefaultReviewPreflightCacheTTL is how long a registry or advisory answer
	// is reused so repeated reviews do not hammer the services.
	DefaultReviewPreflightCacheTTL = 6 * time.Hour
	// DefaultReviewPreflightRequestTimeout bounds one outbound request.
	DefaultReviewPreflightRequestTimeout = 10 * time.Second
	// DefaultReviewPreflightMaxDependencies caps how many changed dependencies
	// one PR looks up; the rest are listed as not checked.
	DefaultReviewPreflightMaxDependencies = 30
	// MaxReviewPreflightRules caps operator-defined rules per scope.
	MaxReviewPreflightRules = 30
	// MaxReviewPreflightRuleTextLen caps one rule's text.
	MaxReviewPreflightRuleTextLen = 500
	// DefaultReviewPreflightSeverityFloor mentions every advisory.
	DefaultReviewPreflightSeverityFloor = "low"
	// DefaultReviewPreflightBlockSeverity is the advisory severity from which
	// the reviewer treats a vulnerable dependency as an in-scope blocker.
	DefaultReviewPreflightBlockSeverity = "high"
	// ReviewPreflightIgnoreReleaseAge is the ignore id that silences the
	// release-age note for a package.
	ReviewPreflightIgnoreReleaseAge = "release-age"
	// ReviewPreflightDateLayout is the layout of an ignore entry's expiry.
	ReviewPreflightDateLayout = "2006-01-02"
)

// ReviewPreflightSeverities lists the accepted severities, lowest first.
var ReviewPreflightSeverities = []string{"low", "medium", "high", "critical"}

// DefaultReviewPreflightRuleDocPatterns are the rule documents looked for in a
// repository when review.preflight.rule_docs is on. Matching is case
// insensitive; a pattern without a slash matches at the repository root only;
// "dir/**" matches any text file below dir.
var DefaultReviewPreflightRuleDocPatterns = []string{
	"AGENTS.md",
	"CLAUDE.md",
	"CONTRIBUTING*",
	"DESIGN*.md",
	"ARCHITECTURE*.md",
	"STYLE*.md",
	"CODEOWNERS",
	".github/CODEOWNERS",
	".github/copilot-instructions.md",
	".github/CONTRIBUTING*",
	".cursor/rules/**",
	"docs/design/**",
	"docs/adr/**",
	"docs/architecture/**",
	"docs/CODEOWNERS",
}

// ReviewPreflightRule is a short operator-defined rule the reviewer enforces.
type ReviewPreflightRule struct {
	ID       string `yaml:"id,omitempty" json:"id,omitempty"`
	Text     string `yaml:"text,omitempty" json:"text,omitempty"`
	Severity string `yaml:"severity,omitempty" json:"severity,omitempty"`
}

// ReviewPreflightIgnore accepts a known advisory (or the release-age note) for
// a package, with a reason and an optional expiry.
type ReviewPreflightIgnore struct {
	ID      string `yaml:"id,omitempty" json:"id,omitempty"`
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
	Expires string `yaml:"expires,omitempty" json:"expires,omitempty"`
}

// ReviewPreflightConfig is review.preflight and repos.<repo>.review_preflight.
// Every field is optional; an unset field inherits (repo from hive-wide, hive
// from the documented default).
type ReviewPreflightConfig struct {
	// RuleDocs turns rule-document discovery on or off. Unset means on: it
	// uses the GitHub API the hive already talks to and sends nothing new.
	RuleDocs *bool `yaml:"rule_docs,omitempty" json:"rule_docs,omitempty"`
	// RuleDocPaths adds patterns to DefaultReviewPreflightRuleDocPatterns.
	RuleDocPaths []string `yaml:"rule_doc_paths,omitempty" json:"rule_doc_paths,omitempty"`
	// RuleDocExclude removes any document matching one of these patterns.
	RuleDocExclude []string `yaml:"rule_doc_exclude,omitempty" json:"rule_doc_exclude,omitempty"`
	// RuleDocBudgetBytes is the total bytes of rule documents a review reads.
	RuleDocBudgetBytes int `yaml:"rule_doc_budget_bytes,omitempty" json:"rule_doc_budget_bytes,omitempty"`
	// Rules are named rules the reviewer enforces. Repo entries are merged
	// with hive-wide ones by id; a repo entry wins.
	Rules []ReviewPreflightRule `yaml:"rules,omitempty" json:"rules,omitempty"`

	// Dependencies turns on the dependency checks. Unset means OFF: they send
	// package names and versions to third-party services.
	Dependencies *bool `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	// Vulnerabilities queries OSV for each changed dependency. Unset means on
	// when Dependencies is on.
	Vulnerabilities *bool `yaml:"vulnerabilities,omitempty" json:"vulnerabilities,omitempty"`
	// ReleaseAge looks up each changed dependency's publish date. Unset means
	// on when Dependencies is on.
	ReleaseAge *bool `yaml:"release_age,omitempty" json:"release_age,omitempty"`
	// Offline reports every network lookup as "could not check: offline".
	Offline *bool `yaml:"offline,omitempty" json:"offline,omitempty"`
	// OSVURL overrides DefaultReviewPreflightOSVURL (an internal mirror).
	OSVURL string `yaml:"osv_url,omitempty" json:"osv_url,omitempty"`
	// RegistryURLs overrides a registry base URL by ecosystem id (go, npm,
	// pypi, crates, maven, rubygems, packagist, nuget).
	RegistryURLs map[string]string `yaml:"registry_urls,omitempty" json:"registry_urls,omitempty"`
	// MinReleaseAgeDays is the supply-chain window. 0 means the default (14),
	// a negative value disables the age note.
	MinReleaseAgeDays int `yaml:"min_release_age_days,omitempty" json:"min_release_age_days,omitempty"`
	// SeverityFloor is the lowest advisory severity mentioned (low, medium,
	// high, critical). Advisories with no stated severity are always listed.
	SeverityFloor string `yaml:"severity_floor,omitempty" json:"severity_floor,omitempty"`
	// BlockSeverity is the severity from which an advisory is an in-scope
	// blocker rather than an informational note.
	BlockSeverity string `yaml:"block_severity,omitempty" json:"block_severity,omitempty"`
	// CacheTTL is a Go duration string; empty means the default (6h).
	CacheTTL string `yaml:"cache_ttl,omitempty" json:"cache_ttl,omitempty"`
	// RequestTimeout is a Go duration string; empty means the default (10s).
	RequestTimeout string `yaml:"request_timeout,omitempty" json:"request_timeout,omitempty"`
	// MaxDependencies caps lookups per PR; 0 means the default (30).
	MaxDependencies int `yaml:"max_dependencies,omitempty" json:"max_dependencies,omitempty"`
	// Ignore lists accepted advisories. Repo entries are merged with
	// hive-wide ones; an entry with the same id and package is replaced.
	Ignore []ReviewPreflightIgnore `yaml:"ignore,omitempty" json:"ignore,omitempty"`
}

// ReviewPreflightResolved is the effective evidence policy for one repository.
type ReviewPreflightResolved struct {
	RuleDocs           bool
	RuleDocPatterns    []string
	RuleDocExclude     []string
	RuleDocBudgetBytes int
	Rules              []ReviewPreflightRule

	Dependencies      bool
	Vulnerabilities   bool
	ReleaseAge        bool
	Offline           bool
	OSVURL            string
	RegistryURLs      map[string]string
	MinReleaseAgeDays int // 0 disables the age note
	SeverityFloor     string
	BlockSeverity     string
	CacheTTL          time.Duration
	RequestTimeout    time.Duration
	MaxDependencies   int
	Ignore            []ReviewPreflightIgnore
}

func pfBoolOr(over, base *bool, def bool) bool {
	switch {
	case over != nil:
		return *over
	case base != nil:
		return *base
	}
	return def
}

func pfStrOr(over, base, def string) string {
	if s := strings.TrimSpace(over); s != "" {
		return s
	}
	if s := strings.TrimSpace(base); s != "" {
		return s
	}
	return def
}

func pfDurOr(over, base string, def time.Duration) time.Duration {
	for _, raw := range []string{over, base} {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func pfIntOr(over, base, def int) int {
	if over != 0 {
		return over
	}
	if base != 0 {
		return base
	}
	return def
}

func pfSeverityOr(over, base, def string) string {
	for _, s := range []string{over, base} {
		s = strings.ToLower(strings.TrimSpace(s))
		for _, ok := range ReviewPreflightSeverities {
			if s == ok {
				return s
			}
		}
	}
	return def
}

// ResolveReviewPreflight merges the hive-wide block with a repo override.
// Precedence per field: repo, then hive-wide, then the default. Lists replace
// (the repo's list wins when set) except rule_doc_paths, rules and ignore,
// which add to the hive-wide entries. A list set to an empty value in the repo
// still inherits; use rule_doc_exclude to remove documents.
func ResolveReviewPreflight(hive, repo ReviewPreflightConfig) ReviewPreflightResolved {
	out := ReviewPreflightResolved{
		RuleDocs:           pfBoolOr(repo.RuleDocs, hive.RuleDocs, true),
		RuleDocBudgetBytes: pfIntOr(repo.RuleDocBudgetBytes, hive.RuleDocBudgetBytes, DefaultReviewPreflightRuleDocBudgetBytes),
		Dependencies:       pfBoolOr(repo.Dependencies, hive.Dependencies, false),
		Vulnerabilities:    pfBoolOr(repo.Vulnerabilities, hive.Vulnerabilities, true),
		ReleaseAge:         pfBoolOr(repo.ReleaseAge, hive.ReleaseAge, true),
		Offline:            pfBoolOr(repo.Offline, hive.Offline, false) || strings.EqualFold(strings.TrimSpace(os.Getenv(ReviewPreflightOfflineEnv)), "true"),
		OSVURL:             strings.TrimRight(pfStrOr(repo.OSVURL, hive.OSVURL, DefaultReviewPreflightOSVURL), "/"),
		SeverityFloor:      pfSeverityOr(repo.SeverityFloor, hive.SeverityFloor, DefaultReviewPreflightSeverityFloor),
		BlockSeverity:      pfSeverityOr(repo.BlockSeverity, hive.BlockSeverity, DefaultReviewPreflightBlockSeverity),
		CacheTTL:           pfDurOr(repo.CacheTTL, hive.CacheTTL, DefaultReviewPreflightCacheTTL),
		RequestTimeout:     pfDurOr(repo.RequestTimeout, hive.RequestTimeout, DefaultReviewPreflightRequestTimeout),
		MaxDependencies:    pfIntOr(repo.MaxDependencies, hive.MaxDependencies, DefaultReviewPreflightMaxDependencies),
	}
	if out.RuleDocBudgetBytes < MinReviewPreflightRuleDocBudgetBytes {
		out.RuleDocBudgetBytes = DefaultReviewPreflightRuleDocBudgetBytes
	}
	if out.MaxDependencies < 0 {
		out.MaxDependencies = DefaultReviewPreflightMaxDependencies
	}
	switch days := pfIntOr(repo.MinReleaseAgeDays, hive.MinReleaseAgeDays, DefaultReviewPreflightMinAgeDays); {
	case days > 0:
		out.MinReleaseAgeDays = days
	}
	out.RuleDocPatterns = append(append([]string(nil), DefaultReviewPreflightRuleDocPatterns...), hive.RuleDocPaths...)
	out.RuleDocPatterns = append(out.RuleDocPatterns, repo.RuleDocPaths...)
	out.RuleDocExclude = hive.RuleDocExclude
	if len(repo.RuleDocExclude) > 0 {
		out.RuleDocExclude = repo.RuleDocExclude
	}
	out.RegistryURLs = map[string]string{}
	for _, m := range []map[string]string{hive.RegistryURLs, repo.RegistryURLs} {
		for k, v := range m {
			if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
				out.RegistryURLs[strings.ToLower(strings.TrimSpace(k))] = v
			}
		}
	}
	out.Rules = mergeEvidenceRules(hive.Rules, repo.Rules)
	out.Ignore = mergeEvidenceIgnores(hive.Ignore, repo.Ignore)
	return out
}

func mergeEvidenceRules(hive, repo []ReviewPreflightRule) []ReviewPreflightRule {
	var out []ReviewPreflightRule
	index := map[string]int{}
	for _, list := range [][]ReviewPreflightRule{hive, repo} {
		for _, r := range list {
			r.ID = strings.TrimSpace(r.ID)
			r.Text = strings.TrimSpace(r.Text)
			r.Severity = pfSeverityOr(r.Severity, "", "medium")
			if r.ID == "" || r.Text == "" {
				continue
			}
			if i, ok := index[r.ID]; ok {
				out[i] = r
				continue
			}
			index[r.ID] = len(out)
			out = append(out, r)
		}
	}
	return out
}

func mergeEvidenceIgnores(hive, repo []ReviewPreflightIgnore) []ReviewPreflightIgnore {
	var out []ReviewPreflightIgnore
	index := map[string]int{}
	for _, list := range [][]ReviewPreflightIgnore{hive, repo} {
		for _, ig := range list {
			ig.ID = strings.TrimSpace(ig.ID)
			ig.Package = strings.TrimSpace(ig.Package)
			if ig.ID == "" {
				continue
			}
			key := strings.ToLower(ig.ID) + "\x00" + strings.ToLower(ig.Package)
			if i, ok := index[key]; ok {
				out[i] = ig
				continue
			}
			index[key] = len(out)
			out = append(out, ig)
		}
	}
	return out
}

// ReviewPreflightFor is the effective evidence policy for repo.
func (c *Config) ReviewPreflightFor(repo string) ReviewPreflightResolved {
	if c == nil {
		return ResolveReviewPreflight(ReviewPreflightConfig{}, ReviewPreflightConfig{})
	}
	rs, _ := c.RepoSettingsFor(repo)
	return ResolveReviewPreflight(c.Review.Preflight, rs.ReviewPreflight)
}

// Validate rejects values that would otherwise be silently ignored.
func (e ReviewPreflightConfig) Validate(scope string) error {
	for _, d := range []struct{ name, v string }{{"cache_ttl", e.CacheTTL}, {"request_timeout", e.RequestTimeout}} {
		if strings.TrimSpace(d.v) == "" {
			continue
		}
		if dur, err := time.ParseDuration(strings.TrimSpace(d.v)); err != nil || dur <= 0 {
			return fmt.Errorf("%s.%s %q is not a positive Go duration", scope, d.name, d.v)
		}
	}
	for _, s := range []struct{ name, v string }{{"severity_floor", e.SeverityFloor}, {"block_severity", e.BlockSeverity}} {
		if strings.TrimSpace(s.v) == "" {
			continue
		}
		if pfSeverityOr(s.v, "", "") == "" {
			return fmt.Errorf("%s.%s %q is not one of %s", scope, s.name, s.v, strings.Join(ReviewPreflightSeverities, ", "))
		}
	}
	if e.RuleDocBudgetBytes < 0 || (e.RuleDocBudgetBytes > 0 && e.RuleDocBudgetBytes < MinReviewPreflightRuleDocBudgetBytes) {
		return fmt.Errorf("%s.rule_doc_budget_bytes %d must be 0 (default) or at least %d", scope, e.RuleDocBudgetBytes, MinReviewPreflightRuleDocBudgetBytes)
	}
	if e.MaxDependencies < 0 {
		return fmt.Errorf("%s.max_dependencies %d must not be negative", scope, e.MaxDependencies)
	}
	if len(e.Rules) > MaxReviewPreflightRules {
		return fmt.Errorf("%s.rules has %d entries, over the %d limit", scope, len(e.Rules), MaxReviewPreflightRules)
	}
	for i, r := range e.Rules {
		if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("%s.rules[%d] needs both id and text", scope, i)
		}
		if len(r.Text) > MaxReviewPreflightRuleTextLen {
			return fmt.Errorf("%s.rules[%d] text is %d characters, over the %d limit", scope, i, len(r.Text), MaxReviewPreflightRuleTextLen)
		}
		if strings.TrimSpace(r.Severity) != "" && pfSeverityOr(r.Severity, "", "") == "" {
			return fmt.Errorf("%s.rules[%d] severity %q is not one of %s", scope, i, r.Severity, strings.Join(ReviewPreflightSeverities, ", "))
		}
	}
	for i, ig := range e.Ignore {
		if strings.TrimSpace(ig.ID) == "" || strings.TrimSpace(ig.Reason) == "" {
			return fmt.Errorf("%s.ignore[%d] needs both id and reason: an accepted risk must say why", scope, i)
		}
		if ex := strings.TrimSpace(ig.Expires); ex != "" {
			if _, err := time.Parse(ReviewPreflightDateLayout, ex); err != nil {
				return fmt.Errorf("%s.ignore[%d] expires %q is not a %s date", scope, i, ex, ReviewPreflightDateLayout)
			}
		}
	}
	return nil
}

// ValidateReviewPreflight checks the hive-wide block and every repo override.
func (c *Config) ValidateReviewPreflight() error {
	if err := c.Review.Preflight.Validate("review.preflight"); err != nil {
		return err
	}
	for key, rs := range c.Repos {
		if err := rs.ReviewPreflight.Validate("repos." + key + ".review_preflight"); err != nil {
			return err
		}
	}
	return nil
}
