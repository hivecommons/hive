package gfi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// soak returns a candidate shaped like hivecommons/hive#13267: a
// maintainer's post-hoc soak checklist, `help wanted`, no structured marker.
func soak() Candidate {
	c := good()
	c.Number = 13267
	c.Title = "🌱 Post-hoc soak evidence for stable promotion of 7d1b5a3ae"
	c.Body = "Tracking post-hoc soak and smoke evidence for the `promote-stable.yml` emergency promotion of candidate sha256:0cd6 over the previous stable.\n\n" +
		"To close:\n- [ ] 24h after promotion: hub dashboard shows hosted stable hives healthy on the new digest\n- [ ] Spot-check one stable spoke: `/api/version` matches `7d1b5a3ae`\n- [ ] Record the evidence here\n"
	c.Labels = []string{"help wanted"}
	return c
}

func TestOpsTrackingReason(t *testing.T) {
	cases := []struct {
		name        string
		title, body string
		want        string
		ok          bool
	}{
		{"marker", "Fix thing", "Do it.\n\n<!-- hive:ops-tracking -->\n", "marker", true},
		{"marker spaced", "Fix thing", "<!--  hive:ops-tracking -->", "marker", true},
		{"24h after promotion", "", "- [ ] 24h after promotion: hub dashboard healthy", "soak-checklist", true},
		{"hours after rollout", "", "Check 48 hours after the rollout that nothing restarts", "soak-checklist", true},
		{"post-hoc soak title", "🌱 Post-hoc soak evidence for 7d1b5a3ae", "short", "soak-checklist", true},
		{"promote-stable vocabulary", "", "the promote-stable run summary", "soak-checklist", true},
		{"plain bug", "Fix off-by-one in pkg/paginate", "The last page is dropped after the first call.", "", false},
		{"after without hours", "Retry after promotion", "retry the request after promotion of the token", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OpsTrackingReason(tc.title, tc.body)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("OpsTrackingReason = %q,%v want %q,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRecheckAfterMarkerRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 21, 21, 20, 0, 0, time.FixedZone("x", -4*3600))
	m := RecheckAfterMarker(at)
	if m != "<!-- hive:recheck-after 2026-10-22T01:20:00Z -->" {
		t.Fatalf("marker %q", m)
	}
	got, ok := ParseRecheckAfter("body\n" + m + "\nmore")
	if !ok || !got.Equal(at) {
		t.Fatalf("parse %v %v", got, ok)
	}
	for _, text := range []string{
		"<!-- hive:recheck-after 2026-10-21T21:20Z -->",
		"<!--hive:recheck-after 2026-10-21T21:20:00+00:00-->",
	} {
		if got, ok := ParseRecheckAfter(text); !ok || got.Hour() != 21 || got.Minute() != 20 {
			t.Fatalf("%q → %v %v", text, got, ok)
		}
	}
	for _, text := range []string{"", "<!-- hive:recheck-after tomorrow -->", "<!-- hive:recheck-after 2026-13-45T99:99Z -->", "<!-- hive:blocked-on pr=a/b#1 -->"} {
		if _, ok := ParseRecheckAfter(text); ok {
			t.Fatalf("%q should not parse", text)
		}
	}
}

func TestScoreSkipsOpsTrackingAndTimeGated(t *testing.T) {
	p := Policy{}
	if d := Score(soak(), p, now); d.Eligible || d.Skip != "ops-tracking:soak-checklist" {
		t.Fatalf("soak checklist: %+v", d)
	}
	c := good()
	c.Body += "\n<!-- hive:ops-tracking -->\n"
	if d := Score(c, p, now); d.Eligible || d.Skip != "ops-tracking:marker" {
		t.Fatalf("marker: %+v", d)
	}
	c = good()
	c.Body += "\n" + RecheckAfterMarker(now.Add(24*time.Hour)) + "\n"
	if d := Score(c, p, now); d.Eligible || d.Skip != SkipTimeGated {
		t.Fatalf("future deadline: %+v", d)
	}
	c.Body = good().Body + "\n" + RecheckAfterMarker(now.Add(-time.Hour)) + "\n"
	if d := Score(c, p, now); d.Eligible || d.Skip != SkipTimeGated {
		t.Fatalf("past deadline is still the hive's to re-check, not a newcomer's: %+v", d)
	}
	// The label gates still come first: a blocked soak issue reports blocked.
	c = soak()
	c.Labels = append(c.Labels, "blocked")
	if d := Score(c, p, now); d.Eligible || d.Skip != "excluded-label:blocked" {
		t.Fatalf("blocked: %+v", d)
	}
	if d := Score(good(), p, now); !d.Eligible {
		t.Fatalf("control: %+v", d)
	}
}

func TestWithdrawable(t *testing.T) {
	yes := []string{"hard-suppress:needs-human", "excluded-label:blocked", "excluded-label:Hold", "ops-tracking:marker", "ops-tracking:soak-checklist", SkipTimeGated}
	no := []string{"", "too-fresh", "score-1-below-2", "excluded-label:epic", "excluded-label:kind/epic", "title:rfc", "claimed", "assigned", "pull-request"}
	for _, s := range yes {
		if !Withdrawable(s) {
			t.Errorf("%q should withdraw", s)
		}
	}
	for _, s := range no {
		if Withdrawable(s) {
			t.Errorf("%q should not withdraw", s)
		}
	}
}

func TestSweepWithdrawsMislabelledBlockedIssue(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	var events []Event
	opts := quietOpts()
	opts.Audit = func(e Event) { events = append(events, e) }

	// #13267 as the sweep met it after the fact: labelled, `help wanted`,
	// `blocked`, with the earlier welcome comment still on it.
	c := soak()
	c.Labels = []string{Label, HelpWantedLabel, "blocked"}
	forge.comments[c.Key()] = []string{Marker + "\n### 🌱 Good first issue"}

	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Withdrawn) != 1 || res.Withdrawn[0] != c.Key() || len(res.Selected) != 0 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(forge.removed[c.Key()], ","); got != Label+","+HelpWantedLabel {
		t.Fatalf("removed %q", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 {
		t.Fatalf("comment count %d (must edit, not add)", n)
	}
	body := forge.comments[c.Key()][0]
	if !strings.Contains(body, SupersededMarker) || !strings.Contains(body, "withdrawn") || !strings.Contains(body, "`blocked` label") || !strings.Contains(body, "https://hive.example/contribute") {
		t.Fatalf("comment %q", body)
	}
	if len(events) != 1 || events[0].Action != "withdraw" || events[0].Reason != "excluded-label:blocked" {
		t.Fatalf("events %+v", events)
	}
	if st := s.Stats(); st.Withdrawn != 1 || st.Released != 0 {
		t.Fatalf("stats %+v", st)
	}

	// Second pass over the same stale snapshot: remembered, no second
	// removal or comment.
	res = s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Withdrawn) != 0 || len(forge.removed[c.Key()]) != 2 || forge.commentCount(c.Repo, c.Number) != 1 {
		t.Fatalf("second pass %+v removed=%v", res, forge.removed[c.Key()])
	}

	// Once unblocked and otherwise good, the cooldown still keeps it out.
	unblocked := good()
	unblocked.Number = c.Number
	if res := s.Sweep(context.Background(), forge, []Candidate{unblocked}, opts); res.Skipped["reselect-cooldown"] != 1 {
		t.Fatalf("cooldown %+v", res)
	}
}

// An issue the hive advertised in this process and that is parked for a
// person afterwards is withdrawn on the next pass, not skipped as
// already-selected (projectbluefin/utah-packages#527).
func TestSweepWithdrawsOwnSelectionThatParks(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()

	c := good()
	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 1 || !s.Selected(c.Key()) {
		t.Fatalf("first pass did not select: %+v", res)
	}

	// Still eligible on the next pass: left alone, as before.
	labelled := c
	labelled.Labels = append([]string{Label, HelpWantedLabel}, c.Labels...)
	if res := s.Sweep(context.Background(), forge, []Candidate{labelled}, opts); res.Skipped["already-selected"] != 1 || len(res.Withdrawn) != 0 {
		t.Fatalf("eligible own selection: %+v", res)
	}

	// The enumeration now reports it parked at 045.
	parked := labelled
	parked.Labels = append(append([]string(nil), labelled.Labels...), "stage/045-needs-decision")
	res = s.Sweep(context.Background(), forge, []Candidate{parked}, opts)
	if len(res.Withdrawn) != 1 || res.Withdrawn[0] != c.Key() || res.Skipped["already-selected"] != 0 || res.Errors != 0 {
		t.Fatalf("parked own selection not withdrawn: %+v", res)
	}
	if got := strings.Join(forge.removed[c.Key()], ","); got != Label+","+HelpWantedLabel {
		t.Fatalf("removed %q", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 {
		t.Fatalf("comment count %d (the welcome comment must be edited, not a second one added)", n)
	}
	if body := forge.comments[c.Key()][0]; !strings.Contains(body, SupersededMarker) || !strings.Contains(body, "`stage/045-needs-decision`") {
		t.Fatalf("comment %q", body)
	}
	if s.Selected(c.Key()) {
		t.Fatal("withdrawn issue still remembered as selected")
	}

	// A later pass neither withdraws again nor re-advertises.
	res = s.Sweep(context.Background(), forge, []Candidate{parked}, opts)
	if len(res.Withdrawn) != 0 || len(forge.removed[c.Key()]) != 2 || forge.commentCount(c.Repo, c.Number) != 1 {
		t.Fatalf("second pass %+v removed=%v", res, forge.removed[c.Key()])
	}
}

func TestSweepWithdrawPostsCorrectionWhenNoWelcomeComment(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()

	// A hand-applied label on an ops-tracking checklist: no hive comment yet.
	c := soak()
	c.Labels = []string{Label}
	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Withdrawn) != 1 || res.Skipped["ops-tracking:soak-checklist"] != 1 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(forge.removed[c.Key()], ","); got != Label {
		t.Fatalf("removed %q (help wanted was not on it)", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 || !strings.Contains(forge.comments[c.Key()][0], "operations checklist") {
		t.Fatalf("comments %v", forge.comments[c.Key()])
	}

	// A fresh process meeting the superseded note posts nothing more.
	s2 := NewSweeper()
	if err := s2.Withdraw(context.Background(), forge, c, SkipTimeGated, opts); err != nil {
		t.Fatal(err)
	}
	if forge.commentCount(c.Repo, c.Number) != 1 {
		t.Fatal("second correction posted")
	}
}

func TestSweepLeavesHandLabelledSoftRejectsAlone(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	c := good()
	c.Labels = []string{Label, "kind/epic"}
	res := s.Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
	if len(res.Withdrawn) != 0 || len(forge.removed[c.Key()]) != 0 || res.Skipped["excluded-label:kind/epic"] != 1 {
		t.Fatalf("epic must not be withdrawn: %+v removed=%v", res, forge.removed[c.Key()])
	}
}

func TestWithdrawErrorsSurface(t *testing.T) {
	forge := newFakeForge()
	forge.failOn = "remove"
	s := NewSweeper()
	c := soak()
	c.Labels = []string{Label, HelpWantedLabel, "blocked"}
	res := s.Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
	if res.Errors != 1 || len(res.Withdrawn) != 0 {
		t.Fatalf("result %+v", res)
	}
	if s.Withdraw(context.Background(), nil, c, "x", quietOpts()) != nil {
		t.Fatal("nil forge is a no-op")
	}
}

// freshForge is a fakeForge whose live labels differ from the snapshot.
type freshForge struct {
	*fakeForge
	live map[string][]string
	err  error
}

func (f *freshForge) IssueLabels(_ context.Context, repo string, n int) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.live[key(repo, n)], nil
}

func TestSweepReScoresWithLiveLabelsBeforeLabelling(t *testing.T) {
	c := good()
	base := newFakeForge()
	forge := &freshForge{fakeForge: base, live: map[string][]string{c.Key(): {"kind/bug", "size/S", "blocked"}}}
	s := NewSweeper()
	var events []Event
	opts := quietOpts()
	opts.Audit = func(e Event) { events = append(events, e) }

	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 0 || res.Skipped["stale:excluded-label:blocked"] != 1 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if len(base.added[c.Key()]) != 0 || base.commentCount(c.Repo, c.Number) != 0 || len(events) != 0 {
		t.Fatalf("nothing may be written on a stale candidate: added=%v comments=%d events=%+v", base.added[c.Key()], base.commentCount(c.Repo, c.Number), events)
	}

	// Live labels say it was labelled meanwhile AND blocked: withdraw.
	forge.live[c.Key()] = []string{Label, HelpWantedLabel, "blocked"}
	res = s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if res.Skipped["stale:excluded-label:blocked"] != 1 || len(base.removed[c.Key()]) != 2 || len(events) != 1 || events[0].Action != "withdraw" {
		t.Fatalf("withdraw from stale: %+v removed=%v events=%+v", res, base.removed[c.Key()], events)
	}

	// Live labels agree: proceed, and apply works from the live set so a
	// label GitHub already has is not re-added.
	s = NewSweeper()
	base = newFakeForge()
	forge = &freshForge{fakeForge: base, live: map[string][]string{c.Key(): {"kind/bug", HelpWantedLabel}}}
	res = s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 1 || strings.Join(base.added[c.Key()], ",") != Label {
		t.Fatalf("live-agree pass %+v added=%v", res, base.added[c.Key()])
	}

	// A failed read never advertises.
	s = NewSweeper()
	forge.err = errors.New("rate limited")
	res = s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 0 || res.Skipped["stale:label-read"] != 1 {
		t.Fatalf("read failure %+v", res)
	}
}
