package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// A trusted bot's pin bump on a human-merge path fails
// auto_merge.human_merge_pin_only at its first head (the upstream floating
// tag moved before the bot caught up), so the gate adds needs-owner-merge.
// Every lane skips a PR wearing it before it reaches the gate, so the bot's
// corrected head was never checked again and a person merged a bump the
// policy admits (projectbluefin/bluefin-bling#197, projectbluefin/utah#1439).
// The stage-label pass re-runs the exception on a later head and lifts the
// hive's own label once, with a notice; a person's label, a second hold
// source, a moved head, a write-back refusal and a repository without the
// exception all keep it.

var (
	// pinOnlyLiftHeldHead is the head the gate held; pinOnlyLiftHead is the
	// bot's corrected head; pinOnlyLiftMovedHead is a push during the read;
	// pinOnlyLiftEarlierHead is a head the hive lifted the hold for before.
	pinOnlyLiftHeldHead    = strings.Repeat("1", 40)
	pinOnlyLiftHead        = strings.Repeat("2", 40)
	pinOnlyLiftMovedHead   = strings.Repeat("3", 40)
	pinOnlyLiftEarlierHead = strings.Repeat("4", 40)
)

// pinOnlyLiftNoticeComment is the App's pin-only lift notice for head.
func pinOnlyLiftNoticeComment(head, at string) map[string]any {
	return map[string]any{
		"body":       humanMergePathLiftMarker + head + " -->\n" + humanMergePathPinOnlyLiftNotice(head),
		"user":       map[string]string{"login": testHiveAppBotLogin, "type": "Bot"},
		"created_at": at,
		"updated_at": at,
	}
}

// pinOnlyLiftReheldEvents is the hold history after one lift: the gate's
// hold, the hive's lift at 16:30 and the gate putting the hold back at 17:00
// when the lane re-checked the lifted head, all by the App.
func pinOnlyLiftReheldEvents() []map[string]any {
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	event := func(kind, at string) map[string]any {
		return map[string]any{"event": kind, "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": at}
	}
	return []map[string]any{event("labeled", humanMergePathLiftHoldAt), event("unlabeled", "2026-11-10T16:30:00Z"), event("labeled", "2026-11-10T17:00:00Z")}
}

const pinOnlyLiftWorkflow = ".github/workflows/ci.yml"

// pinOnlyLiftFixture describes the held PR #21 the fixture serves.
type pinOnlyLiftFixture struct {
	// noticeHead is the head stamped on the gate's hold notice (default
	// pinOnlyLiftHeldHead).
	noticeHead string
	// upstreamTag is the commit actions/checkout's tags/v5 points at
	// (default testPinNewSHA, the commit the head pins).
	upstreamTag string
	// events is the hold label history (default: the App's one hold).
	events []map[string]any
	// comments are served before the gate's notice, laterComments after it.
	comments      []map[string]any
	laterComments []map[string]any
	// labels are added to the PR's labels.
	labels []string
	// moveHeadOnRead, when set, makes the Nth GET /pulls/21 after the
	// upstream tag read report pinOnlyLiftMovedHead: 1 is the release's
	// first read of the head, 2 its final re-read just before it removes
	// the label. Every other read reports the observed head, so the rest of
	// the pass sees the PR as before.
	moveHeadOnRead int
}

// pinOnlyLiftReads counts the upstream tag reads and the PR reads after
// them, and records whether the moved head was served (guarded by
// stageAPI.mu).
type pinOnlyLiftReads struct {
	upstream     int
	prReadsAfter int
	moved        bool
}

func pinOnlyLiftStageAPI(t *testing.T, f pinOnlyLiftFixture) (*stageAPI, *pinOnlyLiftReads) {
	t.Helper()
	if f.noticeHead == "" {
		f.noticeHead = pinOnlyLiftHeldHead
	}
	if f.upstreamTag == "" {
		f.upstreamTag = testPinNewSHA
	}
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	if f.events == nil {
		f.events = []map[string]any{{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": humanMergePathLiftHoldAt}}
	}
	notice := map[string]any{
		"id":         11,
		"body":       hgithub.HumanMergePathComment([]string{pinOnlyLiftWorkflow}) + hgithub.HumanMergeHeadMarker(f.noticeHead) + "\n",
		"user":       app,
		"created_at": "2026-11-10T16:00:03Z",
		"updated_at": "2026-11-10T16:00:03Z",
	}
	comments := append(append(append([]map[string]any(nil), f.comments...), notice), f.laterComments...)
	api := newHumanMergePathLiftAPI(t, 21, []string{pinOnlyLiftWorkflow}, comments, f.events)
	labels := []map[string]string{{"name": hgithub.HumanMergePathLabel}}
	for _, l := range f.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	reads := &pinOnlyLiftReads{}
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/21":
			head := pinOnlyLiftHead
			if reads.upstream > 0 {
				reads.prReadsAfter++
				if f.moveHeadOnRead > 0 && reads.prReadsAfter == f.moveHeadOnRead {
					reads.moved = true
					head = pinOnlyLiftMovedHead
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 21, "head": map[string]string{"sha": head}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": "shared-pat-account", "type": "User"}, "changed_files": 1,
				"labels": labels,
			})
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/21/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{testPinFile(pinOnlyLiftWorkflow)})
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/21/commits":
			_ = json.NewEncoder(w).Encode([]map[string]any{testPinCommit(pinOnlyLiftHead, "shared-pat-account", true)})
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/21/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/actions/checkout/git/ref/tags/v5":
			reads.upstream++
			_ = json.NewEncoder(w).Encode(testPinRef("commit", f.upstreamTag))
			return true
		}
		return false
	}
	return api, reads
}

// pinOnlyLiftException admits action-sha bumps on the workflows by the
// fixture's PR author.
func pinOnlyLiftException() config.PinOnlyException {
	return config.PinOnlyException{Enabled: true, Paths: []string{".github/workflows/*.yml"}, Kinds: []string{"action-sha"}, TrustedAuthors: []string{"shared-pat-account"}}
}

// newPinOnlyLiftEngine is the stage engine with human-merge paths on the
// workflows, the pin-only exception when pinOnly, and a captured log.
func newPinOnlyLiftEngine(t *testing.T, api *stageAPI, pinOnly bool) (*Engine, *stagelabels.Store, *bytes.Buffer) {
	t.Helper()
	e, store := newHumanMergePathLiftEngine(t, api)
	if pinOnly {
		e.SetHumanMergePinOnly(pinOnlyFor("acme/widget", pinOnlyLiftException()))
	}
	var logs bytes.Buffer
	e.logger = slog.New(slog.NewTextHandler(&logs, nil))
	return e, store, &logs
}

// recordPinOnlyHeldPR records PR #21 at the bot's corrected head, wearing
// the human-merge-path label the lanes stop on.
func recordPinOnlyHeldPR(e *Engine, extra ...string) {
	labels := append([]string{hgithub.HumanMergePathLabel, "author/human", "role/contributor", "stage/050-review-pending"}, extra...)
	pr := stagePR(21, labels...)
	pr.Head.SHA = gh.Ptr(pinOnlyLiftHead)
	pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, autoMergeReasonHumanMergeRequired, false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
}

func upstreamReads(api *stageAPI, reads *pinOnlyLiftReads) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return reads.upstream
}

func movedHeadServed(api *stageAPI, reads *pinOnlyLiftReads) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	return reads.moved
}

// assertPinOnlyHoldKept fails unless the pass left the label on, removed no
// `hold`, and posted no lift notice.
func assertPinOnlyHoldKept(t *testing.T, api *stageAPI) {
	t.Helper()
	if n, h := removedPathLabel(api), removedHold(api); n != 0 || h != 0 {
		t.Fatalf("removed=%v, want the label kept and no hold touched", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notices = %q, want none", notices)
	}
}

func TestApplyStageLabels_PinOnlyLiftsHoldWhenALaterHeadQualifies(t *testing.T) {
	api, reads := pinOnlyLiftStageAPI(t, pinOnlyLiftFixture{})
	e, store, logs := newPinOnlyLiftEngine(t, api, true)
	ctx := context.Background()

	recordPinOnlyHeldPR(e)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if n := removedPathLabel(api); n != 1 {
		t.Fatalf("hold removed %d times (removed=%v), want once; log:\n%s", n, api.removed, logs.String())
	}
	notices := humanMergePathLiftNotices(api)
	if len(notices) != 1 || !strings.HasPrefix(notices[0], humanMergePathLiftMarker+pinOnlyLiftHead+" -->\n") ||
		!strings.Contains(notices[0], "the head at "+pinOnlyLiftHead+" qualifies for the pin-only exception in `"+humanMergePinOnlyConfigKey+"`") ||
		!strings.Contains(notices[0], "A label a person sets, and any `hold`, is never removed this way.") {
		t.Fatalf("lift notices = %q, want one naming the head and the pin-only exception", notices)
	}
	if upstreamReads(api, reads) == 0 {
		t.Fatal("upstream tag never read: the lift did not run the gate's pin-only check")
	}
	if !strings.Contains(logs.String(), "current head qualifies for the pin-only exception") {
		t.Fatalf("lift not logged:\n%s", logs.String())
	}
	if rec, ok := store.Get(stageKey("acme", "widget", 21)); ok && rec.Hold != nil {
		t.Fatalf("stored hold provenance survived the lift: %+v", rec.Hold)
	}

	// The same head seen held again on a later pass: the notice is not
	// repeated and the hold is not taken off a second time.
	api.reset()
	recordPinOnlyHeldPR(e)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	assertPinOnlyHoldKept(t, api)
}

// The hive lifted the label for an earlier head, the lane's gate re-checked
// that head, failed it (the tag moved again, or an API error) and put the
// label back without a second notice. The bot's next head qualifies: the
// label comes off again, once, for that head, on every hive. Only the gate
// writes needs-owner-merge as the App, so the hive_id rule the `hold` days
// needed (the dashboard hold wrote `hold` as the App too) is gone.
func TestApplyStageLabels_PinOnlyLiftsAgainForAHeadAfterAReHold(t *testing.T) {
	for _, tc := range []struct {
		name      string
		holdLabel string
		lifted    bool
	}{
		{name: "hive with an id", holdLabel: "hive-pause/test", lifted: true},
		{name: "hive without an id", holdLabel: "", lifted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, reads := pinOnlyLiftStageAPI(t, pinOnlyLiftFixture{
				laterComments: []map[string]any{pinOnlyLiftNoticeComment(pinOnlyLiftEarlierHead, "2026-11-10T16:30:00Z")},
				events:        pinOnlyLiftReheldEvents(),
			})
			e, _, logs := newPinOnlyLiftEngine(t, api, true)
			client, ok := e.transport.(*hgithub.Client)
			if !ok {
				t.Fatalf("transport is %T, want *github.Client", e.transport)
			}
			client.SetCanonicalHoldLabel(tc.holdLabel)

			recordPinOnlyHeldPR(e)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !tc.lifted {
				assertPinOnlyHoldKept(t, api)
				if upstreamReads(api, reads) != 0 {
					t.Fatal("upstream tag read for a hold the release cannot lift")
				}
				return
			}
			if n := removedPathLabel(api); n != 1 {
				t.Fatalf("hold removed %d times (removed=%v), want once; log:\n%s", n, api.removed, logs.String())
			}
			notices := humanMergePathLiftNotices(api)
			if len(notices) != 1 || !strings.HasPrefix(notices[0], humanMergePathLiftMarker+pinOnlyLiftHead+" -->\n") {
				t.Fatalf("lift notices = %q, want one for the new head", notices)
			}
			if upstreamReads(api, reads) == 0 {
				t.Fatal("upstream tag never read: the lift did not run the gate's pin-only check")
			}
		})
	}
}

// A head that fails the exception is logged at info once, not on every pass
// while it waits for a person. The queued sweep builds a fresh engine for
// each stage pass (ApplyStageLabels), so the memory must live on the shared
// stage tracker: the second pass here runs on a second engine.
func TestApplyStageLabels_PinOnlyLiftLogsAFailingHeadOnce(t *testing.T) {
	api, reads := pinOnlyLiftStageAPI(t, pinOnlyLiftFixture{upstreamTag: strings.Repeat("c", 40)})
	first, _, logs := newPinOnlyLiftEngine(t, api, true)
	second := New(first.transport, Options{
		Now:               first.now,
		StageLabelPolicy:  func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, StalledRequireQuiet: true} },
		StageTracker:      first.stageTracker,
		StageStore:        first.stageStore,
		CheckSummary:      greenChecks,
		HumanMergePaths:   func(string) []string { return []string{".github/workflows/**"} },
		HumanMergePinOnly: pinOnlyFor("acme/widget", pinOnlyLiftException()),
		Logger:            first.logger,
	})
	ctx := context.Background()
	for _, e := range []*Engine{first, second} {
		api.reset()
		recordPinOnlyHeldPR(e)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		assertPinOnlyHoldKept(t, api)
	}
	if n := upstreamReads(api, reads); n != 2 {
		t.Fatalf("upstream tag read %d times, want once per pass", n)
	}
	if n := strings.Count(logs.String(), "pin-only exception not applied to the current head"); n != 1 {
		t.Fatalf("verdict logged %d times at info, want once; log:\n%s", n, logs.String())
	}
}

func TestApplyStageLabels_PinOnlyLiftKeepsHold(t *testing.T) {
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	appHold := map[string]any{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": humanMergePathLiftHoldAt}
	for _, tc := range []struct {
		name    string
		fixture pinOnlyLiftFixture
		// noPinOnly leaves the repository without a pin-only exception.
		noPinOnly bool
		// holdLabel is the hive's own hold label ("" for a hive without an
		// id), so a re-hold case tests the rule it names.
		holdLabel string
		// wantUpstream is whether the pass reached the upstream tag read.
		wantUpstream bool
		// wantLog is a line the pass must log, when set.
		wantLog string
	}{
		{
			name:         "the later head fails the exception too",
			fixture:      pinOnlyLiftFixture{upstreamTag: strings.Repeat("c", 40)},
			wantUpstream: true,
			wantLog:      "pin-only exception not applied to the current head",
		},
		{
			name:    "the hold was put on this head",
			fixture: pinOnlyLiftFixture{noticeHead: pinOnlyLiftHead},
		},
		{
			name: "a person's hold",
			fixture: pinOnlyLiftFixture{events: []map[string]any{
				appHold,
				pathLabelEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
				pathLabelEvent("labeled", "maintainer", "2026-11-10T17:00:05Z"),
			}},
		},
		{
			name:    "another hold source",
			fixture: pinOnlyLiftFixture{comments: []map[string]any{reporterTrustNoticeComment("2026-11-10T12:00:00Z")}},
			wantLog: "kept for another hold source",
		},
		{
			name:         "the head moved before the release read it",
			fixture:      pinOnlyLiftFixture{moveHeadOnRead: 1},
			wantUpstream: true,
		},
		{
			// Only the final re-read just before the label removal sees
			// the push: dropping that re-read would lift the hold.
			name:         "the head moved just before the label removal",
			fixture:      pinOnlyLiftFixture{moveHeadOnRead: 2},
			wantUpstream: true,
		},
		{
			name:      "a person removed the hold and the gate put it back",
			holdLabel: "hive-pause/test",
			fixture: pinOnlyLiftFixture{events: []map[string]any{
				appHold,
				pathLabelEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
				{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": "2026-11-10T17:30:00Z"},
			}},
			wantLog: "the pin-only release cannot lift it",
		},
		{
			name: "the hive lifted the hold and a person put it back",
			fixture: pinOnlyLiftFixture{
				laterComments: []map[string]any{pinOnlyLiftNoticeComment(pinOnlyLiftEarlierHead, "2026-11-10T16:30:00Z")},
				events: []map[string]any{
					appHold,
					{"event": "unlabeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": "2026-11-10T16:30:00Z"},
					pathLabelEvent("labeled", "maintainer", "2026-11-10T17:00:00Z"),
				},
			},
		},
		{
			name:      "lifted at this head before and the gate held it again",
			holdLabel: "hive-pause/test",
			fixture: pinOnlyLiftFixture{
				laterComments: []map[string]any{pinOnlyLiftNoticeComment(pinOnlyLiftHead, "2026-11-10T16:30:00Z")},
				events:        pinOnlyLiftReheldEvents(),
			},
		},
		{
			name:      "no pin-only exception on the repository",
			noPinOnly: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, reads := pinOnlyLiftStageAPI(t, tc.fixture)
			e, _, logs := newPinOnlyLiftEngine(t, api, !tc.noPinOnly)
			if tc.holdLabel != "" {
				client, ok := e.transport.(*hgithub.Client)
				if !ok {
					t.Fatalf("transport is %T, want *github.Client", e.transport)
				}
				client.SetCanonicalHoldLabel(tc.holdLabel)
			}

			recordPinOnlyHeldPR(e)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertPinOnlyHoldKept(t, api)
			if got := upstreamReads(api, reads) > 0; got != tc.wantUpstream {
				t.Fatalf("upstream tag read = %v, want %v; log:\n%s", got, tc.wantUpstream, logs.String())
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("log lacks %q:\n%s", tc.wantLog, logs.String())
			}
			if tc.fixture.moveHeadOnRead > 0 && !movedHeadServed(api, reads) {
				t.Fatalf("GET /pulls/21 #%d after the pin-only check never happened", tc.fixture.moveHeadOnRead)
			}
		})
	}
}

// The lift is an unsolicited write on a PR the hive did not open: the
// write-back gate's human_merge_path_release policy decides it, and a
// refusal is logged.
func TestApplyStageLabels_PinOnlyLiftHonoursTheWritebackGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		lifted bool
	}{
		{name: "not opted in", lifted: false},
		{name: "opted in", labels: []string{writeback.DefaultOptInLabel}, lifted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _ := pinOnlyLiftStageAPI(t, pinOnlyLiftFixture{labels: tc.labels})
			e, _, logs := newPinOnlyLiftEngine(t, api, true)
			client, ok := e.transport.(*hgithub.Client)
			if !ok {
				t.Fatalf("transport is %T, want *github.Client", e.transport)
			}
			client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, ""))

			recordPinOnlyHeldPR(e, tc.labels...)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := removedPathLabel(api) == 1 && len(humanMergePathLiftNotices(api)) == 1; got != tc.lifted {
				t.Fatalf("lifted = %v (removed=%v), want %v; log:\n%s", got, api.removed, tc.lifted, logs.String())
			}
			if refused := strings.Contains(logs.String(), "lift refused by the write-back gate"); refused == tc.lifted {
				t.Fatalf("write-back refusal logged = %v, want %v; log:\n%s", refused, !tc.lifted, logs.String())
			}
		})
	}
}
