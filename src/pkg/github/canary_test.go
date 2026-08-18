package github

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/ioscan"
)

func TestCreatePRBlocksCanaryWhenFailClosed(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("quality")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	called := false
	c := &Client{client: gh.NewClient(nil), org: "org"}
	c.SetCanaryScanner(true, true, r, func(leak ioscan.CanaryLeak) {
		called = true
		if leak.Agent != "quality" || !strings.HasPrefix(leak.Source, "hive-open-pr:") {
			t.Fatalf("leak = %+v", leak)
		}
	})
	_, err = c.CreatePR(context.Background(), "org/repo", "quality/fix", "main", "fix", "body "+cny.Token)
	if err == nil || !strings.Contains(err.Error(), "ioscan canary leak") {
		t.Fatalf("CreatePR error = %v, want canary block", err)
	}
	if !called {
		t.Fatal("leak callback was not called")
	}
}
