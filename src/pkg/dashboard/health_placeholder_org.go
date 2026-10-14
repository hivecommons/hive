package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Setup blocker for a hive whose project.org is still a pool-slot placeholder
// (#11846, part of #11800). Paired with a bare repo name ("kie") the spoke
// cannot resolve a GitHub App installation for the placeholder org, every
// enumeration 404s and the governor never kicks — yet nothing operator-visible
// failed. The check fails so the dashboard tooltip, /api/health/deep and the
// hub's health-check-failing alert all say what to do.

const healthCheckProjectOrg = "project_org"

// placeholderOrgPrefixes are the org-name prefixes a slot placeholder carries:
// the pool's "available-" and the legacy "placeholder" form.
var placeholderOrgPrefixes = []string{"available-", "placeholder"}

// projectOrgHealth grades project.org. ok=false when it does not apply: an
// unclaimed slot (no repo yet) legitimately carries the placeholder org.
func projectOrgHealth(p config.ProjectConfig) (status, detail string, ok bool) {
	org := strings.ToLower(strings.TrimSpace(p.Org))
	placeholder := false
	for _, pre := range placeholderOrgPrefixes {
		if strings.HasPrefix(org, pre) {
			placeholder = true
			break
		}
	}
	if !placeholder {
		return "", "", false
	}
	repo := strings.TrimSpace(p.PrimaryRepo)
	if repo == "" && len(p.Repos) > 0 {
		repo = strings.TrimSpace(p.Repos[0])
	}
	if repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return "fail", "project.org " + p.Org + " is a slot placeholder — enter Owner/repo (e.g. PUT /api/config/governor/repos) " +
		"so GitHub App discovery can find the installation; until then every GitHub call fails and the governor never kicks", true
}

func (s *Server) projectOrgCheck() (status, detail string, ok bool) {
	if s.deps == nil || s.deps.Config == nil {
		return "", "", false
	}
	return projectOrgHealth(s.deps.Config.Project)
}
