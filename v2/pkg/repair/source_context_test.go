package repair

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestRepairSourceContextIsTrackedBoundedDeterministicAndSafe(t *testing.T) {
	repository, _ := seedGitRepository(t)
	writeRepairSourceFixture(t, repository, "package.json", "{\"scripts\":{\"test\":\"node test.js\"}}\n")
	writeRepairSourceFixture(t, repository, "docs/outside.txt", "outside-selection-marker\n")
	writeRepairSourceFixture(t, repository, "src/a.txt", "alpha\n")
	writeRepairSourceFixture(t, repository, "src/auth/session.go", "auth-path-marker\n")
	writeRepairSourceFixture(t, repository, "src/binary.dat", "BINARY_PRIVATE_MARKER\x00")
	if err := os.WriteFile(filepath.Join(repository, "src", "invalid.txt"), []byte{0xff, 0xfe, 0xfd}, 0o600); err != nil {
		t.Fatal(err)
	}
	writeRepairSourceFixture(t, repository, "src/oversized.txt", strings.Repeat("x", repairSourceContextMaxFileBytes+1))
	writeRepairSourceFixture(t, repository, "src/untracked.txt", "untracked-marker\n")
	writeRepairSourceFixture(t, repository, "AGENTS.md", "HIVE_MALICIOUS_AGENTS_CONTEXT_MARKER\n")
	writeRepairSourceFixture(t, repository, ".codex/config.toml", "developer_instructions = \"HIVE_MALICIOUS_CODEX_CONFIG_CONTEXT_MARKER\"\n")
	writeRepairSourceFixture(t, repository, "src/nested/.codex/config.toml", "developer_instructions = \"HIVE_NESTED_CODEX_CONFIG_CONTEXT_MARKER\"\n")
	runCommand(t, repository, "git", "add", "package.json", "docs/outside.txt", "src/a.txt", "src/auth/session.go", "src/binary.dat", "src/invalid.txt", "src/oversized.txt", "AGENTS.md", ".codex/config.toml", "src/nested/.codex/config.toml")

	// A Git symlink must be excluded even when core.symlinks=false leaves a
	// regular placeholder file in the Windows worktree.
	writeRepairSourceFixture(t, repository, "src/link.txt", "SYMLINK_TARGET_SENTINEL")
	blob := strings.TrimSpace(gitOutput(t, repository, "hash-object", "-w", "src/link.txt"))
	runCommand(t, repository, "git", "update-index", "--add", "--cacheinfo", "120000,"+blob+",src/link.txt")
	tree := strings.TrimSpace(gitOutput(t, repository, "write-tree"))

	first, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"**/*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"**/*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("repair source context is not deterministic")
	}
	if len(first) > repairSourceContextMaxBytes {
		t.Fatalf("rendered context size = %d, limit = %d", len(first), repairSourceContextMaxBytes)
	}
	for _, forbidden := range []string{"auth-path-marker", "BINARY_PRIVATE_MARKER", "untracked-marker", "SYMLINK_TARGET_SENTINEL", "HIVE_MALICIOUS_AGENTS_CONTEXT_MARKER", "HIVE_MALICIOUS_CODEX_CONFIG_CONTEXT_MARKER", "HIVE_NESTED_CODEX_CONFIG_CONTEXT_MARKER", strings.Repeat("x", 256)} {
		if strings.Contains(first, forbidden) {
			t.Fatalf("unsafe or unselected source entered context: %q", forbidden)
		}
	}
	document := decodeRepairSourceContext(t, first)
	if len(document.Files) != 4 || document.Files[0].Path != "package.json" || document.Files[1].Path != "docs/outside.txt" || document.Files[2].Path != "src/a.txt" || document.Files[3].Path != "src/value.txt" {
		t.Fatalf("included source files = %+v", document.Files)
	}
	if document.SourceTreeOID != tree || document.Files[2].Content != "alpha\n" || document.Files[2].GitMode != "100644" || !validGitObjectOID(document.Files[2].GitOID) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(document.Files[2].SHA256) {
		t.Fatalf("source entry lost path/digest/content binding: %+v", document.Files[2])
	}
	if document.Omitted.RepositoryControl != 3 || document.Omitted.Sensitive < 1 || document.Omitted.NonRegular < 1 || document.Omitted.Binary < 2 || document.Omitted.Oversized < 1 {
		t.Fatalf("omission audit is incomplete: %+v", document.Omitted)
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(document.ContextSHA256) {
		t.Fatalf("context digest is invalid: %q", document.ContextSHA256)
	}
}

func TestRepairSourceContextFailsClosedOnCredentialLikeContent(t *testing.T) {
	repository, _ := seedGitRepository(t)
	writeRepairSourceFixture(t, repository, "src/allowed_test.go", "package demo\nconst leaked = \""+"github"+"_pat_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456\"\n")
	runCommand(t, repository, "git", "add", "src/allowed_test.go")
	tree := strings.TrimSpace(gitOutput(t, repository, "write-tree"))
	contextValue, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"src/**"}, nil)
	if err == nil || contextValue != "" {
		t.Fatalf("credential-like source did not fail closed: context=%q err=%v", contextValue, err)
	}
	if !strings.Contains(err.Error(), repairSourceSecretRulesetVersion) || !strings.Contains(err.Error(), "github-token-v1") || !strings.Contains(err.Error(), "remove and rotate") || strings.Contains(err.Error(), "github_pat_") {
		t.Fatalf("credential failure is not actionable and safely bounded: %v", err)
	}
}

func TestRepairSourceContextOmitsAmbiguousCredentialAssignmentWithoutStrandingRepair(t *testing.T) {
	repository, _ := seedGitRepository(t)
	writeRepairSourceFixture(t, repository, "src/typed.ts", "const clientSecret: string = \"operator-value-must-not-leave\";\nexport const visible = true;\n")
	writeRepairSourceFixture(t, repository, "src/safe.ts", "function login(password: string): boolean { return password.length > 0; }\n")
	runCommand(t, repository, "git", "add", "src/typed.ts", "src/safe.ts")
	tree := strings.TrimSpace(gitOutput(t, repository, "write-tree"))

	contextValue, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"src/**"}, nil)
	if err != nil {
		t.Fatalf("ambiguous assignment should omit one file, not fail automation: %v", err)
	}
	document := decodeRepairSourceContext(t, contextValue)
	if document.Omitted.Sensitive != 1 || strings.Contains(contextValue, "operator-value-must-not-leave") {
		t.Fatalf("ambiguous assignment was not safely omitted and audited: %+v", document.Omitted)
	}
	foundSafe := false
	for _, file := range document.Files {
		if file.Path == "src/safe.ts" {
			foundSafe = true
		}
	}
	if !foundSafe {
		t.Fatalf("type-only function signature was incorrectly omitted: %+v", document.Files)
	}
}

func TestTestAdequacyContextCanReadApplicationSourceWithoutAuthorizingSourceEdits(t *testing.T) {
	repository, _ := seedGitRepository(t)
	writeRepairSourceFixture(t, repository, "src/calculate.ts", "export function calculate(left: number, right: number): number { return left + right; }\n")
	writeRepairSourceFixture(t, repository, "tests/calculate.test.ts", "// missing edge-case coverage\n")
	runCommand(t, repository, "git", "add", "src/calculate.ts", "tests/calculate.test.ts")
	tree := strings.TrimSpace(gitOutput(t, repository, "write-tree"))

	contextValue, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}, []string{"calculate"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextValue, "src/calculate.ts") || !strings.Contains(contextValue, "return left + right") || !strings.Contains(contextValue, "tests/calculate.test.ts") {
		t.Fatalf("test-adequacy context omitted bounded application or test source: %s", contextValue)
	}
	if err := validateChangedFiles([]string{"src/calculate.ts"}, []string{"test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}); err == nil {
		t.Fatal("read-only application context accidentally expanded edit authorization")
	}
}

func TestRepairSourceContextEnforcesFileAndRenderedByteCaps(t *testing.T) {
	t.Parallel()
	repository, _ := seedGitRepository(t)
	for index := 0; index < repairSourceContextMaxFiles+20; index++ {
		writeRepairSourceFixture(t, repository, fmt.Sprintf("src/generated-%03d.txt", index), strings.Repeat("quoted \\\" text\n", 256))
	}
	runCommand(t, repository, "git", "add", "src")
	tree := strings.TrimSpace(gitOutput(t, repository, "write-tree"))
	finding := visualhive.FindingLifecycle{
		Title:             "verified repository failure",
		RootCauseKey:      "repository-test-failure/exact-command",
		AffectedContracts: []string{"repository-test:exact-command"},
	}
	hints := repairSourceRelevanceHints(finding, "verified failing frame: src/generated-083.txt:27")
	contextValue, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"src/**"}, hints)
	if err != nil {
		t.Fatal(err)
	}
	document := decodeRepairSourceContext(t, contextValue)
	if len(contextValue) > repairSourceContextMaxBytes || len(document.Files) > repairSourceContextMaxFiles {
		t.Fatalf("context exceeded cap: rendered=%d files=%d", len(contextValue), len(document.Files))
	}
	if document.Omitted.ContextLimit == 0 {
		t.Fatalf("context cap omissions were not audited: %+v", document.Omitted)
	}
	if len(document.Files) == 0 || document.Files[0].Path != "src/generated-083.txt" {
		t.Fatalf("late but finding-relevant source was not prioritized: %+v", document.Files)
	}
	paths := make([]string, 0, len(document.Files))
	for _, file := range document.Files {
		paths = append(paths, file.Path)
	}
	if len(paths) > 1 && !sort.StringsAreSorted(paths[1:]) {
		t.Fatalf("source context paths are not stably sorted within their relevance priority: %v", paths)
	}
}

func TestRepairSourcePathRejectsTraversalAndAmbiguity(t *testing.T) {
	for _, unsafe := range []string{"", ".", "../outside", "src/../../outside", "/absolute", `C:/operator/home`, `C:operator/home`, `src\\ambiguous.txt`, "src//double.txt", "src/./dot.txt", "src/newline\npath.txt", "src/control\x7f.txt"} {
		if normalized, ok := safeRepairSourcePath(unsafe); ok {
			t.Fatalf("unsafe path %q normalized to %q", unsafe, normalized)
		}
	}
	if normalized, ok := safeRepairSourcePath("src/safe_test.go"); !ok || normalized != "src/safe_test.go" {
		t.Fatalf("safe repository path was rejected: %q %v", normalized, ok)
	}
}

func TestRepairSourceContextReadsExactSealedTreeNotRacingWorktree(t *testing.T) {
	repository, _ := seedGitRepository(t)
	writeRepairSourceFixture(t, repository, "src/raced/value.txt", "SEALED_OBJECT_VALUE\n")
	runCommand(t, repository, "git", "add", "src/raced/value.txt")
	tree := strings.TrimSpace(gitOutput(t, repository, "write-tree"))

	// Replace the complete parent after sealing. The collector must not open it;
	// even a parent symlink/reparse swap cannot redirect a Git-object read.
	if err := os.RemoveAll(filepath.Join(repository, "src", "raced")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeRepairSourceFixture(t, outside, "value.txt", "OPERATOR_PARENT_SYMLINK_SENTINEL\n")
	if err := os.Symlink(outside, filepath.Join(repository, "src", "raced")); err != nil {
		writeRepairSourceFixture(t, repository, "src/raced/value.txt", "MUTATED_WORKTREE_SENTINEL\n")
	}

	contextValue, err := buildRepairSourceContext(context.Background(), repository, tree, []string{"src/**"}, []string{"src/raced/value.txt"})
	if err != nil {
		t.Fatal(err)
	}
	document := decodeRepairSourceContext(t, contextValue)
	var found *repairSourceContextFile
	for index := range document.Files {
		if document.Files[index].Path == "src/raced/value.txt" {
			found = &document.Files[index]
			break
		}
	}
	if found == nil || found.Content != "SEALED_OBJECT_VALUE\n" || strings.Contains(contextValue, "OPERATOR_PARENT_SYMLINK_SENTINEL") || strings.Contains(contextValue, "MUTATED_WORKTREE_SENTINEL") {
		t.Fatalf("source context was not bound to the sealed object tree: %+v", found)
	}
}

func TestRepairSourceContextRejectsCommitInPlaceOfExactTree(t *testing.T) {
	repository, _ := seedGitRepository(t)
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	if contextValue, err := buildRepairSourceContext(context.Background(), repository, head, []string{"src/**"}, nil); err == nil || contextValue != "" || !strings.Contains(err.Error(), "not an available Git tree") {
		t.Fatalf("commit object was accepted as an exact tree: context=%q err=%v", contextValue, err)
	}
}

func TestRepairSourceSecretRulesAndFalsePositives(t *testing.T) {
	tests := []struct {
		name, content, rule string
	}{
		{"private key", strings.Join([]string{"----", "-BEG", "IN O", "PENS", "SH P", "RIVA", "TE K", "EY--", "---\n", "fake", "\n"}, ""), "private-key-v1"},
		{"AWS AKIA", strings.Join([]string{"cons", "t id", " = \"", "AKIA", "ABCD", "EFGH", "IJKL", "MNOP", "\""}, ""), "aws-access-key-id-v1"},
		{"AWS ASIA", strings.Join([]string{"ASIA", "ABCD", "EFGH", "IJKL", "MNOP"}, ""), "aws-access-key-id-v1"},
		{"Slack", strings.Join([]string{"xoxb", "-123", "4567", "890-", "ABCD", "EFGH", "IJ"}, ""), "slack-token-v1"},
		{"GitLab", strings.Join([]string{"glpa", "t-AB", "CDEF", "GHIJ", "KLMN", "OPQR", "STUV", "WX"}, ""), "gitlab-pat-v1"},
		{"npm", strings.Join([]string{"npm_", "ABCD", "EFGH", "IJKL", "MNOP", "QRST", "UVWX", "YZ12", "3456", "7890"}, ""), "npm-token-v1"},
		{"PyPI", strings.Join([]string{"pypi", "-ABC", "DEFG", "HIJK", "LMNO", "PQRS", "TUVW", "XYZ1", "2345", "6"}, ""), "pypi-token-v1"},
		{"Stripe", strings.Join([]string{"sk_l", "ive_", "ABCD", "EFGH", "IJKL", "MNOP", "QRST", "UVWX", "YZ"}, ""), "stripe-live-key-v1"},
		{"Google", strings.Join([]string{"AIza", "ABCD", "EFGH", "IJKL", "MNOP", "QRST", "UVWX", "YZ12", "3456"}, ""), "google-api-key-v1"},
		{"GitHub", strings.Join([]string{"gith", "ub_p", "at_A", "BCDE", "FGHI", "JKLM", "NOPQ", "RSTU", "VWXY", "Z123", "456"}, ""), "github-token-v1"},
		{"OpenAI", strings.Join([]string{"sk-p", "roj-", "ABCD", "EFGH", "IJKL", "MNOP", "QRST", "UVWX", "YZ12", "3456"}, ""), "openai-api-key-v1"},
		{"URL credentials", strings.Join([]string{"post", "gres", "ql:/", "/dat", "abas", "e-us", "er:a", "ctua", "l-pa", "sswo", "rd@d", "b.in", "vali", "d/ap", "p"}, ""), "url-credentials-v1"},
		{"quoted assignment", strings.Join([]string{`clie`, `nt_s`, `ecre`, `t = `, `"act`, `ual-`, `oper`, `ator`, `-val`, `ue-m`, `ust-`, `not-`, `leav`, `e"`}, ""), "sensitive-assignment-v2"},
		{"unquoted YAML assignment", strings.Join([]string{"pass", "word", ": hu", "nter", "2-pr", "od"}, ""), "sensitive-assignment-v2"},
		{"JSON assignment", strings.Join([]string{`{"cl`, `ient`, `Secr`, `et":`, `"act`, `ual-`, `oper`, `ator`, `-val`, `ue-m`, `ust-`, `not-`, `leav`, `e"}`}, ""), "sensitive-assignment-v2"},
		{"Go short assignment", strings.Join([]string{`toke`, `n :=`, ` "ac`, `tual`, `-ope`, `rato`, `r-va`, `lue-`, `must`, `-not`, `-lea`, `ve"`}, ""), "sensitive-assignment-v2"},
		{"Go typed var assignment", strings.Join([]string{`var `, `toke`, `n st`, `ring`, ` = "`, `actu`, `al-o`, `pera`, `tor-`, `valu`, `e-mu`, `st-n`, `ot-l`, `eave`, `"`}, ""), "sensitive-assignment-v2"},
		{"Go typed const assignment", strings.Join([]string{`cons`, `t cl`, `ient`, `Secr`, `et C`, `usto`, `mSec`, `ret `, `= "a`, `ctua`, `l-op`, `erat`, `or-v`, `alue`, `-mus`, `t-no`, `t-le`, `ave"`}, ""), "sensitive-assignment-v2"},
		{"Java typed assignment", strings.Join([]string{`priv`, `ate `, `stat`, `ic S`, `trin`, `g cl`, `ient`, `Secr`, `et =`, ` "ac`, `tual`, `-ope`, `rato`, `r-va`, `lue-`, `must`, `-not`, `-lea`, `ve";`}, ""), "sensitive-assignment-v2"},
		{"C sharp typed assignment", strings.Join([]string{`inte`, `rnal`, ` rea`, `donl`, `y st`, `ring`, ` pas`, `swor`, `d = `, `"act`, `ual-`, `oper`, `ator`, `-val`, `ue-m`, `ust-`, `not-`, `leav`, `e";`}, ""), "sensitive-assignment-v2"},
		{"C pointer assignment", strings.Join([]string{`cons`, `t ch`, `ar *`, `pass`, `word`, ` = "`, `actu`, `al-o`, `pera`, `tor-`, `valu`, `e-mu`, `st-n`, `ot-l`, `eave`, `";`}, ""), "sensitive-assignment-v2"},
		{"TypeScript typed assignment", strings.Join([]string{`cons`, `t cl`, `ient`, `Secr`, `et: `, `stri`, `ng =`, ` "ac`, `tual`, `-ope`, `rato`, `r-va`, `lue-`, `must`, `-not`, `-lea`, `ve"`}, ""), "sensitive-assignment-v2"},
		{"TypeScript optional field assignment", strings.Join([]string{`priv`, `ate `, `clie`, `ntSe`, `cret`, `?: s`, `trin`, `g = `, `"act`, `ual-`, `oper`, `ator`, `-val`, `ue-m`, `ust-`, `not-`, `leav`, `e"`}, ""), "sensitive-assignment-v2"},
		{"Rust typed assignment", strings.Join([]string{`let `, `pass`, `word`, `: St`, `ring`, ` = "`, `actu`, `al-o`, `pera`, `tor-`, `valu`, `e-mu`, `st-n`, `ot-l`, `eave`, `";`}, ""), "sensitive-assignment-v2"},
		{"Python typed assignment", strings.Join([]string{`pass`, `word`, `: st`, `r = `, `"act`, `ual-`, `oper`, `ator`, `-val`, `ue-m`, `ust-`, `not-`, `leav`, `e"`}, ""), "sensitive-assignment-v2"},
		{"credential entropy", strings.Join([]string{`cred`, `enti`, `al_b`, `lob `, `= "A`, `7m!q`, `P9#v`, `X2@k`, `L8$z`, `R5&n`, `T1*"`}, ""), "credential-assignment-entropy-v2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rule, found := detectRepairSourceSecret([]byte(test.content))
			if !found || rule != test.rule {
				t.Fatalf("secret fixture match = %q,%v, want %q", rule, found, test.rule)
			}
		})
	}
	for _, safe := range []string{
		`func validateToken(token string) error { return nil }`,
		`function validate(password: string): boolean { return true }`,
		`fn validate(password: &str) -> bool { true }`,
		`def validate(password: str) -> bool:`,
		`password: String!`,
		`const tokenCount = 3`,
		`password == "actual-operator-value-must-not-leave"`,
		`token = parseToken(input)`,
		`token: Token`,
		`api_key = "${OPENAI_API_KEY}"`,
		`password: "example-placeholder"`,
		`clientSecret = "fake-client-secret"`,
		`token = "test-token"`,
		`password: dummy-password`,
		`secret: "mock-secret"`,
		`token = "fake-` + "github" + `_pat_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"`,
		`access_key = "test-` + "AK" + `IAABCDEFGHIJKLMNOP"`,
		`checksum = "A7m!qP9#vX2@kL8$zR5&nT1*"`,
		`https://example.test/user:pass`,
	} {
		if rule, found := detectRepairSourceSecret([]byte(safe)); found {
			t.Fatalf("safe source matched rule %s: %q", rule, safe)
		}
	}
}

func TestRepairSourceSecretClassifierUnderstandsUnifiedDiffLinesWithoutLeakingValues(t *testing.T) {
	const secret = "actual-operator-value-must-not-leave"
	patch := "diff --git a/src/value.ts b/src/value.ts\n--- a/src/value.ts\n+++ b/src/value.ts\n+const clientSecret: string = \"" + secret + "\";\n"
	err := validateRepairTextSecrets(patch, "model patch")
	if err == nil || !strings.Contains(err.Error(), "sensitive-assignment-v2") || strings.Contains(err.Error(), secret) {
		t.Fatalf("diff assignment was not rejected without disclosure: %v", err)
	}
}

func TestRepairPromptIsToolFreeAndEmbedsOnlyBoundedContext(t *testing.T) {
	source := repairSourceContextPrefix + `{"path":"src/value.txt","sha256":"abc","content":"broken\\n"}` + repairSourceContextSuffix
	prompt := repairPrompt(visualhive.FindingLifecycle{Title: "repair value", Body: "verified failure"}, "verified evidence", "", "", source)
	for _, required := range []string{"filesystem-denied, tool-contained provider process", "no filesystem, shell, command, network, or GitHub access", source, "Do not invoke or request tools", "Do not run inspection or reproduction commands"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("tool-free prompt lost %q: %s", required, prompt)
		}
	}
	for _, forbidden := range []string{"Inspect the repository", "Run read-only inspection", "when practical"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("tool-free prompt retained filesystem/tool instruction %q", forbidden)
		}
	}
}

func writeRepairSourceFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeRepairSourceContext(t *testing.T, value string) repairSourceContextDocument {
	t.Helper()
	if !strings.HasPrefix(value, repairSourceContextPrefix) || !strings.HasSuffix(value, repairSourceContextSuffix) {
		t.Fatalf("source context delimiters are invalid: %q", value)
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(value, repairSourceContextPrefix), repairSourceContextSuffix)
	var document repairSourceContextDocument
	if err := json.Unmarshal([]byte(encoded), &document); err != nil {
		t.Fatalf("decode source context: %v", err)
	}
	return document
}
