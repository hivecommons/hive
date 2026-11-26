package escalation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// requiredGreenObs is a head red only on checks outside the required set
// while every required check passed: Pending (not Red, not green) with
// RequiredGreen, as escalationObservation reports it.
func requiredGreenObs(num int, sha string, labeled bool) Observation {
	return Observation{Repo: "org/repo", Number: num, HeadSHA: sha, Pending: true, RequiredGreen: true, Labeled: labeled}
}

// escalateHere drives repo#num to a hive-raised escalation whose needs-human
// label is confirmed, exactly as runEscalationSweep leaves it.
func escalateHere(t *testing.T, s *Store, num int) {
	t.Helper()
	escalate(t, s, "org/repo", num)
	s.MarkLabelApplied("org/repo", num)
	if e := s.entries[Key("org/repo", num)]; e == nil || !e.RaisedHere {
		t.Fatalf("setup: MarkEscalated must record RaisedHere, entry = %+v", e)
	}
}

// Danathar/tududi#658: the hive escalated a PR, a reviewer repair made every
// required check green, and only an advisory check stayed red. The head is
// Pending, so lift #1 never fired and needs-human stayed until a person took
// it off. The required-green lift returns it to the automated lane once,
// with its own reason, and the next passes neither re-escalate nor count.
func TestSweep_RequiredGreenLiftsHiveEscalationOnce(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	escalateHere(t, s, 7)

	r := s.Sweep([]Observation{requiredGreenObs(7, "repair", true)}, DefaultThreshold)[Key("org/repo", 7)]
	if !r.Unparked || r.Escalated || r.NewlyEscala {
		t.Fatalf("required-green head must un-park the hive's escalation: %+v", r)
	}
	if r.UnparkReason != RequiredGreenUnparkReason {
		t.Fatalf("UnparkReason = %q, want %q", r.UnparkReason, RequiredGreenUnparkReason)
	}
	if e := s.entries[Key("org/repo", 7)]; e.RaisedHere || e.Escalated || e.LabelApplied || len(e.RedSHAs) != 0 {
		t.Fatalf("un-park must reset the entry and clear RaisedHere: %+v", e)
	}
	if s.IsEscalated("org/repo", 7) {
		t.Fatal("IsEscalated = true after the un-park")
	}

	// The removal did not land yet: the label is still listed. Retry the
	// removal only, with no second comment.
	r = s.Sweep([]Observation{requiredGreenObs(7, "repair", true)}, DefaultThreshold)[Key("org/repo", 7)]
	if !r.Unparked || r.UnparkReason != "" || r.Escalated {
		t.Fatalf("label still listed must retry the removal only: %+v", r)
	}

	// The label is gone and the head stays red only on the advisory check,
	// pass after pass, well past the grace: no attempt is counted, nothing
	// re-escalates and nothing un-parks again.
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })
	for i := 0; i < DefaultThreshold+2; i++ {
		now = now.Add(LabelUnparkGrace + RedPRStaleAfter)
		o := requiredGreenObs(7, "repair", false)
		s.ObserveRed([]Observation{o})
		r = s.Sweep([]Observation{o}, DefaultThreshold)[Key("org/repo", 7)]
		if r.Unparked || r.Escalated || r.NewlyEscala || r.NeedsLabel || r.Exhausted || r.Attempts != 0 {
			t.Fatalf("pass %d after the un-park: %+v", i, r)
		}
	}
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("advisory-only passes counted %d attempts", n)
	}
	if n := s.ReEngagements("org/repo", 7); n != 0 {
		t.Fatalf("advisory-only passes spent %d re-engagements", n)
	}
	if s.StaleRed("org/repo", 7, "repair") {
		t.Fatal("advisory-only head started the staleness clock")
	}
}

// A needs-human the ledger did not raise is never lifted by a required-green
// head: a person's label on a fresh ledger, and an escalation the hive raised
// that a person then lifted and re-applied.
func TestSweep_RequiredGreenLeavesOtherNeedsHumanAlone(t *testing.T) {
	t.Run("a person's label", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		for i := 0; i < 3; i++ {
			r := s.Sweep([]Observation{requiredGreenObs(7, "a", true)}, DefaultThreshold)[Key("org/repo", 7)]
			if r.Unparked || !r.Escalated {
				t.Fatalf("pass %d: a person's needs-human must stay: %+v", i, r)
			}
		}
		if e := s.entries[Key("org/repo", 7)]; e.RaisedHere {
			t.Fatal("an observed label set RaisedHere")
		}
	})

	t.Run("re-applied by a person after a human un-park", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalateHere(t, s, 7)
		now := time.Now().UTC().Add(LabelUnparkGrace + time.Minute)
		s.SetClock(func() time.Time { return now })
		// A person removes the label: the human un-park clears RaisedHere.
		s.Sweep([]Observation{obs("org/repo", 7, "e3", true)}, DefaultThreshold)
		if e := s.entries[Key("org/repo", 7)]; e.RaisedHere || e.Escalated {
			t.Fatalf("human un-park must clear RaisedHere: %+v", e)
		}
		// A person puts it back; the head is now required-green.
		r := s.Sweep([]Observation{requiredGreenObs(7, "f", true)}, DefaultThreshold)[Key("org/repo", 7)]
		if r.Unparked || !r.Escalated {
			t.Fatalf("a re-applied needs-human must stay: %+v", r)
		}
	})

	t.Run("no required-green evidence", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalateHere(t, s, 7)
		o := requiredGreenObs(7, "f", true)
		o.RequiredGreen = false
		r := s.Sweep([]Observation{o}, DefaultThreshold)[Key("org/repo", 7)]
		if r.Unparked || !r.Escalated {
			t.Fatalf("a pending head without required-green evidence must stay parked: %+v", r)
		}
	})

	t.Run("reporter-trust hold", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalateHere(t, s, 7)
		s.SetReporterTrustReason("org/repo", 7, "reporter-trust hold")
		r := s.Sweep([]Observation{requiredGreenObs(7, "f", true)}, DefaultThreshold)[Key("org/repo", 7)]
		if r.Unparked || !r.Escalated {
			t.Fatalf("a reporter-trust hold must not lift on CI: %+v", r)
		}
	})
}

// RaisedHere survives a restart, and a ledger written before the field
// existed reads false: such an entry keeps its label on a required-green head.
func TestSweep_RaisedHerePersistsAndOldEntriesStayParked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	s := Load(path)
	escalateHere(t, s, 7)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw[Key("org/repo", 7)]["raised_here"] != true {
		t.Fatalf("ledger JSON lacks raised_here=true: %s", data)
	}

	s = Load(path)
	if r := s.Sweep([]Observation{requiredGreenObs(7, "f", true)}, DefaultThreshold)[Key("org/repo", 7)]; !r.Unparked {
		t.Fatalf("reloaded hive escalation must lift on a required-green head: %+v", r)
	}

	// The same escalation as an older build wrote it: no raised_here.
	old := filepath.Join(t.TempDir(), "old.json")
	entry := map[string]any{
		"red_shas":         []string{"e1", "e2", "e3"},
		"escalated":        true,
		"label_applied":    true,
		"label_applied_at": time.Now().UTC().Add(-time.Hour),
		"machinery":        MachineryVersion,
		"updated_at":       time.Now().UTC(),
	}
	data, err = json.Marshal(map[string]any{Key("org/repo", 7): entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s = Load(old)
	if r := s.Sweep([]Observation{requiredGreenObs(7, "f", true)}, DefaultThreshold)[Key("org/repo", 7)]; r.Unparked || !r.Escalated {
		t.Fatalf("an entry without raised_here must stay parked: %+v", r)
	}
	// A fully green head still lifts it, as before.
	green := Observation{Repo: "org/repo", Number: 7, HeadSHA: "g", Labeled: true}
	if r := s.Sweep([]Observation{green}, DefaultThreshold)[Key("org/repo", 7)]; !r.Unparked {
		t.Fatalf("a green head must still lift an old entry: %+v", r)
	}
}

// Every reset path clears RaisedHere, so a later needs-human that the hive
// did not raise can never inherit it.
func TestRaisedHereClearedOnReset(t *testing.T) {
	t.Run("reviewer reconciliation", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalate(t, s, "org/repo", 7)
		s.Sweep([]Observation{obsLabeled("org/repo", 7, "r1", true, ReviewerPassedLabel)}, DefaultThreshold)
		if e := s.entries[Key("org/repo", 7)]; e.RaisedHere || e.Escalated {
			t.Fatalf("reviewer reconciliation must clear RaisedHere: %+v", e)
		}
	})

	t.Run("machinery amnesty in Sweep", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalate(t, s, "org/repo", 7)
		s.entries[Key("org/repo", 7)].Machinery = MachineryVersion - 1
		s.Sweep([]Observation{obs("org/repo", 7, "e4", true)}, DefaultThreshold)
		if e := s.entries[Key("org/repo", 7)]; e.RaisedHere || e.Escalated {
			t.Fatalf("amnesty must clear RaisedHere: %+v", e)
		}
	})

	t.Run("machinery amnesty in re-engagement", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalate(t, s, "org/repo", 7)
		s.entries[Key("org/repo", 7)].Machinery = MachineryVersion - 1
		s.ReEngageEligible("org/repo", 7, "e3", 0)
		if e := s.entries[Key("org/repo", 7)]; e.RaisedHere || e.Escalated {
			t.Fatalf("amnesty must clear RaisedHere: %+v", e)
		}
	})

	t.Run("green lift", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "ledger.json"))
		escalateHere(t, s, 7)
		s.Sweep([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "g", Labeled: true}}, DefaultThreshold)
		if e := s.entries[Key("org/repo", 7)]; e.RaisedHere {
			t.Fatalf("green lift must clear RaisedHere: %+v", e)
		}
	})
}

// The hive's needs-human never landed (AddLabels failed) and the head turns
// required-green before the retry: the entry returns to the automated lane
// quietly instead of adding the label for the next pass to remove.
func TestSweep_RequiredGreenWithUnlandedLabelUnparksQuietly(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	escalate(t, s, "org/repo", 7) // comment posted, label call failed

	// Without required-green evidence the label is retried as before.
	if r := s.Sweep([]Observation{pendingObs("org/repo", 7, "f")}, DefaultThreshold)[Key("org/repo", 7)]; !r.NeedsLabel || !r.Escalated {
		t.Fatalf("pending head must keep retrying the label: %+v", r)
	}

	r := s.Sweep([]Observation{requiredGreenObs(7, "f", false)}, DefaultThreshold)[Key("org/repo", 7)]
	if r.NeedsLabel || r.Unparked || r.Escalated || r.NewlyEscala {
		t.Fatalf("required-green head with no label must un-park quietly: %+v", r)
	}
	e := s.entries[Key("org/repo", 7)]
	if e.RaisedHere || e.Escalated || e.AutoUnparked || len(e.RedSHAs) != 0 {
		t.Fatalf("quiet un-park must reset the entry with nothing to retry: %+v", e)
	}

	// A person adds needs-human afterwards: it is theirs and stays.
	r = s.Sweep([]Observation{requiredGreenObs(7, "f", true)}, DefaultThreshold)[Key("org/repo", 7)]
	if r.Unparked || !r.Escalated {
		t.Fatalf("a later needs-human must not be removed: %+v", r)
	}
}

// RequiredGreenLiftDue tells the caller when the label's provenance is worth
// a read, and DisownEscalation records a person's label so it never is again.
func TestRequiredGreenLiftDueAndDisown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	s := Load(path)
	if s.RequiredGreenLiftDue("org/repo", 7) {
		t.Fatal("untracked PR reported due")
	}
	escalateHere(t, s, 7)
	if !s.RequiredGreenLiftDue("org/repo", 7) {
		t.Fatal("hive escalation not reported due")
	}

	s.DisownEscalation("org/repo", 7)
	if s.RequiredGreenLiftDue("org/repo", 7) {
		t.Fatal("disowned escalation still reported due")
	}
	if !s.IsEscalated("org/repo", 7) {
		t.Fatal("disowning must keep the PR escalated")
	}
	if e := Load(path).entries[Key("org/repo", 7)]; e == nil || e.RaisedHere || !e.Escalated {
		t.Fatalf("disown was not persisted: %+v", e)
	}
	if r := s.Sweep([]Observation{requiredGreenObs(7, "f", true)}, DefaultThreshold)[Key("org/repo", 7)]; r.Unparked || !r.Escalated {
		t.Fatalf("disowned escalation lifted: %+v", r)
	}

	// A person's label adopted by the ledger is never due.
	s.Sweep([]Observation{requiredGreenObs(8, "a", true)}, DefaultThreshold)
	if s.RequiredGreenLiftDue("org/repo", 8) {
		t.Fatal("observed label reported due")
	}

	// Nor is a reporter-trust hold, or an un-park still waiting on removal.
	escalateHere(t, s, 9)
	s.SetReporterTrustReason("org/repo", 9, "reporter-trust hold")
	if s.RequiredGreenLiftDue("org/repo", 9) {
		t.Fatal("reporter-trust hold reported due")
	}
	escalateHere(t, s, 10)
	s.Sweep([]Observation{{Repo: "org/repo", Number: 10, HeadSHA: "g", Labeled: true}}, DefaultThreshold)
	if s.RequiredGreenLiftDue("org/repo", 10) {
		t.Fatal("auto-un-parked entry reported due")
	}
}
