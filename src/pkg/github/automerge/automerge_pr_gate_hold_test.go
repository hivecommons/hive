package automerge

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prgate"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

type noopPRGateAPI struct{}

func (noopPRGateAPI) ReadSubject(context.Context, string, int) (prgate.Subject, error) {
	return prgate.Subject{}, nil
}

func (noopPRGateAPI) PostStatus(context.Context, string, int, string, string, string, string) error {
	return nil
}

func installPRGate(t *testing.T, mode prgate.Mode, level string) {
	t.Helper()
	g := prgate.New(noopPRGateAPI{}, func(string) prgate.Mode { return mode }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.Grant = func(context.Context) (prgate.GrantInfo, error) {
		return prgate.GrantInfo{Applicable: true, Installation: "example-org", Level: level}, nil
	}
	g.RefreshGrant(context.Background())
	prgate.SetDefault(g)
	t.Cleanup(func() { prgate.SetDefault(nil) })
}

func TestPRGateHoldReasonUsesTheStageEngineStep(t *testing.T) {
	if got, want := prgate.StepPostStatus, prstage.UnauthorizedStepPostPRGateStatus; got != want {
		t.Fatalf("prgate step %q differs from the stage engine step %q", got, want)
	}
}

// In enforce mode a required gate context that is missing because the hive
// cannot post it is named, not left as a bare required-check-missing.
func TestResolveMissingRequiredCheckNamesTheMissingGrant(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name   string
		mode   prgate.Mode
		level  string
		reason string
		want   string
	}{
		{"enforce, grant missing", prgate.ModeEnforce, "read", "required-check-missing:verify PR contents", prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepPostPRGateStatus},
		{"enforce, grant missing, PR Type", prgate.ModeEnforce, "", "required-check-missing:PR Type", prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepPostPRGateStatus},
		{"enforce, grant present: untouched", prgate.ModeEnforce, "write", "required-check-missing:verify PR contents", "required-check-missing:verify PR contents"},
		{"shadow, grant missing: untouched", prgate.ModeShadow, "read", "required-check-missing:verify PR contents", "required-check-missing:verify PR contents"},
		{"another required context: untouched", prgate.ModeEnforce, "read", "required-check-missing:DCO", "required-check-missing:DCO"},
		{"not a missing-check reason: untouched", prgate.ModeEnforce, "read", "required-check-failing:verify PR contents", "required-check-failing:verify PR contents"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installPRGate(t, tc.mode, tc.level)
			c := &Engine{}
			got := c.resolveMissingRequiredCheck(context.Background(), "o/r", "o", "r", nil, 7, sha, tc.reason, time.Time{})
			if got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMissingRequiredCheckWithoutAGateIsUnchanged(t *testing.T) {
	prgate.SetDefault(nil)
	c := &Engine{}
	const reason = "required-check-missing:verify PR contents"
	if got := c.resolveMissingRequiredCheck(context.Background(), "o/r", "o", "r", nil, 7, "a", reason, time.Time{}); got != reason {
		t.Errorf("reason = %q, want it unchanged with mode off", got)
	}
}
