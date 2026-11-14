package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// The project.reference_repos editor (hivecommons/hive#18136).
//
// Reference repos (#18015) are repositories the hive's agents may study but
// never manage. Until this editor they could only be set in the config file
// or overlay, which a hosted hive's owner cannot reach. Both endpoints are
// OWNER-ONLY: a write-mode entry lets agents open issues and PRs on a repo
// outside the project.
//
// A PUT is refused before anything changes unless the list passes the same
// checks boot applies — the loader's shape rules (config.ValidateReferenceRepos)
// and, when a GitHub proxy is wired, the write-credential check that would
// otherwise stop the next boot with exit 22 — so a dashboard edit can never
// leave the hive unbootable.

// referenceReposSectionResponse is the GET/PUT response body.
type referenceReposSectionResponse struct {
	// ReferenceRepos is the current list, in config order. Always an array
	// (never null).
	ReferenceRepos []config.ReferenceRepo `json:"reference_repos"`
	// WriteKinds is the write vocabulary the editor offers.
	WriteKinds []string `json:"write_kinds"`
}

// handleReferenceReposGet returns project.reference_repos.
func (s *Server) handleReferenceReposGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, referenceReposSection(s.deps.Config))
}

// handleReferenceReposPut replaces project.reference_repos. The body must
// carry the whole list ({"reference_repos": [...]}); send an empty array to
// clear it. A missing key is refused rather than read as "clear".
func (s *Server) handleReferenceReposPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		ReferenceRepos *[]config.ReferenceRepo `json:"reference_repos"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.ReferenceRepos == nil {
		jsonError(w, `reference_repos is required; send {"reference_repos": []} to clear it`, http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config

	// --- validate before mutating anything ---
	// Only the operator's entries are ever stored. A derived entry
	// (hivecommons/hive#18139) lives in memory, is rebuilt from its request
	// issue on every pass and must never land in hive.yaml, so the editor
	// echoing one back is dropped here; the derived set is merged back in
	// after the save.
	prevRepos := cfg.Project.ReferenceRepos
	if err := rejectDerivedReferenceRepoEdits(*body.ReferenceRepos, prevRepos); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	next := normalizeReferenceRepos(config.HandWrittenReferenceRepos(*body.ReferenceRepos))
	if err := rejectDashboardTokenFiles(next, config.HandWrittenReferenceRepos(prevRepos)); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	candidate := cfg.Project
	candidate.ReferenceRepos = next
	if err := config.ValidateReferenceRepos(candidate); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.deps.ApplyReferenceRepos != nil {
		if err := s.deps.ApplyReferenceRepos(r.Context(), next); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// --- apply ---
	previous := len(config.HandWrittenReferenceRepos(prevRepos))
	cfg.Project.ReferenceRepos = next
	if err := s.saveConfig(); err != nil {
		// The proxy already enforces `next`; roll config and proxy back so
		// the enforced list never diverges from the persisted one. The
		// restore must finish even if the caller has gone away.
		cfg.Project.ReferenceRepos = prevRepos
		if s.deps.ApplyReferenceRepos != nil {
			if rerr := s.deps.ApplyReferenceRepos(context.WithoutCancel(r.Context()), prevRepos); rerr != nil && s.logger != nil {
				s.logger.Error("failed to restore proxy reference repos after persist failure", "error", rerr)
			}
		}
		if s.logger != nil {
			s.logger.Error("failed to persist config after reference repos update", "error", err)
		}
		jsonError(w, "failed to save config; reference repos unchanged", http.StatusInternalServerError)
		return
	}
	// The file holds the hand-written list; memory gets the derived entries
	// of the last pass back, as a config reload does.
	if s.deps.RemergeDerivedReferenceRepos != nil {
		s.deps.RemergeDerivedReferenceRepos()
	}
	writable := 0
	for _, rr := range next {
		if !rr.ReadOnly() {
			writable++
		}
	}
	s.auditFromRequest(r, "config_reference_repos", auditDetail(
		"section", "reference_repos",
		"repos", strconv.Itoa(len(next)),
		"previous_repos", strconv.Itoa(previous),
		"write_mode", strconv.Itoa(writable),
		"list", strings.Join(cfg.ReferenceRepoSlugs(), "+"),
	), "")
	if s.logger != nil {
		s.logger.Info("reference repos updated via runtime settings",
			"repos", len(next), "previous_repos", previous, "write_mode", writable)
	}
	jsonResponse(w, referenceReposSection(cfg))
}

// rejectDerivedReferenceRepoEdits compares the incoming list with the live
// derived set (hivecommons/hive#18161): a derived entry is the request
// issue's grant, so the editor may only echo it back unchanged. Changing its
// write set, leaving it out, sending one the hive did not derive, or adding a
// hand-written entry for the same repo (which would override the grant's
// tier) is a 409 that names the issue to change instead.
func rejectDerivedReferenceRepoEdits(incoming, live []config.ReferenceRepo) error {
	key := func(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }
	liveDerived := map[string]config.ReferenceRepo{}
	for _, rr := range live {
		if rr.Derived() {
			liveDerived[key(rr.Repo)] = rr
		}
	}
	conflict := func(rr config.ReferenceRepo, what string) error {
		return fmt.Errorf("reference repo %q is derived from request issue %s and cannot be %s here; edit or close that issue instead", rr.Repo, rr.DerivedFrom, what)
	}
	seen := map[string]bool{}
	for _, in := range normalizeReferenceRepos(incoming) {
		k := key(in.Repo)
		cur, isLive := liveDerived[k]
		switch {
		case !in.Derived() && isLive:
			return conflict(cur, "replaced by a hand-written entry")
		case in.Derived() && !isLive:
			return fmt.Errorf("reference repo %q is not derived from %s on this hive; a derived entry can only be echoed back as the hive serves it", in.Repo, in.DerivedFrom)
		case in.Derived() && (in.DerivedFrom != cur.DerivedFrom || writeSetKey(in.Write) != writeSetKey(cur.Write)):
			return conflict(cur, "edited")
		}
		if in.Derived() {
			seen[k] = true
		}
	}
	for k, cur := range liveDerived {
		if !seen[k] {
			return conflict(cur, "removed")
		}
	}
	return nil
}

// writeSetKey is the write opt-ins as an order-free comparable string.
func writeSetKey(write []string) string {
	kinds := make([]string, 0, len(write))
	for _, w := range write {
		kinds = append(kinds, strings.ToLower(strings.TrimSpace(w)))
	}
	sort.Strings(kinds)
	return strings.Join(kinds, "+")
}

// rejectDashboardTokenFiles refuses a write_token_file the dashboard would
// introduce or change. The path names a file on the hive host that the
// credential check reads and sends to GitHub as a bearer; a hosted hive's
// owner has no host access by design (hivecommons/hive#18154 review), so
// letting the editor pick the path would turn the owner role into an
// arbitrary-file read and a file-existence oracle over the hive process's
// filesystem. A path the operator already set in the config file for the
// same repo round-trips unchanged; everything else is "set it in the file".
func rejectDashboardTokenFiles(next, current []config.ReferenceRepo) error {
	key := func(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }
	fromFile := make(map[string]string, len(current))
	for _, rr := range current {
		if rr.WriteTokenFile != "" {
			fromFile[key(rr.Repo)] = rr.WriteTokenFile
		}
	}
	for i, rr := range next {
		if rr.WriteTokenFile == "" || fromFile[key(rr.Repo)] == rr.WriteTokenFile {
			continue
		}
		return fmt.Errorf("project.reference_repos[%d]: write_token_file for %q can only be set in the config file by the hive operator; a write opt-in saved from the dashboard uses the GitHub App installation on the repo's org", i, rr.Repo)
	}
	return nil
}

// normalizeReferenceRepos trims each entry and folds its write kinds to the
// canonical lower-case, de-duplicated "issues", "prs" order. Unknown kinds
// are kept as typed so the loader's validation names them. A token file
// without any write is kept too, for the same reason.
func normalizeReferenceRepos(in []config.ReferenceRepo) []config.ReferenceRepo {
	out := make([]config.ReferenceRepo, 0, len(in))
	for _, rr := range in {
		rr.Repo = strings.TrimSpace(rr.Repo)
		rr.WriteTokenFile = strings.TrimSpace(rr.WriteTokenFile)
		var known, unknown []string
		seen := map[string]bool{}
		for _, w := range rr.Write {
			k := strings.ToLower(strings.TrimSpace(w))
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			if k != config.ReferenceWriteIssues && k != config.ReferenceWritePRs {
				unknown = append(unknown, strings.TrimSpace(w))
			}
		}
		if seen[config.ReferenceWriteIssues] {
			known = append(known, config.ReferenceWriteIssues)
		}
		if seen[config.ReferenceWritePRs] {
			known = append(known, config.ReferenceWritePRs)
		}
		rr.Write = append(known, unknown...)
		if len(rr.Write) == 0 {
			rr.Write = nil
		}
		out = append(out, rr)
	}
	return out
}

// referenceReposSection renders project.reference_repos for the dashboard.
func referenceReposSection(cfg *config.Config) referenceReposSectionResponse {
	refs := append([]config.ReferenceRepo{}, cfg.Project.ReferenceRepos...)
	return referenceReposSectionResponse{
		ReferenceRepos: refs,
		WriteKinds:     []string{config.ReferenceWriteIssues, config.ReferenceWritePRs},
	}
}
