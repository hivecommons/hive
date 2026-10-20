// Companion checkouts plan the sibling checkouts a target repository's own
// tooling expects beside it (hivecommons/hive#13059).
//
// Some repositories register a sibling repository's path relative to the
// agent workspace root — a spec or docs tool resolving a registered location
// to <root>/<org>/<sibling>/… and reporting `repo_location_missing` when it
// is not there — and their AGENTS.md says to stop. Five contributor parks on
// one repository in one day were agents
// obeying that literally. The owner's decision: when the registered path is
// <root>/<org>/<repo> of a repository the hive's installation can read, the
// agent clones it there (shallow is fine) and continues. This package turns a
// repo's configured companions (project.repo_policies[].companion_repos) into
// the paths and clone commands an assignment prompt or kick carries, so the
// agent has the checkout before any tool asks for it. It does no network or
// filesystem work itself.
package config

import (
	"fmt"
	"path"
	"strings"
)

// CompanionCheckout is one planned companion clone.
type CompanionCheckout struct {
	// Repo is the org-qualified companion repository.
	Repo string
	// Path is where it belongs: <root>/<org>/<repo>, with root rendered as
	// given (a literal path or a shell variable such as $HIVE_WORKSPACE_DIR).
	Path string
}

// CloneCommand is the shell step that provisions the checkout: a shallow
// clone into Path, skipped when a git checkout is already there.
func (c CompanionCheckout) CloneCommand(host string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		host = "https://github.com"
	}
	return fmt.Sprintf("[ -d %s/.git ] || git clone --depth 1 %s/%s %s", c.Path, host, c.Repo, c.Path)
}

// Plan maps companions to their workspace paths under root. Entries that are
// not org/repo shaped, duplicates (case-insensitive), and the primary repo
// itself are dropped; root is required. The result is nil when nothing is
// left, so callers render nothing for a repo without companions.
func PlanCompanionCheckouts(root, primaryRepo string, companions []string) []CompanionCheckout {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	if root == "" || len(companions) == 0 {
		return nil
	}
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(primaryRepo)): true}
	var out []CompanionCheckout
	for _, raw := range companions {
		full := strings.Trim(strings.TrimSpace(raw), "/")
		org, name, ok := strings.Cut(full, "/")
		if !ok || org == "" || name == "" || strings.Contains(name, "/") || org == "." || org == ".." || name == "." || name == ".." {
			continue
		}
		key := strings.ToLower(full)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, CompanionCheckout{Repo: full, Path: root + "/" + path.Join(org, name)})
	}
	return out
}

// Instruction renders the prompt paragraph for a plan: what to clone, where,
// and why it is not a reason to park. Empty for an empty plan.
func CompanionCheckoutInstruction(primaryRepo, host string, plan []CompanionCheckout) string {
	if len(plan) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s registers %d companion repositor", primaryRepo, len(plan))
	if len(plan) == 1 {
		b.WriteString("y")
	} else {
		b.WriteString("ies")
	}
	b.WriteString(" its tooling expects checked out beside it under the same workspace root. Before you run its tools, provision each one (a shallow clone is fine; this is the intended checkout, not a substitution): ")
	for i, c := range plan {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "'%s'", c.CloneCommand(host))
	}
	b.WriteString(". A tool reporting one of these paths missing means clone it, never park or report it.")
	return b.String()
}
