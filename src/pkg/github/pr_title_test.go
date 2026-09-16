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
