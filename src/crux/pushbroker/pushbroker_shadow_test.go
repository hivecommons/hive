package pushbroker

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/authz"
)

const (
	shadowDiffTreeKey = "diff-tree --root --no-commit-id --no-renames --name-only -r -z HEAD"
	shadowShowKey     = "show --text --no-textconv --format= --no-ext-diff HEAD"
)

func shadowScriptedGit(changed string) *scriptedGit {
	return &scriptedGit{
		replies: map[string]string{
			"rev-parse HEAD":  "abc123\n",
			shadowDiffTreeKey: changed,
			shadowShowKey:     "+// harmless\n",
		},
		fails: map[string]error{"rev-parse --verify refs/remotes/origin/work": errors.New("unknown revision")},
	}
}

func shadowBroker(t *testing.T, git *scriptedGit, shadow *authz.Shadow) *Broker {
	t.Helper()
	fixed := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	return &Broker{
		Workspace: fakeGitWorkspace(t), Branch: "work", Repo: "hivecommons/hive", AgentName: "scanner",
		Minter: fakeMinter{"ghs_tok"}, Runner: git, Logger: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return fixed }, Shadow: shadow,
	}
}

// The authz shadow observes the changed-files guard without changing it: for
// each outcome the guard reaches, exactly one event is recorded and it agrees
// with the guard.
func TestBrokerShadowRecordsTheChangedFilesGuard(t *testing.T) {
	cases := []struct {
		name       string
		changed    string
		wantErr    bool
		wantEffect authz.Effect
		wantReason string
		wantPaths  int
	}{
		{"push passes", "pkg/safe.go\x00", false, authz.EffectAllow, authz.ReasonOK, 1},
		{"protected path refused", "policies/x.rego\x00pkg/safe.go\x00", true, authz.EffectDeny, authz.ReasonProtectedPath, 2},
		{"workflow refused", ".github/workflows/ci.yml\x00", true, authz.EffectDeny, authz.ReasonProtectedPath, 1},
		{"nothing committed refused", "", true, authz.EffectDeny, authz.ReasonEmptyPush, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shadow := authz.NewShadow(8)
			_, err := shadowBroker(t, shadowScriptedGit(tc.changed), shadow).Run(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run error = %v, wantErr %v", err, tc.wantErr)
			}
			events := shadow.Events()
			if len(events) != 1 {
				t.Fatalf("events = %d, want one per guard decision", len(events))
			}
			ev := events[0]
			if ev.Effect != string(tc.wantEffect) || ev.ReasonCode != tc.wantReason || ev.Parity != authz.ParityMatch || !ev.Shadow {
				t.Fatalf("event = %+v", ev)
			}
			if ev.Surface != string(authz.SurfaceBroker) || ev.Action.Kind != authz.KindBranchPush || ev.Resource.Repo != "hivecommons/hive" ||
				ev.Resource.Branch != "work" || ev.Actor.Agent != "scanner" || ev.Resource.PathsCount != tc.wantPaths {
				t.Fatalf("event attributes = %+v", ev)
			}
			if st := shadow.Stats(); st.Mismatches != 0 || st.Errors != 0 {
				t.Fatalf("stats = %+v, want no divergence from the existing guard", st)
			}
		})
	}
}

// Enforcing nothing is the contract: the Result, the error and the git
// commands issued are identical with the shadow on and off.
func TestBrokerShadowChangesNoBehaviour(t *testing.T) {
	for _, changed := range []string{"pkg/safe.go\x00", "OWNERS\x00", ""} {
		gitOff, gitOn := shadowScriptedGit(changed), shadowScriptedGit(changed)
		resOff, errOff := shadowBroker(t, gitOff, nil).Run(context.Background())
		resOn, errOn := shadowBroker(t, gitOn, authz.NewShadow(4)).Run(context.Background())
		if (errOff == nil) != (errOn == nil) || (errOff != nil && errOff.Error() != errOn.Error()) {
			t.Fatalf("changed=%q: error differs with the shadow on: %v vs %v", changed, errOff, errOn)
		}
		// Workspaces are distinct temp dirs; compare everything else.
		resOff.Workspace, resOn.Workspace = "", ""
		if !reflect.DeepEqual(resOff, resOn) {
			t.Fatalf("changed=%q: Result differs with the shadow on:\n off=%+v\n on=%+v", changed, resOff, resOn)
		}
		if !slices.Equal(gitOff.calls, gitOn.calls) || gitOff.pushed != gitOn.pushed {
			t.Fatalf("changed=%q: git calls differ with the shadow on:\n off=%v\n on=%v", changed, gitOff.calls, gitOn.calls)
		}
	}
}

// With no Shadow set and HIVE_AUTHZ_SHADOW unset, nothing is recorded anywhere.
func TestBrokerShadowOffRecordsNothing(t *testing.T) {
	if authz.DefaultShadow() != nil {
		t.Skip("HIVE_AUTHZ_SHADOW is set in this environment")
	}
	b := shadowBroker(t, shadowScriptedGit("OWNERS\x00"), nil)
	if _, err := b.Run(context.Background()); err == nil {
		t.Fatal("expected the protected path refusal")
	}
	if got := authz.DefaultShadow().Events(); got != nil {
		t.Fatalf("off recorded %d events", len(got))
	}
}

// A custom protected list reaches the evaluator, and its version differs from
// the default list's, so an event says which policy input it used.
func TestBrokerShadowSeesTheEffectiveProtectedList(t *testing.T) {
	versions := map[string]string{}
	for name, protected := range map[string][]string{"default": nil, "custom": {"docs/"}} {
		shadow := authz.NewShadow(4)
		b := shadowBroker(t, shadowScriptedGit("docs/notes.md\x00"), shadow)
		b.ProtectedPaths = protected
		_, err := b.Run(context.Background())
		events := shadow.Events()
		if len(events) != 1 || events[0].Parity != authz.ParityMatch {
			t.Fatalf("%s: events = %+v", name, events)
		}
		versions[name] = events[0].PolicyVersion
		if wantErr := name == "custom"; (err != nil) != wantErr {
			t.Fatalf("%s: Run error = %v", name, err)
		}
	}
	if versions["default"] == versions["custom"] {
		t.Fatalf("policy version did not reflect the protected list: %v", versions)
	}
}
