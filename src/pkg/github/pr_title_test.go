package github

import (
	"regexp"
	"strings"
	"testing"
)

var (
	projectbluefinCommonPRTitleRE = regexp.MustCompile(`^(feat|fix|chore|docs|style|refactor|perf|test|ci|build|revert)(\(.+\))?(!)?: .+`)
	semanticPullRequestHeaderRE   = regexp.MustCompile(`^(\w*)(?:\((.*)\))?!?: (.*)$`)
)

func TestNormalizePRTitle_ProjectBluefinSamplesPassBothGates(t *testing.T) {
	cases := map[string]string{
		"[scanner] fix(devmode): default virt-manager to qemu:///session, not qemu:///system":                 "fix(devmode): default virt-manager to qemu:///session, not qemu:///system [scanner]",
		"[scanner] fix(bonedigger-report): derive booted image name from live bootc status":                   "fix(bonedigger-report): derive booted image name from live bootc status [scanner]",
		"[architect] docs: record that system_files/nvidia/ is unconsumed and has no CDI preset":              "docs: record that system_files/nvidia/ is unconsumed and has no CDI preset [architect]",
		"[scanner] fix(ci): retry transient GHCR errors in check-oci-refs.py":                                 "fix(ci): retry transient GHCR errors in check-oci-refs.py [scanner]",
		"[quality] test: BATS coverage for etc/profile.d/uwelcome.sh":                                         "test: BATS coverage for etc/profile.d/uwelcome.sh [quality]",
		"[sec-check] fix: polkit org.ublue.privileged.user.setup defaults no/no/auth_admin":                   "fix: polkit org.ublue.privileged.user.setup defaults no/no/auth_admin [sec-check]",
		"[architect] test: repair broken-at-birth bats suites (test_ujust mock brew, powerwash bctl)":         "test: repair broken-at-birth bats suites (test_ujust mock brew, powerwash bctl) [architect]",
		"[quality] test: coverage for mutations.ts confirmed comment plans and fail-closed head revalidation": "test: coverage for mutations.ts confirmed comment plans and fail-closed head revalidation [quality]",
		"[sec-check] fix: verify build-provenance attestation before running published appliance images":      "fix: verify build-provenance attestation before running published appliance images [sec-check]",
		"[architect] test: extension module reachability gate":                                                "test: extension module reachability gate [architect]",
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := NormalizePRTitle(input)
			if got != want {
				t.Fatalf("NormalizePRTitle(%q) = %q, want %q", input, got, want)
			}
			if !projectbluefinCommonPRTitleRE.MatchString(got) {
				t.Fatalf("normalized title %q does not satisfy projectbluefin/common title regex", got)
			}
			if !semanticPullRequestHeaderRE.MatchString(got) {
				t.Fatalf("normalized title %q does not satisfy amannn/action-semantic-pull-request header regex", got)
			}
		})
	}
}

func TestNormalizePRTitle_ConservativeCases(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "already conventional", input: "fix(ci): retry transient GHCR errors", want: "fix(ci): retry transient GHCR errors"},
		{name: "prefixed but non conventional", input: "[scanner] retry transient GHCR errors", want: "[scanner] retry transient GHCR errors"},
		{name: "empty", input: "", want: ""},
		{name: "whitespace only", input: "   \t", want: "   \t"},
		{name: "prefix only", input: "[scanner]", want: "[scanner]"},
		{name: "prefix whitespace only", input: "[scanner]   ", want: "[scanner]   "},
		{name: "unmatched bracket", input: "[scanner fix: retry transient GHCR errors", want: "[scanner fix: retry transient GHCR errors"},
		{name: "suffix already present", input: "[scanner] fix: retry transient GHCR errors [scanner]", want: "fix: retry transient GHCR errors [scanner]"},
		{name: "unsupported type", input: "[scanner] featx: not a conventional type", want: "[scanner] featx: not a conventional type"},
		{name: "safe non empty", input: "[scanner] nope", want: "[scanner] nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizePRTitle(tc.input); got != tc.want {
				t.Fatalf("NormalizePRTitle(%q) = %q, want %q", tc.input, got, tc.want)
			}
			if tc.input != "" && NormalizePRTitle(tc.input) == "" {
				t.Fatalf("NormalizePRTitle(%q) returned empty for non-empty input", tc.input)
			}
		})
	}
}

func TestNormalizePRTitle_LengthBoundary(t *testing.T) {
	lane := "scanner"
	suffix := " [" + lane + "]"
	prefix := "[" + lane + "] "
	base := "fix: "

	maxRemainderSubject := strings.Repeat("a", maxGitHubPRTitleLength-len(base)-len(suffix))
	atLimitInput := prefix + base + maxRemainderSubject
	atLimitWant := base + maxRemainderSubject + suffix
	if got := NormalizePRTitle(atLimitInput); got != atLimitWant {
		t.Fatalf("256-character normalized title = %q, want %q", got, atLimitWant)
	}
	if len([]rune(atLimitWant)) != maxGitHubPRTitleLength {
		t.Fatalf("test setup produced %d-rune title, want %d", len([]rune(atLimitWant)), maxGitHubPRTitleLength)
	}

	tooLongInput := atLimitInput + "a"
	if got := NormalizePRTitle(tooLongInput); got != tooLongInput {
		t.Fatalf("title exceeding GitHub's limit after normalization = %q, want unchanged %q", got, tooLongInput)
	}
}

func TestNormalizePRTitleEmoji(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		// hivecommons/hive#13131: a docs PR opened as 📝 failed the PR Type gate.
		{name: "docs under proposal emoji", input: "📝 docs: describe Prow dco-signoff labels as coexisting with hive's dco pair", want: "📖 docs: describe Prow dco-signoff labels as coexisting with hive's dco pair"},
		{name: "docs already right", input: "📖 docs: explain exit 77", want: "📖 docs: explain exit 77"},
		{name: "proposal keeps its emoji", input: "📝 proposal: central stage engine", want: "📝 proposal: central stage engine"},
		{name: "rfc is a proposal", input: "📖 rfc: opportunity lab", want: "📝 rfc: opportunity lab"},
		{name: "fix under docs emoji", input: "📖 fix(ci): retry GHCR", want: "🐛 fix(ci): retry GHCR"},
		{name: "fix under lock emoji", input: "🔒 fix: polkit defaults", want: "🐛 fix: polkit defaults"},
		{name: "feat", input: "🐛 feat: owner-only sharing", want: "✨ feat: owner-only sharing"},
		{name: "breaking marker wins", input: "✨ feat(api)!: drop v1", want: "⚠️ feat(api)!: drop v1"},
		{name: "breaking presentation variant is equal", input: "⚠ feat!: drop v1", want: "⚠ feat!: drop v1"},
		{name: "chore", input: "📖 chore: bump deps", want: "🌱 chore: bump deps"},
		{name: "scoped docs", input: "📝 docs(readme): fix link", want: "📖 docs(readme): fix link"},
		{name: "extra spaces", input: "📝   docs: spaced", want: "📖 docs: spaced"},
		// Never invent an emoji: repositories gating on a bare header keep it.
		{name: "bare conventional header", input: "docs: explain foo", want: "docs: explain foo"},
		{name: "lane-suffixed bare header", input: "fix: retry [scanner]", want: "fix: retry [scanner]"},
		// Unknown type: the emoji is the author's, not the convention's.
		{name: "unknown type keeps emoji", input: "📝 planning: Q4", want: "📝 planning: Q4"},
		{name: "no header", input: "📝 sweep the docs", want: "📝 sweep the docs"},
		{name: "upper-case type", input: "📝 Docs: title case", want: "📖 Docs: title case"},
		{name: "empty", input: "", want: ""},
		{name: "unicode letter first is not an emoji", input: "Ärger fix: nope", want: "Ärger fix: nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizePRTitleEmoji(tc.input); got != tc.want {
				t.Fatalf("NormalizePRTitleEmoji(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
	long := "📝 docs: " + strings.Repeat("x", maxGitHubPRTitleLength)
	if got := NormalizePRTitleEmoji(long); got != long {
		t.Fatalf("over-long title must be returned unchanged")
	}
}

func TestPRTitleEmojiFor(t *testing.T) {
	if got := PRTitleEmojiFor("docs", false); got != PRTitleEmojiDocs {
		t.Fatalf("docs → %q", got)
	}
	if got := PRTitleEmojiFor("PROPOSAL", false); got != PRTitleEmojiProposal {
		t.Fatalf("proposal → %q", got)
	}
	if got := PRTitleEmojiFor("docs", true); got != PRTitleEmojiBreaking {
		t.Fatalf("breaking → %q", got)
	}
	if got := PRTitleEmojiFor("featx", false); got != "" {
		t.Fatalf("unknown → %q", got)
	}
}
