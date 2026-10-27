package review

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

var defaultBotAuthors = []string{"dependabot[bot]", "renovate[bot]", "github-actions[bot]", "*[bot]"}

func TestIsBotAuthor(t *testing.T) {
	own := "hivecommons-hive[bot]"
	cases := []struct {
		login    string
		patterns []string
		want     bool
	}{
		{"dependabot[bot]", defaultBotAuthors, true},
		{"Renovate[bot]", defaultBotAuthors, true},
		{"github-actions[bot]", defaultBotAuthors, true},
		{"some-other-app[bot]", defaultBotAuthors, true},
		{own, defaultBotAuthors, false},
		{strings.ToUpper(own), defaultBotAuthors, false},
		{"clubanderson", defaultBotAuthors, false},
		{"", defaultBotAuthors, false},
		{"some-other-app[bot]", []string{"dependabot[bot]"}, false},
		{"dependabot[bot]", nil, false},
		{"pre-commit-ci[bot]", []string{" *[BOT] "}, true},
		{"bot", []string{"*"}, false},
	}
	for _, tc := range cases {
		if got := IsBotAuthor(tc.login, tc.patterns, own); got != tc.want {
			t.Errorf("IsBotAuthor(%q, %v) = %v, want %v", tc.login, tc.patterns, got, tc.want)
		}
	}
	// Without an own-app login every [bot] matches the wildcard.
	if !IsBotAuthor(own, defaultBotAuthors, "") {
		t.Fatal("own-app exclusion applied with no own app configured")
	}
}

func TestPathMatchesAnyGlobs(t *testing.T) {
	defaults := []string{".github/workflows/**", "Dockerfile*", "**/Dockerfile*"}
	cases := map[string]bool{
		".github/workflows/ci.yml":        true,
		".github/workflows/sub/x.yaml":    true,
		".github/dependabot.yml":          false,
		"Dockerfile":                      true,
		"Dockerfile.ci":                   true,
		"build/Dockerfile":                true,
		"images/agent/Dockerfile.alpine":  true,
		"docs/Dockerfiles.md":             true,
		"go.mod":                          false,
		"package-lock.json":               false,
		"src/package-lock.json":           false,
		"":                                false,
		"./.github/workflows/release.yml": true,
	}
	for p, want := range cases {
		if got := PathMatchesAny(p, defaults); got != want {
			t.Errorf("PathMatchesAny(%q) = %v, want %v", p, got, want)
		}
	}
	if PathMatchesAny("a/b/c.go", []string{"a/*.go"}) {
		t.Fatal("single star crossed a directory")
	}
	if !PathMatchesAny("a/b/c.go", []string{"a/**/*.go"}) || !PathMatchesAny("a/c.go", []string{"a/**/*.go"}) {
		t.Fatal("double star did not span zero or more directories")
	}
	if !PathMatchesAny("x.go", []string{"?.go"}) || PathMatchesAny("xy.go", []string{"?.go"}) {
		t.Fatal("question mark glob wrong")
	}
	if PathMatchesAny("a.go", nil) || PathMatchesAny("a.go", []string{"", "  "}) {
		t.Fatal("empty globs matched")
	}
}

func botRouteOpts(model string, agents ...AgentCapability) DispatchOptions {
	o := swarmOpts(agents...)
	o.AIAuthor = "hive-bot[bot]"
	o.BotAuthorRoute = BotAuthorRoute{Authors: defaultBotAuthors, Model: model, FullReviewPaths: []string{".github/workflows/**", "Dockerfile*"}}
	return o
}

func botPR(author string, files ...string) PullRequest {
	pr := dispatchPR("sha1")
	pr.Author = author
	pr.ChangedFiles = files
	pr.ChangedFilesKnown = true
	return pr
}

func TestBotAuthorRouteSelectsCheapModel(t *testing.T) {
	pr := botPR("dependabot[bot]", "go.mod", "go.sum")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, botRouteOpts("gpt-6-luna", reviewer("r1")))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if !k.BotAuthorRouted || k.ReviewModel != "gpt-6-luna" || k.ReviewBackend != "" {
		t.Fatalf("kick not routed: %+v", k)
	}
	if !strings.Contains(k.Message, "Run this review with `gpt-6-luna`") {
		t.Fatalf("prompt lacks the model instruction:\n%s", k.Message)
	}
	if plan.State.Pending[0].ReviewModel != "gpt-6-luna" {
		t.Fatalf("pending stamped %q, want gpt-6-luna", plan.State.Pending[0].ReviewModel)
	}
}

func TestBotAuthorRouteBackendModelAndPoolOverride(t *testing.T) {
	pr := botPR("renovate[bot]", "package-lock.json")
	pr.AuthorModel = ""
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "claude-opus-4-6"})
	opts := botRouteOpts("gemini-flash-3-5", agent)
	opts.BotAuthorRoute.Backend = "copilot"
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if !k.BotAuthorRouted || k.ReviewModel != "gemini-flash-3-5" || k.ReviewBackend != "copilot" || k.ReviewModelFallback {
		t.Fatalf("pool model not replaced by the route: %+v", k)
	}
	if !strings.Contains(k.Message, "`copilot/gemini-flash-3-5`") {
		t.Fatalf("prompt lacks backend/model:\n%s", k.Message)
	}
}

func TestBotAuthorRouteNotTaken(t *testing.T) {
	cases := []struct {
		name string
		pr   PullRequest
		opts func(DispatchOptions) DispatchOptions
	}{
		{"route off", botPR("dependabot[bot]", "go.mod"), func(o DispatchOptions) DispatchOptions { o.BotAuthorRoute.Model = ""; return o }},
		{"human author", botPR("clubanderson", "go.mod"), nil},
		{"hive's own app", botPR("hive-bot[bot]", "go.mod"), nil},
		{"workflow touched", botPR("dependabot[bot]", "go.mod", ".github/workflows/ci.yml"), nil},
		{"dockerfile touched", botPR("dependabot[bot]", "Dockerfile.agent"), nil},
		{"changed files unknown", func() PullRequest { p := botPR("dependabot[bot]"); p.ChangedFilesKnown = false; return p }(), nil},
		{"not in a narrowed list", botPR("some-app[bot]", "go.mod"), func(o DispatchOptions) DispatchOptions {
			o.BotAuthorRoute.Authors = []string{"dependabot[bot]"}
			return o
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := botRouteOpts("gpt-6-luna", reviewer("r1"))
			if tc.opts != nil {
				opts = tc.opts(opts)
			}
			plan := PlanDispatch([]PullRequest{tc.pr}, Artifact{}, DispatchState{}, opts)
			if len(plan.ReviewKicks) != 1 {
				t.Fatalf("kicks = %d", len(plan.ReviewKicks))
			}
			k := plan.ReviewKicks[0]
			if k.BotAuthorRouted || k.ReviewModel != "" {
				t.Fatalf("routed when it should not be: %+v", k)
			}
		})
	}
	// An empty known file list (GitHub returned nothing) is still known and
	// routes: there is nothing to except on.
	plan := PlanDispatch([]PullRequest{botPR("dependabot[bot]")}, Artifact{}, DispatchState{}, botRouteOpts("gpt-6-luna", reviewer("r1")))
	if len(plan.ReviewKicks) != 1 || !plan.ReviewKicks[0].BotAuthorRouted {
		t.Fatalf("empty known file list did not route: %+v", plan.ReviewKicks)
	}
}

func TestBotAuthorRouteKeepsPoolSkipAndHold(t *testing.T) {
	// A pool that excludes the author's family with fallback skip still
	// skips: the route does not override a decision to withhold.
	pr := botPR("dependabot[bot]", "go.mod")
	pr.AuthorModel = "claude-fable-5.1"
	agent := poolReviewer("r1", config.ReviewModelPoolEntry{Backend: "copilot", Model: "claude-fable-5.1"})
	agent.ReviewModels.Fallback = config.ReviewModelsFallbackSkip
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, botRouteOpts("gpt-6-luna", agent))
	if len(plan.ReviewKicks) != 0 || len(plan.SkippedReviewModels) == 0 {
		t.Fatalf("route overrode a pool skip: kicks=%+v skipped=%d", plan.ReviewKicks, len(plan.SkippedReviewModels))
	}
}

func TestBotAuthorRouteCombinedPerspectives(t *testing.T) {
	pr := botPR("dependabot[bot]", "go.mod")
	opts := botRouteOpts("gpt-6-luna", reviewer("r1"))
	opts.CombinedPerspectives = true
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 || !plan.ReviewKicks[0].BotAuthorRouted || plan.ReviewKicks[0].ReviewModel != "gpt-6-luna" {
		t.Fatalf("combined kick not routed: %+v", plan.ReviewKicks)
	}
}
