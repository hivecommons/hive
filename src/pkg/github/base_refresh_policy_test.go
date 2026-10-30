package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScheduledBaseRefreshDefersAllClientUpdatePaths(t *testing.T) {
	updates := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		updates++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message":"Updating pull request branch"}`))
	}))
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	scheduled := true
	c.SetScheduledBaseRefresh(func(repo string) bool { return scheduled && repo == "acme/widget" })
	for _, update := range []func() error{
		func() error { return c.UpdateBranch(context.Background(), "acme/widget", 1) },
		func() error { return c.UpdateBranchExpectedHead(context.Background(), "widget", 1, "head", "base") },
	} {
		if err := update(); err == nil || !strings.Contains(err.Error(), "base-refresh:awaiting-admission") {
			t.Fatalf("client update bypassed preparation: %v", err)
		}
	}
	if updates != 0 {
		t.Fatalf("queued client paths mutated branches: %d", updates)
	}
	scheduled = false // live policy changes preserve opted-out behaviour
	if err := c.UpdateBranchExpectedHead(context.Background(), "widget", 1, "head", "base"); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("non-policy repo updates=%d, want 1", updates)
	}
}
