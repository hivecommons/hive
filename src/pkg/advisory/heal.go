package advisory

import (
	"regexp"

	"github.com/kubestellar/hive/pkg/beads"
)

// appAuthHealedCloseReason is stamped into a bead's metadata when the hive
// auto-closes it because the GitHub App demonstrably works. Kept as a named
// constant so logs, tests and the bead trail all agree on the exact wording.
const appAuthHealedCloseReason = "auto-closed: the GitHub App successfully posted the advisory digest, so this access finding no longer holds"

// closeReasonMetadataKey is the metadata key used to record why a bead was
// auto-closed. It matches the key beads.CloseAll already writes so every
// close path is inspectable the same way.
const closeReasonMetadataKey = "close_reason"

// appAuthFindingPatterns recognize advisory findings that describe THIS hive's
// own GitHub access — the App not being installed, or the installation lacking
// repo permissions. These are environment conditions the hive can verify
// itself, unlike code findings, which only an agent can re-check.
//
// The patterns are deliberately narrow: they must match agent phrasings like
// "Insufficient repo permissions" or "GitHub App not installed" without ever
// matching a CODE finding that merely mentions permissions (e.g. "Workflow
// permissions overly broad in ci.yml" or "Add permission checks to admin
// API"). A false close here would silently retire a real repo-content finding,
// so every pattern requires the insufficiency/installation language, not just
// the word "permission".
var appAuthFindingPatterns = []*regexp.Regexp{
	// "Insufficient repo permissions", "insufficient permissions for the app"
	regexp.MustCompile(`(?i)\binsufficient\b[^.\n]*\bpermissions?\b`),
	// "Repo permissions insufficient", "permissions are insufficient"
	regexp.MustCompile(`(?i)\bpermissions?\b[^.\n]*\binsufficient\b`),
	// "GitHub App not installed", "GitHub App lacks repo permissions"
	regexp.MustCompile(`(?i)\bgithub app\b[^.\n]*\b(not installed|missing|lacks|permissions?|access)\b`),
	// The literal 403 body GitHub returns when an installation token cannot
	// act on a repo — agents quote it verbatim in findings.
	regexp.MustCompile(`(?i)resource not accessible by integration`),
}

// IsAppAuthFinding reports whether a finding title describes the hive's own
// GitHub App access (installation / repo permissions) rather than a code
// finding. Only titles — never free-form details — are matched, because the
// title is what PersistAsBeads dedups on and what the digest displays.
func IsAppAuthFinding(title string) bool {
	if title == "" {
		return false
	}
	for _, p := range appAuthFindingPatterns {
		if p.MatchString(title) {
			return true
		}
	}
	return false
}

// CloseHealedAppAuthFindings closes every open advisory-digest bead that
// describes the hive's GitHub App access, across all agent stores. Callers
// invoke it ONLY after the App has PROVEN it can write — a successful
// App-authenticated advisory-digest post — so a finding is never closed on a
// guess (#2575: an "Insufficient repo permissions" bead survived forever after
// the App was correctly installed, because nothing ever re-validated it).
//
// Beads that are already closed/done are left untouched, as are findings that
// do not match the app-auth patterns — a genuinely-failing permission
// condition never reaches this function, because the digest post that gates it
// would have failed. Returns the closed titles for logging.
func CloseHealedAppAuthFindings(stores map[string]*beads.Store) []string {
	var closed []string
	for _, store := range stores {
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Status == beads.StatusClosed || b.Status == beads.StatusDone {
				continue
			}
			if !isAdvisoryBeadType(b.Type) {
				continue
			}
			if !IsAppAuthFinding(b.Title) {
				continue
			}
			if err := store.Close(b.ID); err != nil {
				continue
			}
			_ = store.SetMetadata(b.ID, closeReasonMetadataKey, appAuthHealedCloseReason)
			closed = append(closed, b.Title)
		}
	}
	return closed
}
