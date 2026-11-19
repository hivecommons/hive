package adminmcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func stageGovernor() map[string]any {
	g := surfaceGovernor()
	g["stageAbandonAfter"] = ""
	g["stageAbandonCloseRepos"] = []string{}
	g["repos"] = []string{"example/testrepo", "example/other"}
	return g
}

func TestStageLabelsOpRegisteredOnTheLabelsEndpoint(t *testing.T) {
	op := mustOp(t, WriteOpGovernorStageLabels)
	if d := op.Description(); !strings.Contains(d, "/api/config/governor/labels") || !strings.Contains(d, "Owner only") {
		t.Fatalf("description = %q", d)
	}
}

func TestStageLabelsOpRejectsBadArgs(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: stageGovernor()})
	for name, args := range map[string]map[string]any{
		"no change":       {},
		"unknown key":     {"abandon_afterr": "1h"},
		"bad duration":    {"abandon_after": "soon"},
		"negative":        {"abandon_after": "-1h"},
		"not a string":    {"abandon_after": 7},
		"list not object": {"abandon_close_repos": []any{"testrepo"}},
		"unwatched repo":  {"abandon_close_repos": map[string]any{"add": []any{"elsewhere/unwatched"}}},
		"remove missing":  {"abandon_close_repos": map[string]any{"remove": []any{"testrepo"}}},
		"already off":     {"abandon_after": "0"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorStageLabels, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
}

// Both keys go to the Labels endpoint; a bare repository name is sent as
// owner/repo; adding a close repository is disclosed as widening.
func TestStageLabelsOpWritesBothKeys(t *testing.T) {
	provider := &surfaceProvider{governor: stageGovernor()}
	h := surfaceHandler(provider)
	p, id := capPreview(t, h, WriteOpGovernorStageLabels, map[string]any{
		"abandon_after":       "168h",
		"abandon_close_repos": map[string]any{"add": []any{"testrepo"}},
	})
	if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/governor/labels" {
		t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
	}
	want := `{"stage_abandon_after":"168h","stage_abandon_close_repos":["example/testrepo"]}`
	if got := bodyJSON(t, p.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	joined := strings.Join(p.Effects, "\n")
	for _, line := range []string{"abandon_after: off → 168h", "abandon_close_repos: [none] → [example/testrepo]"} {
		if !strings.Contains(joined, line) {
			t.Errorf("effects miss %q:\n%s", line, joined)
		}
	}
	if !isWidening(p) || !strings.Contains(p.WideningDisclosure, "close abandoned contributor PRs in example/testrepo") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(provider.requests) != 1 || bodyJSON(t, provider.requests[0].Body) != want {
		t.Fatalf("executed %+v", provider.requests)
	}
}

// Turning the clock on with no close repositories closes nothing new, and
// "0" turns it off again.
func TestStageLabelsOpIdleClockAloneIsNotWidening(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: stageGovernor()}), WriteOpGovernorStageLabels, map[string]any{"abandon_after": "72h"})
	if isWidening(p) || bodyJSON(t, p.Request.Body) != `{"stage_abandon_after":"72h"}` {
		t.Fatalf("disclosure %q body %s", p.WideningDisclosure, bodyJSON(t, p.Request.Body))
	}
	g := stageGovernor()
	g["stageAbandonAfter"] = "72h"
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpGovernorStageLabels, map[string]any{"abandon_after": "0"})
	if got := bodyJSON(t, p.Request.Body); got != `{"stage_abandon_after":""}` {
		t.Fatalf("off body = %s", got)
	}
}

func TestStageLabelsOpRefusesStalePreview(t *testing.T) {
	provider := &surfaceProvider{governor: stageGovernor()}
	h := surfaceHandler(provider)
	_, id := capPreview(t, h, WriteOpGovernorStageLabels, map[string]any{"abandon_after": "168h"})
	provider.governor["stageAbandonAfter"] = "24h"
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}
}

// An older hive without the keys is refused rather than guessed at.
func TestStageLabelsOpNeedsTheCurrentSettings(t *testing.T) {
	g := surfaceGovernor()
	if _, err := surfaceHandler(&surfaceProvider{governor: g}).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorStageLabels, "args": map[string]any{"abandon_after": "1h"}}); err == nil || !strings.Contains(err.Error(), "stageAbandonAfter") {
		t.Fatalf("missing key: err = %v", err)
	}
	if _, err := (stageLabelsOp{}).PreviewWithProvider(context.Background(), map[string]any{"abandon_after": "1h"}, nil); err == nil {
		t.Fatal("previewed without reading the settings")
	}
}
