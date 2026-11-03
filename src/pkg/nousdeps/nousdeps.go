// Package nousdeps implements pinned campaign dependencies for the
// experimenter role (RFC #11585 part 7, #11700).
//
// A campaign definition may declare the things it needs that are not in the
// hive's own repo — a target repo such as a simulator, data files such as
// workload traces — and every one of them is pinned: repos by a full commit
// SHA, files by URL plus sha256. `hive nous-setup` (bin/nous-runner.sh runs it
// before the engine starts) fetches them into a cache, verifies every pin,
// refuses any host outside the configured allowlist, and writes a dependency
// record next to the campaign's ledger under the durable Nous root so a
// verdict can be reproduced later. Fetch or verification failures end the
// campaign before any compute is spent.
//
// Nothing here is reachable after setup: once the record is written the
// agent needs no new network access, so nobody has to trust what the model
// decides to download.
package nousdeps

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

// File names and layout under the campaign directory (<nous root>/<run-id>/).
const (
	// RecordFile is the dependency record: every resolved pin plus the
	// toolchain versions, written next to ledger.json.
	RecordFile = "dependencies.json"
	// EnvFile is the shell-sourceable companion the runner loads so the
	// campaign can reference fetched paths (NOUS_DEP_<NAME>, NOUS_TARGET_REPO_PATH).
	EnvFile = "dependencies.env"
	// RecordSchema is bumped when RecordFile's shape changes incompatibly.
	RecordSchema = 1

	// CacheSubdir is where fetched dependencies live under the cache root:
	// repos/<host>/<owner>/<name>/<commit> and files/<sha256>/<basename>.
	CacheSubdir = "cache/deps"
	// EnvCacheDir names the agent cache root the RFC asks setup to reuse
	// (HIVE_AGENT_CACHE_DIR). Unset → <nous root>/CacheSubdir.
	EnvCacheDir = "HIVE_AGENT_CACHE_DIR"
	// EnvNousDir is where the Nous framework is installed (bin/nous-install.sh).
	EnvNousDir = "NOUS_DIR"
	// DefaultNousDir matches the Dockerfile and bin/nous-install.sh.
	DefaultNousDir = "/opt/nous"

	// EnvTargetRepoPath is exported by EnvFile for the dependency with
	// RoleTarget: it becomes Nous's target_system.repo_path.
	EnvTargetRepoPath = "NOUS_TARGET_REPO_PATH"
	// EnvDepPrefix prefixes one EnvFile variable per dependency.
	EnvDepPrefix = "NOUS_DEP_"
)

// Dependency roles. Target is the repo experiments run against (exactly one
// per campaign); data is linked into worktrees; tool is anything else.
const (
	RoleTarget = "target"
	RoleData   = "data"
	RoleTool   = "tool"
)

// Dependency kinds, derived from which pin the declaration carries.
const (
	KindRepo = "repo"
	KindFile = "file"
)

// Pin shapes. Anything shorter than a full SHA is a moving target.
const (
	commitSHAHexLen = 40
	sha256HexLen    = 64
	// DefaultRepoHost is prepended to an `owner/name` shorthand.
	DefaultRepoHost = "github.com"
	// httpsPort is the only port an allowlisted https URL may name explicitly.
	httpsPort = "443"
)

var (
	commitSHARe = regexp.MustCompile(`^[0-9a-f]{` + fmt.Sprint(commitSHAHexLen) + `}$`)
	sha256Re    = regexp.MustCompile(`^[0-9a-f]{` + fmt.Sprint(sha256HexLen) + `}$`)
	shorthandRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	envNameRe   = regexp.MustCompile(`[^A-Z0-9]+`)
)

// ValidRoles is the closed set `role:` may name.
var ValidRoles = map[string]bool{RoleTarget: true, RoleData: true, RoleTool: true}

// Sentinel errors callers can match with errors.Is.
var (
	// ErrNotAllowlisted is returned for any fetch whose host is outside the
	// allowlist, including a redirect hop.
	ErrNotAllowlisted = errors.New("nousdeps: host is not in the fetch allowlist")
	// ErrUnpinned is returned for a dependency without a full pin.
	ErrUnpinned = errors.New("nousdeps: dependency is not pinned")
	// ErrPinMismatch is returned when what was fetched does not match the pin.
	ErrPinMismatch = errors.New("nousdeps: fetched content does not match its pin")
	// ErrInvalidDependency covers every other declaration problem.
	ErrInvalidDependency = errors.New("nousdeps: invalid dependency")
)

// Dependency is one declared campaign dependency as written in campaign.yaml.
// Exactly one of Repo or URL is set.
type Dependency struct {
	// Name identifies the dependency in the record and EnvFile. Empty
	// derives it from the repo or file name.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Repo is `owner/name` (on DefaultRepoHost) or a full https URL.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Commit pins Repo to a full 40-hex commit SHA.
	Commit string `yaml:"commit,omitempty" json:"commit,omitempty"`
	// URL is a full https URL to a single file.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// SHA256 pins URL to its 64-hex content hash.
	SHA256 string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
	// Role is RoleTarget, RoleData or RoleTool. Empty → RoleTarget for the
	// first repo, RoleData for files, RoleTool for further repos.
	Role string `yaml:"role,omitempty" json:"role,omitempty"`
}

// Kind reports KindRepo or KindFile from which pin is declared.
func (d Dependency) Kind() string {
	if strings.TrimSpace(d.Repo) != "" {
		return KindRepo
	}
	return KindFile
}

// SourceURL is the https URL the dependency is fetched from: the expanded
// shorthand for repos, the URL itself for files.
func (d Dependency) SourceURL() string {
	if d.Kind() == KindRepo {
		repo := strings.TrimSpace(d.Repo)
		if shorthandRe.MatchString(repo) {
			return "https://" + DefaultRepoHost + "/" + repo
		}
		return repo
	}
	return strings.TrimSpace(d.URL)
}

// Pin is the declared pin: the commit for repos, the sha256 for files.
func (d Dependency) Pin() string {
	if d.Kind() == KindRepo {
		return strings.ToLower(strings.TrimSpace(d.Commit))
	}
	return strings.ToLower(strings.TrimSpace(d.SHA256))
}

// EffectiveName is Name, or a name derived from the source.
func (d Dependency) EffectiveName() string {
	if n := strings.TrimSpace(d.Name); n != "" {
		return n
	}
	u, err := url.Parse(d.SourceURL())
	if err != nil || u.Path == "" || u.Path == "/" {
		return ""
	}
	return strings.TrimSuffix(path.Base(u.Path), ".git")
}

// EnvName is the EnvFile variable for this dependency: EnvDepPrefix plus the
// upper-cased name with every run of non-alphanumerics folded to "_".
func (d Dependency) EnvName() string {
	n := envNameRe.ReplaceAllString(strings.ToUpper(d.EffectiveName()), "_")
	return EnvDepPrefix + strings.Trim(n, "_")
}

// Validate checks one declaration: exactly one source, a full pin of the
// right shape, an https URL without credentials or fragment, a known role.
// It does not consult the allowlist; see Allowlist.CheckURL.
func (d Dependency) Validate() error {
	hasRepo := strings.TrimSpace(d.Repo) != ""
	hasURL := strings.TrimSpace(d.URL) != ""
	switch {
	case hasRepo && hasURL:
		return fmt.Errorf("%w: %q declares both repo and url", ErrInvalidDependency, d.EffectiveName())
	case !hasRepo && !hasURL:
		return fmt.Errorf("%w: declaration has neither repo nor url", ErrInvalidDependency)
	}
	if role := strings.ToLower(strings.TrimSpace(d.Role)); role != "" && !ValidRoles[role] {
		return fmt.Errorf("%w: %q has unknown role %q (must be %s)", ErrInvalidDependency, d.EffectiveName(), d.Role, sortedRoles())
	}
	if hasRepo {
		if strings.TrimSpace(d.SHA256) != "" {
			return fmt.Errorf("%w: repo %q must be pinned by commit, not sha256", ErrInvalidDependency, d.Repo)
		}
		if !commitSHARe.MatchString(d.Pin()) {
			return fmt.Errorf("%w: repo %q needs a full %d-hex commit (got %q)", ErrUnpinned, d.Repo, commitSHAHexLen, d.Commit)
		}
	} else {
		if strings.TrimSpace(d.Commit) != "" {
			return fmt.Errorf("%w: file %q must be pinned by sha256, not commit", ErrInvalidDependency, d.URL)
		}
		if !sha256Re.MatchString(d.Pin()) {
			return fmt.Errorf("%w: file %q needs a full %d-hex sha256 (got %q)", ErrUnpinned, d.URL, sha256HexLen, d.SHA256)
		}
	}
	if _, err := parseHTTPSURL(d.SourceURL()); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidDependency, d.EffectiveName(), err)
	}
	if d.EffectiveName() == "" {
		return fmt.Errorf("%w: %q has no derivable name; set name:", ErrInvalidDependency, d.SourceURL())
	}
	return nil
}

// parseHTTPSURL accepts only an absolute https URL with a hostname, no
// credentials, no fragment and no port other than 443.
func parseHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("unparseable url %q", raw)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("url %q must use https", raw)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("url %q has no host", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("url %q must not carry credentials", raw)
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("url %q must not carry a fragment", raw)
	}
	if p := u.Port(); p != "" && p != httpsPort {
		return nil, fmt.Errorf("url %q must not name a port other than %s", raw, httpsPort)
	}
	return u, nil
}

// ValidateAll validates every declaration, applies default roles, requires
// unique names and at most one RoleTarget, and returns the normalised list.
func ValidateAll(deps []Dependency) ([]Dependency, error) {
	out := make([]Dependency, 0, len(deps))
	names := map[string]bool{}
	targets := 0
	sawRepo := false
	for i, d := range deps {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("dependencies[%d]: %w", i, err)
		}
		d.Role = strings.ToLower(strings.TrimSpace(d.Role))
		if d.Role == "" {
			switch {
			case d.Kind() == KindFile:
				d.Role = RoleData
			case !sawRepo:
				d.Role = RoleTarget
			default:
				d.Role = RoleTool
			}
		}
		if d.Kind() == KindRepo {
			sawRepo = true
		}
		if d.Role == RoleTarget {
			if d.Kind() != KindRepo {
				return nil, fmt.Errorf("dependencies[%d]: %w: only a repo can have role %s", i, ErrInvalidDependency, RoleTarget)
			}
			targets++
			if targets > 1 {
				return nil, fmt.Errorf("dependencies[%d]: %w: more than one dependency has role %s", i, ErrInvalidDependency, RoleTarget)
			}
		}
		d.Name = d.EffectiveName()
		if names[d.Name] {
			return nil, fmt.Errorf("dependencies[%d]: %w: duplicate name %q", i, ErrInvalidDependency, d.Name)
		}
		names[d.Name] = true
		out = append(out, d)
	}
	return out, nil
}

func sortedRoles() string {
	roles := make([]string, 0, len(ValidRoles))
	for r := range ValidRoles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	return strings.Join(roles, ", ")
}

// Allowlist is the set of exact hostnames setup may fetch from. A nil or
// empty allowlist denies everything: the feature is deny-by-default and the
// conservative default list is applied by config, not here.
type Allowlist []string

// NewAllowlist normalises hosts (lower-case, trimmed, de-duplicated).
func NewAllowlist(hosts []string) Allowlist {
	out := make(Allowlist, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// AllowlistFromEnv reads config.NousFetchAllowlistEnv, which the hive exports
// to experimenter agents with the resolved list. Unset or blank → the
// conservative default; a value of only separators (how the hive exports an
// explicitly empty yaml list) → deny all.
func AllowlistFromEnv(getenv func(string) string) Allowlist {
	if getenv == nil {
		getenv = os.Getenv
	}
	return NewAllowlist(config.NousConfig{}.ResolveFetchAllowlist(getenv))
}

// Allows reports whether host (exact, case-insensitive) is allowlisted.
// Wildcards and suffix matches are deliberately unsupported.
func (a Allowlist) Allows(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, h := range a {
		if h == host {
			return true
		}
	}
	return false
}

// CheckURL parses raw as an https URL (see parseHTTPSURL) and refuses it
// with ErrNotAllowlisted when its host is outside the list.
func (a Allowlist) CheckURL(raw string) (*url.URL, error) {
	u, err := parseHTTPSURL(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDependency, err)
	}
	if !a.Allows(u.Hostname()) {
		return nil, fmt.Errorf("%w: %q (allowed: %s)", ErrNotAllowlisted, u.Hostname(), a.describe())
	}
	return u, nil
}

func (a Allowlist) describe() string {
	if len(a) == 0 {
		return "none — nous.fetch_allowlist is empty"
	}
	return strings.Join(a, ", ")
}

// CampaignDeps is what Load returns: the campaign name and its validated,
// role-defaulted dependency list.
type CampaignDeps struct {
	Name         string
	Dependencies []Dependency
}

// campaignFile is the slice of campaign.yaml this package reads. The list
// may sit under `campaign.dependencies` (next to the other campaign keys) or
// at the top level as the RFC's example shows; the nested form wins.
type campaignFile struct {
	Campaign struct {
		Name         string       `yaml:"name"`
		Dependencies []Dependency `yaml:"dependencies"`
	} `yaml:"campaign"`
	Dependencies []Dependency `yaml:"dependencies"`
}

// Load reads and validates the dependencies declared in a campaign.yaml. A
// campaign with no declarations is valid and yields an empty list — setup
// then records only the toolchain versions.
func Load(campaignPath string) (CampaignDeps, error) {
	data, err := os.ReadFile(campaignPath)
	if err != nil {
		return CampaignDeps{}, err
	}
	return Parse(data)
}

// Parse is Load for in-memory YAML.
func Parse(data []byte) (CampaignDeps, error) {
	var f campaignFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return CampaignDeps{}, fmt.Errorf("nousdeps: parse campaign: %w", err)
	}
	deps := f.Campaign.Dependencies
	if deps == nil {
		deps = f.Dependencies
	}
	validated, err := ValidateAll(deps)
	if err != nil {
		return CampaignDeps{}, fmt.Errorf("nousdeps: %w", err)
	}
	return CampaignDeps{Name: strings.TrimSpace(f.Campaign.Name), Dependencies: validated}, nil
}
