package dashboard

import (
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// refFromIssueMap builds the canonical, source-aware identity for one
// actionable item as it appears in the dashboard status payload
// (kubestellar/hive#4245).
//
// ReadyQueue and selectTask both read actionable issues as generic maps — the
// status payload round-trips through JSON, so the concrete github.Issue type is
// gone by the time they see it. Both used to read only the integer "number",
// skip anything at zero, and rebuild "repo#number" inline. That silently
// dropped every Linear and Jira item (they have no issue number) and, wherever
// a zero-numbered item did survive, collapsed unrelated work onto one "repo#0"
// identity — one hold, one cooldown, one active slot shared between them.
//
// repoFull scopes the identity, so the same external key in two repositories
// stays distinct. The returned Ref has an empty Key() when the item carries no
// usable identity at all; callers must skip such an item rather than
// fabricating a key for it.
func refFromIssueMap(repoFull string, issue map[string]any) worksource.Ref {
	return worksource.Ref{
		SourceType: stringFromAny(issue["source_type"]),
		Repo:       repoFull,
		ExternalID: stringFromAny(issue["external_id"]),
		Number:     intFromAny(issue["number"]),
		URL:        stringFromAny(issue["url"]),
	}
}

func dependenciesFromIssueMap(issue map[string]any) []ghpkg.IssueDependency {
	raw, _ := issue["depends_on"].([]any)
	deps := make([]ghpkg.IssueDependency, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key := stringFromAny(m["key"])
		resolved, _ := m["resolved"].(bool)
		deps = append(deps, ghpkg.IssueDependency{Key: key, Resolved: resolved})
	}
	return deps
}

// intFromAny reads an integer that survived a JSON round-trip. encoding/json
// decodes every number into float64 when the target is `any`, but the value can
// still arrive as a real int when the payload was never marshalled (tests, and
// the in-process status builder), so both are accepted.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// stringFromAny reads a string field, yielding "" for a missing or
// wrongly-typed value rather than panicking on the type assertion.
func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}
