package cruxhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api/callback"
)

// CreatePR asks the per-repo open-PRs gate before it opens anything, and a
// refusal reads as forbidden to the runtime.
func TestCreatePRRepoPRRefusal(t *testing.T) {
	f := &fakeDeps{}
	deps := allDeps(f)
	deps.RepoPRRefusal = func(repo string) string {
		if repo == "o/docs" {
			return "pull requests are off for o/docs"
		}
		return ""
	}
	caps := New(deps)
	ctx := context.Background()
	_, err := caps.CreatePR(ctx, callback.CreatePRRequest{Repo: "o/docs", Head: "h", Base: "b", Title: "t"})
	if !errors.Is(err, callback.ErrForbidden) || !strings.Contains(err.Error(), "off for o/docs") {
		t.Fatalf("refused repo: err = %v", err)
	}
	pr, err := caps.CreatePR(ctx, callback.CreatePRRequest{Repo: "o/r", Head: "h", Base: "b", Title: "t"})
	if err != nil || pr.Number != 9 {
		t.Fatalf("another repo must be unaffected: pr=%+v err=%v", pr, err)
	}
}
