package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReferenceRepo is one entry of project.reference_repos (hivecommons/hive#18015):
// a repository the hive's agents may STUDY — clone, fetch, read through the
// GitHub API — without it being an authorized repo. The motivating case is a
// documentation-assessment hive (CNCF TechDocs) whose deliverable lives in its
// own authorized repo but describes another project's repos: the agent has to
// read those repos, and must not be told they are out of scope, yet nothing
// about the assessment should ever land on them.
//
// The default tier is READ-ONLY: public clones and GETs, never carrying the
// hive's App token (which is not valid for a foreign installation anyway).
// Every write is refused by the proxy with a 403 that names reference_repos.
//
// Write is the rare opt-in for an assessor who also files findings upstream.
// It is deliberately narrow — open an issue, open a PR on a topic branch,
// comment on the hive's own items — and the credential behind it has to be one
// GitHub actually honours for that repo: the App's installation on the TARGET
// org (minted on demand, scoped to this one repo) or an operator-supplied
// write_token_file. Boot refuses a write-mode entry that has neither.
type ReferenceRepo struct {
	// Repo is the explicit "owner/name" reference. Bare names are rejected:
	// a reference repo is by definition outside the project's own list, so
	// there is no org to qualify it against without guessing.
	Repo string `yaml:"repo" json:"repo"`
	// Write lists the opt-in write surfaces: "issues" and/or "prs". Empty is
	// the read-only default.
	Write []string `yaml:"write,omitempty" json:"write,omitempty"`
	// WriteTokenFile is a path on the hive host holding a token that is
	// authorized for Repo, used instead of minting from the App installation
	// on the target org. Only meaningful with Write.
	WriteTokenFile string `yaml:"write_token_file,omitempty" json:"write_token_file,omitempty"`
	// DerivedFrom is the "owner/repo#N" of the accepted request issue this
	// entry was derived from (hivecommons/hive#18139), empty for an entry an
	// operator wrote. It is deliberately excluded from YAML: a derived entry
	// is rebuilt from the issue on every config load and must never land in
	// an overlay or the runtime config, where it would outlive the grant
	// (see hivecommons/hive#18137). The dashboard reads it as JSON.
	DerivedFrom string `yaml:"-" json:"derived_from,omitempty"`
}

// Derived reports whether the entry came from an accepted request issue
// rather than from the operator's config.
func (r ReferenceRepo) Derived() bool { return r.DerivedFrom != "" }

const (
	ReferenceWriteIssues = "issues"
	ReferenceWritePRs    = "prs"
)

// UnmarshalYAML accepts both the scalar form ("owner/name", read-only) and the
// mapping form ({repo: owner/name, write: [...]}).
func (r *ReferenceRepo) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Repo = strings.TrimSpace(value.Value)
		r.Write = nil
		r.WriteTokenFile = ""
		return nil
	}
	type plain ReferenceRepo
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*r = ReferenceRepo(p)
	r.Repo = strings.TrimSpace(r.Repo)
	return nil
}

// MarshalYAML writes the scalar form back for a read-only entry so a config
// that was written as a plain list round-trips unchanged.
func (r ReferenceRepo) MarshalYAML() (any, error) {
	if len(r.Write) == 0 && r.WriteTokenFile == "" {
		return r.Repo, nil
	}
	type plain ReferenceRepo
	return plain(r), nil
}

// WritesIssues reports whether the entry opted in to opening issues (and
// commenting on its own).
func (r ReferenceRepo) WritesIssues() bool { return r.hasWrite(ReferenceWriteIssues) }

// WritesPRs reports whether the entry opted in to pushing topic branches and
// opening PRs.
func (r ReferenceRepo) WritesPRs() bool { return r.hasWrite(ReferenceWritePRs) }

// ReadOnly reports whether the entry has no write opt-in at all.
func (r ReferenceRepo) ReadOnly() bool { return !r.WritesIssues() && !r.WritesPRs() }

func (r ReferenceRepo) hasWrite(kind string) bool {
	for _, w := range r.Write {
		if strings.EqualFold(strings.TrimSpace(w), kind) {
			return true
		}
	}
	return false
}

// WriteLabel is the human-facing tier: "read-only", "issues", "prs" or
// "issues+prs". The dashboard chip and the kick both print it.
func (r ReferenceRepo) WriteLabel() string { return r.Tier().String() }

// Owner returns the owner half of Repo ("" when malformed).
func (r ReferenceRepo) Owner() string {
	owner, _, _ := strings.Cut(r.Repo, "/")
	return owner
}

// Name returns the repository half of Repo ("" when malformed).
func (r ReferenceRepo) Name() string {
	_, name, _ := strings.Cut(r.Repo, "/")
	return name
}

// referenceRepoKey is the case-folded comparison key for a reference entry.
func referenceRepoKey(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}

// ReferenceRepoFor returns the reference entry covering repo ("owner/name",
// case-insensitive), and whether one exists.
func (c *Config) ReferenceRepoFor(repo string) (ReferenceRepo, bool) {
	if c == nil {
		return ReferenceRepo{}, false
	}
	key := referenceRepoKey(repo)
	if key == "" {
		return ReferenceRepo{}, false
	}
	for _, r := range c.Project.ReferenceRepos {
		if referenceRepoKey(r.Repo) == key {
			return r, true
		}
	}
	return ReferenceRepo{}, false
}

// IsReferenceRepo reports whether repo ("owner/name") is a reference repo.
func (c *Config) IsReferenceRepo(repo string) bool {
	_, ok := c.ReferenceRepoFor(repo)
	return ok
}

// ReferenceReposWithWrite returns the entries that opted in to any write.
func (c *Config) ReferenceReposWithWrite() []ReferenceRepo {
	if c == nil {
		return nil
	}
	var out []ReferenceRepo
	for _, r := range c.Project.ReferenceRepos {
		if !r.ReadOnly() {
			out = append(out, r)
		}
	}
	return out
}

// validReferenceRepoSlug accepts exactly "owner/name" with no whitespace, no
// scheme and no extra path.
func validReferenceRepoSlug(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return false
	}
	if strings.ContainsAny(repo, " \t\n:@") || strings.Contains(name, "/") {
		return false
	}
	return true
}

// validateReferenceRepos enforces the shape of project.reference_repos: every
// entry is an explicit owner/name, none duplicates another, none is also an
// authorized repo (a repo is either managed or reference, never both — the
// two tiers would otherwise contradict each other on every write), write
// kinds are known, and write_token_file is only set alongside write.
func (c *Config) validateReferenceRepos() error {
	if len(c.Project.ReferenceRepos) == 0 {
		return nil
	}
	authorized := make(map[string]bool, len(c.Project.Repos))
	for _, repo := range c.Project.Repos {
		authorized[referenceRepoKey(QualifyRepo(c.Project.Org, repo))] = true
	}
	seen := make(map[string]bool, len(c.Project.ReferenceRepos))
	for i, r := range c.Project.ReferenceRepos {
		if !validReferenceRepoSlug(r.Repo) {
			return fmt.Errorf("project.reference_repos[%d]: %q must be an explicit owner/name (a reference repo lives outside this project, so there is no org to qualify a bare name against)", i, r.Repo)
		}
		key := referenceRepoKey(r.Repo)
		if authorized[key] {
			return fmt.Errorf("project.reference_repos[%d]: %q is already in project.repos — a repository is either authorized (agents work it) or reference (agents only study it), not both", i, r.Repo)
		}
		if seen[key] {
			return fmt.Errorf("project.reference_repos[%d]: %q is listed twice", i, r.Repo)
		}
		seen[key] = true
		for _, w := range r.Write {
			switch strings.ToLower(strings.TrimSpace(w)) {
			case ReferenceWriteIssues, ReferenceWritePRs:
			default:
				return fmt.Errorf("project.reference_repos[%d]: write %q is not one of %q, %q", i, w, ReferenceWriteIssues, ReferenceWritePRs)
			}
		}
		if r.WriteTokenFile != "" && r.ReadOnly() {
			return fmt.Errorf("project.reference_repos[%d]: write_token_file is set but write is empty — a read-only reference repo never carries a credential", i)
		}
	}
	return nil
}

// ReferenceRepoSlugs returns the reference repos as sorted "owner/name"
// strings, for kicks and environment.
func (c *Config) ReferenceRepoSlugs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Project.ReferenceRepos))
	for _, r := range c.Project.ReferenceRepos {
		out = append(out, r.Repo)
	}
	sort.Strings(out)
	return out
}

// ReferenceRepoTier is the access tier of a reference repo as a pair of
// flags, for callers that branch on the opt-ins rather than print them.
type ReferenceRepoTier struct {
	Issues bool
	PRs    bool
}

// ReferenceTierReadOnly is the default tier: no writes.
var ReferenceTierReadOnly = ReferenceRepoTier{}

// String yields the same label as ReferenceRepo.WriteLabel: "read-only",
// "issues", "prs" or "issues+prs".
func (t ReferenceRepoTier) String() string {
	switch {
	case t.Issues && t.PRs:
		return "issues+prs"
	case t.Issues:
		return ReferenceWriteIssues
	case t.PRs:
		return ReferenceWritePRs
	}
	return "read-only"
}

// Tier returns the entry's write opt-ins as a ReferenceRepoTier.
func (r ReferenceRepo) Tier() ReferenceRepoTier {
	return ReferenceRepoTier{Issues: r.WritesIssues(), PRs: r.WritesPRs()}
}

// ReferenceRepoTier returns the tier of the reference repo matching slug
// ("owner/name", case-insensitive) and false when slug is not a reference
// repo.
func (p ProjectConfig) ReferenceRepoTier(slug string) (ReferenceRepoTier, bool) {
	r, ok := (&Config{Project: p}).ReferenceRepoFor(slug)
	if !ok {
		return ReferenceTierReadOnly, false
	}
	return r.Tier(), true
}
