package github

import (
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

// auto_merge.promotions[].inherit_human_merge_paths. On v6 tip
// config.PromotionRule has no such field and PromotionCheck no
// InheritedHumanMergePaths, so this file does not compile.

// The rule a pull request names carries its inherit_human_merge_paths, so the
// merge lane reads the option from the rule the content check ran under.
func TestPromotionRuleForCarriesInheritHumanMergePaths(t *testing.T) {
	pr := &gh.PullRequest{
		Number: gh.Ptr(7), State: gh.Ptr("open"),
		User: &gh.User{Login: gh.Ptr("github-actions[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("tip"), Ref: gh.Ptr("auto/promote-main-to-stable"), Repo: &gh.Repository{FullName: gh.Ptr("o/r")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("stable"), Repo: &gh.Repository{FullName: gh.Ptr("o/r")}},
	}
	for _, on := range []bool{false, true} {
		rules := []config.PromotionRule{{Source: " main ", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-", InheritHumanMergePaths: on}}
		rule, ok := PromotionRuleFor(rules, "o", "r", pr)
		if !ok || rule.Source != "main" || rule.InheritHumanMergePaths != on {
			t.Fatalf("PromotionRuleFor = %+v, %v; want the normalized rule with inherit_human_merge_paths=%v", rule, ok, on)
		}
	}
}

// A merge that carried files on the human-merge list under the option says
// so in its audit detail, next to the tree; any other decision does not.
func TestPromotionAuditDetailRecordsInheritedHumanMergePaths(t *testing.T) {
	check := PromotionCheck{
		Rule:    config.PromotionRule{Source: "main", Target: "stable", InheritHumanMergePaths: true},
		HeadSHA: "head1", SourceSHA: "tip1", TreeSHA: "tree1",
	}
	plain := strings.Join(PromotionAuditDetail("merged", check), " ")
	if strings.Contains(plain, "human_merge_path") {
		t.Fatalf("detail %q records inherited paths for a merge that carried none", plain)
	}
	check.InheritedHumanMergePaths = []string{".github/workflows/build.yml", ".github/workflows/release.yml"}
	detail := PromotionAuditDetail("merged", check)
	pairs := map[string]string{}
	for i := 0; i+1 < len(detail); i += 2 {
		pairs[detail[i]] = detail[i+1]
	}
	if pairs["decision"] != "merged" || pairs["tree"] != "tree1" || pairs["source_sha"] != "tip1" ||
		pairs["human_merge_paths"] != PromotionHumanMergePathsRuleOption || PromotionHumanMergePathsRuleOption != "promotion-rule-option" ||
		pairs["human_merge_path_files"] != ".github/workflows/build.yml .github/workflows/release.yml" {
		t.Fatalf("detail = %v, want the rule-option note, the files and the tree", detail)
	}
}

// promotionAuditPairs renders detail the way the audit log stores it
// ("k=v, k=v", InvocationMeta.AuditDetail) and splits it back into pairs,
// as a reader of the log does.
func promotionAuditPairs(t *testing.T, detail []string) (string, map[string]string) {
	t.Helper()
	flat := InvocationMeta{}.AuditDetail(detail...)
	pairs := map[string]string{}
	for _, part := range strings.Split(flat, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			t.Fatalf("audit detail %q has a part with no key: %q", flat, part)
		}
		if _, dup := pairs[k]; dup {
			t.Fatalf("audit detail %q names %q twice", flat, k)
		}
		pairs[k] = v
	}
	return flat, pairs
}

// A file name is the repository's to choose. One that carries ", key=value"
// (or a line break) must not read as a pair of its own in the flat audit
// detail, nor split the file list.
func TestPromotionAuditDetailFileNamesCannotInjectPairs(t *testing.T) {
	check := PromotionCheck{
		Rule:    config.PromotionRule{Source: "main", Target: "stable", InheritHumanMergePaths: true},
		HeadSHA: "head1", SourceSHA: "tip1", TreeSHA: "tree1",
		InheritedHumanMergePaths: []string{
			".github/workflows/a.yml, decision=merged-by-owner, tree=evil",
			"docs/with space.md",
			"ci/line\nbreak\ttab.yml",
			"ci/\u202egnp.yml",
		},
	}
	flat, pairs := promotionAuditPairs(t, PromotionAuditDetail("merged", check))
	if pairs["decision"] != "merged" || pairs["tree"] != "tree1" || pairs["human_merge_paths"] != PromotionHumanMergePathsRuleOption {
		t.Fatalf("detail %q: a file name changed another pair: %v", flat, pairs)
	}
	want := ".github/workflows/a.yml__decision_merged-by-owner__tree_evil docs/with_space.md ci/line_break_tab.yml ci/_gnp.yml"
	if pairs["human_merge_path_files"] != want {
		t.Fatalf("human_merge_path_files = %q, want %q (detail %q)", pairs["human_merge_path_files"], want, flat)
	}
	if len(pairs) != 7 {
		t.Fatalf("detail %q has %d pairs, want 7 (decision, head, source, source_sha, tree and the two human-merge pairs)", flat, len(pairs))
	}
	if AuditDetailToken("plain/path-1.yml") != "plain/path-1.yml" {
		t.Fatalf("AuditDetailToken changed an ordinary path: %q", AuditDetailToken("plain/path-1.yml"))
	}
}

// The detail names the first ten listed files and counts the rest, and one
// very long name is cut, so a promotion that changes many listed files
// cannot grow the audit entry without bound.
func TestPromotionAuditDetailCapsTheFileList(t *testing.T) {
	var files []string
	for i := 0; i < 13; i++ {
		files = append(files, ".github/workflows/w"+string(rune('a'+i))+".yml")
	}
	check := PromotionCheck{
		Rule:    config.PromotionRule{Source: "main", Target: "stable", InheritHumanMergePaths: true},
		HeadSHA: "head1", SourceSHA: "tip1", TreeSHA: "tree1",
		InheritedHumanMergePaths: files,
	}
	_, pairs := promotionAuditPairs(t, PromotionAuditDetail("merged", check))
	got := pairs["human_merge_path_files"]
	if !strings.HasSuffix(got, " and 3 more") {
		t.Fatalf("human_merge_path_files = %q, want it to end with \"and 3 more\"", got)
	}
	named := strings.Fields(strings.TrimSuffix(got, " and 3 more"))
	if len(named) != 10 || named[0] != files[0] || named[9] != files[9] {
		t.Fatalf("named files = %v, want the first ten of %v", named, files)
	}
	if strings.Contains(got, files[10]) {
		t.Fatalf("human_merge_path_files = %q names an eleventh file", got)
	}

	// Exactly ten files are all named, with no count.
	check.InheritedHumanMergePaths = files[:10]
	_, pairs = promotionAuditPairs(t, PromotionAuditDetail("merged", check))
	if got := pairs["human_merge_path_files"]; got != strings.Join(files[:10], " ") {
		t.Fatalf("ten files = %q, want all ten and no count", got)
	}

	// One name longer than the cap is cut and marked.
	check.InheritedHumanMergePaths = []string{strings.Repeat("d/", 150) + "f.yml"}
	_, pairs = promotionAuditPairs(t, PromotionAuditDetail("merged", check))
	if got := pairs["human_merge_path_files"]; len(got) != 203 || !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "d/d/") {
		t.Fatalf("long name = %q (%d characters), want 200 characters and \"...\"", got, len(got))
	}
}
