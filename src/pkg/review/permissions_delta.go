package review

import (
	"path"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// ChangedFile is one file of a PR diff as the GitHub files API reports it:
// the path, its status (added, modified, removed, renamed) and the unified
// patch hunks. Patch is empty when GitHub omitted it (binary or too large).
type ChangedFile struct {
	Path   string
	Status string
	Patch  string
}

// PermissionsTighteningReason is the audit reason recorded on an aggregate
// whose permissions/sensitive-path risk classes were suppressed because the
// PR's workflow diff only narrows `permissions:` (hivecommons/hive#13068).
const PermissionsTighteningReason = "permissions-tightening-only: workflow diff only removes or narrows permissions scopes; permissions-credentials and sensitive-paths risk classes suppressed"

// permissionsTighteningSuppressedClasses are the DecisionRiskClasses that key
// on the TOPIC of permissions and workflows rather than the DIRECTION of the
// change. They are skipped for a PR WorkflowPermissionsTighteningOnly proved
// to be a strict reduction; every other class still applies.
var permissionsTighteningSuppressedClasses = map[string]bool{
	"permissions-credentials": true,
	"sensitive-paths":         true,
}

// permissionsTighteningSuppressedTerms are the riskEscalationTerms a
// perspective summary saying "permissions narrowed, looks correct" trips on.
var permissionsTighteningSuppressedTerms = map[string]bool{
	"permission": true,
	"privilege":  true,
}

// permissionScopeKeys is the closed set of GITHUB_TOKEN permission scopes.
// A `<key>: read` line outside this set (an action input, an env var) is not
// a permission and must not be mistaken for one.
var permissionScopeKeys = map[string]bool{
	"actions": true, "attestations": true, "checks": true, "contents": true,
	"deployments": true, "discussions": true, "id-token": true, "issues": true,
	"models": true, "packages": true, "pages": true, "pull-requests": true,
	"repository-projects": true, "security-events": true, "statuses": true,
}

const (
	permissionLevelNone = iota
	permissionLevelRead
	permissionLevelWrite
)

var permissionLevels = map[string]int{
	"none": permissionLevelNone, "read": permissionLevelRead, "write": permissionLevelWrite,
}

// permissionShorthandLevels are the `permissions:` one-liners. A bare header
// (`permissions:` followed by a scope block) is permissionHeaderBare: its
// effective level is only knowable from the scopes beneath it.
var permissionShorthandLevels = map[string]int{
	"{}": permissionLevelNone, "read-all": permissionLevelRead, "write-all": permissionLevelWrite,
}

const permissionHeaderBare = -1

var (
	permissionScopeLineRe  = regexp.MustCompile(`^(\s*)([a-z-]+):\s*(none|read|write)\s*(#.*)?$`)
	permissionHeaderLineRe = regexp.MustCompile(`^(\s*)permissions:\s*(\{\}|read-all|write-all)?\s*(#.*)?$`)
	diffHunkHeaderRe       = regexp.MustCompile(`^@@ `)
)

// WorkflowPermissionsTighteningOnly reports whether a PR's diff is a pure
// privilege reduction of GitHub Actions `permissions:` -- the shape of
// hivecommons/pluk#305, which narrowed a workflow-level `contents: write` to
// `contents: read` with green CI and was still parked on a human because
// the review classifier keyed on the topic (permissions, workflows) and not
// the direction (hivecommons/hive#13068).
//
// It is true only when every changed `.github/workflows/*.yml|yaml` patch
// consists solely of (a) removed permission scopes, (b) a scope lowered
// write -> read/none or read -> none at the same block, or (c) a
// `permissions:` block replaced by a provably narrower shorthand, and no
// other changed file is a sentinel-sensitive path. Any added job, step,
// trigger, `uses:` or `secrets.*` line is a non-permission line and fails
// the check; so does a new or widened scope, a new `id-token: write`, a
// removed `permissions:` block, a missing patch, or a deleted or renamed
// workflow. Anything the parser cannot prove is a reduction is reported as
// NOT a reduction: this check only ever removes a human hold, so it fails
// closed to today's behaviour.
func WorkflowPermissionsTighteningOnly(files []ChangedFile) bool {
	sawWorkflow := false
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			return false
		}
		if !isWorkflowPath(p) {
			if intent.PathMatchesAny(p, sentinel.DefaultSensitivePaths) {
				return false
			}
			continue
		}
		sawWorkflow = true
		switch strings.ToLower(strings.TrimSpace(f.Status)) {
		case "", "modified", "changed":
		default:
			return false
		}
		if !patchIsPermissionsTighteningOnly(f.Patch) {
			return false
		}
	}
	return sawWorkflow
}

func isWorkflowPath(p string) bool {
	dir, base := path.Split(p)
	if !strings.HasSuffix(dir, ".github/workflows/") {
		return false
	}
	ext := strings.ToLower(path.Ext(base))
	return ext == ".yml" || ext == ".yaml"
}

// permissionLineChange is one +/- line of a hunk that the parser recognised
// as a permissions line. indent pins it to its block: a scope lowered in
// job A must not pay for a scope raised in job B.
type permissionLineChange struct {
	indent string
	key    string // scope key, or "permissions" for a header/shorthand line
	level  int
}

func patchIsPermissionsTighteningOnly(patch string) bool {
	if strings.TrimSpace(patch) == "" {
		return false
	}
	hunks := splitDiffHunks(patch)
	if len(hunks) == 0 {
		return false
	}
	changed := false
	for _, hunk := range hunks {
		ok, sawChange := hunkIsPermissionsTighteningOnly(hunk)
		if !ok {
			return false
		}
		changed = changed || sawChange
	}
	return changed
}

func splitDiffHunks(patch string) [][]string {
	var hunks [][]string
	var cur []string
	for _, line := range strings.Split(patch, "\n") {
		if diffHunkHeaderRe.MatchString(line) {
			if cur != nil {
				hunks = append(hunks, cur)
			}
			cur = []string{}
			continue
		}
		if cur == nil {
			// Text before the first @@ is a file header the API never
			// includes; anything else there is not a diff we understand.
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil
		}
		cur = append(cur, line)
	}
	if cur != nil {
		hunks = append(hunks, cur)
	}
	return hunks
}

// hunkIsPermissionsTighteningOnly classifies one hunk. It returns ok=false
// the moment a changed line is anything but a permissions line or blank.
// Otherwise it pairs each added permissions line with a removed one in the
// same change run (the contiguous +/- lines between two context lines, which
// is where a unified diff puts a replaced line) at the same indent and key
// that was strictly higher. Unpaired added lines are new or widened grants;
// unpaired removed scope lines are plain removals; an unpaired removed
// header is a dropped block (which widens back to the inherited or default
// grant) and fails.
func hunkIsPermissionsTighteningOnly(lines []string) (ok bool, changed bool) {
	var removed, added []permissionLineChange
	flush := func() bool {
		ok := changeRunNarrows(removed, added)
		removed, added = nil, nil
		return ok
	}
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, `\`) {
			continue
		}
		marker, body := line[0], line[1:]
		switch marker {
		case ' ':
			if !flush() {
				return false, false
			}
			continue
		case '-', '+':
		default:
			return false, false
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		change, isPerm := parsePermissionLine(body)
		if !isPerm {
			return false, false
		}
		changed = true
		if marker == '-' {
			removed = append(removed, change)
		} else {
			added = append(added, change)
		}
	}
	return flush(), changed
}

func changeRunNarrows(removed, added []permissionLineChange) bool {
	used := make([]bool, len(removed))
	for _, a := range added {
		paired := false
		for i, r := range removed {
			if used[i] || r.indent != a.indent || r.key != a.key {
				continue
			}
			if !permissionReplacementNarrows(r.level, a.level) {
				continue
			}
			used[i] = true
			paired = true
			break
		}
		if !paired {
			return false
		}
	}
	for i, r := range removed {
		if !used[i] && r.key == "permissions" {
			return false
		}
	}
	return true
}

// permissionReplacementNarrows says whether replacing a permissions line of
// level from with one of level to is provably a reduction. A bare header is
// of unknown level: only `{}` may replace it, and it may replace nothing,
// since the scopes that follow a new header would be new grants.
func permissionReplacementNarrows(from, to int) bool {
	if to == permissionHeaderBare {
		return false
	}
	if from == permissionHeaderBare {
		return to == permissionLevelNone
	}
	return to < from
}

func parsePermissionLine(body string) (permissionLineChange, bool) {
	if m := permissionHeaderLineRe.FindStringSubmatch(body); m != nil {
		level := permissionHeaderBare
		if m[2] != "" {
			level = permissionShorthandLevels[m[2]]
		}
		return permissionLineChange{indent: m[1], key: "permissions", level: level}, true
	}
	if m := permissionScopeLineRe.FindStringSubmatch(body); m != nil && permissionScopeKeys[m[2]] {
		return permissionLineChange{indent: m[1], key: m[2], level: permissionLevels[m[3]]}, true
	}
	return permissionLineChange{}, false
}
