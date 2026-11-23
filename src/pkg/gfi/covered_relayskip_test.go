package gfi

import (
	"context"
	"strings"
	"testing"
)

// relaySkipNeedsTriage stands in for hub.contribute_skip_labels with the
// labels the default set shares with the GFI gates (`blocked`, `tracking`)
// plus `needs-triage`, which (like config.HubConfig
// .MatchContributeSkipLabelFor) does not skip a trusted author's issue.
func relaySkipNeedsTriage(labels []string, authorTrusted bool) (string, bool) {
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "needs-triage":
			if !authorTrusted {
				return l, true
			}
		case "blocked", "tracking":
			return l, true
		}
	}
	return "", false
}

func TestScoreRelaySkip(t *testing.T) {
	p := Policy{RelaySkip: relaySkipNeedsTriage}
	c := good()
	c.Labels = append(c.Labels, "needs-triage")
	if d := Score(c, p, now); d.Eligible || d.Skip != SkipRelaySkip+":needs-triage" {
		t.Fatalf("needs-triage: %+v", d)
	}
	if d := Score(c, Policy{}, now); !d.Eligible {
		t.Fatalf("no matcher, no relay gate: %+v", d)
	}
	if d := Score(good(), p, now); !d.Eligible {
		t.Fatalf("control: %+v", d)
	}
	// The relay hands a trusted author's issue out despite needs-triage
	// (hivecommons/hive#13298), so it may be advertised too.
	c = good()
	c.Labels = append(c.Labels, "needs-triage")
	c.AuthorTrusted = true
	if d := Score(c, p, now); !d.Eligible {
		t.Fatalf("trusted author with needs-triage: %+v", d)
	}
	// The withdrawable gates still come first: `blocked` is in the relay's
	// default skip set too, and must keep withdrawing.
	c = good()
	c.Labels = append(c.Labels, "blocked", "needs-triage")
	if d := Score(c, p, now); d.Skip != "excluded-label:blocked" || !Withdrawable(d.Skip) {
		t.Fatalf("blocked: %+v", d)
	}
}

func TestWithdrawableCoveredAndRelaySkip(t *testing.T) {
	for _, s := range []string{SkipLinkedPR, "excluded-label:stage/040-covered-by-pr", "excluded-label:Stage/040-Covered-By-PR"} {
		if !Withdrawable(s) {
			t.Errorf("%q should withdraw", s)
		}
	}
	for _, s := range []string{SkipRelaySkip + ":needs-triage", SkipRelaySkip + ":question", "excluded-label:stage/030-pr-in-progress"} {
		if Withdrawable(s) {
			t.Errorf("%q should not withdraw", s)
		}
	}
}

// projectbluefin/website#1031: advertised at stage/010, then a pull request
// covered it. The next sweep takes both labels off and edits the welcome
// comment to say a PR covers it, without calling it "not newcomer work".
func TestSweepWithdrawsIssueAPullRequestCovers(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	var events []Event
	opts := quietOpts()
	opts.Audit = func(e Event) { events = append(events, e) }

	c := good()
	c.Labels = append([]string{Label, HelpWantedLabel}, c.Labels...)
	c.HasLinkedPR = true
	forge.comments[c.Key()] = []string{Marker + "\n### 🌱 Good first issue"}

	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Withdrawn) != 1 || res.Skipped[SkipLinkedPR] != 1 || len(res.Selected) != 0 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(forge.removed[c.Key()], ","); got != Label+","+HelpWantedLabel {
		t.Fatalf("removed %q", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 {
		t.Fatalf("comment count %d (must edit, not add)", n)
	}
	body := forge.comments[c.Key()][0]
	if !strings.Contains(body, SupersededMarker) || !strings.Contains(body, "a pull request now covers this issue, so it is no longer open to new contributors") || !strings.Contains(body, "https://hive.example/contribute") {
		t.Fatalf("comment %q", body)
	}
	if strings.Contains(body, "Sorry for the noise") || strings.Contains(body, "not open to new contributors —") {
		t.Fatalf("covered note reuses the not-newcomer-work wording: %q", body)
	}
	if len(events) != 1 || events[0].Action != "withdraw" || events[0].Reason != SkipLinkedPR {
		t.Fatalf("events %+v", events)
	}

	// The covered stage label alone withdraws the same way.
	staged := good()
	staged.Number = 1032
	staged.Labels = append([]string{Label}, "stage/040-covered-by-pr")
	if res := s.Sweep(context.Background(), forge, []Candidate{staged}, opts); len(res.Withdrawn) != 1 {
		t.Fatalf("stage/040: %+v", res)
	}
	if body := forge.comments[staged.Key()][0]; !strings.Contains(body, "a pull request now covers this issue") {
		t.Fatalf("stage/040 comment %q", body)
	}
}

// A relay contributor (or person) who claimed the issue and opened the PR is
// the outcome the label exists for: it stays until the issue closes.
func TestSweepLeavesClaimedCoveredIssueAlone(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	c := good()
	c.Labels = append([]string{Label, HelpWantedLabel}, c.Labels...)
	c.HasLinkedPR = true
	c.ClaimedBy = "newcomer"
	res := s.Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
	if len(res.Withdrawn) != 0 || len(forge.removed[c.Key()]) != 0 || res.Skipped["claimed"] != 1 {
		t.Fatalf("claimed covered issue withdrawn: %+v removed=%v", res, forge.removed[c.Key()])
	}
}

// An issue wearing a relay-skip label is never advertised; one already
// advertised that later gains it keeps the invite (triage flaps the label).
func TestSweepRelaySkipNeverSelectsNorWithdraws(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()
	opts.Policy.RelaySkip = relaySkipNeedsTriage

	untriaged := good()
	untriaged.Labels = append(untriaged.Labels, "needs-triage")
	res := s.Sweep(context.Background(), forge, []Candidate{untriaged}, opts)
	if len(res.Selected) != 0 || res.Skipped[SkipRelaySkip+":needs-triage"] != 1 || forge.commentCount(untriaged.Repo, untriaged.Number) != 0 {
		t.Fatalf("untriaged issue advertised: %+v", res)
	}

	advertised := good()
	advertised.Number = 1688
	advertised.Labels = append([]string{Label, HelpWantedLabel, "needs-triage"}, advertised.Labels...)
	res = s.Sweep(context.Background(), forge, []Candidate{advertised}, opts)
	if len(res.Withdrawn) != 0 || len(forge.removed[advertised.Key()]) != 0 {
		t.Fatalf("relay-skip withdrew an advertised issue: %+v removed=%v", res, forge.removed[advertised.Key()])
	}
}
