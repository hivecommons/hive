package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A held PR is removed from the actionable Items by design — the hold gate is
// a merge checkpoint. But HoldItem carries no head SHA and no CI status, so
// before hivecommons/hive#7438 a held PR was invisible to every red-PR
// consumer and a red held PR could never be repaired. fetchPRs must therefore
// also return the FULL PullRequest for each held PR, with the head origin
// filled in, while leaving the actionable list untouched.
func TestFetchPRs_HeldPRIsReturnedInFullForCIRepair(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    188,
			Title:     "fix: git diff gate",
			User:      wireUser{Login: "acme-bot[bot]"},
			Labels:    []wireLabel{{Name: "hold"}},
			CreatedAt: hoursAgo(2),
			HTMLURL:   "https://github.com/acme/widget/pull/188",
			Head: &wireBranch{
				Ref:  "hive/sec-check/188",
				SHA:  "deadbeef",
				Repo: &wireRepo{FullName: "acme/widget"},
			},
		},
		{
			Number:    189,
			Title:     "chore: ordinary PR",
			User:      wireUser{Login: "acme-bot[bot]"},
			CreatedAt: hoursAgo(2),
			HTMLURL:   "https://github.com/acme/widget/pull/189",
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})

	actionable, held, heldPRs, _, total, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if total != 2 {
		t.Errorf("totalPRs = %d, want 2", total)
	}
	if len(actionable) != 1 || actionable[0].Number != 189 {
		t.Errorf("actionable = %+v, want only #189 — the hold gate must keep #188 out", actionable)
	}
	if len(held) != 1 || held[0].Number != 188 {
		t.Fatalf("held hold-items = %+v, want #188", held)
	}
	if len(heldPRs) != 1 {
		t.Fatalf("heldPRs = %d, want 1 — a held PR must still be repairable", len(heldPRs))
	}
	got := heldPRs[0]
	if got.Number != 188 {
		t.Errorf("heldPRs[0].Number = %d, want 188", got.Number)
	}
	if got.HeadSHA != "deadbeef" {
		t.Errorf("heldPRs[0].HeadSHA = %q, want %q — without it CI can never be enriched", got.HeadSHA, "deadbeef")
	}
	if got.HeadRef != "hive/sec-check/188" {
		t.Errorf("heldPRs[0].HeadRef = %q, want %q", got.HeadRef, "hive/sec-check/188")
	}
	if got.FromFork {
		t.Error("heldPRs[0].FromFork = true, want false for a same-repo branch")
	}
}

// A held DRAFT stays out of the repair list, exactly as an ordinary draft stays
// out of the actionable list: nobody should be nagged to repair work that is
// declared in progress.
func TestFetchPRs_HeldDraftIsNotRepairable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    190,
			Title:     "wip: held draft",
			User:      wireUser{Login: "acme-bot[bot]"},
			Labels:    []wireLabel{{Name: "hold"}},
			Draft:     true,
			CreatedAt: hoursAgo(2),
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})

	_, held, heldPRs, _, _, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if len(held) != 1 {
		t.Errorf("held hold-items = %d, want 1 — hold accounting is unchanged for drafts", len(held))
	}
	if len(heldPRs) != 0 {
		t.Errorf("heldPRs = %+v, want empty for a draft", heldPRs)
	}
}
