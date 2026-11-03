package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageFactsServer is a GitHub stand-in for one issue (or PR) under o/r: it
// serves the item's labels and state and records label writes.
type stageFactsServer struct {
	mu      sync.Mutex
	srv     *httptest.Server
	labels  []string
	state   string
	isPR    bool
	body    string
	added   []string
	removed []string
	// prState / prMerged answer GET /pulls/N (the covering-PR re-read);
	// prReads counts those reads.
	prState  string
	prMerged bool
	prReads  int
}

func newStageFactsServer(t *testing.T, labels ...string) *stageFactsServer {
	t.Helper()
	s := &stageFactsServer{labels: labels, state: "open"}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode(wireLabelsOf(s.labels))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			s.prReads++
			state := s.prState
			if state == "" {
				state = "open"
			}
			body := map[string]any{"number": 8, "state": state, "merged": s.prMerged}
			if s.prMerged {
				body["merged_at"] = "2026-10-22T12:00:00Z"
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/"):
			body := map[string]any{"number": 7, "state": s.state, "labels": wireLabelsOf(s.labels), "user": map[string]any{"login": "dev"}, "body": s.body}
			if s.isPR {
				body["pull_request"] = map[string]any{"url": "https://api.github.com/repos/o/r/pulls/7"}
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.added = append(s.added, body...)
			for _, l := range body {
				if !issueHasLabel(s.labels, l) {
					s.labels = append(s.labels, l)
				}
			}
			_ = json.NewEncoder(w).Encode(wireLabelsOf(s.labels))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
			label := r.URL.Path[strings.LastIndex(r.URL.Path, "/labels/")+len("/labels/"):]
			s.removed = append(s.removed, label)
			s.labels = removeIssueLabel(s.labels, label)
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"message":"unexpected %s %s"}`, r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func wireLabelsOf(labels []string) []map[string]string {
	out := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func (s *stageFactsServer) snapshot() (labels, added, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels...), append([]string(nil), s.added...), append([]string(nil), s.removed...)
}

func TestStageFacts_DefaultSinkAndOverride(t *testing.T) {
	var nilClient *Client
	if nilClient.StageFacts() != nil {
		t.Fatal("a nil client has no sink")
	}
	nilClient.SetStageFactSink(&stageenginetest.Recorder{}) // must not panic

	srv := newStageFactsServer(t)
	c := testClient(t, srv.srv.URL)
	def := c.StageFacts()
	if def == nil {
		t.Fatal("default sink is nil")
	}
	if c.StageFacts() != def {
		t.Fatal("default sink must be built once")
	}
	rec := &stageenginetest.Recorder{}
	c.SetStageFactSink(rec)
	if c.StageFacts() != rec {
		t.Fatal("SetStageFactSink did not install the sink")
	}
	c.SetStageFactSink(nil)
	if got := c.StageFacts(); got == nil || got == rec {
		t.Fatal("nil must restore a default sink")
	}

	ref := c.IssueStageRef("r", 7)
	if ref != (stageengine.Ref{Owner: "o", Repo: "r", Number: 7, Kind: stagelabels.IssueItem}) {
		t.Fatalf("IssueStageRef(bare name) = %+v", ref)
	}
	if pr := c.PRStageRef("o/r", 8); pr.Kind != stagelabels.PRItem || pr.Number != 8 || pr.Owner != "o" {
		t.Fatalf("PRStageRef = %+v", pr)
	}
}

// The default sink end to end: a Claimed fact on an issue the enumeration
// never staged reads the issue, derives stage/020 and writes it through the
// client's labeler — the label the retired direct write produced — keeping
// the role and `stalled` the issue already wears.
func TestStageFacts_DefaultSinkWritesDerivedStage(t *testing.T) {
	srv := newStageFactsServer(t, "bug", stagelabels.StalledLabel, stagelabels.Actionable.Label())
	c := testClient(t, srv.srv.URL)
	ref := c.IssueStageRef("o/r", 7)

	if err := c.StageFacts().Claimed(context.Background(), ref, stageengine.ClaimFact{Holder: "hive-a"}); err != nil {
		t.Fatal(err)
	}
	labels, added, removed := srv.snapshot()
	if !issueHasLabel(labels, stagelabels.Claimed.Label()) || issueHasLabel(labels, stagelabels.Actionable.Label()) {
		t.Fatalf("labels = %v, want 020 replacing 010", labels)
	}
	if !issueHasLabel(labels, stagelabels.StalledLabel) || !issueHasLabel(labels, "bug") {
		t.Fatalf("labels = %v, want stalled and bug kept", labels)
	}
	if strings.Join(added, ",") != stagelabels.Claimed.Label() || strings.Join(removed, ",") != stagelabels.Actionable.Label() {
		t.Fatalf("added=%v removed=%v", added, removed)
	}

	// Releasing puts the issue back at 010 now (the enumeration used to
	// do it a cycle later).
	if err := c.StageFacts().ClaimReleased(context.Background(), ref, "hive-a"); err != nil {
		t.Fatal(err)
	}
	labels, _, _ = srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.Actionable {
		t.Fatalf("labels after release = %v, want 010", labels)
	}
}

// hivecommons/hive#12999: an agent claim fact on an epic (a never-claim
// label the fresh read supplies) never stages it 030 — the umbrella issue
// is healed to 010 and the stale 030 comes off.
func TestStageFacts_ClaimOnEpicNeverStages030(t *testing.T) {
	srv := newStageFactsServer(t, "Epic", "help wanted", stagelabels.PRInProgress.Label())
	c := testClient(t, srv.srv.URL)
	ref := c.IssueStageRef("o/r", 7)

	if err := c.StageFacts().Claimed(context.Background(), ref, stageengine.ClaimFact{Holder: "scanner", Kind: claims.KindAgent}); err != nil {
		t.Fatal(err)
	}
	labels, added, removed := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.Actionable {
		t.Fatalf("labels = %v, want 010 on an epic", labels)
	}
	if issueHasLabel(labels, stagelabels.PRInProgress.Label()) || strings.Join(removed, ",") != stagelabels.PRInProgress.Label() {
		t.Fatalf("labels=%v removed=%v, want the stale 030 removed", labels, removed)
	}
	if strings.Join(added, ",") != stagelabels.Actionable.Label() {
		t.Fatalf("added=%v, want only 010", added)
	}
}

// A fact on an issue that closed meanwhile clears its stage instead of
// re-staging it (the enumeration never sees closed issues).
func TestStageFacts_ClosedIssueLosesStage(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.Claimed.Label(), "bug")
	srv.state = "closed"
	c := testClient(t, srv.srv.URL)

	if err := c.StageFacts().ClaimReleased(context.Background(), c.IssueStageRef("o/r", 7), "hive-a"); err != nil {
		t.Fatal(err)
	}
	labels, added, _ := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.None || len(added) != 0 || !issueHasLabel(labels, "bug") {
		t.Fatalf("labels = %v added = %v, want no stage and nothing added", labels, added)
	}
}

// A Ref built from an issue number may name a pull request; the fresh read
// swaps the kind and a park lands on the PR family (085), not 045.
func TestStageFacts_ParkOnPullRequestClampsToPRStage(t *testing.T) {
	srv := newStageFactsServer(t, "kind/feature")
	srv.isPR = true
	c := testClient(t, srv.srv.URL)

	if err := c.StageFacts().Parked(context.Background(), c.IssueStageRef("o/r", 7), NeedsDecisionStageLabel); err != nil {
		t.Fatal(err)
	}
	labels, _, _ := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.NeedsDecision || issueHasLabel(labels, NeedsDecisionStageLabel) {
		t.Fatalf("labels = %v, want %s only", labels, stagelabels.NeedsDecision.Label())
	}
}

func TestEnrichStageFacts(t *testing.T) {
	srv := newStageFactsServer(t, NeedsDecisionStageLabel, issueNeedsDirectionLabel, LikelyDoneLabel, "bug", "hold")
	c := testClient(t, srv.srv.URL)
	ref := c.IssueStageRef("o/r", 7)

	// A plain fact sees everything the issue wears, plus what it reports.
	f, err := c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: ref, Labels: []string{"hive/in-progress", "Stage/045-Needs-Decision"}, Issue: &stageengine.IssueFacts{}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Open || strings.Join(f.Park.Holds, ",") != "hold" || f.Park.DecisionLabel != NeedsDecisionStageLabel || !f.Issue.LikelyDone || f.Author.Login != "dev" {
		t.Fatalf("enriched = %+v", f)
	}
	if !issueHasLabel(f.Labels, "hive/in-progress") || !issueHasLabel(f.Labels, "bug") {
		t.Fatalf("labels = %v, want union", f.Labels)
	}
	if n := strings.Count(strings.ToLower(strings.Join(f.Labels, ",")), NeedsDecisionStageLabel); n != 1 {
		t.Fatalf("labels = %v, want the fact's stage spelling dropped (GitHub's labels are the current stage)", f.Labels)
	}

	// An un-park fact is derived without the decision-park labels its
	// caller removes, but Facts.Labels — the writer's plan hint — keeps
	// the stage/045 the issue wears, so the writer takes it off
	// (projectbluefin/website#926).
	f, err = c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: ref, Park: stageengine.ParkFacts{ApprovedDirection: true}, Issue: &stageengine.IssueFacts{}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Park.DecisionLabel != "" || strings.Join(f.Park.Holds, ",") != "hold" {
		t.Fatalf("un-park enriched = %+v, want no decision park and the hold kept", f)
	}
	if !issueHasLabel(f.Labels, NeedsDecisionStageLabel) {
		t.Fatalf("un-park labels = %v, want the worn %s kept for the writer", f.Labels, NeedsDecisionStageLabel)
	}
	if in := issueInputFrom(f); issueHasLabel(in.Labels, NeedsDecisionStageLabel) || issueHasLabel(in.Labels, issueNeedsDirectionLabel) || !issueHasLabel(in.Labels, "hold") {
		t.Fatalf("un-park derivation labels = %v, want decision-park labels gone and the hold kept", in.Labels)
	}

	// hivecommons/hive#13268: the author kind is filled in from the issue
	// when the fact has none, so an out-of-cycle re-derivation knows
	// whether 015-triage-needed applies; a stated kind is kept.
	if f.Author.Kind != authorkind.Human {
		t.Fatalf("author kind = %q, want human for login dev", f.Author.Kind)
	}
	f, err = c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: ref, Author: stageengine.AuthorFacts{Kind: authorkind.Hive}, Issue: &stageengine.IssueFacts{}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Author.Kind != authorkind.Hive {
		t.Fatalf("author kind = %q, want the fact's own kind kept", f.Author.Kind)
	}

	// hivecommons/hive#15159: a PR fact resolves the author kind too, so a
	// human's CHANGES_REQUESTED on a contributor's clanker PR derives 072
	// returned-to-contributor, not 071 needs-author.
	srv.isPR = true
	srv.body = "Fixes it.\n\n— hive: thatguywob-hive"
	pf, err := c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: c.PRStageRef("o/r", 7), PR: &stageengine.PRFacts{}})
	if err != nil {
		t.Fatal(err)
	}
	if pf.Author.Kind != authorkind.Clanker {
		t.Fatalf("PR author kind = %q, want clanker from the attribution trailer", pf.Author.Kind)
	}
	srv.body = "Hand written."
	pf, err = c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: c.PRStageRef("o/r", 7), PR: &stageengine.PRFacts{}})
	if err != nil {
		t.Fatal(err)
	}
	if pf.Author.Kind != authorkind.Human {
		t.Fatalf("PR author kind = %q, want human for a plain PR", pf.Author.Kind)
	}
	srv.isPR = false

	var nilClient *Client
	if _, err := nilClient.enrichStageFacts(context.Background(), stageengine.Facts{}); err == nil {
		t.Fatal("nil client must fail")
	}
	srv.srv.Close()
	if _, err := c.enrichStageFacts(context.Background(), stageengine.Facts{Ref: ref}); err == nil {
		t.Fatal("a failed read must surface")
	}
}

func TestUnionAndWithoutLabels(t *testing.T) {
	got := unionLabels([]string{"bug", "stage/010-actionable"}, []string{" bug ", "BUG", "x", "", "stage/020-claimed", "Stage/045-Needs-Decision"})
	if strings.Join(got, ",") != "bug,stage/010-actionable,x" {
		t.Fatalf("unionLabels = %v", got)
	}
	if got := withoutLabels([]string{"a", "B", "c"}, []string{"b"}); strings.Join(got, ",") != "a,c" {
		t.Fatalf("withoutLabels = %v", got)
	}
}
