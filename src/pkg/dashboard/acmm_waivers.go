package dashboard

import (
	"context"
	"strings"

	"gopkg.in/yaml.v3"
)

// acmmWaiverPaths are the repo-root filenames checked for a waiver
// declaration, in precedence order. Both spellings are accepted because
// repos are inconsistent about .yml vs .yaml and a maintainer who picks
// the other one should not silently get no waivers.
var acmmWaiverPaths = []string{".acmm.yml", ".acmm.yaml"}

// acmmMaxWaiverFileBytes caps how much of a repo-supplied .acmm.yml is
// parsed. The file is untrusted input from a watched repository, and the
// evaluator runs it on every scan of every repo.
const acmmMaxWaiverFileBytes = 64 * 1024

// ACMMWaiver records a repository's declaration that a criterion is
// satisfied somewhere other than the file the criterion looks for.
//
// A waiver is NOT "ignore this criterion". It asserts the capability
// exists and names where, so the dashboard can count the criterion as met
// while still showing the reader that the evidence is off-repo. That
// distinction is the whole point: a criterion silently skipped decays into
// a maturity score nobody can audit, which is the failure the ACMM panel
// exists to prevent.
type ACMMWaiver struct {
	// ID is the criterion this waiver covers, e.g. "acmm:ai-fix-workflow".
	ID string `yaml:"id"`
	// SatisfiedBy names what provides the capability instead — "hive",
	// a sibling repo, an external service. Free text, surfaced verbatim.
	// Required: a waiver that cannot name where the capability went is not
	// a waiver, it is the criterion switched off.
	SatisfiedBy string `yaml:"satisfied_by"`
	// Reason is the justification. Required: a waiver whose author could
	// not be bothered to say why is indistinguishable from a criterion
	// quietly switched off, so an empty reason voids the waiver.
	Reason string `yaml:"reason"`
}

// acmmWaiverFile is the on-disk shape of .acmm.yml.
type acmmWaiverFile struct {
	Waivers []ACMMWaiver `yaml:"waivers"`
}

// parseACMMWaivers parses a .acmm.yml body into a criterion-ID-keyed map,
// dropping entries that cannot be honoured.
//
// Three classes are dropped, all silently from the caller's perspective
// but each for a stated reason:
//
//   - Unknown criterion ID. A typo must not become an invisible waiver
//     sitting in the file looking effective. Validating against
//     universalCriteria means a renamed criterion re-opens as a real gap
//     rather than staying green on a stale ID.
//   - Missing satisfied_by or reason. Both are the difference between "this
//     capability lives over there, here is why" and "ignore this line".
//     A declaration that cannot say where the capability went, or why, is
//     the second thing wearing the first thing's clothes, so it is refused
//     and the criterion stays red.
//   - Duplicate ID. First declaration wins, so a later edit appending a
//     second entry cannot quietly widen an existing waiver.
func parseACMMWaivers(body []byte) map[string]ACMMWaiver {
	if len(body) == 0 || len(body) > acmmMaxWaiverFileBytes {
		return nil
	}

	var f acmmWaiverFile
	if err := yaml.Unmarshal(body, &f); err != nil {
		return nil
	}
	if len(f.Waivers) == 0 {
		return nil
	}

	known := make(map[string]bool, len(universalCriteria))
	for _, c := range universalCriteria {
		known[c.ID] = true
	}

	out := make(map[string]ACMMWaiver, len(f.Waivers))
	for _, w := range f.Waivers {
		w.ID = strings.TrimSpace(w.ID)
		w.Reason = strings.TrimSpace(w.Reason)
		w.SatisfiedBy = strings.TrimSpace(w.SatisfiedBy)
		if w.ID == "" || w.Reason == "" || w.SatisfiedBy == "" || !known[w.ID] {
			continue
		}
		if _, dup := out[w.ID]; dup {
			continue
		}
		out[w.ID] = w
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fetchACMMWaivers reads and parses a repository's waiver declaration.
//
// It costs zero extra GitHub calls for the common case: the root listing is
// already in dirCache from prefetchDirectories, so a repo with no .acmm.yml
// is settled from cache. Only a repo that actually declares waivers pays for
// the content fetch. That matters because a full refresh is already ~29
// GetContents calls per repo and this runs inside the same per-repo timeout.
func (s *Server) fetchACMMWaivers(ctx context.Context, owner, repo string, dirCache map[string]map[string]bool) map[string]ACMMWaiver {
	root, haveRoot := dirCache[""]

	ghClient := s.deps.GHClient.GoGitHub()
	if ghClient == nil {
		return nil
	}

	for _, path := range acmmWaiverPaths {
		// When the root listing is cached and does not name the file, skip
		// the call. When it is missing (prefetch failed), fall through and
		// let the API answer rather than assuming no waivers exist.
		if haveRoot && !root[path] {
			continue
		}
		fileContent, _, _, err := ghClient.Repositories.GetContents(ctx, owner, repo, path, nil)
		if err != nil || fileContent == nil {
			continue
		}
		body, err := fileContent.GetContent()
		if err != nil {
			// Oversized or binary content decodes with an error; a repo
			// whose .acmm.yml is unreadable gets no waivers, not a crash.
			if s.logger != nil {
				s.logger.Warn("ACMM waiver file could not be decoded",
					"repo", repo, "path", path, "error", err)
			}
			continue
		}
		waivers := parseACMMWaivers([]byte(body))
		if len(waivers) > 0 && s.logger != nil {
			s.logger.Info("ACMM waivers applied",
				"repo", repo, "path", path, "count", len(waivers))
		}
		return waivers
	}
	return nil
}
