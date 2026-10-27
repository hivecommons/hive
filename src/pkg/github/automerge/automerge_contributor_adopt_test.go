package automerge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// adoptAPI layers the git-ref / pull-request routes the adoption needs on
// top of stageAPI and records what the hive did.
type adoptAPI struct {
	mu          sync.Mutex
	refs        map[string]string // refs/heads/x → sha
	refUpdates  []string
	pulls       []map[string]any
	nextPR      int
	superseders map[int]map[string]any // number → GET payload
	prEdits     []string
	newCommits  []map[string]any // POST git/commits bodies
	commits     map[string]adoptCommit
	// failRefs makes POST git/refs answer 500; refAttempts counts them.
	failRefs    bool
	refAttempts int
	// compareFiles are the base-side files served by GET compare/head...base;
	// failCompare makes it answer 500.
	compareFiles []string
	failCompare  bool
	// prFiles are the files served by GET pulls/n/files (the adopted PR's
	// diff); failPRFiles makes it answer 500.
	prFiles     []map[string]any
	failPRFiles bool
	// heads is the head GET pulls/n reports for an observed PR (the
	// harness keeps it in step with what the lane saw); liveHead, when set,
	// overrides it: the author pushed after the lane recorded its head.
	heads     map[int]string
	liveHead  string
	failPRGet bool // GET pulls/n answers 500
	// changedFiles is the changed_files count GET pulls/n reports for an
	// observed PR; 0 omits it.
	changedFiles int
	// permissions is the role GET collaborators/<login>/permission reports
	// per login (default "write"); failPermission makes it answer 500;
	// permissionReads counts the calls; onPermission runs inside each read.
	permissions     map[string]string
	failPermission  bool
	permissionReads int
	onPermission    func()
	// failEditStatus, when non-zero, is the status PATCH pulls/n answers.
	failEditStatus int
	// Stale-red recovery fixtures (hivecommons/hive#14500): compareBehind
	// and compareBase are what GET compare/base...head reports; baseChecks
	// is name → conclusion served by GET commits/<base>/check-runs;
	// failedRuns are the workflow runs GET actions/runs?head_sha= lists;
	// updateBranchStatus, when non-zero, is the status PUT update-branch
	// answers with updateBranchMessage. The recorder slices count calls.
	compareBehind       int
	compareBase         string
	baseChecks          map[string]string
	failBaseChecks      bool
	failedRuns          []int64
	updateBranchStatus  int
	updateBranchMessage string
	branchUpdates       []string
	reruns              []int64
	baseCheckReads      int
}

type adoptCommit struct {
	login    string // GitHub login of author/committer, or git:<name> for an unlinked identity
	verified bool
}

func newAdoptAPI() *adoptAPI {
	return &adoptAPI{refs: map[string]string{}, nextPR: 90, superseders: map[int]map[string]any{}, commits: map[string]adoptCommit{}, heads: map[int]string{}}
}

func (a *adoptAPI) handle(w http.ResponseWriter, r *http.Request) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/collaborators/") && strings.HasSuffix(path, "/permission"):
		a.permissionReads++
		if a.onPermission != nil {
			a.onPermission()
		}
		if a.failPermission {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted failure"})
			return true
		}
		login := strings.TrimSuffix(path[strings.Index(path, "/collaborators/")+len("/collaborators/"):], "/permission")
		role := nonEmpty(a.permissions[login], "write")
		permission := role
		if role == "maintain" {
			permission = "write" // GitHub's legacy field; role_name carries maintain
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"permission": permission, "role_name": role, "user": map[string]string{"login": login}})
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/pulls/") && strings.HasSuffix(path, "/files"):
		if a.failPRFiles {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted failure"})
			return true
		}
		files := a.prFiles
		if files == nil {
			files = []map[string]any{{"filename": "src/widget.go", "status": "modified", "additions": 3, "deletions": 1}}
		}
		_ = json.NewEncoder(w).Encode(files)
		return true
	case r.Method == http.MethodPost && path == "/repos/acme/widget/git/commits":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.newCommits = append(a.newCommits, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": fmt.Sprintf("signed%d", len(a.newCommits))})
		return true
	case r.Method == http.MethodPost && path == "/repos/acme/widget/git/refs":
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.refAttempts++
		if a.failRefs {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted failure"})
			return true
		}
		if _, exists := a.refs[body.Ref]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Reference already exists"})
			return true
		}
		a.refs[body.Ref] = body.SHA
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": body.Ref, "object": map[string]string{"sha": body.SHA}})
		return true
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/repos/acme/widget/git/refs/"):
		var body struct {
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ref := "refs/" + strings.TrimPrefix(path, "/repos/acme/widget/git/refs/")
		a.refs[ref] = body.SHA
		a.refUpdates = append(a.refUpdates, ref+"="+body.SHA)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": ref, "object": map[string]string{"sha": body.SHA}})
		return true
	case r.Method == http.MethodPost && path == "/repos/acme/widget/pulls":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.pulls = append(a.pulls, body)
		n := a.nextPR
		a.nextPR++
		a.superseders[n] = map[string]any{"number": n, "state": "open", "merged": false}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "html_url": "https://github.com/acme/widget/pull/90"})
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/pulls/") && !strings.HasSuffix(path, "/commits"):
		if a.failPRGet {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted failure"})
			return true
		}
		var n int
		_, _ = sscanInt(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), &n)
		payload, ok := a.superseders[n]
		head, observed := a.heads[n]
		if !ok && !observed {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if observed {
			served := map[string]any{"number": n, "state": "open"}
			for k, v := range payload {
				served[k] = v
			}
			if a.liveHead != "" {
				head = a.liveHead
			}
			served["head"] = map[string]any{"sha": head}
			if a.changedFiles > 0 {
				served["changed_files"] = a.changedFiles
			}
			payload = served
		}
		_ = json.NewEncoder(w).Encode(payload)
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/compare/"):
		if a.failCompare {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted failure"})
			return true
		}
		files := []map[string]string{}
		for _, f := range a.compareFiles {
			files = append(files, map[string]string{"filename": f})
		}
		payload := map[string]any{"files": files, "behind_by": a.compareBehind}
		if a.compareBase != "" {
			payload["base_commit"] = map[string]any{"sha": a.compareBase}
		}
		_ = json.NewEncoder(w).Encode(payload)
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/commits/") && strings.HasSuffix(path, "/check-runs"):
		a.baseCheckReads++
		if a.failBaseChecks {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted failure"})
			return true
		}
		runs := []map[string]any{}
		id := int64(1)
		for name, conclusion := range a.baseChecks {
			run := map[string]any{"id": id, "name": name, "status": "completed", "conclusion": conclusion}
			if conclusion == "pending" {
				run["status"] = "in_progress"
				delete(run, "conclusion")
			}
			runs = append(runs, run)
			id++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
		return true
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/repos/acme/widget/pulls/") && strings.HasSuffix(path, "/update-branch"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		expected, _ := body["expected_head_sha"].(string)
		a.branchUpdates = append(a.branchUpdates, strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), "/update-branch")+"@"+expected)
		if a.updateBranchStatus != 0 {
			w.WriteHeader(a.updateBranchStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": nonEmpty(a.updateBranchMessage, "scripted refusal")})
			return true
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
		return true
	case r.Method == http.MethodGet && path == "/repos/acme/widget/actions/runs":
		sha := r.URL.Query().Get("head_sha")
		runs := []map[string]any{}
		for _, id := range a.failedRuns {
			runs = append(runs, map[string]any{"id": id, "workflow_id": id, "run_number": 1, "run_attempt": 1, "head_sha": sha, "status": "completed", "conclusion": "failure"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
		return true
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/repos/acme/widget/actions/runs/") && strings.HasSuffix(path, "/rerun-failed-jobs"):
		var id int
		_, _ = sscanInt(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/actions/runs/"), "/rerun-failed-jobs"), &id)
		a.reruns = append(a.reruns, int64(id))
		w.WriteHeader(http.StatusCreated)
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/commits/"):
		sha := strings.TrimPrefix(path, "/repos/acme/widget/commits/")
		commit, ok := a.commits[sha]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		verification := map[string]any{"verified": commit.verified}
		if name, ok := strings.CutPrefix(commit.login, "git:"); ok {
			// Unlinked identity: bare git author/committer name, no GitHub user.
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": nil, "committer": nil, "commit": map[string]any{"author": map[string]any{"name": name}, "committer": map[string]any{"name": name}, "verification": verification}})
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": map[string]any{"login": commit.login}, "committer": map[string]any{"login": commit.login}, "commit": map[string]any{"verification": verification}})
		return true
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/repos/acme/widget/pulls/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.prEdits = append(a.prEdits, strings.TrimPrefix(path, "/repos/acme/widget/pulls/")+":"+body["state"].(string))
		if a.failEditStatus != 0 {
			w.WriteHeader(a.failEditStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "scripted refusal"})
			return true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		return true
	}
	return false
}

func sscanInt(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

// allowMaintainerEdits makes GET pulls/n report maintainer_can_modify.
func (a *adoptAPI) allowMaintainerEdits(n int, allow bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.superseders[n] = map[string]any{"number": n, "state": "open", "merged": false, "maintainer_can_modify": allow}
}

func (a *adoptAPI) setCommitAuthor(sha, login string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits[sha] = adoptCommit{login: login}
}

func (a *adoptAPI) setVerifiedCommitAuthor(sha, login string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits[sha] = adoptCommit{login: login, verified: true}
}

func (a *adoptAPI) setSuperseder(n int, state string, merged bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.superseders[n] = map[string]any{"number": n, "state": state, "merged": merged}
}

// setSupersederHead is setSuperseder with the superseder's head SHA served.
func (a *adoptAPI) setSupersederHead(n int, state string, merged bool, head string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.superseders[n] = map[string]any{"number": n, "state": state, "merged": merged, "head": map[string]any{"sha": head}}
}

func adoptPR(number int, head string, labels ...string) *gh.PullRequest {
	pr := nudgePR(number, head, labels...)
	pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("v5")}
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("mallory/widget")}
	pr.Body = gh.Ptr("Fixes the widget.\n\nCloses #12\nfixes #13\n\n— hive: backend=pi model=gpt-6\n")
	return pr
}

func workflowRefusedComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, AdoptionWorkflowRefusedMarker) {
			out = append(out, c)
		}
	}
	return out
}

func adoptionComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, AdoptionMarkerPrefix) {
			out = append(out, c)
		}
	}
	return out
}

type adoptHarness struct {
	t     *testing.T
	api   *stageAPI
	adopt *adoptAPI
	clock *contributorLaneClock
	sink  *fakeNudgeSink
	e     *Engine
}

func newAdoptHarness(t *testing.T, outcome ContributorNudgeOutcome, policy StageLabelPolicy) (*adoptHarness, func()) {
	t.Helper()
	sink := &fakeNudgeSink{outcome: outcome}
	return newAdoptHarnessWith(t, sink, sink, policy)
}

// newAdoptHarnessWith wires an arbitrary sink (wrapping fake) into the engine.
func newAdoptHarnessWith(t *testing.T, fake *fakeNudgeSink, sink ContributorNudgeSink, policy StageLabelPolicy) (*adoptHarness, func()) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	adopt := newAdoptAPI()
	api.extra = adopt.handle
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	return &adoptHarness{t: t, api: api, adopt: adopt, clock: clock, sink: fake, e: e}, closeAPI
}

func (h *adoptHarness) observe(pr *gh.PullRequest, reason string) StageLabelResult {
	h.t.Helper()
	h.adopt.mu.Lock()
	h.adopt.heads[pr.GetNumber()] = pr.GetHead().GetSHA()
	h.adopt.mu.Unlock()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
	h.e.noteStageRole("acme", "widget", pr.GetNumber(), stagelabels.RoleContributor)
	res, err := h.e.ApplyStageLabels(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *adoptHarness) addedLabels() []string {
	var out []string
	for _, a := range h.api.added {
		out = append(out, a...)
	}
	return out
}

func adoptPolicy() StageLabelPolicy {
	return StageLabelPolicy{Enabled: true, ContributorNudge: true, Adoption: true, AdoptionStrictAttribution: true, GraceOnline: 2 * time.Hour, GraceOffline: time.Hour, HiveID: "acme-hive"}
}

func TestAdoptionGraceSelection(t *testing.T) {
	p := adoptPolicy()
	if got := adoptionGrace(stagelabels.Record{NudgeOnline: true}, p); got != 2*time.Hour {
		t.Fatalf("online grace %v", got)
	}
	if got := adoptionGrace(stagelabels.Record{}, p); got != time.Hour {
		t.Fatalf("offline grace %v", got)
	}
	if got := adoptionGrace(stagelabels.Record{NudgeOnline: true}, StageLabelPolicy{}); got != 2*time.Hour {
		t.Fatalf("default online grace %v", got)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: now}
	none := ContributorPresence{}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(59*time.Minute), none, false); due {
		t.Fatal("due before grace")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(time.Hour), none, false); !due || reason != AdoptReasonGrace {
		t.Fatalf("not due at grace: %v %q", due, reason)
	}
	if due, _ := adoptionDue(rec, "bbb", p, now.Add(2*time.Hour), none, false); due {
		t.Fatal("due for a head that was never nudged")
	}
	// Live presence: online relay keeps the online grace; an offline relay is
	// adopted AdoptAfterOffline after its last heartbeat (not the nudge);
	// an unregistered author after AdoptAfterUnregistered.
	online := ContributorPresence{Registered: true, Online: true}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(time.Hour), online, true); due {
		t.Fatal("online relay adopted before the online grace")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(2*time.Hour), online, true); !due || reason != AdoptReasonGrace {
		t.Fatalf("online relay at grace: %v %q", due, reason)
	}
	offline := ContributorPresence{Registered: true, LastSeen: now.Add(10 * time.Minute)}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(24*time.Minute), offline, true); due {
		t.Fatal("offline relay adopted before 15m since its last heartbeat")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(25*time.Minute), offline, true); !due || reason != AdoptReasonOffline {
		t.Fatalf("offline relay: %v %q", due, reason)
	}
	p.AdoptAfterOffline = 5 * time.Minute
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(15*time.Minute), offline, true); !due {
		t.Fatal("after_offline knob ignored")
	}
	unregistered := ContributorPresence{}
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(14*time.Minute), unregistered, true); due {
		t.Fatal("unregistered author adopted before 15m")
	}
	if due, reason := adoptionDue(rec, "aaa", p, now.Add(15*time.Minute), unregistered, true); !due || reason != AdoptReasonUnregistered {
		t.Fatalf("unregistered author: %v %q", due, reason)
	}
	// Records nudged before FirstNudgeAt existed carry only LastNudgeAt;
	// the grace counts from there instead of never becoming due.
	legacy := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, LastNudgeAt: now}
	if due, _ := adoptionDue(legacy, "aaa", StageLabelPolicy{}, now.Add(24*time.Minute), offline, true); due {
		t.Fatal("legacy record adopted before the offline wait")
	}
	if due, reason := adoptionDue(legacy, "aaa", StageLabelPolicy{}, now.Add(40*time.Minute), offline, true); !due || reason != AdoptReasonOffline {
		t.Fatalf("legacy record not adopted: %v %q", due, reason)
	}
	if due, _ := adoptionDue(stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1}, "aaa", StageLabelPolicy{}, now.Add(24*time.Hour), offline, true); due {
		t.Fatal("record with no nudge time adopted")
	}
	p.AdoptAfterUnregistered = time.Hour
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(time.Hour), unregistered, true); !due {
		t.Fatal("after_unregistered knob ignored")
	}
	rec.AdoptedHead = "aaa"
	if due, _ := adoptionDue(rec, "aaa", p, now.Add(3*time.Hour), none, false); due {
		t.Fatal("due twice for one head")
	}
	rec.AdoptedHead = ""
	rec.AdoptMode = stagelabels.AdoptModeInPlace
	if due, _ := adoptionDue(rec, "ccc", p, now.Add(3*time.Hour), none, false); due {
		t.Fatal("due while adopted in place")
	}
	if got := adoptionCloses("Closes #12\nfixes acme/other#3\nnot closes #9 here\n\nHive-Run: acme/widget#44\n"); len(got) != 3 || got[0] != "Closes #12" || got[1] != "fixes acme/other#3" || got[2] != "Fixes #44" {
		t.Fatalf("closes lines %q", got)
	}
}

func TestAdoptionDCORehomeDecisionAndRendering(t *testing.T) {
	obs := stageObservation{
		Number:  7,
		Author:  "mallory",
		Title:   "✨ feature: add widgets",
		BaseRef: "v5",
		Body:    "Implements widgets.\n\nHive-Run: acme/widget#44\n",
		Detail:  stageDetail{DCO: stagelabels.DCONo, DCOTotal: 3, DCOUnsigned: 2},
	}
	if !adoptionNeedsDCORehome(obs, false) {
		t.Fatal("unsigned commits without branch push permission must re-home")
	}
	if !adoptionNeedsDCORehome(obs, true) {
		t.Fatal("unknown unsigned commit ownership must re-home")
	}
	obs.Detail.DCOUnsignedHive = 2
	if adoptionNeedsDCORehome(obs, true) {
		t.Fatal("branch push permission with only hive-authored unsigned commits allows in-place repair")
	}
	if !adoptionNeedsDCORehome(obs, false) {
		t.Fatal("hive-authored unsigned commits without branch push permission must still re-home")
	}
	inPlace := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeInPlace}
	if adoptedHeadNeedsDCORehome(obs, inPlace, true) {
		t.Fatal("already adopted in place with App-owned unsigned commits and push access stays in place")
	}
	if !adoptedHeadNeedsDCORehome(obs, inPlace, false) {
		t.Fatal("already adopted in place without push access must re-home")
	}
	if adoptedHeadNeedsDCORehome(obs, stagelabels.Record{AdoptedHead: "aaa", AdoptedBy: 90, AdoptMode: stagelabels.AdoptModeSupersede}, false) {
		t.Fatal("a superseded adoption is never re-homed again")
	}
	obs.Detail.DCOUnsignedHive = 0
	obs.Detail.DCOUnsigned = 0
	if adoptionNeedsDCORehome(obs, false) {
		t.Fatal("signed head does not need DCO re-home")
	}
	obs.Detail.DCOUnsigned = 2
	body := AdoptionPRBody(obs, stagelabels.Record{Reason: "check-failing:dco"}, true, "")
	for _, want := range []string{
		"**DCO repair required:**",
		"cherry-pick each original commit with `git cherry-pick -s`",
		"git commit --author",
		"same title",
		"close #7",
		"✨ feature: add widgets",
		"Supersedes #7 (adopted; re-pushed with DCO sign-off, original authorship preserved).",
		"Fixes #44",
		"Credit: original work by @mallory.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// A successful re-sign replay names the new tip and drops the manual
	// re-home steps the hive already performed.
	body = AdoptionPRBody(obs, stagelabels.Record{Reason: "check-failing:dco"}, true, "5e5e5e5e5e5e5e5e")
	if !strings.Contains(body, "re-signed as `"+shortSHA("5e5e5e5e5e5e5e5e")+"`") || strings.Contains(body, ", unchanged.") {
		t.Fatalf("re-signed body must name the re-signed tip:\n%s", body)
	}
	for _, unwanted := range []string{"**DCO repair required:**", "git cherry-pick -s"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("re-signed body still carries %q:\n%s", unwanted, body)
		}
	}
}

// Online author: nudge + 2h grace; nothing is adopted before the grace, then
// the hive supersedes the PR, comments once and moves it to 074.
func TestAdoptionOnlineGraceThenAdopt(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, adoptPolicy())
	defer done()

	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	if len(nudgeComments(h.api)) != 1 || len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("first cycle: comments=%v pulls=%d", h.api.comments, len(h.adopt.pulls))
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

	// Just under the online grace: still the contributor's.
	h.api.reset()
	h.clock.advance(2*time.Hour - time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("adopted before the online grace: added=%v pulls=%d", h.api.added, len(h.adopt.pulls))
	}

	// Grace elapsed: branch pushed at the contributor head, App PR opened,
	// one comment, stage flips to 074.
	h.clock.advance(time.Minute)
	res := h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "aaa" {
		t.Fatalf("adopt branch sha=%q refs=%v", got, h.adopt.refs)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v", h.adopt.pulls)
	}
	pull := h.adopt.pulls[0]
	if pull["head"] != "hive/adopt-7" || pull["base"] != "v5" || pull["title"] != "fix: widget" {
		t.Fatalf("superseding PR=%v", pull)
	}
	body, _ := pull["body"].(string)
	for _, want := range []string{
		"Supersedes #7 (adopted after CI went red and the contributor was unavailable; original commits by @mallory preserved)",
		"Closes #12", "fixes #13", "fork `mallory/widget`", "rebase this branch on `v5`", "`check-failing:unit`", "Do not open another PR",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("superseding body missing %q:\n%s", want, body)
		}
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || !strings.Contains(comments[0], "by=acme-hive reason=grace-elapsed mode=supersede superseded_by=90 head=aaa") || !strings.Contains(comments[0], "@mallory") ||
		!strings.Contains(comments[0], "red for 2h") || !strings.Contains(comments[0], "relay nudge went unanswered") || !strings.Contains(comments[0], "#90") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	if res.Comments != 1 {
		t.Fatalf("res=%+v", res)
	}
	// The stage flips on the next cycle (the record is adopted at this head).
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	added := h.addedLabels()
	if len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("074 transition: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if len(h.sink.resolved) != 1 || h.sink.resolved[0] != 7 {
		t.Fatalf("fix queue not resolved on adoption: %v", h.sink.resolved)
	}
	// Steady state: no churn, one adoption per head, superseder polled at most every 10m.
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 1 {
		t.Fatalf("churn after adoption: added=%v removed=%v comments=%v pulls=%d", h.api.added, h.api.removed, h.api.comments, len(h.adopt.pulls))
	}
}

// Offline / external author: comment-only nudge, 1h grace, then 074 and the
// superseding PR. Once the superseder merges the original is closed.
func TestAdoptionOfflineGraceThenSupersederMergedClosesOriginal(t *testing.T) {
	// A sink without Presence: the nudge-time graces apply (the author's
	// `— hive:` trailer still makes the PR a clanker's).
	fake := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "comment"}}
	h, done := newAdoptHarnessWith(t, fake, nudgeOnlySink{fake}, adoptPolicy())
	defer done()

	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(59 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatal("adopted before the offline grace")
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("offline grace did not adopt: pulls=%d", len(h.adopt.pulls))
	}
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "red for 1h") || !strings.Contains(c[0], "no relay session was reachable") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074: %v", h.api.added)
	}

	// Superseder merges: close note + close the original (polled after 10m).
	h.adopt.setSuperseder(90, "closed", true)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed: %v", h.adopt.prEdits)
	}
	if len(h.api.comments) != 1 || !strings.HasPrefix(h.api.comments[0], adoptionCloseMarker) || !strings.Contains(h.api.comments[0], "#90") || res.Comments != 1 {
		t.Fatalf("close note: %v res=%+v", h.api.comments, res)
	}
}

// Superseder closed unmerged: the adoption is forgotten and the PR falls
// back to 072 (a maintainer declined the takeover).
func TestAdoptionSupersederClosedUnmergedReturnsTo072(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.prEdits) != 0 {
		t.Fatalf("original closed although superseder was abandoned: %v", h.adopt.prEdits)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("fallback to 072: added=%v removed=%v", h.api.added, h.api.removed)
	}
}

// A new head from the author before the grace re-arms the nudge and is never
// adopted; a green head leaves 072 entirely.
func TestAdoptionNewHeadFromAuthorNotAdopted(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	// The author is pushing, so their relay is online: the 2h online grace.
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(50 * time.Minute)
	h.api.reset()
	// Still red at a new head: fresh nudge, grace restarts.
	h.observe(adoptPR(7, "bbb", labels...), "check-failing:unit")
	if len(nudgeComments(h.api)) != 1 || !strings.Contains(nudgeComments(h.api)[0], "head=bbb n=1") || len(h.adopt.pulls) != 0 {
		t.Fatalf("new head: comments=%v pulls=%d", h.api.comments, len(h.adopt.pulls))
	}
	h.api.reset()
	h.clock.advance(50 * time.Minute) // 100m since first nudge, 50m since the second head's
	h.observe(adoptPR(7, "bbb", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || h.api.writes() != 0 {
		t.Fatalf("adopted a freshly re-nudged head: pulls=%d writes=%d", len(h.adopt.pulls), h.api.writes())
	}
	// Author goes green: back on the normal path, no adoption ever.
	h.api.reset()
	h.clock.advance(3 * time.Hour)
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", adoptPR(7, "ccc", labels...), "contributor-lane:no-verdict", false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/050-review-pending" || len(h.adopt.pulls) != 0 {
		t.Fatalf("green head: added=%v pulls=%d", h.api.added, len(h.adopt.pulls))
	}
	if len(h.sink.resolved) != 1 {
		t.Fatalf("resolved=%v", h.sink.resolved)
	}
}

// Adoption disabled: the PR stays in 072 past every grace.
func TestAdoptionDisabledStaysReturned(t *testing.T) {
	p := adoptPolicy()
	p.Adoption = false
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted with adoption off: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
}

// Self-authored (App) PRs are never adopted: they are in 070, not 072.
func TestAdoptionSkipsSelfAuthored(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: now.Add(-5 * time.Hour)}
	if due, _ := adoptionDue(rec, "aaa", adoptPolicy(), now, ContributorPresence{}, false); !due {
		t.Fatal("precondition")
	}
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	pr := adoptPR(8, "aaa")
	pr.User.Login = gh.Ptr(testHiveAppBotLogin)
	h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The App's own PR is author/hive even though its body carries the
	// relay-style trailer: the App login wins (authorkind rule 1).
	if added := h.addedLabels(); strings.Join(added, " ") != "author/hive stage/070-fixing" {
		t.Fatalf("self PR stage: %v", h.api.added)
	}
	h.clock.advance(5 * time.Hour)
	h.api.reset()
	pr = adoptPR(8, "aaa", "stage/070-fixing", "author/hive")
	pr.User.Login = gh.Ptr(testHiveAppBotLogin)
	h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.adopt.pulls) != 0 || len(h.api.comments) != 0 {
		t.Fatalf("self PR touched: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
}

// hivecommons/hive#12527: the contributor's relay registered here, answered
// nothing and dropped off. Adoption follows AdoptAfterOffline (15m default)
// counted from its last heartbeat — not the nudge-time graces.
func TestAdoptionRelayOfflineAdoptsAfterOfflineWindow(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	start := h.clock.at
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	if len(nudgeComments(h.api)) != 1 {
		t.Fatalf("nudge: %v", h.api.comments)
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

	// Relay went away 20m after the nudge.
	h.sink.presence = &ContributorPresence{Registered: true, LastSeen: start.Add(20 * time.Minute)}
	h.api.reset()
	h.clock.advance(34 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("adopted before 15m offline: added=%v comments=%v", h.api.added, h.api.comments)
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	comments := adoptionComments(h.api)
	if len(h.adopt.pulls) != 1 || len(comments) != 1 ||
		!strings.Contains(comments[0], "by=acme-hive reason=contributor-offline mode=supersede superseded_by=90 head=aaa") ||
		!strings.Contains(comments[0], "your hive has been offline since the nudge") {
		t.Fatalf("offline adoption: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074: %v", h.api.added)
	}
}

// An author with no contributor profile here (another hive posted the PR)
// is adopted after AdoptAfterUnregistered.
func TestAdoptionUnregisteredAuthor(t *testing.T) {
	p := adoptPolicy()
	p.AdoptAfterUnregistered = 90 * time.Minute
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.sink.presence = &ContributorPresence{}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(89 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatal("adopted before after_unregistered")
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "reason=contributor-unregistered") || !strings.Contains(c[0], "no hive of yours is registered here") {
		t.Fatalf("unregistered adoption: %v", h.api.comments)
	}
}

// A base that moved past a .github/workflows/ change cannot be synced into the
// PR branch by the App (#12865, #6985): supersede instead of adopting in place.
func TestAdoptionInPlaceWorkflowGuard(t *testing.T) {
	cases := []struct {
		name        string
		files       []string
		failCompare bool
		wantInPlace bool
	}{
		{name: "base moved with workflow change", files: []string{"src/a.go", ".github/workflows/x.yml"}},
		{name: "base moved with only non-workflow files", files: []string{"src/a.go", ".github/CODEOWNERS"}, wantInPlace: true},
		{name: "compare error", failCompare: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.allowMaintainerEdits(7, true)
			h.adopt.compareFiles = tc.files
			h.adopt.failCompare = tc.failCompare
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			h.clock.advance(30 * time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
			comments := adoptionComments(h.api)
			if len(comments) != 1 {
				t.Fatalf("adoption comments: %v", h.api.comments)
			}
			if got := strings.Contains(comments[0], "mode=in-place"); got != tc.wantInPlace {
				t.Fatalf("in-place = %v, want %v: %s", got, tc.wantInPlace, comments[0])
			}
			if superseded := len(h.adopt.pulls) == 1; superseded == tc.wantInPlace {
				t.Fatalf("superseding PRs = %d, in-place want %v", len(h.adopt.pulls), tc.wantInPlace)
			}
		})
	}
}

// In-place adoption: the PR allows maintainer edits, so the hive takes over
// the PR's own branch (no superseding PR), flips to 074, keeps the adoption
// across its own pushes and yields when the contributor pushes.
func TestAdoptionInPlaceWhenMaintainerCanModify(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(30 * time.Minute)
	res := h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("superseded instead of adopting in place: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || res.Comments != 1 ||
		!strings.Contains(comments[0], "by=acme-hive reason=contributor-offline mode=in-place head=aaa") ||
		!strings.Contains(comments[0], "@mallory") || !strings.Contains(comments[0], "stage/074-adopted-by-hive") {
		t.Fatalf("in-place comment: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() || rec.AdoptReason != AdoptReasonOffline || rec.AdoptedBy != 0 {
		t.Fatalf("record: %+v", rec)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("074 transition: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if len(h.sink.resolved) != 1 || h.sink.resolved[0] != 7 {
		t.Fatalf("fix queue not resolved: %v", h.sink.resolved)
	}
	adopted := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}

	// The hive's fixer pushes: still red, adoption follows the new head, no churn.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("churn after hive push: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" {
		t.Fatalf("adoption did not follow the hive's head: %+v", rec)
	}

	// A fixer agent pushes with a bare git identity (no GitHub login): the
	// hive keeps ownership only when it recorded this exact pushed head.
	h.adopt.setCommitAuthor("bb2", "git:scanner")
	h.e.stageStoreOrShared().MarkHivePushedHead(stageKey("acme", "widget", 7), "bb2")
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bb2", adopted...), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("churn after fixer push: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bb2" {
		t.Fatalf("adoption did not follow the fixer's head: %+v", rec)
	}

	// The contributor is back and pushes: one yield note, adoption forgotten,
	// the ordinary 072 flow (fresh nudge for the new head) resumes.
	h.adopt.setCommitAuthor("ccc", "mallory")
	h.api.reset()
	h.clock.advance(time.Minute)
	res = h.observe(adoptPR(7, "ccc", adopted...), "check-failing:unit")
	var yields int
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, AdoptionYieldMarker) {
			yields++
		}
	}
	if yields != 1 || len(nudgeComments(h.api)) != 1 || res.Comments != 2 {
		t.Fatalf("yield: comments=%v res=%+v", h.api.comments, res)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("back to 072: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.AdoptedInPlace() || rec.YieldedHead != "ccc" {
		t.Fatalf("adoption not released: %+v", rec)
	}
	// Same head again: the yield note is not repeated.
	h.api.reset()
	h.observe(adoptPR(7, "ccc", labels...), "check-failing:unit")
	if len(h.api.comments) != 0 {
		t.Fatalf("yield repeated: %v", h.api.comments)
	}
}

func TestAdoptionInPlaceForConflictingOfflineContributor(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "conflicting")
	if got := nudgeComments(h.api); len(got) != 1 || !strings.Contains(got[0], "needs a rebase") {
		t.Fatalf("conflict nudge: %v", h.api.comments)
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "conflicting")
	comments := adoptionComments(h.api)
	if len(comments) != 1 ||
		!strings.Contains(comments[0], "reason=contributor-offline mode=in-place head=aaa") ||
		!strings.Contains(comments[0], "your hive has been offline since the nudge") {
		t.Fatalf("conflicting offline adoption: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() || rec.AdoptReason != AdoptReasonOffline {
		t.Fatalf("record: %+v", rec)
	}
}

func TestAlreadyAdoptedInPlaceDCOFailureRehomesAndClosesOriginal(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.api.commits = map[int][]string{7: {"fix: unsigned agent commit"}}
	h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

	h.api.reset()
	h.clock.advance(time.Minute)
	res := h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
	if res.Comments != 1 {
		t.Fatalf("comments=%d, want 1", res.Comments)
	}
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "aaa" {
		t.Fatalf("rehome branch sha=%q refs=%v", got, h.adopt.refs)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v", h.adopt.pulls)
	}
	body, _ := h.adopt.pulls[0]["body"].(string)
	for _, want := range []string{"**DCO repair required:**", "git cherry-pick -s", "Supersedes #7"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rehome body missing %q:\n%s", want, body)
		}
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || !strings.Contains(comments[0], "reason=dco-rehome mode=supersede superseded_by=90 head=aaa") {
		t.Fatalf("rehome comment: %v", h.api.comments)
	}
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed after DCO rehome: %v", h.adopt.prEdits)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedInPlace() || rec.AdoptedBy != 90 || rec.AdoptMode != stagelabels.AdoptModeSupersede || rec.AdoptReason != AdoptReasonDCORehome {
		t.Fatalf("record not moved to supersede adoption: %+v", rec)
	}
}

// The dco-rehome branch must not inherit the contributor's unsigned commits
// (#14666): each is replayed with a Signed-off-by and the branch points at the
// new tip.
func TestDCORehomeBranchCarriesResignedCommits(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.api.commits = map[int][]string{7: {"fix: unsigned contributor commit"}}
	h.observe(adoptPR(7, "c0"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "c0", AdoptReasonOffline, h.clock.at)

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "c0", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
	if len(h.adopt.newCommits) != 1 {
		t.Fatalf("re-signed commits=%v", h.adopt.newCommits)
	}
	msg, _ := h.adopt.newCommits[0]["message"].(string)
	if !strings.Contains(msg, "fix: unsigned contributor commit") || !signedOffByRE.MatchString(msg) {
		t.Fatalf("re-signed message lacks sign-off or original text: %q", msg)
	}
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "signed1" {
		t.Fatalf("rehome branch sha=%q, want the re-signed tip; refs=%v", got, h.adopt.refs)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v", h.adopt.pulls)
	}
	body, _ := h.adopt.pulls[0]["body"].(string)
	if !strings.Contains(body, "re-signed as `signed1`") || strings.Contains(body, "**DCO repair required:**") || strings.Contains(body, "git cherry-pick -s") {
		t.Fatalf("re-signed rehome body must name the new tip and omit the manual DCO repair block:\n%s", body)
	}
}

// The ownership split is cached on the store record, so the decision does
// not depend on the stage label being present on the PR.
func TestAlreadyAdoptedInPlaceDCOFailureRehomesFromCachedSplitWithoutStageLabel(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.api.commits = map[int][]string{7: {"fix: unsigned contributor commit"}}
	h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	store := h.e.stageStoreOrShared()
	if rec, _ := store.Get(key); rec.DCOUnsigned != 1 || rec.DCOUnsignedHive != 0 {
		t.Fatalf("cached split: %+v", rec)
	}
	store.MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

	h.api.reset()
	h.clock.advance(time.Minute)
	reads := h.api.commitReads
	h.observe(adoptPR(7, "aaa", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
	// The cached split spares the DCO decision a commit read; the re-sign
	// replay needs the commit trees and messages, so it lists them exactly once.
	if h.api.commitReads != reads+1 {
		t.Fatalf("commit reads=%d, want cached decision plus one re-sign listing (%d)", h.api.commitReads, reads+1)
	}
	if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 1 {
		t.Fatalf("expected re-home from cached split: pulls=%v edits=%v", h.adopt.pulls, h.adopt.prEdits)
	}
}

func TestAlreadyAdoptedInPlaceRehomeIsTriedOncePerHead(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.adopt.failRefs = true
	h.api.commits = map[int][]string{7: {"fix: unsigned contributor commit"}}
	h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

	labels := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel}
	h.api.reset()
	h.clock.advance(time.Minute)
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", adoptPR(7, "aaa", labels...), "check-failing:DCO", false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err == nil {
		t.Fatal("ApplyStageLabels should surface the failed re-home push")
	}
	if h.adopt.refAttempts != 1 {
		t.Fatalf("ref attempts=%d, want 1", h.adopt.refAttempts)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedInPlace() || !rec.Adopted("aaa") || rec.AdoptedBy != 0 || rec.AdoptReason != AdoptReasonDCORehome {
		t.Fatalf("attempt not recorded: %+v", rec)
	}

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:DCO")
	if h.adopt.refAttempts != 1 || len(h.adopt.pulls) != 0 {
		t.Fatalf("failed re-home retried: attempts=%d pulls=%v", h.adopt.refAttempts, h.adopt.pulls)
	}
}

func TestAlreadyAdoptedInPlaceAppAuthoredDCOFailureStaysInPlace(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.api.commitDetails = map[int][]stageCommitFixture{7: {
		{message: "feat: signed contributor\n\nSigned-off-by: Mallory <mallory@example.com>", authorLogin: "mallory", authorName: "Mallory", authorEmail: "mallory@example.com"},
		{message: "fix: unsigned hive fixer", authorLogin: testHiveAppBotLogin, authorName: "architect", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
	}}
	h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.DCOUnsigned != 1 || rec.DCOUnsignedHive != 1 {
		t.Fatalf("cached split: %+v", rec)
	}
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

	h.api.reset()
	h.clock.advance(time.Minute)
	res := h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
	if res.Comments != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 || len(h.adopt.prEdits) != 0 {
		t.Fatalf("App-authored unsigned commits should stay in-place for rewrite: res=%+v pulls=%v refs=%v edits=%v comments=%v", res, h.adopt.pulls, h.adopt.refs, h.adopt.prEdits, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.AdoptedBy != 0 {
		t.Fatalf("record should remain in-place adoption: %+v", rec)
	}
}

// A lane-looking git email is contributor-controlled and never makes an
// unsigned commit hive-owned: without App linkage the PR re-homes.
func TestAlreadyAdoptedInPlaceForgedLaneEmailDCOFailureRehomes(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.api.commitDetails = map[int][]stageCommitFixture{7: {
		{message: "feat: signed contributor\n\nSigned-off-by: Mallory <mallory@example.com>", authorLogin: "mallory", authorName: "Mallory", authorEmail: "mallory@example.com"},
		{message: "fix: looks like the hive", authorName: "architect", authorEmail: "architect@hive.kubestellar.io"},
	}}
	h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
	if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("forged lane email must re-home: pulls=%v edits=%v", h.adopt.pulls, h.adopt.prEdits)
	}
}

// Without push access even App-owned unsigned commits cannot be rewritten
// on the contributor branch, so the adopted PR re-homes.
func TestAlreadyAdoptedInPlaceAppAuthoredDCOFailureWithoutPushRehomes(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, false)
	h.api.commitDetails = map[int][]stageCommitFixture{7: {
		{message: "fix: unsigned hive fixer", authorLogin: testHiveAppBotLogin, authorName: "architect", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
	}}
	h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
	if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("maintainer edits revoked must re-home: pulls=%v edits=%v", h.adopt.pulls, h.adopt.prEdits)
	}
}

func TestAdoptionYieldsOnForgeableCommitIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity string
	}{
		{name: "forged scanner author name", identity: "git:scanner"},
		{name: "forged app login in git name", identity: "git:" + testHiveAppBotLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			h.clock.advance(30 * time.Minute)
			h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
			h.e.stageStoreOrShared().MarkHivePushedHead(stageKey("acme", "widget", 7), "prior-hive-head")
			h.adopt.setCommitAuthor("bbb", tc.identity)

			h.api.reset()
			h.clock.advance(time.Minute)
			res := h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
			if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.AdoptedInPlace() || rec.YieldedHead != "bbb" {
				t.Fatalf("forgeable identity kept adoption: %+v", rec)
			}
			var yields int
			for _, c := range h.api.comments {
				if strings.HasPrefix(c, AdoptionYieldMarker) {
					yields++
				}
			}
			if yields != 1 || res.Comments == 0 {
				t.Fatalf("yield comments=%d res=%+v comments=%v", yields, res, h.api.comments)
			}
		})
	}
}

func TestAdoptionKeepsOnAppLinkedOrRecordedHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*adoptHarness, string)
	}{
		{name: "app committer login", setup: func(h *adoptHarness, head string) {
			h.adopt.setVerifiedCommitAuthor(head, testHiveAppBotLogin)
		}},
		{name: "recorded hive-pushed head", setup: func(h *adoptHarness, head string) {
			h.adopt.setCommitAuthor(head, "git:scanner")
			h.e.stageStoreOrShared().MarkHivePushedHead(stageKey("acme", "widget", 7), head)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			h.clock.advance(30 * time.Minute)
			h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
			tc.setup(h, "bbb")

			h.api.reset()
			h.clock.advance(time.Minute)
			h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
			if h.api.writes() != 0 {
				t.Fatalf("churn after hive-evidenced push: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
			}
			if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" {
				t.Fatalf("adoption did not follow hive-evidenced head: %+v", rec)
			}
		})
	}
}

func TestAdoptionKeepsWhenHeadCommitLookupFails(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "missing", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("churn after unknown pusher: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "missing" {
		t.Fatalf("adoption did not stay on unknown pusher: %+v", rec)
	}
}

func TestAdoptionStrictAttributionSafetyValveBeforeRepoEvidence(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	h.adopt.setCommitAuthor("bbb", "git:scanner")

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("safety valve churned: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" {
		t.Fatalf("safety valve did not keep adoption before repo evidence: %+v", rec)
	}
}

// Adoption is an ownership state, not a CI state (hivecommons/hive#12736):
// once the hive owns a PR in place, its own pushes going pending, green or
// red again all read 074 — never 050/060/072 — until the merge gate takes
// the PR; a contributor push releases the adoption even on a green head.
func TestAdoptionPersistsWhileHiveOwnsThePR(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", returned...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", returned...), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("not adopted in place at 074: %+v", rec)
	}
	adopted := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
	stageIs := func(want stagelabels.Stage, step string) {
		t.Helper()
		if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != want {
			t.Fatalf("%s: stage %s, want %s (added=%v removed=%v)", step, rec.Stage, want, h.api.added, h.api.removed)
		}
	}

	// The hive's fixer pushes and CI is pending: still 074, nothing written.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), "check-pending")
	stageIs(stagelabels.AdoptedByHive, "ci pending")
	if h.api.writes() != 0 {
		t.Fatalf("ci pending churn: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}

	// Green, awaiting a verdict (the lane says 050): still 074.
	h.api.reset()
	h.observe(adoptPR(7, "bbb", adopted...), contributorLaneReasonNoVerdict)
	stageIs(stagelabels.AdoptedByHive, "green no-verdict")
	if h.api.writes() != 0 {
		t.Fatalf("green churn: added=%v removed=%v", h.api.added, h.api.removed)
	}

	// Approved but CI still running (the lane says 060): still 074.
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"})
	h.observe(adoptPR(7, "bbb", adopted...), "check-pending")
	stageIs(stagelabels.AdoptedByHive, "approved ci pending")

	// Red again: 074, not 072 — no nudge to the contributor the hive stood in for.
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{})
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	stageIs(stagelabels.AdoptedByHive, "red again")
	if h.api.writes() != 0 || len(nudgeComments(h.api)) != 0 {
		t.Fatalf("red churn: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}

	// The label drifted to 050 (the bug this test pins): the reconciler
	// writes 074 back and removes 050.
	h.api.reset()
	h.observe(adoptPR(7, "bbb", "stage/050-review-pending", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	stageIs(stagelabels.AdoptedByHive, "drifted label")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/050-review-pending" {
		t.Fatalf("heal 050 → 074: added=%v removed=%v", h.api.added, h.api.removed)
	}

	// The merge gate takes it: approved and soaking is 095, the adoption
	// record stays (it is released by the yield / close paths, not here).
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"})
	h.observe(adoptPR(7, "bbb", adopted...), contributorLaneReasonSoaking+"(remaining=5m)")
	stageIs(stagelabels.PreMergeSoak, "merge gate")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/095-pre-merge-soak" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074 → 095: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() {
		t.Fatalf("adoption dropped at the merge gate: %+v", rec)
	}

	// The contributor pushes over a green head: the hive yields even though
	// the lane never saw red — one yield note, then the normal pipeline.
	h.adopt.setCommitAuthor("ccc", "mallory")
	h.api.reset()
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{})
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "ccc", "stage/095-pre-merge-soak", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	stageIs(stagelabels.ReviewPending, "released")
	var yields int
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, AdoptionYieldMarker) {
			yields++
		}
	}
	if yields != 1 {
		t.Fatalf("yield on green head: comments=%v", h.api.comments)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/050-review-pending" {
		t.Fatalf("released → 050: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedInPlace() || rec.YieldedHead != "ccc" {
		t.Fatalf("adoption not released: %+v", rec)
	}
}

// A superseding adoption is bound to the adopted head: the original going
// green at that head stays 074 (the superseder carries it); a new head from
// the author is the author's again.
func TestAdoptionSupersedePersistsAtAdoptedHead(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", returned...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("not superseded: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	key := stageKey("acme", "widget", 7)
	h.api.reset()
	h.observe(adoptPR(7, "aaa", "stage/050-review-pending", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("green at adopted head: stage %s", rec.Stage)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" || len(h.api.removed) != 1 || h.api.removed[0] != "stage/050-review-pending" {
		t.Fatalf("heal 050 → 074: added=%v removed=%v", h.api.added, h.api.removed)
	}
	h.api.reset()
	h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.ReviewPending {
		t.Fatalf("author's new head: stage %s", rec.Stage)
	}
}

// In-place requested but the fork forbids maintainer edits: supersede.
func TestAdoptionInPlaceFallsBackToSupersede(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, false)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("expected superseding PR: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "mode=supersede") {
		t.Fatalf("comment: %v", h.api.comments)
	}
}

// hold / do-not-merge parks the PR: adoption is due but never performed.
func TestAdoptionBlockedByHold(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker", "hold"), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted a held PR: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	if got := adoptionBlocked([]string{"size/L", "do-not-merge/hold"}); got != "do-not-merge/hold" {
		t.Fatalf("adoptionBlocked=%q", got)
	}
}

// Adoption is not a merge (hivecommons/hive#13212): a body-marker clanker PR
// that deletes tests is adopted with a carried fence. Workflow edits refuse
// supersession before publishing an upstream branch. For test deletions,
// the fence travels with it — the record carries it, the superseding App
// PR's body carries the marker the App merge lanes refuse on, and the
// comment on the original says the merge stays manual. Only a verdict the
// fence could not reach defers adoption: a head that moved since the lane
// recorded it is not judged by the new head's diff, and a file list that is
// unreadable or shorter than changed_files waits for the next sweep, with
// no attempt recorded.
func TestAdoptionCarriesDiffFence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files        []map[string]any
		failFiles    bool
		liveHead     string
		changedFiles int
		wantFence    string
		wantDefer    bool
	}{
		{name: "plain diff adopts", files: []map[string]any{{"filename": "src/widget.go", "status": "modified", "additions": 3, "deletions": 1}}},
		{name: "workflow path", files: []map[string]any{{"filename": "src/widget.go", "status": "modified"}, {"filename": ".github/workflows/ci.yml", "status": "modified"}}, wantFence: contributorLaneReasonWorkflowPath},
		{name: "action definition", files: []map[string]any{{"filename": ".github/actions/setup/action.yml", "status": "modified"}}, wantFence: contributorLaneReasonWorkflowPath},
		{name: "test file removed", files: []map[string]any{{"filename": "src/widget_test.go", "status": "removed", "deletions": 4}}, wantFence: contributorLaneReasonTestsRemoved},
		{name: "tests gutted", files: []map[string]any{{"filename": "src/widget_test.go", "status": "modified", "additions": 1, "deletions": 40}}, wantFence: contributorLaneReasonTestsRemoved},
		{name: "file list unavailable", failFiles: true, wantDefer: true},
		{name: "head moved since observed", liveHead: "bbb", wantDefer: true},
		{name: "file list incomplete", changedFiles: 3001, wantDefer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptAfterUnregistered = 30 * time.Minute
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			// No profile here: author/clanker comes from the PR body alone.
			h.sink.presence = &ContributorPresence{}
			h.adopt.prFiles = tc.files
			h.adopt.failPRFiles = tc.failFiles
			h.adopt.liveHead = tc.liveHead
			h.adopt.changedFiles = tc.changedFiles
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			h.api.reset()
			h.clock.advance(time.Hour)
			h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
			key := stageKey("acme", "widget", 7)
			if tc.wantFence == contributorLaneReasonWorkflowPath {
				// Refused, not deferred: one attempt recorded, one note,
				// nothing published, and the PR goes to a person (085).
				rec, _ := h.e.stageStoreOrShared().Get(key)
				if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 || len(h.adopt.refs) != 0 || h.adopt.refAttempts != 0 {
					t.Fatalf("workflow-path head published: pulls=%d refs=%v comments=%v", len(h.adopt.pulls), h.adopt.refs, h.api.comments)
				}
				if c := workflowRefusedComments(h.api); len(c) != 1 || !strings.Contains(c[0], "cherry-pick") {
					t.Fatalf("refusal note = %v, want one", h.api.comments)
				}
				if !rec.Adopted("aaa") || rec.AdoptedBy != 0 || rec.AdoptDiffFence != tc.wantFence || rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason {
					t.Fatalf("refusal not recorded as an attempt: %+v", rec)
				}
				h.api.reset()
				h.clock.advance(time.Minute)
				h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
				rec, _ = h.e.stageStoreOrShared().Get(key)
				if len(h.adopt.pulls) != 0 || h.adopt.refAttempts != 0 || len(workflowRefusedComments(h.api)) != 0 || len(stalledComments(h.api)) != 0 || rec.Stage != stagelabels.NeedsDecision {
					t.Fatalf("refused head looped or missed 085: rec=%+v comments=%v", rec, h.api.comments)
				}
				return
			}
			if tc.wantDefer {
				if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 || len(h.adopt.refs) != 0 || h.adopt.refAttempts != 0 {
					t.Fatalf("deferred adoption went ahead: pulls=%d refs=%v comments=%v", len(h.adopt.pulls), h.adopt.refs, h.api.comments)
				}
				if added := strings.Join(h.addedLabels(), " "); strings.Contains(added, "stage/074") {
					t.Fatalf("deferred adoption moved to 074: %v", h.api.added)
				}
				if rec, ok := h.e.stageStoreOrShared().Get(key); ok && rec.Adopted("aaa") {
					t.Fatal("deferred adoption was recorded as an attempt")
				}
				// Not sticky: once the fence can read the head, the same
				// head is adopted on the next sweep.
				h.adopt.prFiles, h.adopt.failPRFiles = nil, false
				h.adopt.liveHead, h.adopt.changedFiles = "", 0
				h.api.reset()
				h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
				if len(h.adopt.pulls) != 1 {
					t.Fatalf("readable diff not adopted after deferral: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
				}
				return
			}
			comments := adoptionComments(h.api)
			if len(h.adopt.pulls) != 1 || len(comments) != 1 {
				t.Fatalf("not adopted: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
			}
			rec, _ := h.e.stageStoreOrShared().Get(key)
			if rec.AdoptDiffFence != tc.wantFence {
				t.Fatalf("record fence = %q, want %q", rec.AdoptDiffFence, tc.wantFence)
			}
			body, _ := h.adopt.pulls[0]["body"].(string)
			if got := carriedFenceReason(body); got != tc.wantFence {
				t.Fatalf("superseding PR body carries fence %q, want %q:\n%s", got, tc.wantFence, body)
			}
			if tc.wantFence == "" {
				if strings.Contains(comments[0], "merge stays manual") || strings.Contains(body, "Merge stays manual") {
					t.Fatalf("clean diff announced a manual merge: comment=%q body=%q", comments[0], body)
				}
				return
			}
			if !strings.Contains(comments[0], "merge stays manual") || !strings.Contains(comments[0], decisionCause(tc.wantFence)) {
				t.Fatalf("adoption comment does not say the merge stays manual for %s: %q", tc.wantFence, comments[0])
			}
			if !strings.Contains(body, "**Merge stays manual**") || !strings.Contains(body, "#7") {
				t.Fatalf("superseding PR body does not carry the manual-merge rule:\n%s", body)
			}
		})
	}
}

// In place, the fence is carried on the record and the adoption comment:
// the PR stays contributor-authored, the fix lane repairs CI on its branch
// and the contributor lane's own diff fence routes the green head to 085.
func TestAdoptionInPlaceCarriesDiffFence(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.AdoptAfterUnregistered = 30 * time.Minute
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.sink.presence = &ContributorPresence{}
	h.adopt.allowMaintainerEdits(7, true)
	h.adopt.prFiles = []map[string]any{{"filename": "src/widget_test.go", "status": "removed", "deletions": 40}}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker", "needs-rebase"}
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonTestsRemoved)
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("in-place adoption superseded: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || !strings.Contains(comments[0], "mode=in-place") {
		t.Fatalf("in-place adoption comment missing: %v", h.api.comments)
	}
	if !strings.Contains(comments[0], "merge stays manual") || !strings.Contains(comments[0], decisionCause(contributorLaneReasonTestsRemoved)) {
		t.Fatalf("in-place adoption comment does not say the merge stays manual: %q", comments[0])
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() || rec.AdoptDiffFence != contributorLaneReasonTestsRemoved {
		t.Fatalf("record = %+v, want in-place adoption carrying %s", rec, contributorLaneReasonTestsRemoved)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonTestsRemoved)
	if added := strings.Join(h.addedLabels(), " "); !strings.Contains(added, "stage/074-adopted-by-hive") {
		t.Fatalf("fenced in-place adoption not at 074: %v", h.api.added)
	}
	// The hive's fixer rebases and repairs CI; the contributor lane still
	// reads the fenced diff on the green head, so the PR is a person's to
	// merge (085) — never the lane's — and the adoption record holds.
	adopted := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adopted...), contributorLaneReasonTestsRemoved)
	if added := strings.Join(h.addedLabels(), " "); !strings.Contains(added, "stage/085-needs-decision") {
		t.Fatalf("green fenced head not handed to a person: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.AdoptedInPlace() || rec.AdoptedHead != "bbb" || rec.AdoptDiffFence != contributorLaneReasonTestsRemoved {
		t.Fatalf("adoption did not follow the hive's head with its fence: %+v", rec)
	}
	// The contributor pushes again: the yield forgets the fence with the
	// adoption, so a fresh head is judged afresh.
	h.adopt.setCommitAuthor("ccc", "mallory")
	h.api.reset()
	h.observe(adoptPR(7, "ccc", "stage/085-needs-decision", "role/contributor", "author/clanker"), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); rec.AdoptedInPlace() || rec.AdoptDiffFence != "" {
		t.Fatalf("yield kept the fence: %+v", rec)
	}
}

// The dco-rehome takeover refuses workflow publication and carries test fences:
// the App PR is opened with the marker and the original closed, exactly as
// a clean re-home, instead of releasing the adoption back to 072 where the
// fence used to hold it (hivecommons/hive#12850). A head that moved since
// the lane observed it, an incomplete list or a transient files-API failure
// is only deferred: the adoption stays and the next sweep re-reads.
func TestAlreadyAdoptedInPlaceDCORehomeCarriesDiffFence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files        []map[string]any
		changedFiles int
		liveHead     string
		failFiles    bool
		wantFence    string
		wantDefer    bool
	}{
		{name: "workflow path refuses publication", files: []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified"}}, wantFence: contributorLaneReasonWorkflowPath},
		{name: "test file removed carries", files: []map[string]any{{"filename": "src/widget_test.go", "status": "removed", "deletions": 4}}, wantFence: contributorLaneReasonTestsRemoved},
		{name: "incomplete file list defers", changedFiles: 3001, wantDefer: true},
		{name: "head moved defers", liveHead: "bbb", wantDefer: true},
		{name: "files API failure defers", failFiles: true, wantDefer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			p.DCORequired = func(string) (bool, bool) { return true, true }
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.adopt.allowMaintainerEdits(7, true)
			h.api.commits = map[int][]string{7: {"fix: unsigned agent commit"}}
			h.observe(adoptPR(7, "aaa"), "check-failing:DCO")
			key := stageKey("acme", "widget", 7)
			h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)

			h.adopt.prFiles = tc.files
			h.adopt.changedFiles = tc.changedFiles
			h.adopt.liveHead = tc.liveHead
			h.adopt.failPRFiles = tc.failFiles
			h.api.reset()
			h.clock.advance(time.Minute)
			h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel), "check-failing:DCO")
			rec, _ := h.e.stageStoreOrShared().Get(key)
			if tc.wantFence == contributorLaneReasonWorkflowPath {
				if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 || h.adopt.refAttempts != 0 || len(h.adopt.prEdits) != 0 || len(adoptionComments(h.api)) != 0 || len(workflowRefusedComments(h.api)) != 1 {
					t.Fatalf("workflow-path re-home not refused once: pulls=%v refs=%v edits=%v comments=%v", h.adopt.pulls, h.adopt.refs, h.adopt.prEdits, h.api.comments)
				}
				if !rec.AdoptedInPlace() || rec.AdoptReason == AdoptReasonDCORehome || rec.AdoptDiffFence != tc.wantFence || rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason {
					t.Fatalf("refused re-home not recorded as terminal: %+v", rec)
				}
				return
			}
			if tc.wantDefer {
				if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 || h.adopt.refAttempts != 0 || len(h.adopt.prEdits) != 0 || len(adoptionComments(h.api)) != 0 {
					t.Fatalf("deferred re-home went ahead: pulls=%v refs=%v edits=%v comments=%v", h.adopt.pulls, h.adopt.refs, h.adopt.prEdits, h.api.comments)
				}
				if !rec.AdoptedInPlace() || rec.AdoptReason == AdoptReasonDCORehome || rec.AdoptDiffFence != "" {
					t.Fatalf("deferred re-home changed the adoption: %+v", rec)
				}
				return
			}
			if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 1 || len(adoptionComments(h.api)) != 1 {
				t.Fatalf("fenced re-home did not go ahead: pulls=%v edits=%v comments=%v", h.adopt.pulls, h.adopt.prEdits, h.api.comments)
			}
			if rec.AdoptedInPlace() || !rec.Adopted("aaa") || rec.AdoptedBy == 0 || rec.AdoptReason != AdoptReasonDCORehome || rec.AdoptDiffFence != tc.wantFence {
				t.Fatalf("re-homed record = %+v, want supersession carrying %s", rec, tc.wantFence)
			}
			body, _ := h.adopt.pulls[0]["body"].(string)
			if got := carriedFenceReason(body); got != tc.wantFence {
				t.Fatalf("superseding PR body carries fence %q, want %q:\n%s", got, tc.wantFence, body)
			}
			if c := adoptionComments(h.api)[0]; !strings.Contains(c, "reason="+AdoptReasonDCORehome) || !strings.Contains(c, "merge stays manual") {
				t.Fatalf("re-home comment = %q, want dco-rehome marker and the manual-merge note", c)
			}
		})
	}
}

// The 072 interrupt is for author/clanker only: a human's PR (no hive
// trailer, no profile) sits at 071 needs-author and a hive-authored PR (the
// App's own login) at 070 fixing, with neither nudge nor adoption.
func TestAdoptionAndNudgeOnlyForClankers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		mutate func(pr *gh.PullRequest)
	}{
		{"human", "stage/071-needs-author", func(pr *gh.PullRequest) { pr.Body = gh.Ptr("Fixes the widget.\n") }},
		{"hive", "stage/070-fixing", func(pr *gh.PullRequest) {
			// The App's own PR; an agent/<lane> stamp on a PR the App did
			// not author is a stray (#12545) and is not hive evidence.
			pr.User.Login = gh.Ptr(testHiveAppBotLogin)
			pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr("agent/scanner")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
			defer done()
			h.sink.presence = &ContributorPresence{}
			pr := adoptPR(7, "aaa")
			tc.mutate(pr)
			h.observe(pr, "check-failing:unit")
			if added := strings.Join(h.addedLabels(), " "); !strings.Contains(added, tc.stage) || strings.Contains(added, "stage/072-returned-to-contributor") {
				t.Fatalf("%s: %v", tc.stage, h.api.added)
			}
			if len(h.api.comments) != 0 || len(h.sink.nudges) != 0 {
				t.Fatalf("interrupt fired for %s author: comments=%v nudges=%d", tc.name, h.api.comments, len(h.sink.nudges))
			}
			h.api.reset()
			h.clock.advance(6 * time.Hour)
			pr = adoptPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker")
			tc.mutate(pr)
			h.observe(pr, "check-failing:unit")
			if len(h.api.comments) != 0 || len(h.adopt.pulls) != 0 || strings.Contains(strings.Join(h.addedLabels(), " "), "stage/074") {
				t.Fatalf("adopted a %s PR: added=%v comments=%v pulls=%d", tc.name, h.api.added, h.api.comments, len(h.adopt.pulls))
			}
		})
	}
}

func TestDelegatedSupersedeAdoptsParkedPRWithoutGrace(t *testing.T) {
	policy := adoptPolicy()
	policy.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, policy)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	labels := []string{"stage/085-needs-decision", delegatedecision.LabelAdoptSupersede}
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonNoReviewer)
	if len(h.adopt.pulls) != 1 || h.adopt.refs["refs/heads/hive/adopt-7"] != "aaa" {
		t.Fatalf("supersede not executed: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
	body, _ := h.adopt.pulls[0]["body"].(string)
	for _, want := range []string{"Supersedes #7", "@mallory", "Closes #12", "fixes #13"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonNoReviewer)
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("duplicate adoption: %v", h.adopt.pulls)
	}
}

func TestDelegatedSupersedeHonorsHoldAndDisabledAdoption(t *testing.T) {
	for _, held := range []bool{true, false} {
		t.Run(map[bool]string{true: "hold", false: "disabled"}[held], func(t *testing.T) {
			policy := adoptPolicy()
			policy.Adoption = held
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, policy)
			defer done()
			labels := []string{delegatedecision.LabelAdoptSupersede}
			if held {
				labels = append(labels, "hold")
			}
			h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonNoReviewer)
			if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
				t.Fatalf("adopted: %v", h.adopt.pulls)
			}
		})
	}
}

func TestDelegatedSupersedeRehomesInPlaceAdoption(t *testing.T) {
	policy := adoptPolicy()
	policy.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, policy)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, "aaa", AdoptReasonOffline, h.clock.at)
	h.observe(adoptPR(7, "aaa", "stage/085-needs-decision", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if len(h.adopt.pulls) != 1 || rec.AdoptMode != stagelabels.AdoptModeSupersede || rec.AdoptedBy != 90 {
		t.Fatalf("in-place adoption not superseded: record=%+v pulls=%v", rec, h.adopt.pulls)
	}
}

func TestDelegatedSupersedeFailureIsOneAttemptAndClearsLabel(t *testing.T) {
	policy := adoptPolicy()
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, policy)
	defer done()
	h.adopt.failRefs = true
	labels := []string{"stage/085-needs-decision", delegatedecision.LabelAdoptSupersede}
	pr := adoptPR(7, "aaa", labels...)
	h.adopt.mu.Lock()
	h.adopt.heads[7] = "aaa"
	h.adopt.mu.Unlock()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoReviewer, false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err == nil {
		t.Fatal("ApplyStageLabels should surface the failed supersede push")
	}
	if h.adopt.refAttempts != 1 || len(h.adopt.pulls) != 0 {
		t.Fatalf("attempts=%d pulls=%v", h.adopt.refAttempts, h.adopt.pulls)
	}
	key := stageKey("acme", "widget", 7)
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.Adopted("aaa") || rec.AdoptedBy != 0 {
		t.Fatalf("attempt not recorded: %+v", rec)
	}
	if !slices.Contains(h.api.removed, delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("request label kept after a recorded attempt: removed=%v", h.api.removed)
	}

	// With the request consumed the PR re-parks at 085 for the owner: the
	// next sweep neither retries the push nor touches the label again.
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/085-needs-decision"), contributorLaneReasonNoReviewer)
	if h.adopt.refAttempts != 1 || len(h.api.removed) != 0 {
		t.Fatalf("failed delegated supersede retried: attempts=%d removed=%v", h.adopt.refAttempts, h.api.removed)
	}
}

// A delegated supersede of a workflow-path head is refused as one recorded
// attempt: the request label clears, one note says why, nothing is published
// and the next sweep does not retry (hivecommons/hive#14055).
func TestDelegatedSupersedeWorkflowPathIsTerminal(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, adoptPolicy())
	defer done()
	h.adopt.prFiles = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified"}}
	pr := adoptPR(7, "aaa", "stage/085-needs-decision", delegatedecision.LabelAdoptSupersede)
	h.adopt.mu.Lock()
	h.adopt.heads[7] = "aaa"
	h.adopt.mu.Unlock()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoReviewer, false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.adopt.refAttempts != 0 || len(h.adopt.refs) != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("workflow-path head published: attempts=%d refs=%v pulls=%v", h.adopt.refAttempts, h.adopt.refs, h.adopt.pulls)
	}
	if c := workflowRefusedComments(h.api); len(c) != 1 {
		t.Fatalf("refusal notes = %v, want one", h.api.comments)
	}
	key := stageKey("acme", "widget", 7)
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.Adopted("aaa") || rec.AdoptedBy != 0 || rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason {
		t.Fatalf("refusal not recorded: %+v", rec)
	}
	if !slices.Contains(h.api.removed, delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("request label kept after a refused attempt: removed=%v", h.api.removed)
	}

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/085-needs-decision"), contributorLaneReasonNoReviewer)
	if h.adopt.refAttempts != 0 || len(h.api.removed) != 0 || len(workflowRefusedComments(h.api)) != 0 {
		t.Fatalf("refused delegated supersede retried: attempts=%d removed=%v comments=%v", h.adopt.refAttempts, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("refused head left 085: stage %s", rec.Stage)
	}
}

// A head already escalated and noted as stalled still gets the refusal note
// when a maintainer's delegated supersede is refused: the label must not vanish
// unexplained.
func TestDelegatedSupersedeWorkflowPathNotesOverStalledNote(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, adoptPolicy())
	defer done()
	h.adopt.prFiles = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified"}}
	pr := adoptPR(7, "aaa", "stage/085-needs-decision", delegatedecision.LabelAdoptSupersede)
	h.adopt.mu.Lock()
	h.adopt.heads[7] = "aaa"
	h.adopt.mu.Unlock()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoReviewer, false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptionEscalated(key, "aaa", stagelabels.LaneReasonAdoptionStalledPrefix+"stalled")
	if _, err := h.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := workflowRefusedComments(h.api); len(c) != 1 {
		t.Fatalf("refusal notes = %v, want one", h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason {
		t.Fatalf("refusal not recorded: %+v", rec)
	}
	if !slices.Contains(h.api.removed, delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("request label kept after a refused attempt: removed=%v", h.api.removed)
	}
	if h.adopt.refAttempts != 0 || len(h.adopt.refs) != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("workflow-path head published: attempts=%d", h.adopt.refAttempts)
	}
}

// Every caller, including second takeovers, must pass fresh evidence before
// creating OR updating a ref. A record's old fence is not evidence for a head.
func TestSupersedeRevalidatesBeforeRefMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*adoptAPI)
	}{
		{"workflow", func(a *adoptAPI) {
			a.prFiles = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified"}}
		}},
		{"action", func(a *adoptAPI) {
			a.prFiles = []map[string]any{{"filename": ".github/actions/setup/action.yml", "status": "modified"}}
		}},
		{"head moved", func(a *adoptAPI) { a.liveHead = "bbb" }},
		{"incomplete files", func(a *adoptAPI) { a.changedFiles = 3001 }},
		{"unreadable files", func(a *adoptAPI) { a.failPRFiles = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{}, adoptPolicy())
			defer done()
			h.adopt.heads[7] = "aaa"
			h.adopt.refs["refs/heads/hive/adopt-7"] = "old"
			tc.prepare(h.adopt)
			obs := stageObservation{Owner: "acme", Repo: "widget", Display: "acme/widget", Number: 7, HeadSHA: "aaa", BaseRef: "v5"}
			n, _ := h.e.supersedeAdoptedPR(context.Background(), h.e.stageStoreOrShared(), stageKey("acme", "widget", 7), obs, stagelabels.Record{}, adoptPolicy(), AdoptReasonDCORehome, 0, false, true, h.clock.at)
			if n != 0 || h.adopt.refAttempts != 0 || len(h.adopt.refUpdates) != 0 || h.adopt.refs["refs/heads/hive/adopt-7"] != "old" || len(h.adopt.pulls) != 0 || len(h.adopt.prEdits) != 0 {
				t.Fatalf("unsafe supersession mutated repository: n=%d refs=%v updates=%v pulls=%v edits=%v", n, h.adopt.refs, h.adopt.refUpdates, h.adopt.pulls, h.adopt.prEdits)
			}
		})
	}
}

// adoptThenLeave074 adopts PR 7 at head "aaa" (offline grace) and returns
// the labels the PR wears at 074. Shared by the hivecommons/hive#14471
// cases, which then move the PR off 074 before the superseder merges.
func adoptThenLeave074(t *testing.T) (*adoptHarness, func()) {
	t.Helper()
	fake := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "comment"}}
	h, done := newAdoptHarnessWith(t, fake, nudgeOnlySink{fake}, adoptPolicy())
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("not adopted: pulls=%d", len(h.adopt.pulls))
	}
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) == 0 || added[len(added)-1] != "stage/074-adopted-by-hive" {
		t.Fatalf("074: %v", h.api.added)
	}
	return h, done
}

// hivecommons/hive#14471: a `needs-human` label parks the adopted PR at 085,
// which does not yield to adoption. The superseder's merge must still close
// the original, once.
func TestAdoptionSupersederMergedClosesOriginalParkedAt085(t *testing.T) {
	h, done := adoptThenLeave074(t)
	defer done()
	parked := []string{"stage/074-adopted-by-hive", "needs-human", "role/contributor", "author/clanker"}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", parked...), "label:needs-human")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/085-needs-decision" {
		t.Fatalf("085 from needs-human: added=%v removed=%v", h.api.added, h.api.removed)
	}
	at085 := []string{"stage/085-needs-decision", "needs-human", "role/contributor", "author/clanker"}
	h.adopt.setSuperseder(90, "closed", true)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.observe(adoptPR(7, "aaa", at085...), "label:needs-human")
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed from 085: %v", h.adopt.prEdits)
	}
	closeNotes := 0
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, adoptionCloseMarker) && strings.Contains(c, "#90") {
			closeNotes++
		}
	}
	if closeNotes != 1 || res.Comments < 1 {
		t.Fatalf("close note: %v res=%+v", h.api.comments, res)
	}
	rec, ok := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !ok || rec.SupersededBy != 90 || rec.SupersededOpen {
		t.Fatalf("record not marked superseded: ok=%v rec=%+v", ok, rec)
	}
	// Idempotent: the PR still reads open to the lane (GitHub lag) but the
	// note and close never repeat.
	h.api.reset()
	h.adopt.prEdits = nil
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", at085...), "label:needs-human")
	if len(h.adopt.prEdits) != 0 || len(h.api.comments) != 0 {
		t.Fatalf("repeated settle: edits=%v comments=%v", h.adopt.prEdits, h.api.comments)
	}
}

// hivecommons/hive#14471 (#14403 → #14461): the contributor pushes a new
// head after the adoption, so the PR falls back to 072; the fix lane carries
// that head into the superseder and merges it. The original closes.
func TestAdoptionSupersederMergedWithLaterHeadClosesOriginal(t *testing.T) {
	h, done := adoptThenLeave074(t)
	defer done()
	h.api.reset()
	h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("new head should leave 074: added=%v", h.api.added)
	}
	at072 := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.adopt.setSupersederHead(90, "closed", true, "bbb")
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "bbb", at072...), "check-failing:unit")
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed after superseder merged its head: %v", h.adopt.prEdits)
	}
}

// hivecommons/hive#14471: the superseder merged the adopted head but the
// original has commits past it. One note per head, no close.
func TestAdoptionSupersederMergedDivergedHeadNotesOnce(t *testing.T) {
	h, done := adoptThenLeave074(t)
	defer done()
	at072 := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.observe(adoptPR(7, "bbb", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	h.adopt.setSupersederHead(90, "closed", true, "aaa")
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "bbb", at072...), "check-failing:unit")
	if len(h.adopt.prEdits) != 0 {
		t.Fatalf("closed a PR whose head the superseder did not carry: %v", h.adopt.prEdits)
	}
	notes := 0
	for _, c := range h.api.comments {
		if strings.HasPrefix(c, AdoptionSupersederMergedDivergedMarker) && strings.Contains(c, "#90") && strings.Contains(c, "bbb") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("diverged note: %v", h.api.comments)
	}
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "bbb", at072...), "check-failing:unit")
	if len(h.api.comments) != 0 || len(h.adopt.prEdits) != 0 {
		t.Fatalf("diverged note repeated: comments=%v edits=%v", h.api.comments, h.adopt.prEdits)
	}
}

// hivecommons/hive#14471: GitHub refuses the close (the App cannot close
// this fork PR). The note stands, the record is superseded-open so the
// tracker drops the row, nothing repeats and no error is raised.
func TestAdoptionSupersederMergedCloseRefusedMarksSupersededOpen(t *testing.T) {
	h, done := adoptThenLeave074(t)
	defer done()
	at074 := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
	h.adopt.mu.Lock()
	h.adopt.failEditStatus = http.StatusForbidden
	h.adopt.mu.Unlock()
	h.adopt.setSuperseder(90, "closed", true)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", at074...), "check-failing:unit")
	if len(h.adopt.prEdits) != 1 {
		t.Fatalf("close not attempted: %v", h.adopt.prEdits)
	}
	rec, ok := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !ok || rec.SupersededBy != 90 || !rec.SupersededOpen {
		t.Fatalf("record not superseded-open: ok=%v rec=%+v", ok, rec)
	}
	h.api.reset()
	h.adopt.prEdits = nil
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", at074...), "check-failing:unit")
	if len(h.adopt.prEdits) != 0 || len(h.api.comments) != 0 {
		t.Fatalf("refused close retried: edits=%v comments=%v", h.adopt.prEdits, h.api.comments)
	}
}

// hivecommons/hive#14471: a `Refs #N` line travels into the superseder body.
func TestAdoptionClosesCarriesRefs(t *testing.T) {
	got := adoptionCloses("Refs #14389 — addresses root cause 2\nRefs #5\nCloses #12\n")
	if len(got) != 2 || got[0] != "Refs #5" || got[1] != "Closes #12" {
		t.Fatalf("adoptionCloses: %v", got)
	}
}
