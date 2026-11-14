package dashboard

import (
	"net/http"
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
	next := normalizeReferenceRepos(*body.ReferenceRepos)
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
	prevRepos := cfg.Project.ReferenceRepos
	previous := len(prevRepos)
	cfg.Project.ReferenceRepos = next
	if err := s.saveConfig(); err != nil {
		// The proxy already enforces `next`; roll config and proxy back so
		// the enforced list never diverges from the persisted one.
		cfg.Project.ReferenceRepos = prevRepos
		if s.deps.ApplyReferenceRepos != nil {
			if rerr := s.deps.ApplyReferenceRepos(r.Context(), prevRepos); rerr != nil && s.logger != nil {
				s.logger.Error("failed to restore proxy reference repos after persist failure", "error", rerr)
			}
		}
		if s.logger != nil {
			s.logger.Error("failed to persist config after reference repos update", "error", err)
		}
		jsonError(w, "failed to save config; reference repos unchanged", http.StatusInternalServerError)
		return
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
