package dashboard

import (
	"context"
	"errors"
	"sync"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// verdictBlockerFixture records what the hub asked the forge to record.
type verdictBlockerFixture struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *verdictBlockerFixture) record(_ context.Context, repo string, number int, b ghpkg.VerdictBlocker) (ghpkg.VerdictBlockerOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repo+"->"+b.String())
	if f.err != nil {
		return "", f.err
	}
	return ghpkg.VerdictBlockerLinked, nil
}

func (f *verdictBlockerFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// hivecommons/hive#15023: a blocked verdict that names its blocker records it
// once per task; a reason with no reference, a non-issue task and a
// re-delivered verdict write nothing.
func TestRecordVerdictBlocker_SeamAndDedupe(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := &verdictBlockerFixture{}
	hub.verdictBlockerRecorder = fx.record
	task := &WSTaskAssign{Kind: "issue", Repo: "projectbluefin/testsuite", Number: 975}

	hub.recordVerdictBlocker(nil, "blocked on ublue-os/server#450")
	hub.recordVerdictBlocker(&WSTaskAssign{Kind: "pr", Repo: "o/r", Number: 5}, "blocked on #6")
	hub.recordVerdictBlocker(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5, ExternalID: "LIN-1"}, "blocked on #6")
	hub.recordVerdictBlocker(task, blockedReason)
	if n := fx.count(); n != 0 {
		t.Fatalf("no reference or a non-GitHub-issue task must record nothing, got %v", fx.calls)
	}

	hub.recordVerdictBlocker(task, "#975 is blocked on the open issue ublue-os/server#450")
	if len(fx.calls) != 1 || fx.calls[0] != "projectbluefin/testsuite->ublue-os/server#450" {
		t.Fatalf("the named blocker must be recorded on the task issue, got %v", fx.calls)
	}
	hub.recordVerdictBlocker(task, "still blocked on ublue-os/server#450")
	if n := fx.count(); n != 1 {
		t.Fatalf("a re-delivered verdict must not record twice, got %v", fx.calls)
	}
}

func TestRecordVerdictBlocker_ErrorAllowsRetry(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := &verdictBlockerFixture{err: errors.New("boom")}
	hub.verdictBlockerRecorder = fx.record
	task := &WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5}

	hub.recordVerdictBlocker(task, "blocked on o/x#9")
	fx.err = nil
	hub.recordVerdictBlocker(task, "blocked on o/x#9")
	if n := fx.count(); n != 2 {
		t.Fatalf("a failed record must not dedupe the retry, got %v", fx.calls)
	}
}

// A tooling-failure verdict is reclassified idle before the blocker hook, so
// a reason that happens to cite a reference writes nothing.
func TestRecordVerdictBlocker_ToolingReclassifiedWritesNothing(t *testing.T) {
	reason := "the push to my fork is rejected; see hivecommons/hive#11775"
	verdict, ok := reclassifyToolingBlockedVerdict(completionVerdictBlocked, reason)
	if !ok || verdict == completionVerdictBlocked {
		t.Fatalf("tooling reason must be reclassified away from blocked, got (%q, %v)", verdict, ok)
	}
}

func TestRecordVerdictBlocker_NoClientIsLogged(t *testing.T) {
	hub, _ := covK2Hub(t)
	if hub.server != nil && hub.server.deps != nil {
		hub.server.deps.GHClient = nil
	}
	if _, err := hub.recordVerdictBlockerOnForge(context.Background(), "o/r", 5, ghpkg.VerdictBlocker{Repo: "o/r", Number: 6}); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("no GitHub client must report ErrNoGitHubClient, got %v", err)
	}
}
