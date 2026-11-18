package authz_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/authz"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// This file is the parity corpus for the first mirrored decision, the push
// broker's changed-files guard (RFC #18988 phase 0/1). It lives in an external
// test package so it can import pushbroker, which itself imports authz.

// existingChangedFilesVerdict is the push broker's guard as it stands today
// (pushbroker.Broker.guardChangedFiles): an empty push is refused first, then
// a push that touches a protected path, using the exported matcher the broker
// itself calls. The second return is the list the broker would record as
// ProtectedReject.
func existingChangedFilesVerdict(files, guards []string) (authz.Verdict, []string) {
	if len(files) == 0 {
		return authz.Verdict{Effect: authz.EffectDeny, ReasonCode: authz.ReasonEmptyPush}, nil
	}
	if rejected := pushbroker.ProtectedPathViolations(files, guards); len(rejected) > 0 {
		return authz.Verdict{Effect: authz.EffectDeny, ReasonCode: authz.ReasonProtectedPath}, rejected
	}
	return authz.Verdict{Effect: authz.EffectAllow, ReasonCode: authz.ReasonOK}, nil
}

func brokerPushRequest(files, guards []string) authz.Request {
	return authz.Request{
		Actor:    authz.Actor{Agent: "scanner", Identified: true},
		Action:   authz.Action{Kind: authz.KindBranchPush, Surface: authz.SurfaceBroker, Forge: authz.ForgeGitHub},
		Resource: authz.Resource{Repo: "org/name", Branch: "work", Paths: files},
		Context:  authz.Context{ProtectedPaths: guards},
	}
}

// assertParity fails when the seam and the existing guard disagree on effect,
// reason code, or how many files were rejected.
func assertParity(t *testing.T, label string, files, guards []string) {
	t.Helper()
	want, rejected := existingChangedFilesVerdict(files, guards)
	got := authz.Decide(brokerPushRequest(files, guards))
	if !want.Matches(got) {
		t.Errorf("%s: files=%q guards=%q: seam = %s/%s, existing guard = %s/%s", label, files, guards, got.Effect, got.ReasonCode, want.Effect, want.ReasonCode)
	}
	if got.MatchedPaths != len(rejected) {
		t.Errorf("%s: files=%q guards=%q: seam matched %d paths, existing guard rejected %d (%q)", label, files, guards, got.MatchedPaths, len(rejected), rejected)
	}
}

func TestKindBranchPushIsTheEffectsKind(t *testing.T) {
	if authz.KindBranchPush != effects.KindBranchPush {
		t.Fatalf("authz.KindBranchPush = %q, effects.KindBranchPush = %q: one action vocabulary, not two", authz.KindBranchPush, effects.KindBranchPush)
	}
}

// TestParityPushBrokerChangedFiles replays the cases the broker's own tests
// already encode (pushbroker_test.go and pushbroker_coverage_test.go) through
// the seam. Every row must produce the same (effect, reason code) as the guard.
func TestParityPushBrokerChangedFiles(t *testing.T) {
	defaults := pushbroker.DefaultProtectedPaths
	corpus := []struct {
		name   string
		files  []string
		guards []string
	}{
		{"TestProtectedPathViolationsTable", []string{"policies/guard.yaml", "OWNERS", "hive.yaml.dashboard", "pkg/safe.go"}, defaults},
		{"TestBrokerRejectsProtectedPaths", []string{".github/workflows/ci.yaml"}, defaults},
		{"quoted names", []string{".github/workflows/dépl.yml", `policies/a"b.md`, "policies/c\td.md"}, defaults},
		{"heuristic fixture is not a path rule", []string{"fixture_test.go"}, defaults},
		{"custom protected override", []string{"docs/notes.md"}, []string{"docs/"}},
		{"custom override leaves defaults unprotected", []string{"OWNERS"}, []string{"docs/"}},
		{"empty push", nil, defaults},
		{"empty slice", []string{}, defaults},
	}
	for _, protected := range []string{".github/workflows/ci.yml", "policies/x.rego", "OWNERS", "bin/gh-wrapper.sh"} {
		// TestBrokerRejectsRenamedProtectedPaths: both rename endpoints are checked.
		corpus = append(corpus, struct {
			name   string
			files  []string
			guards []string
		}{"rename endpoints " + protected, []string{protected, "notes/moved.txt"}, defaults})
	}
	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) { assertParity(t, tc.name, tc.files, tc.guards) })
	}
}

// TestParityPushBrokerPathShapes crosses protected-path guards with the ways a
// path can be spelled, so a difference in cleaning or in the directory-versus-
// file rule shows up as a mismatch rather than going unnoticed.
func TestParityPushBrokerPathShapes(t *testing.T) {
	guardSets := [][]string{
		pushbroker.DefaultProtectedPaths,
		{"docs/"},
		{".github/workflows"},
		{"./policies/"},
		{"a/b/"},
		{"OWNERS", "OWNERS/"},
		{"/"},
		{"."},
		{"./"},
		{""},
		{"../"},
		nil,
	}
	var files []string
	for _, guard := range pushbroker.DefaultProtectedPaths {
		bare := strings.TrimSuffix(guard, "/")
		files = append(files,
			guard, bare, bare+"/child", bare+"/deep/er/child.txt", "./"+bare, "./"+bare+"/child",
			"x/"+bare, "x/"+bare+"/child", strings.ToUpper(bare), bare+"-old", bare+"x/child", bare+"//child",
			"sub/../"+bare, "sub/../"+bare+"/child")
	}
	files = append(files, "", ".", "..", "../x", "/", "/abs/policies/x", "a/../OWNERS", "a//b", "docs/readme.md", "a/b/c", "a/b")

	checked := 0
	for gi, guards := range guardSets {
		for _, file := range files {
			assertParity(t, fmt.Sprintf("guardset %d single", gi), []string{file}, guards)
			checked++
		}
		assertParity(t, fmt.Sprintf("guardset %d all", gi), files, guards)
		assertParity(t, fmt.Sprintf("guardset %d safe only", gi), []string{"pkg/safe.go", "README.md"}, guards)
		assertParity(t, fmt.Sprintf("guardset %d empty", gi), nil, guards)
	}
	if checked == 0 {
		t.Fatal("the shape corpus is empty")
	}
}

// TestParityHoldsForPolicyVersionAcrossListOrder guards the one input the
// existing guard does not have: the version. Reordering the protected list must
// change nothing about the effect or the reason.
func TestParityHoldsForPolicyVersionAcrossListOrder(t *testing.T) {
	forward := slices.Clone(pushbroker.DefaultProtectedPaths)
	reversed := slices.Clone(forward)
	slices.Reverse(reversed)
	files := []string{"OWNERS", "pkg/a.go"}
	a := authz.Decide(brokerPushRequest(files, forward))
	b := authz.Decide(brokerPushRequest(files, reversed))
	if a != b {
		t.Fatalf("list order changed the decision: %+v vs %+v", a, b)
	}
}
