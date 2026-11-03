package evidence

import (
	"testing"
	"time"
)

func TestIssueReopenRates(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	at := now.Add(-29 * 24 * time.Hour)
	closure := func(repo string, issue int, gate bool, when time.Time) IssueClosure {
		return IssueClosure{Repo: repo, Issue: issue, ClarityGate: gate, At: when}
	}
	a := &Bundle{Repo: "o/r", IssueClosures: []IssueClosure{
		closure("O/R", 1, true, at), closure("o/r", 2, true, at),
		closure("o/r", 3, false, at), closure("o/r", 4, false, now.Add(-31*24*time.Hour)),
		closure("o/r", 5, false, now.Add(time.Hour)),
		closure("o/other", 1, false, at),
		closure("o/r", 6, false, at),
	}, IssueReopens: []IssueReopen{
		{Issue: 1, At: at.Add(20 * 24 * time.Hour)}, // later than the legacy 14-day window
		{Issue: 1, At: at.Add(21 * 24 * time.Hour)}, // repeated reopen counts once
		{Issue: 2, At: at.Add(-time.Second)},        // before closure
		{Issue: 3, At: now.Add(time.Hour)},          // future observation
		{Issue: 6, At: at.Add(time.Hour)},
		{Repo: "o/other", Issue: 1, At: at.Add(time.Hour)},
	}}
	// A newer head with no closures must not hide the merged head. Reclosing
	// an issue moves it to the latest cohort; an earlier reopen is not a failure
	// of the later closure. Duplicate bundles do not inflate either count.
	b := &Bundle{Repo: "o/r", IssueClosures: []IssueClosure{closure("o/r", 6, true, now.Add(-time.Hour))}}
	legacy := &Bundle{Repo: "o/r", IssueReopens: []IssueReopen{{Issue: 99, At: now}}}
	rows := IssueReopenRates([]*Bundle{nil, a, a, b, legacy}, now, "O/R")
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].ClarityGate || rows[0].Closed != 1 || rows[0].Reopened != 0 || rows[0].Rate == nil || *rows[0].Rate != 0 {
		t.Fatalf("ungated = %+v", rows[0])
	}
	if !rows[1].ClarityGate || rows[1].Closed != 3 || rows[1].Reopened != 1 || rows[1].Rate == nil || *rows[1].Rate != 1.0/3 {
		t.Fatalf("gated = %+v", rows[1])
	}
	all := IssueReopenRates([]*Bundle{a, b}, now, "")
	if len(all) != 4 || all[0].Repo != "o/other" || all[0].Closed != 1 || all[0].Reopened != 1 || all[1].Rate != nil {
		t.Fatalf("all = %+v", all)
	}
	if rows := IssueReopenRates([]*Bundle{legacy}, now, ""); len(rows) != 0 {
		t.Fatalf("legacy must be unknown, got %+v", rows)
	}
}

func TestIssueReopenRatesWindowBoundary(t *testing.T) {
	now := time.Now().UTC()
	b := &Bundle{Repo: "o/r", IssueClosures: []IssueClosure{{Repo: "o/r", Issue: 1, At: now.Add(-30 * 24 * time.Hour)}}}
	rows := IssueReopenRates([]*Bundle{b}, now, "")
	if len(rows) != 2 || rows[0].Closed != 1 || rows[1].Rate != nil {
		t.Fatalf("rows = %+v", rows)
	}
}
