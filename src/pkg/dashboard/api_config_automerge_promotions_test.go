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

// inherit_human_merge_paths round-trips per rule, is off unless sent, and
// goes off again when a later save leaves it out (the map is replaced
// whole). On v6 tip the endpoint drops the key.
func TestAutoMergePromotions_InheritHumanMergePathsRoundTrip(t *testing.T) {
	s := repoPolicyServer(t)
	inherit := autoMergePromotionBody()
	inherit["inherit_human_merge_paths"] = true
	plain := autoMergePromotionBody()
	plain["target"] = "beta"
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"promotions": map[string]any{"api": []any{inherit, plain}}}); rec.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rules := s.deps.Config.AutoMerge.PromotionRulesFor("acme/api")
	if len(rules) != 2 || !rules[0].InheritHumanMergePaths || rules[1].InheritHumanMergePaths {
		t.Fatalf("stored rules = %+v, want the option on for the first rule only", rules)
	}
	// The audit entry names the rule the option was switched on for, and
	// only that one.
	if got := repoOverrideAuditDetails(s, "config_auto_merge"); !strings.Contains(got, "section=auto_merge, promotion_inherit_on=acme/api:stable") || strings.Contains(got, "acme/api:beta") || strings.Contains(got, "promotion_inherit_off") {
		t.Fatalf("audit detail = %q, want promotion_inherit_on=acme/api:stable and nothing else about the option", got)
	}

	rec := doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rec.Code)
	}
	var got struct {
		Promotions map[string][]map[string]any `json:"promotions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	r := got.Promotions["acme/api"]
	if len(r) != 2 || r[0]["inherit_human_merge_paths"] != true {
		t.Fatalf("GET promotions = %+v, want inherit_human_merge_paths true on the first rule", got.Promotions)
	}
	if _, set := r[1]["inherit_human_merge_paths"]; set {
		t.Fatalf("GET reports the option on a rule that never set it: %+v", r[1])
	}

	// A save that leaves the key out turns it off: narrowing needs no more
	// than the rule without it.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"promotions": map[string]any{"api": []any{autoMergePromotionBody()}}}); rec.Code != http.StatusOK {
		t.Fatalf("second put: %d %s", rec.Code, rec.Body.String())
	}
	if rules := s.deps.Config.AutoMerge.PromotionRulesFor("acme/api"); len(rules) != 1 || rules[0].InheritHumanMergePaths {
		t.Fatalf("rules after a save without the key = %+v, want the option off", rules)
	}
	if got := repoOverrideAuditDetails(s, "config_auto_merge"); !strings.Contains(got, "section=auto_merge, promotion_inherit_off=acme/api:stable") {
		t.Fatalf("audit detail = %q, want promotion_inherit_off=acme/api:stable for the save that turned it off", got)
	}
	// A save that leaves the option as it is says nothing about it.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"promotions": map[string]any{"api": []any{autoMergePromotionBody()}}}); rec.Code != http.StatusOK {
		t.Fatalf("third put: %d %s", rec.Code, rec.Body.String())
	}
	if got := repoOverrideAuditDetails(s, "config_auto_merge"); strings.Count(got, "promotion_inherit_on=") != 1 || strings.Count(got, "promotion_inherit_off=") != 1 {
		t.Fatalf("audit details = %q, want the option named once on and once off over the three saves", got)
	}
	// Not a boolean: the body does not decode, and nothing changes.
	bad := autoMergePromotionBody()
	bad["inherit_human_merge_paths"] = "yes"
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"promotions": map[string]any{"api": []any{bad}}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-boolean option: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if rules := s.deps.Config.AutoMerge.PromotionRulesFor("acme/api"); len(rules) != 1 || rules[0].InheritHumanMergePaths {
		t.Fatalf("a refused write changed the rules: %+v", rules)
	}
}

// Each rule row has the tick box, the help says in plain words what it lets
// through, and only a ticked box stages the key.
func TestAutoMergePromotionsInheritSettingsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="am-promotion-inherit"`,
		"r.inherit_human_merge_paths === true",
		"Also merge files a person must merge (inherit_human_merge_paths)",
		"Files on that list then reach the target with no person.",
		// What is proved and what is not, and where the cases are.
		"The hive proves only that the promotion carries the source branch&apos;s current tree.",
		"It does not check how or when a listed file reached the source",
		"the page this help mark opens lists the cases",
		// The human-merge paths help points at the exception.
		"a branch-promotion rule with &quot;Also merge files a person must merge&quot; ticked (below) lets its promotions through",
		"<code>inherit_human_merge_paths</code>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}
	// The help does not state what the hive never checks.
	for _, not := range []string{"same change already merged", "where the same list applied", "inherits exactly what the source allowed", "on the strength of the same change"} {
		if strings.Contains(html, not) {
			t.Fatalf("static dashboard still says %q", not)
		}
	}
	runNodeScript(t, parityMarkDirtyStub+jsFunc(t, html, "markDirty")+"\n"+jsFunc(t, html, "splitConfigLines")+"\n"+jsFunc(t, html, "markDirtyAutoMergePromotions")+`
const row = (repo, v, inherit) => ({
  dataset: { repo },
  querySelector: cls => ({ '.am-promotion-source': { value: v[0] }, '.am-promotion-target': { value: v[1] }, '.am-promotion-prefix': { value: v[2] }, '.am-promotion-authors': { value: v[3] }, '.am-promotion-inherit': { checked: inherit } })[cls] || null,
});
let rows = [
  row('acme/api', ['main', 'stable', 'auto/promote-', 'github-actions[bot]'], true),
  row('acme/api', ['main', 'beta', 'auto/beta-', 'github-actions[bot]'], false),
  row('acme/api', ['', '', '', ''], true),
];
global.document = { querySelectorAll: sel => sel === '#am-promotions .am-promotion-row' ? rows : [] };

// A ticked rule stages the key as true; an unticked one leaves it out, and
// a tick on an otherwise empty row does not make a rule.
markDirtyAutoMergePromotions();
assert.deepEqual(_configState.dirty['auto-merge'].promotions, {
  'acme/api': [
    { source: 'main', target: 'stable', head_ref_prefix: 'auto/promote-', authors: ['github-actions[bot]'], inherit_human_merge_paths: true },
    { source: 'main', target: 'beta', head_ref_prefix: 'auto/beta-', authors: ['github-actions[bot]'] },
  ],
});
`)
}
