package github

import "strings"

// dependencyBotAuthors are forge bots whose PRs are dependency bumps, not hive
// fix attempts. They carry the "[bot]" suffix that otherwise marks a PR as
// agent-authored, but nothing in the hive opened them and no hive agent is
// iterating on them. Mirrors the hub's default contribute deny-authors list
// (config: hub.contribute_deny_authors).
var dependencyBotAuthors = map[string]bool{
	"renovate[bot]":    true,
	"dependabot[bot]":  true,
	"mergeraptor[bot]": true,
}

// IsDependencyBotAuthor reports whether login is one of the dependency bots.
func IsDependencyBotAuthor(login string) bool {
	return dependencyBotAuthors[strings.ToLower(strings.TrimSpace(login))]
}

// DependencyBotBranch reports whether a PR's head branch belongs to a
// dependency bot that keeps it up to date itself: a `renovate/` head ref (any
// Renovate deployment, whatever account it runs as) or a dependency-bot
// author. Renovate refuses to rebase a branch once another identity has
// committed to it, so every hive write that merges the base into such a
// branch — update-branch, a merges-API merge-forward, a fixer rebase — strands
// it behind its base for good (projectbluefin/utah#1097: three hive base
// merges, then "will not automatically rebase … does not recognize the last
// commit author"). Callers leave the base sync to the bot instead.
func DependencyBotBranch(headRef, author string) bool {
	return strings.HasPrefix(strings.TrimSpace(headRef), "renovate/") || IsDependencyBotAuthor(author)
}
