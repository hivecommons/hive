package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15020: the hive held its own approved PR for the trust of
// an issue reporter, and the operator could not tell who held it or why.
// The stage-label pass now reads the hold's provenance (label event actor +
// the gate's notice), records it on the row, and lifts a reporter-trust hold
// the hive put on a PR it authored itself. Holds a person set stay.

func reporterTrustFinding15020() hgithub.ReporterTrust {
	return hgithub.ReporterTrust{OwnsNeedsHuman: true, Held: true, Issue: 15017, Repo: "acme/widget", Reporter: "Karibhou", Association: "CONTRIBUTOR", Reason: "issue acme/widget#15017 reporter @Karibhou (CONTRIBUTOR) is not trusted"}
}

func reporterTrustNoticeComment(at string) map[string]any {
	return map[string]any{
		"body":       hgithub.ReporterTrustNoticeBody(reporterTrustFinding15020()),
		"user":       map[string]string{"login": testHiveAppBotLogin},
		"created_at": at,
	}
}

func holdEvent(event, actor, at string) map[string]any {
	return map[string]any{
		"event":      event,
		"label":      map[string]string{"name": "hold"},
		"actor":      map[string]string{"login": actor},
		"created_at": at,
	}
}

func TestHoldProvenance(t *testing.T) {
	const labelAt = "2026-10-20T17:14:14Z"
	for _, tc := range []struct {
		name     string
		comments []map[string]any
		events   []map[string]any
		want     *stagelabels.HoldProvenance
		notice   bool
	}{
		{
			name:     "hive label plus notice is a reporter-trust hold",
			comments: []map[string]any{reporterTrustNoticeComment("2026-10-20T17:14:18Z")},
			events:   []map[string]any{holdEvent("labeled", testHiveAppBotLogin, labelAt)},
			want:     &stagelabels.HoldProvenance{By: "hive", Source: "reporter-trust", Reason: "reporter trust (issue #15017 by @Karibhou, contributor)", Issue: 15017, Reporter: "Karibhou", Association: "CONTRIBUTOR", Since: time.Date(2026, 10, 20, 17, 14, 14, 0, time.UTC)},
			notice:   true,
		},
		{
			name:   "hive label without a notice is only attributed",
			events: []map[string]any{holdEvent("labeled", testHiveAppBotLogin, labelAt)},
			want:   &stagelabels.HoldProvenance{By: "hive", Source: "label", Since: time.Date(2026, 10, 20, 17, 14, 14, 0, time.UTC)},
		},
		{
			name:     "a stale notice from an earlier hold does not explain a fresh one",
			comments: []map[string]any{reporterTrustNoticeComment("2026-10-19T09:00:00Z")},
			events:   []map[string]any{holdEvent("labeled", testHiveAppBotLogin, "2026-10-19T08:59:58Z"), holdEvent("unlabeled", "clubanderson", "2026-10-19T10:00:00Z"), holdEvent("labeled", testHiveAppBotLogin, labelAt)},
			want:     &stagelabels.HoldProvenance{By: "hive", Source: "label", Since: time.Date(2026, 10, 20, 17, 14, 14, 0, time.UTC)},
			notice:   true,
		},
		{
			name:     "a person's hold is theirs even with a hive notice on the thread",
			comments: []map[string]any{reporterTrustNoticeComment("2026-10-20T17:14:18Z")},
			events:   []map[string]any{holdEvent("labeled", testHiveAppBotLogin, labelAt), holdEvent("unlabeled", "clubanderson", "2026-10-20T18:00:00Z"), holdEvent("labeled", "ClubAnderson", "2026-10-20T18:00:05Z")},
			want:     &stagelabels.HoldProvenance{By: "clubanderson", Source: "label", Since: time.Date(2026, 10, 20, 18, 0, 5, 0, time.UTC)},
			notice:   true,
		},
		{
			name:     "newest event is an unlabel: nothing to attribute",
			comments: []map[string]any{reporterTrustNoticeComment("2026-10-20T17:14:18Z")},
			events:   []map[string]any{holdEvent("labeled", testHiveAppBotLogin, labelAt), holdEvent("unlabeled", "clubanderson", "2026-10-20T18:00:00Z")},
			notice:   true,
		},
		{
			name:     "no events readable: the notice alone attributes it",
			comments: []map[string]any{reporterTrustNoticeComment("2026-10-20T17:14:18Z")},
			want:     &stagelabels.HoldProvenance{By: "hive", Source: "reporter-trust", Reason: "reporter trust (issue #15017 by @Karibhou, contributor)", Issue: 15017, Reporter: "Karibhou", Association: "CONTRIBUTOR", Since: time.Date(2026, 10, 20, 17, 14, 18, 0, time.UTC)},
			notice:   true,
		},
		{name: "nothing at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/widget/issues/15020/comments":
					if tc.comments == nil {
						tc.comments = []map[string]any{}
					}
					_ = json.NewEncoder(w).Encode(tc.comments)
				case "/repos/acme/widget/issues/15020/events":
					if tc.events == nil {
						tc.events = []map[string]any{}
					}
					_ = json.NewEncoder(w).Encode(tc.events)
				default:
					http.NotFound(w, r)
				}
			})
			got, notice, err := c.holdProvenance(context.Background(), "acme", "widget", 15020)
			if err != nil {
				t.Fatal(err)
			}
			if (notice != nil) != tc.notice {
				t.Fatalf("notice = %+v, want present=%v", notice, tc.notice)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("provenance = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestHoldProvenanceErrors(t *testing.T) {
	var nilEngine *Engine
	if prov, notice, err := nilEngine.holdProvenance(context.Background(), "acme", "widget", 1); prov != nil || notice != nil || err != nil {
		t.Fatalf("nil engine = (%v, %v, %v), want no-op", prov, notice, err)
	}
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	if _, _, err := c.holdProvenance(context.Background(), "acme", "widget", 1); err == nil || !strings.Contains(err.Error(), "listing comments") {
		t.Fatalf("err = %v, want listing-comments failure", err)
	}
	c, _ = releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode([]map[string]any{reporterTrustNoticeComment("2026-10-20T17:14:18Z")})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, notice, err := c.holdProvenance(context.Background(), "acme", "widget", 1); err == nil || !strings.Contains(err.Error(), "listing issue events") || notice == nil {
		t.Fatalf("(%v, %v), want listing-issue-events failure with the notice still returned", notice, err)
	}
}

func TestReleaseSelfImposedReporterTrustHoldGuards(t *testing.T) {
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 15020, Labels: []string{"hold", "needs-human"}}
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected %s %s", r.Method, r.URL.Path) })
	for name, prov := range map[string]*stagelabels.HoldProvenance{
		"nil":            nil,
		"operator hold":  {By: "clubanderson", Source: "label"},
		"hive, no gate":  {By: "hive", Source: "label"},
		"hive, other":    {By: "hive", Source: "self-authorization"},
		"gate, operator": {By: "pat", Source: "reporter-trust"},
	} {
		if released, err := c.releaseSelfImposedReporterTrustHold(context.Background(), obs, prov, nil); released || err != nil {
			t.Fatalf("%s: (%v, %v), want untouched", name, released, err)
		}
	}
}

func TestReleaseSelfImposedReporterTrustHoldErrors(t *testing.T) {
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 15020, Labels: []string{"hold", "needs-human"}}
	prov := &stagelabels.HoldProvenance{By: "hive", Source: "reporter-trust", Reason: "reporter trust (issue #15017 by @Karibhou, contributor)"}
	c, _ := releaseEngine(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	if released, err := c.releaseSelfImposedReporterTrustHold(context.Background(), obs, prov, nil); released || err == nil || !strings.Contains(err.Error(), "removing hold label") {
		t.Fatalf("(%v, %v), want removing-hold-label failure", released, err)
	}
	c, _ = releaseEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, "gone", http.StatusNotFound) // already removed: tolerated
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if released, err := c.releaseSelfImposedReporterTrustHold(context.Background(), obs, prov, nil); released || err == nil || !strings.Contains(err.Error(), "posting release comment") {
		t.Fatalf("(%v, %v), want posting-release-comment failure after tolerated 404", released, err)
	}
}

// The live case end to end through ApplyStageLabels: a hive-authored PR
// carrying the hive's own reporter-trust hold has `hold` and the gate's
// `needs-human` released on the pass, with a comment saying why; the row is
// skipped for this pass (re-staged next pass without the hold) and its
// stored provenance cleared.
func TestApplyStageLabels_ReleasesSelfImposedReporterTrustHold(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	api.issueComments = map[int][]map[string]any{15020: {reporterTrustNoticeComment("2026-10-20T17:14:18Z")}}
	api.issueEvents = map[int][]map[string]any{15020: {holdEvent("labeled", testHiveAppBotLogin, "2026-10-20T17:14:14Z")}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()

	pr := stagePR(15020, "hold", "needs-human", "author/hive", "role/trusted", "stage/085-needs-decision")
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin), Type: gh.Ptr("Bot")}
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "label:hold", false, nil)
	res, err := e.ApplyStageLabels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Comments != 1 {
		t.Fatalf("result = %+v, want the row skipped after the release with one comment", res)
	}
	if strings.Join(api.removed, ",") != "hold,needs-human" || len(api.added) != 0 {
		t.Fatalf("removed=%v added=%v, want hold and needs-human released and nothing staged yet", api.removed, api.added)
	}
	if len(api.comments) != 1 || !IsReporterTrustReleaseComment(api.comments[0]) || !strings.Contains(api.comments[0], "reporter trust (issue #15017 by @Karibhou, contributor)") || !strings.Contains(api.comments[0], "The hive is this PR's author") {
		t.Fatalf("release comment = %q", api.comments)
	}
	if rec, ok := store.Get(stageKey("acme", "widget", 15020)); ok && rec.Hold != nil {
		t.Fatalf("stored hold provenance survived the release: %+v", rec.Hold)
	}

	// Next pass, the hold gone: the PR stages normally and nothing is
	// released again (the events now end in the hive's own unlabel).
	api.reset()
	api.issueEvents[15020] = append(api.issueEvents[15020], holdEvent("unlabeled", testHiveAppBotLogin, "2026-10-20T17:40:01Z"))
	pr = stagePR(15020, "author/hive", "role/trusted", "stage/085-needs-decision")
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin), Type: gh.Ptr("Bot")}
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "queued", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	for _, body := range api.comments {
		if IsReporterTrustReleaseComment(body) {
			t.Fatalf("a second release comment was posted: %q", body)
		}
	}
	for _, removed := range api.removed {
		if removed == "hold" || removed == "needs-human" {
			t.Fatalf("removed %q again on an unheld PR: %v", removed, api.removed)
		}
	}
}

// A hold a person set on a hive-authored PR is theirs: the pass records who
// and leaves it, and the 085 comment names them.
func TestApplyStageLabels_OperatorHoldOnHivePRIsKept(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	api.issueComments = map[int][]map[string]any{15020: {reporterTrustNoticeComment("2026-10-20T17:14:18Z")}}
	api.issueEvents = map[int][]map[string]any{15020: {
		holdEvent("labeled", testHiveAppBotLogin, "2026-10-20T17:14:14Z"),
		holdEvent("unlabeled", "clubanderson", "2026-10-20T18:00:00Z"),
		holdEvent("labeled", "clubanderson", "2026-10-20T18:00:05Z"),
	}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 18, 30, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()

	pr := stagePR(15020, "hold", "author/hive", "role/trusted", "stage/060-reviewed")
	pr.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin), Type: gh.Ptr("Bot")}
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", pr, "label:hold", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	for _, removed := range api.removed {
		if removed == "hold" {
			t.Fatalf("a person's hold was released: %v", api.removed)
		}
	}
	rec, ok := store.Get(stageKey("acme", "widget", 15020))
	if !ok || rec.Hold == nil || rec.Hold.By != "clubanderson" || rec.Hold.Source != stagelabels.HoldSourceLabel {
		t.Fatalf("record hold = %+v (ok=%v), want attributed to clubanderson", rec.Hold, ok)
	}
	var decision string
	for _, body := range api.comments {
		if strings.HasPrefix(body, stagelabels.DecisionCommentMarker) {
			decision = body
		}
	}
	if !strings.Contains(decision, "the `hold` label was set by @clubanderson") {
		t.Fatalf("085 comment does not name who held it:\n%s", decision)
	}
}

// A contributor's PR the hive held for reporter trust keeps its hold (the
// gate still applies to PRs the hive did not author); the row carries the
// provenance and the 085 comment says the hive set it, why, and how to
// release or keep it.
func TestApplyStageLabels_ReporterTrustHoldOnContributorPRIsNamed(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	api.issueComments = map[int][]map[string]any{21: {reporterTrustNoticeComment("2026-10-20T17:14:18Z")}}
	api.issueEvents = map[int][]map[string]any{21: {holdEvent("labeled", testHiveAppBotLogin, "2026-10-20T17:14:14Z")}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	ctx := context.Background()

	pr := stagePR(21, "hold", "needs-human", "author/human", "role/contributor", "stage/050-review-pending")
	pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "excluded-label:hold", false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	for _, removed := range api.removed {
		if removed == "hold" || removed == "needs-human" {
			t.Fatalf("the gate's hold on a PR the hive did not author was released: %v", api.removed)
		}
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || !rec.Hold.SelfImposed() || !rec.Hold.ReporterTrust() || rec.Hold.Issue != 15017 || rec.Hold.Reporter != "Karibhou" {
		t.Fatalf("record hold = %+v (ok=%v), want the hive's reporter-trust provenance", rec.Hold, ok)
	}
	var decision string
	for _, body := range api.comments {
		if strings.HasPrefix(body, stagelabels.DecisionCommentMarker) {
			decision = body
		}
	}
	for _, want := range []string{
		"the `hold` label was set by the hive itself: reporter trust (issue #15017 by @Karibhou, contributor)",
		"The hive set `hold` itself for reporter trust (issue #15017 by @Karibhou, contributor). **Release** it",
		"or **keep** it",
	} {
		if !strings.Contains(decision, want) {
			t.Fatalf("085 comment lacks %q:\n%s", want, decision)
		}
	}
	if strings.Contains(decision, "the `hold` label is set") {
		t.Fatalf("085 comment still prints the bare hold line:\n%s", decision)
	}
}
