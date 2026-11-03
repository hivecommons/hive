package review

import (
	"regexp"
	"strings"
)

// Bot-author model route (hivecommons/hive: reviewer-cost analysis, Oct 13-27
// 2026). PRs opened by other bots (Dependabot, Renovate, github-actions) cost
// $38.61/day across three hosted hives, $30.60/day on the busiest, every one
// reviewed on the frontier model. A lockfile bump does not need it. The route
// sends a bot-authored PR's review kick to a configured cheaper model unless
// the PR touches a path the operator wants read at full strength.

// BotAuthorRoute is review.bot_authors / review.bot_author_model /
// review.bot_author_full_review_paths as the planner sees them. Model empty
// means the route is off.
type BotAuthorRoute struct {
	Authors         []string
	Backend         string
	Model           string
	FullReviewPaths []string
}

// Enabled reports whether a model is configured for the route.
func (r BotAuthorRoute) Enabled() bool {
	return strings.TrimSpace(r.Model) != ""
}

// IsBotAuthor reports whether login matches one of the patterns: an exact
// case-insensitive login, or a `*suffix` pattern such as `*[bot]`. The hive's
// own App login (ownApp) never matches, whatever the patterns say: the hive's
// own PRs are not dependency bumps.
func IsBotAuthor(login string, patterns []string, ownApp string) bool {
	login = strings.TrimSpace(login)
	if login == "" || strings.EqualFold(login, strings.TrimSpace(ownApp)) {
		return false
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "*") {
			if suffix := p[1:]; suffix != "" && strings.HasSuffix(strings.ToLower(login), strings.ToLower(suffix)) {
				return true
			}
			continue
		}
		if strings.EqualFold(login, p) {
			return true
		}
	}
	return false
}

// PathMatchesAny reports whether path matches any glob. `**` spans path
// separators, `*` and `?` do not; a pattern without a slash is matched against
// the base name as well as the whole path, so `Dockerfile*` finds
// `build/Dockerfile.ci`.
func PathMatchesAny(path string, globs []string) bool {
	path = strings.TrimSpace(strings.TrimPrefix(path, "./"))
	if path == "" {
		return false
	}
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	for _, g := range globs {
		g = strings.TrimSpace(strings.TrimPrefix(g, "./"))
		if g == "" {
			continue
		}
		re, err := globRegexp(g)
		if err != nil {
			continue
		}
		if re.MatchString(path) || (!strings.Contains(g, "/") && re.MatchString(base)) {
			return true
		}
	}
	return false
}

func globRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			i++
			// `**/` may match nothing at all, so `.github/workflows/**`
			// and `**/Dockerfile` both behave as operators expect.
			if i+1 < len(glob) && glob[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// botAuthorRouteFor decides whether this PR's review kick goes to the bot
// route's model. ok is false, with a reason, when the route is off, the
// author is not a listed bot, the changed-file list is unknown (a PR that
// cannot be checked against FullReviewPaths is not downgraded), or a changed
// file matches FullReviewPaths.
func botAuthorRouteFor(pr PullRequest, opts DispatchOptions) (backend, model string, ok bool, reason string) {
	route := opts.BotAuthorRoute
	if !route.Enabled() {
		return "", "", false, "route off"
	}
	if !IsBotAuthor(pr.Author, route.Authors, opts.AIAuthor) {
		return "", "", false, "author not a listed bot"
	}
	if !pr.ChangedFilesKnown {
		return "", "", false, "changed files unknown"
	}
	for _, f := range pr.ChangedFiles {
		if PathMatchesAny(f, route.FullReviewPaths) {
			return "", "", false, "touches full-review path " + f
		}
	}
	return strings.TrimSpace(route.Backend), strings.TrimSpace(route.Model), true, ""
}

// applyBotAuthorRoute replaces the model the pool chose with the bot route's
// when the PR qualifies. A pool decision to skip or hold for a human is
// kept: those are about independence from the author model, which a bot PR
// has none of, but the planner does not second-guess them here.
func applyBotAuthorRoute(choice ReviewModelChoice, pr PullRequest, opts DispatchOptions) (ReviewModelChoice, bool) {
	if choice.Skip || choice.RequiresHuman {
		return choice, false
	}
	backend, model, ok, _ := botAuthorRouteFor(pr, opts)
	if !ok {
		return choice, false
	}
	choice.Backend = backend
	choice.Model = model
	choice.Fallback = false
	choice.Configured = true
	return choice, true
}
