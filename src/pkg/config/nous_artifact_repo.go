package config

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// Optional GitHub artifact repository (RFC #11585 part 8, #11701).
//
// When an owner names a repository here, the hive mirrors each finished
// campaign's folder under <nous_dir> to that repository as a pull request
// against the configured branch. Nothing is mirrored, and no GitHub call is
// made, while Repo is empty. The mirror is write-only to the named repo,
// goes through the hive's own GitHub identity (never an agent's scoped token
// or the owner's personal token), and every push is audited. It promotes
// nothing: a verdict still needs an owner's explicit Promote.
const (
	// NousArtifactRepoEnv overrides nous.artifact_repo.repo (owner/name).
	NousArtifactRepoEnv = "HIVE_NOUS_ARTIFACT_REPO"
	// NousArtifactBranchEnv overrides nous.artifact_repo.branch.
	NousArtifactBranchEnv = "HIVE_NOUS_ARTIFACT_BRANCH"
	// NousArtifactPathEnv overrides nous.artifact_repo.path.
	NousArtifactPathEnv = "HIVE_NOUS_ARTIFACT_PATH"
	// NousArtifactSyncIntervalEnv overrides nous.artifact_repo.sync_interval_seconds
	// (Go duration or bare seconds).
	NousArtifactSyncIntervalEnv = "HIVE_NOUS_ARTIFACT_SYNC_INTERVAL"

	// DefaultNousArtifactBranch is the PR target branch when none is set.
	// Empty is deliberately not "main": the hive resolves the repository's
	// default branch instead of guessing.
	DefaultNousArtifactBranch = ""
	// DefaultNousArtifactPath is the folder below which one directory per
	// campaign is written.
	DefaultNousArtifactPath = "campaigns"
	// DefaultNousArtifactSyncInterval is how often the hive looks for
	// finished campaigns that have not been mirrored yet.
	DefaultNousArtifactSyncInterval = 5 * time.Minute
	// MinNousArtifactSyncInterval floors the configured cadence so a typo
	// cannot turn the mirror into a polling loop against the API budget.
	MinNousArtifactSyncInterval = 30 * time.Second
)

// NousArtifactRepoConfig is `nous.artifact_repo`. All fields are optional;
// an empty Repo disables the feature entirely.
type NousArtifactRepoConfig struct {
	// Repo is the destination repository as owner/name. Empty = off.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Branch is the pull-request target branch. Empty resolves the
	// repository's default branch at sync time.
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`
	// Path is the folder inside the repository that holds one directory per
	// campaign. Relative, no "..". Empty applies DefaultNousArtifactPath.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// SyncIntervalSeconds is the scan cadence. Zero applies
	// DefaultNousArtifactSyncInterval.
	SyncIntervalSeconds int `yaml:"sync_interval_seconds,omitempty" json:"sync_interval_seconds,omitempty"`
}

// ResolvedNousArtifactRepo is the effective settings after env > yaml >
// default resolution. Enabled is false when no repository is named.
type ResolvedNousArtifactRepo struct {
	Enabled      bool
	Repo         string
	Branch       string
	Path         string
	SyncInterval time.Duration
}

// Resolve applies precedence env > yaml > default. A blank env value is
// treated as unset so `HIVE_NOUS_ARTIFACT_REPO=` cannot override yaml by
// accident; pass nil to skip the env tier.
func (a NousArtifactRepoConfig) Resolve(getenv func(string) string) ResolvedNousArtifactRepo {
	env := func(key string) string {
		if getenv == nil {
			return ""
		}
		return strings.TrimSpace(getenv(key))
	}
	out := ResolvedNousArtifactRepo{
		Repo:         firstNonEmpty(env(NousArtifactRepoEnv), strings.TrimSpace(a.Repo)),
		Branch:       firstNonEmpty(env(NousArtifactBranchEnv), strings.TrimSpace(a.Branch), DefaultNousArtifactBranch),
		Path:         NormalizeNousArtifactPath(firstNonEmpty(env(NousArtifactPathEnv), a.Path)),
		SyncInterval: a.resolveSyncInterval(env(NousArtifactSyncIntervalEnv)),
	}
	out.Enabled = out.Repo != ""
	return out
}

func (a NousArtifactRepoConfig) resolveSyncInterval(raw string) time.Duration {
	d := DefaultNousArtifactSyncInterval
	if a.SyncIntervalSeconds > 0 {
		d = time.Duration(a.SyncIntervalSeconds) * time.Second
	}
	if raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			d = parsed
		} else if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			d = time.Duration(secs) * time.Second
		}
	}
	if d < MinNousArtifactSyncInterval {
		d = MinNousArtifactSyncInterval
	}
	return d
}

// NormalizeNousArtifactPath cleans a path prefix into the form the syncer
// uses: forward slashes, no leading or trailing slash, DefaultNousArtifactPath
// when empty or ".". It does not reject ".." — Validate does.
func NormalizeNousArtifactPath(p string) string {
	p = strings.Trim(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "/")
	p = path.Clean(p)
	if p == "" || p == "." {
		return DefaultNousArtifactPath
	}
	return p
}

// Validate rejects a repo that is not owner/name, a branch with ref-unsafe
// characters, and a path that escapes the repository. Empty is valid (off).
func (a NousArtifactRepoConfig) Validate() error {
	repo := strings.TrimSpace(a.Repo)
	if repo == "" {
		return nil
	}
	if err := ValidateNousArtifactRepoName(repo); err != nil {
		return err
	}
	if err := ValidateNousArtifactBranch(a.Branch); err != nil {
		return err
	}
	if err := ValidateNousArtifactPath(a.Path); err != nil {
		return err
	}
	if a.SyncIntervalSeconds < 0 {
		return fmt.Errorf("artifact_repo.sync_interval_seconds must not be negative (got %d)", a.SyncIntervalSeconds)
	}
	return nil
}

// ValidateNousArtifactRepoName requires exactly owner/name with no scheme,
// host, whitespace or ".git" suffix.
func ValidateNousArtifactRepoName(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, " \t:@?#\\") || strings.HasSuffix(repo, ".git") {
		return fmt.Errorf("artifact_repo.repo %q must be owner/name", repo)
	}
	return nil
}

// ValidateNousArtifactBranch rejects branch names git itself refuses and
// anything that could be read as a ref expression. Empty is valid.
func ValidateNousArtifactBranch(branch string) error {
	b := strings.TrimSpace(branch)
	if b == "" {
		return nil
	}
	if strings.ContainsAny(b, " \t~^:?*[\\") || strings.Contains(b, "..") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasPrefix(b, "refs/") || strings.HasSuffix(b, ".lock") {
		return fmt.Errorf("artifact_repo.branch %q is not a valid branch name", branch)
	}
	return nil
}

// ValidateNousArtifactPath rejects absolute paths and any ".." segment.
// Empty is valid (default).
func ValidateNousArtifactPath(p string) error {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") {
		return fmt.Errorf("artifact_repo.path %q must be relative to the repository root", p)
	}
	for _, seg := range strings.FieldsFunc(strings.ReplaceAll(raw, "\\", "/"), func(r rune) bool { return r == '/' }) {
		if seg == ".." {
			return fmt.Errorf("artifact_repo.path %q must not contain \"..\"", p)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
