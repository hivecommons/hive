package skillreg

import "strings"

// # Repo scope for a BYO agent (#6204)
//
// AgentSpec says what an agent IS — name, backend, model, mode, default skills.
// Until now it could not say what an agent is FOR: a spec, once declared,
// applied to every repository the hive manages. In a hive holding a Go service,
// a Rust CLI and a Terraform module, a specialist declared for one of them was
// either absent everywhere or present everywhere, waking on cadence and hunting
// for concerns the other repos do not have.
//
// The scope is an OPTIONAL extension rather than a new AgentSpec method, and
// that choice is the whole design:
//
//   - AgentSpec is a published contract that third parties implement. Adding a
//     method to a Go interface breaks every existing implementation at compile
//     time — a spec that compiled last release would stop compiling. The package
//     doc calls the contract "intentionally small and stable"; a required field
//     is exactly the cost that warns against.
//   - So the interface is untouched. RepoScoped is a separate, optional
//     interface; SpecRepos asks any AgentSpec for its scope and answers "no
//     scope" for the implementations that have never heard of it. An existing
//     spec keeps working, unchanged, and means what it always meant: hive-wide.
//
// Absent or empty scope means EVERY repo, which is both the old behaviour and
// the right default: an agent that does not say what it is for is for
// everything.

// RepoScoped is the optional half of the BYO contract: an AgentSpec that also
// implements it declares which repositories it serves.
//
// Implement it when a custom agent is a specialist — a schema-migration
// reviewer, a protocol-compatibility checker, an image-build auditor — and
// leave it off when the agent is general. Do NOT return an empty non-nil slice
// to mean "no repos": empty means unscoped, and there is deliberately no way to
// spell "this agent serves nothing", because that is what deleting it is for.
type RepoScoped interface {
	// Repos names the repositories this agent serves, as a hive spells them in
	// project.repos: a bare name ("console") or an explicit cross-org reference
	// ("laredo/cuga-agent"). Nil or empty means every repo.
	Repos() []string
}

// Compile-time assertion that the YAML-backed spec carries a scope.
var _ RepoScoped = (*SpecData)(nil)

// Repos implements RepoScoped for the YAML-backed spec.
func (s *SpecData) Repos() []string { return s.RepoScope }

// SpecRepos returns the repositories spec serves, or nil when it declares no
// scope — including when spec is an implementation that predates RepoScoped
// entirely. This is the accessor callers should use; it is the reason adding
// per-repo scope did not have to change AgentSpec.
func SpecRepos(spec AgentSpec) []string {
	scoped, ok := spec.(RepoScoped)
	if !ok {
		return nil
	}
	return NormalizeSpecRepos(scoped.Repos())
}

// SpecServesRepo reports whether spec serves repo. An unscoped spec serves every
// repo, so this is true for it whatever repo is asked about.
//
// Matching is case-insensitive because repository names are, and it accepts
// either spelling on either side: a spec scoped to "console" serves a repo the
// caller names "console" or "acme/console", and a spec scoped to
// "laredo/cuga-agent" is matched by that full reference. Nothing here knows the
// hive's org, so an org-qualified name matches a bare one by comparing the
// trailing segment — the hive-side predicate in pkg/config, which does know the
// org, is the stricter one and is what enforcement uses.
func SpecServesRepo(spec AgentSpec, repo string) bool {
	scope := SpecRepos(spec)
	if len(scope) == 0 {
		return true
	}
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return false
	}
	for _, entry := range scope {
		if repoRefsMatch(entry, want) {
			return true
		}
	}
	return false
}

// repoRefsMatch compares two repo references case-insensitively, treating a
// bare name and an "owner/name" reference as equal when the names agree. b is
// expected already lower-cased and trimmed.
func repoRefsMatch(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	if a == b {
		return true
	}
	return baseRepoName(a) == baseRepoName(b)
}

func baseRepoName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// NormalizeSpecRepos trims each entry and drops the blanks, returning nil when
// nothing survives. Callers get either a meaningful scope or no scope at all —
// never a list of empty strings that would silently match nothing.
func NormalizeSpecRepos(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
