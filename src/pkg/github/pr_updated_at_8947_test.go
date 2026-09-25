package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchPRsCarriesUpdatedAtForRepoCardBands(t *testing.T) {
	actionableUpdated := "2026-09-21T10:11:12Z"
	heldUpdated := "2026-09-20T09:08:07Z"
	draftUpdated := "2026-09-19T08:07:06Z"
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    101,
			Title:     "fix: actionable",
			User:      wireUser{Login: "bot"},
			CreatedAt: "2026-09-01T00:00:00Z",
			UpdatedAt: actionableUpdated,
		},
		{
			Number:    102,
			Title:     "fix: held",
			User:      wireUser{Login: "bot"},
			Labels:    []wireLabel{{Name: "hold"}},
			CreatedAt: "2026-09-01T00:00:00Z",
			UpdatedAt: heldUpdated,
		},
		{
			Number:    103,
			Title:     "wip: stale draft",
			User:      wireUser{Login: "hive[bot]"},
			Draft:     true,
			CreatedAt: "2026-09-01T00:00:00Z",
			UpdatedAt: draftUpdated,
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	c.appBotLogin = "hive[bot]"

	actionable, _, heldPRs, staleDrafts, _, _, _, err := c.fetchPRs(t.Context(), "widget")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	assertUpdatedAt := func(name string, got time.Time, want string) {
		t.Helper()
		wantTime, err := time.Parse(time.RFC3339, want)
		if err != nil {
			t.Fatalf("parse fixture time: %v", err)
		}
		if !got.Equal(wantTime) {
			t.Fatalf("%s UpdatedAt = %s, want %s", name, got.Format(time.RFC3339), want)
		}
	}
	if len(actionable) != 1 {
		t.Fatalf("actionable = %d, want 1", len(actionable))
	}
	assertUpdatedAt("actionable", actionable[0].UpdatedAt, actionableUpdated)
	if len(heldPRs) != 1 {
		t.Fatalf("heldPRs = %d, want 1", len(heldPRs))
	}
	assertUpdatedAt("held PR", heldPRs[0].UpdatedAt, heldUpdated)
	if len(staleDrafts) != 1 {
		t.Fatalf("staleDrafts = %d, want 1", len(staleDrafts))
	}
	assertUpdatedAt("stale draft", staleDrafts[0].UpdatedAt, draftUpdated)
}
