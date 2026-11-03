package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/pushbroker"
)

type wiringPRCreator struct{ id int }

// wiringMinter stands in for the host's pushbroker.GitHubAppMinter: the
// manager only needs a distinguishable TokenMinter.
type wiringMinter struct{ id int }

func (*wiringMinter) MintPushToken(context.Context, string) (string, error) { return "", nil }

func (*wiringPRCreator) CreatePR(context.Context, string, string, string, string, string) (crux.PRResult, error) {
	return crux.PRResult{}, nil
}

// #9621: SandboxGitHubWiring reports what the sandbox executor will use, so
// the hive can prove a client rebuild re-pointed it.
func TestSandboxGitHubWiringReportsLatestSetters(t *testing.T) {
	m := NewManager(nil, slog.Default(), ProjectContext{})
	if pr, minter := m.SandboxGitHubWiring(); pr != nil || minter != nil {
		t.Fatalf("fresh manager wiring = (%v, %v), want (nil, nil)", pr, minter)
	}
	first, second := &wiringPRCreator{id: 1}, &wiringPRCreator{id: 2}
	firstMinter, secondMinter := &wiringMinter{id: 1}, &wiringMinter{id: 2}

	m.SetSandboxPRClient(first)
	m.SetSandboxPushMinter(firstMinter)
	m.SetSandboxPRClient(second)
	m.SetSandboxPushMinter(secondMinter)

	pr, minter := m.SandboxGitHubWiring()
	if pr != second {
		t.Fatalf("PR client = %v, want the latest one set", pr)
	}
	if minter != pushbroker.TokenMinter(secondMinter) {
		t.Fatalf("push minter = %#v, want the latest one set", minter)
	}
}
