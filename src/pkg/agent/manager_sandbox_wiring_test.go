package agent

import (
	"context"
	"log/slog"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/pushbroker"
)

type wiringPRCreator struct{ id int }

func (*wiringPRCreator) CreatePR(context.Context, string, string, string, string, string) (ghpkg.CreatePRResult, error) {
	return ghpkg.CreatePRResult{}, nil
}

// #9621: SandboxGitHubWiring reports what the sandbox executor will use, so
// the hive can prove a client rebuild re-pointed it.
func TestSandboxGitHubWiringReportsLatestSetters(t *testing.T) {
	m := NewManager(nil, slog.Default(), ProjectContext{})
	if pr, minter := m.SandboxGitHubWiring(); pr != nil || minter != nil {
		t.Fatalf("fresh manager wiring = (%v, %v), want (nil, nil)", pr, minter)
	}
	first, second := &wiringPRCreator{id: 1}, &wiringPRCreator{id: 2}
	firstAuth, secondAuth := &ghpkg.AppAuth{}, &ghpkg.AppAuth{}

	m.SetSandboxPRClient(first)
	m.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: firstAuth})
	m.SetSandboxPRClient(second)
	m.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: secondAuth})

	pr, minter := m.SandboxGitHubWiring()
	if pr != second {
		t.Fatalf("PR client = %v, want the latest one set", pr)
	}
	got, ok := minter.(pushbroker.GitHubAppMinter)
	if !ok || got.Auth != secondAuth {
		t.Fatalf("push minter = %#v, want the latest AppAuth", minter)
	}
}
