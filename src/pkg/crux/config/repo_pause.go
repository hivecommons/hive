package config

import (
	"strings"
)

// QualifyRepo returns repo as an "org/name" reference. An entry that already
// carries a slash is a deliberate cross-org reference and is returned
// unchanged — prefixing it again is the "laredo/laredo/cuga-agent" bug that has
// bitten every place this join was written inline.
func QualifyRepo(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") {
		return repo
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return repo
	}
	return org + "/" + repo
}
