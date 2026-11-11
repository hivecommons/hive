package proxy

// isGitHubGraphQLMutation reports whether the document holds a depth-zero
// mutation operation. An anchored `^\s*mutation` regex misses same-line
// prefixes (a leading fragment, comma or BOM), so every GitHub GraphQL gate
// uses the string- and comment-aware token scan the Linear path already
// relies on.
func isGitHubGraphQLMutation(query string) bool {
	return countGraphQLKeyword(query, "mutation") > 0
}
