package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// ── #6902: one ladder, one vocabulary, asserted on both surfaces ──────────────
//
// The acceptance criterion this file exists for: for EVERY exclusion cause, one
// table row asserts all three of
//
//	(a) selectTask does not assign the item,
//	(b) it is absent from the Ready queue, and
//	(c) it appears in Withheld with the expected reason.
//
// (a) and (b) are the invariant — this feature explains admission, it must
// never change it — and (c) is the feature. Asserting them together is what
// makes the two loops unable to diverge silently: a gate added to selectTask
// but not to admissionQueueSnapshot fails (b) or (c); a reason wired into the
// diagnostics but not actually enforced fails (a).
//
// #700 is the untouched control in every row. It must stay offerable and
// selectable no matter what withholds #601, so a single withheld candidate can
// never be mistaken for a stalled queue.

// withheldHub builds the two-issue fixture the dependency tests use, then lets
// a case decorate #601's issue map with whatever the gate under test reads
// (labels, assignees, is_tracker, updated_at).
func withheldHub(t *testing.T, decorate func(issue map[string]any)) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	issue601 := map[string]any{
		"number": float64(601),
		"title":  "dependent work",
		"url":    "https://github.com/projectbluefin/dakota/issues/601",
		"author": "someone",
	}
	if decorate != nil {
		decorate(issue601)
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "dakota",
			Full: "projectbluefin/dakota",
			ActionableIssues: []any{
				issue601,
				map[string]any{
					"number": float64(700),
					"title":  "unrelated ready work",
					"url":    "https://github.com/projectbluefin/dakota/issues/700",
					"author": "someone",
				},
			},
		}},
	}
	s.statusMu.Unlock()
	return hub, s
}

const withheldKey601 = "projectbluefin/dakota#601"

// findWithheld returns the withheld row for a key, or false when the ladder did
// not record one.
func findWithheld(items []AdmissionWithheldItem, key string) (AdmissionWithheldItem, bool) {
	for _, item := range items {
		if item.Key == key {
			return item, true
		}
	}
	return AdmissionWithheldItem{}, false
}

type withheldCase struct {
	name   string
	reason string
	// decorate mutates #601's issue map before the hub is built, for gates that
	// read the issue itself.
	decorate func(issue map[string]any)
	// arrange puts the hub/server into the state the gate refuses on.
	arrange func(t *testing.T, hub *ContributeWSHub, s *Server)
	// evidence asserts the reason-specific fields, when the gate has any.
	evidence func(t *testing.T, item AdmissionWithheldItem)
}

func withheldCases() []withheldCase {
	return []withheldCase{
		{
			name:     "tracker",
			reason:   withheldReasonTracker,
			decorate: func(issue map[string]any) { issue["is_tracker"] = true },
		},
		{
			name:   "completion cooldown",
			reason: withheldReasonCooldown,
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				hub.completedMu.Lock()
				hub.completedTasks[withheldKey601] = time.Now()
				hub.completedMu.Unlock()
			},
			evidence: func(t *testing.T, item AdmissionWithheldItem) {
				if item.CooldownUntil == "" {
					t.Error("a cooldown refusal must carry the expiry the gate enforces")
				}
				if _, err := time.Parse(time.RFC3339, item.CooldownUntil); err != nil {
					t.Errorf("cooldown_until %q is not RFC3339: %v", item.CooldownUntil, err)
				}
			},
		},
		{
			name:   "failure cooldown",
			reason: withheldReasonFailureCooldown,
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				hub.completedMu.Lock()
				hub.failedTasks[withheldKey601] = time.Now()
				hub.completedMu.Unlock()
			},
			evidence: func(t *testing.T, item AdmissionWithheldItem) {
				if item.CooldownUntil == "" {
					t.Error("a failure-cooldown refusal must carry its expiry")
				}
			},
		},
		{
			name:   "no_work_needed verdict",
			reason: withheldReasonNoWorkNeeded,
			// The verdict is voided by activity NEWER than the verdict, and
			// fails OPEN when the issue's updated_at is unknown — so the gate
			// only bites with a timestamp that predates the record.
			decorate: func(issue map[string]any) {
				issue["updated_at"] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			},
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				hub.completedMu.Lock()
				hub.noWorkVerdicts[withheldKey601] = noWorkVerdictRecord{RecordedAt: time.Now()}
				hub.completedMu.Unlock()
			},
		},
		{
			name:   "in flight",
			reason: withheldReasonInFlight,
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				hub.mu.Lock()
				hub.connections["busy"] = &ContributorConnection{
					profile:  &ContributorProfile{GitHubUsername: "busy", ContributorID: "c-busy"},
					lastPong: time.Now(),
					currentTask: &WSTaskAssign{
						TaskID: "ct-busy",
						Kind:   "issue",
						Repo:   "projectbluefin/dakota",
						Number: 601,
					},
				}
				hub.mu.Unlock()
			},
		},
		{
			name:   "open PR claim",
			reason: contributorAdmissionReasonOpenPRClaim,
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
					if number == 601 {
						return ghpkg.IssueClaim{
							PRNumber: 999,
							PRURL:    "https://github.com/projectbluefin/dakota/pull/999",
							PRAuthor: "claimer",
						}, true
					}
					return ghpkg.IssueClaim{}, false
				}
			},
			evidence: func(t *testing.T, item AdmissionWithheldItem) {
				if item.ClaimURL != "https://github.com/projectbluefin/dakota/pull/999" {
					t.Errorf("claim_url = %q, want the claiming PR's URL", item.ClaimURL)
				}
				if item.ClaimAuthor != "claimer" {
					t.Errorf("claim_author = %q, want claimer", item.ClaimAuthor)
				}
			},
		},
		{
			name:     "workflow blocked label",
			reason:   contributorAdmissionReasonWorkflowBlocked,
			decorate: func(issue map[string]any) { issue["labels"] = []any{"blocked"} },
		},
		{
			name:     "contributor title filter",
			reason:   withheldReasonContributorFilter,
			decorate: func(issue map[string]any) { issue["title"] = "WIP do not touch" },
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				s.deps.Config.Hub.ContributeDenyTitles = []string{"WIP"}
			},
			evidence: func(t *testing.T, item AdmissionWithheldItem) {
				if item.Filter != "title" {
					t.Errorf("filter = %q, want title — the operator has to know WHICH filter matched", item.Filter)
				}
			},
		},
		{
			name:     "assigned to another contributor",
			reason:   withheldReasonAssignedToOther,
			decorate: func(issue map[string]any) { issue["assignees"] = []any{"someone-else"} },
			arrange: func(t *testing.T, hub *ContributeWSHub, s *Server) {
				s.deps.Config.Hub.ContributeSkipAssignedToOthers = true
			},
			evidence: func(t *testing.T, item AdmissionWithheldItem) {
				if len(item.Assignees) != 1 || item.Assignees[0] != "someone-else" {
					t.Errorf("assignees = %v, want [someone-else]", item.Assignees)
				}
			},
		},
	}
}

// TestWithheld_EveryExclusionCauseIsExplainedAndStillEnforced is the shared
// ladder regression: each cause is asserted on the assignment path AND both
// queue surfaces in one row, so the two loops cannot drift apart unnoticed.
func TestWithheld_EveryExclusionCauseIsExplainedAndStillEnforced(t *testing.T) {
	for _, tc := range withheldCases() {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := withheldHub(t, tc.decorate)
			if tc.arrange != nil {
				tc.arrange(t, hub, s)
			}

			// (b) absent from Ready — and the control is untouched.
			assertQueue(t, hub, 700)

			// (a) still refused at assignment. selectTask is the authority; if
			// it handed out #601 the explanation would be a lie.
			assertAssigns(t, hub, 700)

			// (c) explained, with the expected reason.
			snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
			item, ok := findWithheld(snap.withheld, withheldKey601)
			if !ok {
				t.Fatalf("#601 is withheld but unexplained; collected %v", snap.withheld)
			}
			if item.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", item.Reason, tc.reason)
			}
			if item.Detail == "" {
				t.Error("every withheld row needs operator-facing prose, not just a code")
			}
			if item.Title == "" || item.URL == "" {
				t.Error("a withheld row must carry enough metadata to be a link")
			}
			// The control is never explained: it is not withheld.
			if _, present := findWithheld(snap.withheld, "projectbluefin/dakota#700"); present {
				t.Error("the offerable control must not appear in the withheld list")
			}
			if tc.evidence != nil {
				tc.evidence(t, item)
			}
		})
	}
}

// A disabled repository refuses every candidate at once, above the per-issue
// ladder, so it is the one cause whose control also disappears — asserted on
// its own rather than bent into the table.
func TestWithheld_DisabledRepositoryExplainsEveryCandidate(t *testing.T) {
	hub, s := withheldHub(t, nil)
	s.deps.Config.Hub.DisabledRepos = []string{"projectbluefin/dakota"}

	assertQueue(t, hub)
	if msg := hub.selectTask(depTestConn()); msg != nil && msg.Type == "task_assign" {
		t.Fatalf("a disabled repository must assign nothing, got #%d", msg.Number)
	}

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	if len(snap.withheld) != 2 {
		t.Fatalf("want both candidates explained, got %d: %v", len(snap.withheld), snap.withheld)
	}
	for _, item := range snap.withheld {
		if item.Reason != withheldReasonDisabledRepo {
			t.Errorf("%s reason = %q, want %q", item.Key, item.Reason, withheldReasonDisabledRepo)
		}
	}
}

// Operator holds stay their OWN group. They are the operator's decision, they
// keep the existing on-hold treatment in the queue, and conflating them with
// automatic withholding would tell an operator the hive refused something they
// refused themselves.
func TestWithheld_OperatorHoldIsNotWithheld(t *testing.T) {
	hub, s := withheldHub(t, nil)
	s.deps.Config.Hub.ContributeQueueHold = []string{withheldKey601}
	s.deps.Config.Hub.ContributeQueueHoldReasons = map[string]string{withheldKey601: "waiting on design review"}

	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	if _, present := findWithheld(snap.withheld, withheldKey601); present {
		t.Fatal("a manually held issue must not be reported as automatically withheld")
	}
	var held *ReadyQueueItem
	for i := range snap.queue {
		if snap.queue[i].Key == withheldKey601 {
			held = &snap.queue[i]
		}
	}
	if held == nil || !held.Held {
		t.Fatal("a held issue must still be surfaced in the queue with Held set")
	}
	if held.HeldReason != "waiting on design review" {
		t.Errorf("held reason = %q, want the operator's note", held.HeldReason)
	}
}

// Displaying a withheld row must not make it assignable. This is the acceptance
// criterion that says the feature explains admission rather than widening it.
func TestWithheld_CollectingDoesNotChangeAdmission(t *testing.T) {
	for _, tc := range withheldCases() {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := withheldHub(t, tc.decorate)
			if tc.arrange != nil {
				tc.arrange(t, hub, s)
			}
			// The queue a caller asking for explanations gets must be the same
			// queue a caller not asking for them gets.
			plain := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldNone)
			full := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
			if len(plain.queue) != len(full.queue) {
				t.Fatalf("queue length changed with diagnostics on: %d vs %d", len(plain.queue), len(full.queue))
			}
			for i := range plain.queue {
				if plain.queue[i].Key != full.queue[i].Key {
					t.Fatalf("queue order changed with diagnostics on at %d: %q vs %q",
						i, plain.queue[i].Key, full.queue[i].Key)
				}
			}
			if plain.offerableTotal != full.offerableTotal {
				t.Fatalf("offerable total changed with diagnostics on: %d vs %d",
					plain.offerableTotal, full.offerableTotal)
			}
			if plain.withheld != nil {
				t.Fatalf("withheldNone must collect nothing, got %v", plain.withheld)
			}
		})
	}
}

// The withheld list is bounded exactly like the queue, so a pathological
// population cannot blow out the payload.
func TestWithheld_IsBoundedByTheQueueLimit(t *testing.T) {
	hub, s := covK2Hub(t)
	issues := make([]any, 0, 20)
	for i := 1; i <= 20; i++ {
		issues = append(issues, map[string]any{
			"number":     float64(i),
			"title":      "tracker",
			"url":        "https://example.test/i",
			"is_tracker": true,
		})
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "r", Full: "acme/r", ActionableIssues: issues}}}
	s.statusMu.Unlock()

	snap := hub.admissionQueueSnapshot(5, withheldAll)
	if len(snap.withheld) != 5 {
		t.Fatalf("withheld = %d rows, want the limit (5)", len(snap.withheld))
	}
}

// The #4246 shadow surface keeps its exact membership: convergence judgments
// only. #6902 broadened the vocabulary the ladder RECORDS, and that must not
// leak into a payload whose contract predates it.
func TestWithheld_ConvergenceScopeExcludesTheNewReasons(t *testing.T) {
	hub, s := withheldHub(t, func(issue map[string]any) { issue["is_tracker"] = true })
	diagShadow(t, s)

	shadow := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldConvergence)
	if _, present := findWithheld(shadow.withheld, withheldKey601); present {
		t.Fatalf("a tracker refusal is not a convergence diagnostic: %v", shadow.withheld)
	}
	full := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	if _, present := findWithheld(full.withheld, withheldKey601); !present {
		t.Fatal("the operator surface must still explain the tracker refusal")
	}
}

// Default GET /api/contribute/queue is unchanged for existing clients; the
// full explanation set is opt-in.
//
// Pinned to mode=off explicitly since #7260 moved the DEFAULT to shadow: this
// test is about the ?withheld=1 opt-in, not about which mode a fresh hive
// starts in, and under shadow the payload legitimately carries convergence
// diagnostics.
func TestWithheld_QueueEndpointOptIn(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "off")
	hub, s := withheldHub(t, func(issue map[string]any) { issue["is_tracker"] = true })
	s.contributeHub = hub

	decode := func(t *testing.T, target string) map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleContributeQueue(rec, httptest.NewRequest(http.MethodGet, target, nil))
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad queue JSON: %v", err)
		}
		return resp
	}

	base := decode(t, "/api/contribute/queue")
	if _, ok := base["withheld"]; ok {
		t.Fatal("the default payload must not gain a withheld field")
	}
	if _, ok := base["withheld_total"]; ok {
		t.Fatal("the default payload must not gain withheld_total")
	}

	opted := decode(t, "/api/contribute/queue?withheld=1")
	raw, ok := opted["withheld"]
	if !ok {
		t.Fatal("?withheld=1 must return the withheld collection")
	}
	var items []AdmissionWithheldItem
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("withheld did not decode: %v", err)
	}
	item, found := findWithheld(items, withheldKey601)
	if !found {
		t.Fatalf("?withheld=1 did not explain #601: %v", items)
	}
	if item.Reason != withheldReasonTracker {
		t.Errorf("reason = %q, want %q", item.Reason, withheldReasonTracker)
	}
	// The queue itself is identical either way — opting into the explanation
	// must not change what is on offer.
	if string(base["queue"]) != string(opted["queue"]) {
		t.Error("?withheld=1 changed the queue payload; it must only add an explanation")
	}
}

// Every reason the ladder can emit has operator-facing prose. A code with no
// label renders as a bare identifier in the UI, which is the failure this
// vocabulary exists to prevent.
func TestWithheld_EveryReasonHasALabel(t *testing.T) {
	reasons := []string{
		contributorAdmissionReasonOpenPRClaim,
		contributorAdmissionReasonWorkflowBlocked,
		contributorAdmissionReasonDependencyBlocked,
		contributorAdmissionReasonDependencyUnknown,
		withheldReasonDisabledRepo,
		withheldReasonTracker,
		withheldReasonCooldown,
		withheldReasonNoWorkNeeded,
		withheldReasonFailureCooldown,
		withheldReasonInFlight,
		withheldReasonContributorFilter,
		withheldReasonAssignedToOther,
	}
	for _, reason := range reasons {
		if label, ok := withheldReasonLabels[reason]; !ok || label == "" {
			t.Errorf("reason %q has no operator-facing label", reason)
		}
	}
}

// rejectingContributorFilter must agree with the boolean expression it replaced:
// same outcome, plus the name of the filter that said no.
func TestRejectingContributorFilter(t *testing.T) {
	hub := config.HubConfig{
		ContributeDenyTitles:  []string{"WIP"},
		ContributeDenyAuthors: []string{"botty"},
		ContributeDenyLabels:  []string{"wontfix"},
	}
	cases := []struct {
		name   string
		title  string
		author string
		labels []string
		want   string
	}{
		{name: "all pass", title: "fix thing", author: "human", labels: []string{"bug"}, want: ""},
		{name: "title", title: "WIP thing", author: "human", want: "title"},
		{name: "author", title: "fix thing", author: "botty", want: "author"},
		{name: "label", title: "fix thing", author: "human", labels: []string{"wontfix"}, want: "label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rejectingContributorFilter(hub, tc.title, tc.author, tc.labels); got != tc.want {
				t.Fatalf("rejectingContributorFilter = %q, want %q", got, tc.want)
			}
		})
	}
}
