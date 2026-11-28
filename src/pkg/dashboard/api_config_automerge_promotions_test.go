package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// auto_merge.promotions on PUT/PATCH /api/config/auto-merge and in Settings
// → Features → Auto-merge. On v6 tip the endpoint does not take or report
// promotions, so the round trip below stores nothing.

func autoMergePromotionBody() map[string]any {
	return map[string]any{
		"source": " main ", "target": "stable", "head_ref_prefix": "auto/promote-",
		"authors": []string{"github-actions[bot]", "github-actions[bot]"},
	}
}

func TestAutoMergePromotions_PutGetRoundTrip(t *testing.T) {
	s := repoPolicyServer(t)
	rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"promotions": map[string]any{"api": []any{autoMergePromotionBody()}, "web": []any{}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Stored as owner/repo, trimmed and de-duplicated; a repository with no
	// rules is dropped.
	rules := s.deps.Config.AutoMerge.PromotionRulesFor("acme/api")
	if len(rules) != 1 || rules[0].Source != "main" || rules[0].Target != "stable" || rules[0].HeadRefPrefix != "auto/promote-" || strings.Join(rules[0].Authors, ",") != "github-actions[bot]" {
		t.Fatalf("stored rules = %+v", s.deps.Config.AutoMerge.Promotions)
	}
	if _, ok := s.deps.Config.AutoMerge.Promotions["acme/api"]; !ok || len(s.deps.Config.AutoMerge.Promotions) != 1 {
		t.Fatalf("promotions keys = %+v, want only acme/api", s.deps.Config.AutoMerge.Promotions)
	}

	rec = doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after put: expected 200, got %d", rec.Code)
	}
	var got struct {
		Promotions map[string][]config.PromotionRule `json:"promotions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if r := got.Promotions["acme/api"]; len(r) != 1 || r[0].Target != "stable" || strings.Join(r[0].Authors, ",") != "github-actions[bot]" {
		t.Fatalf("promotions did not round-trip: %+v", got.Promotions)
	}

	// Absent leaves it alone; an empty map clears it.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"max_merges": 2}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated put: %d %s", rec.Code, rec.Body.String())
	}
	if len(s.deps.Config.AutoMerge.PromotionRulesFor("acme/api")) != 1 {
		t.Fatal("a put without promotions changed them")
	}
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"promotions": map[string]any{}}); rec.Code != http.StatusOK {
		t.Fatalf("clear put: %d %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.AutoMerge.Promotions != nil {
		t.Fatalf("an empty map did not clear promotions: %+v", s.deps.Config.AutoMerge.Promotions)
	}
}

func TestAutoMergePromotions_InvalidIsRefusedWithNoChange(t *testing.T) {
	s := repoPolicyServer(t)
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"promotions": map[string]any{"api": []any{autoMergePromotionBody()}}}); rec.Code != http.StatusOK {
		t.Fatalf("seed put: %d %s", rec.Code, rec.Body.String())
	}
	with := func(key string, value any) []any {
		r := autoMergePromotionBody()
		r[key] = value
		return []any{r}
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown repo", map[string]any{"promotions": map[string]any{"other/repo": []any{autoMergePromotionBody()}}}, "promotions: other/repo is not in project.repos"},
		{"twice", map[string]any{"promotions": map[string]any{"api": []any{autoMergePromotionBody()}, "ACME/api": []any{autoMergePromotionBody()}}}, "names acme/api twice"},
		{"source equals target", map[string]any{"promotions": map[string]any{"api": with("target", "main")}}, "must be different"},
		{"no author", map[string]any{"promotions": map[string]any{"api": with("authors", []string{})}}, "at least one author"},
		{"blank author", map[string]any{"promotions": map[string]any{"api": with("authors", []string{" "})}}, "non-empty logins"},
		{"empty source", map[string]any{"promotions": map[string]any{"api": with("source", "")}}, "source is required"},
		{"empty head ref prefix", map[string]any{"promotions": map[string]any{"api": with("head_ref_prefix", " ")}}, "head_ref_prefix is required"},
		{"target twice", map[string]any{"promotions": map[string]any{"api": append(with("source", "develop"), autoMergePromotionBody())}}, "more than one rule"},
		// A valid key alongside an invalid one must not half-apply.
		{"mixed", map[string]any{"max_merges": 7, "squash_only": []string{"api"}, "promotions": map[string]any{"api": with("target", "main")}}, "must be different"},
	} {
		rec := doPut(s, "/api/config/auto-merge", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: expected 400 containing %q, got %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	am := s.deps.Config.AutoMerge
	if rules := am.PromotionRulesFor("acme/api"); len(rules) != 1 || rules[0].Target != "stable" || len(am.Promotions) != 1 {
		t.Fatalf("rejected writes changed promotions: %+v", am.Promotions)
	}
	if am.MaxMerges != 0 || am.SquashOnly != nil {
		t.Fatalf("rejected writes changed other settings: %+v", am)
	}
}

// Settings → Features → Auto-merge edits the rules per repository and
// stages auto_merge.promotions whole.
func TestAutoMergePromotionsSettingsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="am-promotions"`,
		`class="am-promotion-source"`,
		`class="am-promotion-target"`,
		`class="am-promotion-prefix"`,
		`class="am-promotion-authors"`,
		`data-change-action="markDirtyAutoMergePromotions"`,
		"function markDirtyAutoMergePromotions()",
		"markDirty('auto-merge', 'promotions'",
		"am.promotions",
		"yamlHint('auto_merge.promotions')",
		"settingHelpMark('hive-merge', 'branch-promotion-pull-requests'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}
	runNodeScript(t, parityMarkDirtyStub+jsFunc(t, html, "markDirty")+"\n"+jsFunc(t, html, "splitConfigLines")+"\n"+jsFunc(t, html, "markDirtyAutoMergePromotions")+`
const row = (repo, v) => ({
  dataset: { repo },
  querySelector: cls => ({ '.am-promotion-source': { value: v[0] }, '.am-promotion-target': { value: v[1] }, '.am-promotion-prefix': { value: v[2] }, '.am-promotion-authors': { value: v[3] } })[cls] || null,
});
let rows = [
  row('acme/api', ['main', 'stable', 'auto/promote-', 'github-actions[bot], release-bot']),
  row('acme/api', ['', '', '', '']),
  row('acme/web', [' ', '', '', '']),
];
global.document = { querySelectorAll: sel => sel === '#am-promotions .am-promotion-row' ? rows : [] };

// Filled rows are staged grouped by repository; empty rows drop out.
markDirtyAutoMergePromotions();
assert.deepEqual(_configState.dirty['auto-merge'].promotions, {
  'acme/api': [{ source: 'main', target: 'stable', head_ref_prefix: 'auto/promote-', authors: ['github-actions[bot]', 'release-bot'] }],
});

// Clearing every row stages an empty map, which removes every rule.
rows = [row('acme/api', ['', '', '', ''])];
markDirtyAutoMergePromotions();
assert.deepEqual(_configState.dirty['auto-merge'].promotions, {});
`)
}
