package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	mqClassicNotProtected = `{"message":"Branch not protected"}`
	mqRulesetRequired     = `[{"type":"required_status_checks","ruleset_id":1,"parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"test"}]}}]`
	mqRulesetMergeQueue   = `[{"type":"merge_queue","ruleset_id":2,"parameters":{"check_response_timeout_minutes":60,"grouping_strategy":"ALLGREEN","max_entries_to_build":5,"max_entries_to_merge":5,"merge_method":"MERGE","min_entries_to_merge":1,"min_entries_to_merge_wait_minutes":5}}]`
)

// mqFixture describes one repo as the GitHub API fixture serves it.
type mqFixture struct {
	repoStatus int    // GET /repos/myorg/repo1; 0 = 200
	ownerType  string // owner.type in that response
	rules      string // GET /repos/myorg/repo1/rules/branches/main body
	rootFiles  []string
}

func mqGitHubServer(t *testing.T, f mqFixture, captured *map[string]interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if f.repoStatus != 0 && f.repoStatus != http.StatusOK {
			w.WriteHeader(f.repoStatus)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"name": "repo1", "default_branch": "main",
			"owner": map[string]interface{}{"login": "myorg", "type": f.ownerType},
		})
	})
	mux.HandleFunc("/repos/myorg/repo1/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(mqClassicNotProtected))
	})
	mux.HandleFunc("/repos/myorg/repo1/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rules := f.rules
		if rules == "" {
			rules = `[]`
		}
		_, _ = w.Write([]byte(rules))
	})
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/repo1/contents/")
		w.Header().Set("Content-Type", "application/json")
		if path == "" && len(f.rootFiles) > 0 {
			entries := make([]map[string]interface{}, 0, len(f.rootFiles))
			for _, name := range f.rootFiles {
				entries = append(entries, map[string]interface{}{"name": name, "type": "file"})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/myorg/repo1/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": "acmm"})
	})
	mux.HandleFunc("/repos/myorg/repo1/issues", func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			_ = json.NewDecoder(r.Body).Decode(captured)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 9, "html_url": "https://github.com/myorg/repo1/issues/9",
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func mqServer(t *testing.T, f mqFixture, strategy string, captured *map[string]interface{}) *Server {
	t.Helper()
	ts := mqGitHubServer(t, f, captured)
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	if strategy != "" {
		deps.Config.Project.RepoPolicies = []config.RepoPolicy{{Repo: "repo1", MergeStrategy: strategy}}
	}
	off := false
	deps.Config.Governor.AttributionTrailer = &off
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)
	return s
}

func mqFind(t *testing.T, results []CriterionResult) CriterionResult {
	t.Helper()
	for _, r := range results {
		if r.ID == acmmMergeQueueID {
			return r
		}
	}
	t.Fatalf("%s missing from results", acmmMergeQueueID)
	return CriterionResult{}
}

func mqLevel(t *testing.T, levels []ACMMLevelScore, lvl int) ACMMLevelScore {
	t.Helper()
	for _, l := range levels {
		if l.Level == lvl {
			return l
		}
	}
	t.Fatalf("level %d missing", lvl)
	return ACMMLevelScore{}
}

func TestACMMMergeQueueCapability(t *testing.T) {
	known := ghpkg.BranchRulesResult{Known: true, Required: map[string]bool{"test": true, "build": true}, MergeQueueKnown: true}
	cases := []struct {
		name     string
		strategy string
		rules    ghpkg.BranchRulesResult
		want     string
	}{
		{"native queue on direct repo", config.MergeStrategyDirect, ghpkg.BranchRulesResult{MergeQueue: true, MergeQueueKnown: true}, acmmMergeQueueProviderNative},
		{"native queue wins over lane", config.MergeStrategyHiveSerialized, ghpkg.BranchRulesResult{MergeQueue: true, MergeQueueKnown: true, Known: true, Required: map[string]bool{"x": true}}, acmmMergeQueueProviderNative},
		{"queue flag without a readable rules endpoint", config.MergeStrategyDirect, ghpkg.BranchRulesResult{MergeQueue: true}, ""},
		{"lane with required checks", config.MergeStrategyHiveSerialized, known, acmmMergeQueueProviderLane},
		{"lane with unknown checks", config.MergeStrategyHiveSerialized, ghpkg.BranchRulesResult{Required: map[string]bool{"x": true}}, ""},
		{"lane with empty known set", config.MergeStrategyHiveSerialized, ghpkg.BranchRulesResult{Known: true, Required: map[string]bool{}}, ""},
		{"direct with required checks", config.MergeStrategyDirect, known, ""},
		{"unset strategy", "", known, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, reason, ok := acmmMergeQueueCapability(tc.strategy, "main", tc.rules)
			if provider != tc.want || ok != (tc.want != "") {
				t.Fatalf("got (%q, %v), want %q", provider, ok, tc.want)
			}
			if ok && !strings.Contains(reason, "main") {
				t.Fatalf("reason %q does not name the branch", reason)
			}
		})
	}
	_, reason, _ := acmmMergeQueueCapability(config.MergeStrategyHiveSerialized, "main", known)
	if !strings.Contains(reason, "build, test") {
		t.Fatalf("lane reason should list required checks in order, got %q", reason)
	}
	unknown := ghpkg.BranchRulesResult{Reason: "branch rulesets unreadable: boom"}
	if _, reason, ok := acmmMergeQueueCapability(config.MergeStrategyHiveSerialized, "main", unknown); ok || reason != unknown.Reason {
		t.Fatalf("lane without credit = (%q, %v), want rules reason %q", reason, ok, unknown.Reason)
	}
	if _, reason, _ := acmmMergeQueueCapability(config.MergeStrategyDirect, "main", unknown); reason != "" {
		t.Fatalf("direct repo reason = %q, want empty", reason)
	}
}

// #10984: a lane repo that earns no credit says why on its row.
func TestACMMMergeQueue_LaneGapCarriesReason(t *testing.T) {
	s := mqServer(t, mqFixture{ownerType: "User", rules: `[]`}, config.MergeStrategyHiveSerialized, nil)
	res := mqFind(t, s.evaluateAllRepos().RepoResults[0].CriteriaResults)
	if res.Passed || !strings.Contains(res.UnsatisfiedReason, "no required checks found") {
		t.Fatalf("result = %+v, want a failing row with the rules reason", res)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"unsatisfied_reason":`) {
		t.Fatalf("serialized result: %s", raw)
	}
}

// AC27: the lane with a known, non-empty required-check set satisfies the
// criterion through SatisfiedBy and counts toward Level 6.
func TestACMMMergeQueue_LaneCreditCountsTowardLevel(t *testing.T) {
	s := mqServer(t, mqFixture{ownerType: "User", rules: mqRulesetRequired}, config.MergeStrategyHiveSerialized, nil)
	eval := s.evaluateAllRepos()
	if len(eval.RepoResults) != 1 {
		t.Fatalf("RepoResults = %d", len(eval.RepoResults))
	}
	rr := eval.RepoResults[0]
	res := mqFind(t, rr.CriteriaResults)
	if !res.Passed || res.SatisfiedBy != acmmMergeQueueProviderLane || res.FileOnly || res.Waived {
		t.Fatalf("lane credit = %+v", res)
	}
	if !strings.Contains(res.SatisfiedReason, "test") {
		t.Fatalf("reason should name the required check: %q", res.SatisfiedReason)
	}
	if got := mqLevel(t, rr.Levels, 6).Detected; got != 1 {
		t.Fatalf("L6 detected = %d, want 1 (the credit counts toward the level)", got)
	}
	if rr.MergeStrategy != config.MergeStrategyHiveSerialized {
		t.Fatalf("MergeStrategy = %q", rr.MergeStrategy)
	}
	agg := mqFind(t, eval.CriteriaResults)
	if agg.SatisfiedBy != acmmMergeQueueProviderLane || agg.Repo != "repo1" {
		t.Fatalf("aggregate row = %+v", agg)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"satisfied_by":"Hive serialized merge lane"`) {
		t.Fatalf("serialized result: %s", raw)
	}
}

// AC28: GitHub's merge queue satisfies the criterion with no marker file.
func TestACMMMergeQueue_NativeQueueCredit(t *testing.T) {
	s := mqServer(t, mqFixture{ownerType: "Organization", rules: mqRulesetMergeQueue}, "", nil)
	eval := s.evaluateAllRepos()
	res := mqFind(t, eval.RepoResults[0].CriteriaResults)
	if !res.Passed || res.SatisfiedBy != acmmMergeQueueProviderNative || res.FileOnly {
		t.Fatalf("native credit = %+v", res)
	}
	if got := mqLevel(t, eval.RepoResults[0].Levels, 6).Detected; got != 1 {
		t.Fatalf("L6 detected = %d, want 1", got)
	}
}

// AC29: a pass resting only on a marker file keeps passing and keeps
// scoring, labelled file only.
func TestACMMMergeQueue_FileOnlyKeepsPassing(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    mqFixture
	}{
		{"rules read, no capability", mqFixture{ownerType: "Organization", rootFiles: []string{"tide.yaml"}}},
		{"repo unreadable", mqFixture{repoStatus: http.StatusNotFound, rootFiles: []string{"tide.yaml"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mqServer(t, tc.f, "", nil)
			eval := s.evaluateAllRepos()
			res := mqFind(t, eval.RepoResults[0].CriteriaResults)
			if !res.Passed || !res.FileOnly || res.SatisfiedBy != "" || res.Waived {
				t.Fatalf("file-only result = %+v", res)
			}
			if got := mqLevel(t, eval.RepoResults[0].Levels, 6).Detected; got != 1 {
				t.Fatalf("L6 detected = %d, want 1 (no readiness regression)", got)
			}
			agg := mqFind(t, eval.CriteriaResults)
			if !agg.Passed || !agg.FileOnly {
				t.Fatalf("aggregate row = %+v", agg)
			}
			raw, _ := json.Marshal(res)
			if !strings.Contains(string(raw), `"file_only":true`) {
				t.Fatalf("serialized result: %s", raw)
			}
		})
	}
}

// A verified capability takes precedence over the marker file, so the pass is
// not labelled file only.
func TestACMMMergeQueue_CapabilityBeatsFileLabel(t *testing.T) {
	s := mqServer(t, mqFixture{ownerType: "Organization", rules: mqRulesetMergeQueue, rootFiles: []string{"tide.yaml"}}, "", nil)
	res := mqFind(t, s.evaluateAllRepos().RepoResults[0].CriteriaResults)
	if !res.Passed || res.FileOnly || res.SatisfiedBy != acmmMergeQueueProviderNative {
		t.Fatalf("result = %+v", res)
	}
}

// The lane without required checks earns nothing, and direct repos with no
// queue and no file still fail as before.
func TestACMMMergeQueue_NoCreditWithoutCapability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy string
		rules    string
	}{
		{"lane without required checks", config.MergeStrategyHiveSerialized, `[]`},
		{"direct with required checks", "", mqRulesetRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mqServer(t, mqFixture{ownerType: "User", rules: tc.rules}, tc.strategy, nil)
			eval := s.evaluateAllRepos()
			res := mqFind(t, eval.RepoResults[0].CriteriaResults)
			if res.Passed || res.SatisfiedBy != "" || res.FileOnly {
				t.Fatalf("result = %+v", res)
			}
			want := tc.strategy
			if want == "" {
				want = config.MergeStrategyDirect
			}
			if eval.RepoResults[0].MergeStrategy != want {
				t.Fatalf("MergeStrategy = %q, want %q", eval.RepoResults[0].MergeStrategy, want)
			}
		})
	}
}

func TestACMMMergeQueueCredit_NilGuards(t *testing.T) {
	s := NewServer(0, acmmEvalTestLogger())
	if _, _, ok := s.acmmMergeQueueCredit(t.Context(), nil, "o", "r"); ok {
		t.Fatal("nil client must not credit")
	}
	if s.acmmRepoOwnedByUser(t.Context(), "o", "r") {
		t.Fatal("nil deps must not report a personal account")
	}
	deps := testDeps(t)
	deps.GHClient = nil
	s.RegisterAPI(deps)
	if s.acmmRepoOwnedByUser(t.Context(), "o", "r") {
		t.Fatal("nil GitHub client must not report a personal account")
	}
}

// acmmOrgMergeQueueGolden is the merge-queue gap ticket body as filed before
// #10891. Organization repositories must keep it byte for byte (AC32).
const acmmOrgMergeQueueGolden = "## ACMM Gap: Merge queue\n\n" +
	"**Level:** L6 Fully Autonomous\n" +
	"**Category:** autonomy\n" +
	"**Criterion ID:** `acmm:merge-queue`\n\n" +
	"### What's needed\n\n" +
	"This repository is missing one of the following files or directories:\n\n" +
	"- `.github/workflows/merge-queue.yml`\n" +
	"- `.github/merge-queue.yml`\n" +
	"- `.prow.yaml`\n" +
	"- `tide.yaml`\n\n" +
	"Adding any one of these will satisfy this criterion and help the repository advance to ACMM Level 6 (Fully Autonomous).\n\n" +
	"### Why it matters\n\n" +
	"Autonomy criteria enable agents to operate independently — generating issues, orchestrating multi-agent workflows, and managing merge queues.\n\n" +
	"### How to fix\n\n" +
	"Create one of the files listed above. The ACMM evaluation checks for file existence — the content can follow your project's conventions.\n\n" +
	"---\n" +
	"*Opened by Hive ACMM Evaluation*"

func mqIssueLabels(t *testing.T, captured map[string]interface{}) []string {
	t.Helper()
	raw, ok := captured["labels"].([]interface{})
	if !ok {
		t.Fatalf("labels missing: %v", captured["labels"])
	}
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		out = append(out, l.(string))
	}
	return out
}

// AC32: organization-owned repos (and repos whose owner type cannot be read)
// keep today's ticket: same title, body and labels.
func TestACMMIssue_MergeQueueOrgTicketUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    mqFixture
	}{
		{"organization", mqFixture{ownerType: "Organization"}},
		{"owner type unreadable", mqFixture{repoStatus: http.StatusNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured map[string]interface{}
			s := mqServer(t, tc.f, "", &captured)
			rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo1", "criterion_id": acmmMergeQueueID})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
			}
			if got := captured["title"]; got != "[ACMM L6] Add Merge queue" {
				t.Fatalf("title = %q", got)
			}
			if got := captured["body"]; got != acmmOrgMergeQueueGolden {
				t.Fatalf("body changed:\n%s", got)
			}
			if got := mqIssueLabels(t, captured); !reflect.DeepEqual(got, []string{"acmm", "ai-fix-requested"}) {
				t.Fatalf("labels = %v", got)
			}
		})
	}
	var c ACMMCriterion
	for _, uc := range universalCriteria {
		if uc.ID == acmmMergeQueueID {
			c = uc
		}
	}
	if title, body := acmmIssueContent(&c); title != "[ACMM L6] Add Merge queue" || body != acmmOrgMergeQueueGolden {
		t.Fatalf("acmmIssueContent drifted: %q\n%s", title, body)
	}
}

func mqAssertPersonalTicket(t *testing.T, captured map[string]interface{}) {
	t.Helper()
	title, _ := captured["title"].(string)
	body, _ := captured["body"].(string)
	for _, forbidden := range []string{"missing one of the following files", "tide.yaml", ".prow.yaml", "merge-queue.yml"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("personal ticket must not contain %q:\n%s", forbidden, body)
		}
	}
	for _, want := range []string{
		"personal account",
		"GitHub's merge queue is only available to repositories owned by an organization",
		"serialized merge lane",
		"required status check",
		"`hive-serialized`",
		"**Criterion ID:** `acmm:merge-queue`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("personal ticket missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(title, "Add Merge queue") || !strings.Contains(title, "serialized merging") {
		t.Fatalf("title = %q", title)
	}
	if got := mqIssueLabels(t, captured); !reflect.DeepEqual(got, []string{"acmm"}) {
		t.Fatalf("labels = %v, want [acmm] (no ai-fix-requested)", got)
	}
}

// AC31: the single "Open Issue" on a personal-account repo.
func TestACMMIssue_MergeQueuePersonalTicket(t *testing.T) {
	var captured map[string]interface{}
	s := mqServer(t, mqFixture{ownerType: "User"}, "", &captured)
	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo1", "criterion_id": acmmMergeQueueID, "criterion_level": 6})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	mqAssertPersonalTicket(t, captured)
	var resp struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.Labels, []string{"acmm"}) {
		t.Fatalf("response labels = %v", resp.Labels)
	}
}

// AC31: "Open Issues for L6" posts one request per failing L6 criterion. Only
// the merge-queue ticket changes on a personal-account repo; the rest keep
// their labels.
func TestACMMIssue_MergeQueuePersonalTicketInBatch(t *testing.T) {
	var captured map[string]interface{}
	s := mqServer(t, mqFixture{ownerType: "User"}, "", &captured)
	sawMergeQueue := false
	for _, c := range universalCriteria {
		if c.Level != 6 {
			continue
		}
		captured = nil
		rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo1", "criterion_id": c.ID, "criterion_level": 6})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body=%s", c.ID, rec.Code, rec.Body.String())
		}
		if c.ID == acmmMergeQueueID {
			sawMergeQueue = true
			mqAssertPersonalTicket(t, captured)
			continue
		}
		if got := mqIssueLabels(t, captured); !reflect.DeepEqual(got, []string{"acmm", "ai-fix-requested"}) {
			t.Fatalf("%s labels = %v", c.ID, got)
		}
	}
	if !sawMergeQueue {
		t.Fatal("batch never reached acmm:merge-queue")
	}
}

func TestACMMMergeQueuePersonalIssueContent_NeverCallsLaneAMergeQueue(t *testing.T) {
	c := &ACMMCriterion{ID: acmmMergeQueueID, Level: 6, Category: "autonomy", Name: "Merge queue"}
	_, body := acmmMergeQueuePersonalIssueContent(c, "repo1")
	for _, bad := range []string{"merge queue lane", "serialized merge queue", "Hive merge queue", "hive merge queue"} {
		if strings.Contains(body, bad) {
			t.Fatalf("body calls the lane a merge queue (%q):\n%s", bad, body)
		}
	}
	if !strings.Contains(body, `{"repo": "repo1", "merge_strategy": "hive-serialized"}`) {
		t.Fatalf("body should show the owner API call:\n%s", body)
	}
	unknown := &ACMMCriterion{ID: acmmMergeQueueID, Level: 99, Category: "autonomy", Name: "Merge queue"}
	if _, b := acmmMergeQueuePersonalIssueContent(unknown, "r"); !strings.Contains(b, "L99 Unknown") {
		t.Fatalf("unknown level fallback missing:\n%s", b)
	}
}

// AC33: the tooltip and the Level 6 description describe the capability and
// name no file.
func TestACMMMergeQueueDescriptions(t *testing.T) {
	html := indexHTML(t)
	tipLine := mqLineContaining(t, html, "'acmm:merge-queue':")
	descLine := mqLineContaining(t, html, "      6: 'Auto issue generation")
	for name, line := range map[string]string{"tip": tipLine, "level 6": descLine} {
		if !strings.Contains(strings.ToLower(line), "a merge queue or equivalent serialized merging") {
			t.Fatalf("%s does not read 'a merge queue or equivalent serialized merging': %s", name, line)
		}
		for _, bad := range []string{"file", ".yml", ".yaml", "tide", "prow"} {
			if strings.Contains(strings.ToLower(line), bad) {
				t.Fatalf("%s mentions %q: %s", name, bad, line)
			}
		}
	}
}

func mqLineContaining(t *testing.T, html, needle string) string {
	t.Helper()
	for _, line := range strings.Split(html, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("index.html has no line containing %q", needle)
	return ""
}

// AC27–AC30 on the panel: the satisfied-by and file-only chips, and the
// owner's lane action on a failing row.
func TestACMMMergeQueueRowRendering(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`acmmCreditChip(c) +`,
		`acmmMergeLaneAction(c, `,
		`fetch('/api/repos/merge-strategy'`,
		`merge_strategy: 'hive-serialized'`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the merge-queue row rendering was NOT executed by this run")
	}
	script := "const window = {_hiveRole: 'owner'};\nlet _acmmEvalData = null;\n" +
		jsFunc(t, html, "escapeHtml") + "\n" +
		jsFunc(t, html, "acmmCreditChip") + "\n" +
		jsFunc(t, html, "acmmMergeLaneAction") + "\n" +
		jsFunc(t, html, "acmmLabelNote") + "\n" + mqRowAssertions
	path := filepath.Join(t.TempDir(), "mq.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("merge-queue row rendering check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const mqRowAssertions = `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }

check('lane credit chip', acmmCreditChip({passed: true, satisfied_by: 'Hive serialized merge lane'}).includes('satisfied by Hive serialized merge lane'));
check('native credit chip', acmmCreditChip({passed: true, satisfied_by: 'GitHub merge queue'}).includes('satisfied by GitHub merge queue'));
check('file-only chip', acmmCreditChip({passed: true, file_only: true}).includes('file only, not verified'));
check('plain detection has no chip', acmmCreditChip({passed: true}) === '');
check('failing row has no chip', acmmCreditChip({passed: false, file_only: true}) === '');

const failing = {id: 'acmm:merge-queue', passed: false};
_acmmEvalData = {repo_results: [{repo: 'repo1', merge_strategy: 'direct'}]};
const action = acmmMergeLaneAction(failing, 'repo1');
check('owner is offered the lane', action.includes('acmmUseSerializedLane') && action.includes('Use serialized merge lane'));
check('lane is never called a merge queue', !/merge queue/i.test(action.replace('acmm:merge-queue', '')));
check('passing row offers nothing', acmmMergeLaneAction({id: 'acmm:merge-queue', passed: true}, 'repo1') === '');
check('other criteria offer nothing', acmmMergeLaneAction({id: 'acmm:claude-md', passed: false}, 'repo1') === '');
check('no repo offers nothing', acmmMergeLaneAction(failing, '') === '');
_acmmEvalData = {repo_results: [{repo: 'repo1', merge_strategy: 'hive-serialized'}]};
check('lane already on explains the gap', acmmMergeLaneAction(failing, 'repo1').includes('needs required checks'));
check('lane gap without a reason adds none', !acmmMergeLaneAction(failing, 'repo1').includes('('));
check('lane gap shows the rules reason', acmmMergeLaneAction({id: 'acmm:merge-queue', passed: false, unsatisfied_reason: 'branch rulesets unreadable: 500'}, 'repo1').includes('(branch rulesets unreadable: 500)'));
_acmmEvalData = {repo_results: [{repo: 'repo1', merge_strategy: 'hive-serialized', criteria_results: [{id: 'acmm:merge-queue', passed: false, unsatisfied_reason: 'rules <x>'}]}]};
check('lane gap reason from the repo row, escaped', acmmMergeLaneAction(failing, 'repo1').includes('(rules &lt;x&gt;)'));
window._hiveRole = 'read';
check('non-owner is not offered the lane', acmmMergeLaneAction(failing, 'repo1') === '');

check('label note from server', acmmLabelNote(['acmm'], 'X') === ' with <code>acmm</code> label.');
check('label note two labels', acmmLabelNote(['acmm', 'ai-fix-requested'], 'X').includes('<code>ai-fix-requested</code> labels.'));
check('label note fallback', acmmLabelNote(null, 'X') === 'X');
check('label note none', acmmLabelNote([], 'X') === '.');

if (fails) process.exit(1);
`
