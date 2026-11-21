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
	g["stageAbandonAfterTrusted"] = ""
	g["stageAbandonCloseRepos"] = []string{}
	g["stageAdoptionPersonAfter"] = ""
	g["stageAdoptionPersonGrace"] = ""
	g["stageAdoptionPersonRepos"] = []string{}
	g["stageAdoptionPersonHumanRepos"] = []string{}
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

// abandon_after_trusted goes to the same endpoint; turning it on where
// close repositories are listed is widening, it is refused below
// abandon_after, and "0" turns it off.
func TestStageLabelsOpWritesTheTrustedTimer(t *testing.T) {
	g := stageGovernor()
	g["stageAbandonAfter"] = "48h"
	g["stageAbandonCloseRepos"] = []string{"example/testrepo"}
	provider := &surfaceProvider{governor: g}
	h := surfaceHandler(provider)
	p, id := capPreview(t, h, WriteOpGovernorStageLabels, map[string]any{"abandon_after_trusted": "168h"})
	want := `{"stage_abandon_after_trusted":"168h"}`
	if got := bodyJSON(t, p.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if joined := strings.Join(p.Effects, "\n"); !strings.Contains(joined, "abandon_after_trusted: off → 168h") {
		t.Errorf("effects miss the change:\n%s", joined)
	}
	if !isWidening(p) || !strings.Contains(p.WideningDisclosure, "trusted contributors' PRs in example/testrepo become closable after 168h idle instead of never") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(provider.requests) != 1 || bodyJSON(t, provider.requests[0].Body) != want {
		t.Fatalf("executed %+v", provider.requests)
	}

	for name, args := range map[string]map[string]any{
		"below abandon_after":       {"abandon_after_trusted": "24h"},
		"abandon_after raised past": {"abandon_after": "336h", "abandon_after_trusted": "200h"},
		"bad duration":              {"abandon_after_trusted": "soon"},
		"not a string":              {"abandon_after_trusted": 7},
	} {
		if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorStageLabels, "args": args}); err == nil {
			t.Errorf("%s: args %v accepted", name, args)
		}
	}

	// No close repositories: turning it on flags only, not widening.
	quiet := stageGovernor()
	quiet["stageAbandonAfter"] = "48h"
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: quiet}), WriteOpGovernorStageLabels, map[string]any{"abandon_after_trusted": "168h"})
	if isWidening(p) {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	on := stageGovernor()
	on["stageAbandonAfter"] = "48h"
	on["stageAbandonAfterTrusted"] = "168h"
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: on}), WriteOpGovernorStageLabels, map[string]any{"abandon_after_trusted": "0"})
	if got := bodyJSON(t, p.Request.Body); got != `{"stage_abandon_after_trusted":""}` {
		t.Fatalf("off body = %s", got)
	}
}

// The person-PR takeover timer's four keys (hivecommons/hive#19395) are
// refused when malformed, as the endpoint would.
func TestStageLabelsOpRejectsBadPersonArgs(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: stageGovernor()})
	for name, args := range map[string]map[string]any{
		"bad after":           {"person_after": "soon"},
		"negative after":      {"person_after": "-1h"},
		"after not a string":  {"person_after": 24},
		"bad grace":           {"person_grace": "later"},
		"negative grace":      {"person_grace": "-5m"},
		"list not object":     {"person_repos": []any{"testrepo"}},
		"unwatched repo":      {"person_repos": map[string]any{"add": []any{"elsewhere/unwatched"}}},
		"unwatched human":     {"person_human_repos": map[string]any{"add": []any{"elsewhere/unwatched"}}},
		"remove missing":      {"person_human_repos": map[string]any{"remove": []any{"testrepo"}}},
		"already off":         {"person_after": "0"},
		"grace already unset": {"person_grace": "0"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorStageLabels, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
}

// All four timer keys go to the Labels endpoint under their stage_adoption_*
// names; bare repository names are sent as owner/repo; opting a repository
// in is disclosed as widening.
func TestStageLabelsOpWritesPersonTakeoverKeys(t *testing.T) {
	provider := &surfaceProvider{governor: stageGovernor()}
	h := surfaceHandler(provider)
	p, id := capPreview(t, h, WriteOpGovernorStageLabels, map[string]any{
		"person_after":       "24h",
		"person_grace":       "12h",
		"person_repos":       map[string]any{"add": []any{"testrepo"}},
		"person_human_repos": map[string]any{"add": []any{"example/other"}},
	})
	if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/governor/labels" {
		t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
	}
	want := `{"stage_adoption_person_after":"24h","stage_adoption_person_grace":"12h","stage_adoption_person_human_repos":["example/other"],"stage_adoption_person_repos":["example/testrepo"]}`
	if got := bodyJSON(t, p.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	joined := strings.Join(p.Effects, "\n")
	for _, line := range []string{
		"person_after: off → 24h",
		"person_grace: 24h (default) → 12h",
		"person_repos: [none] → [example/testrepo]",
		"person_human_repos: [none] → [example/other]",
		"Let hive decide still never adopts a person's PR",
	} {
		if !strings.Contains(joined, line) {
			t.Errorf("effects miss %q:\n%s", line, joined)
		}
	}
	for _, part := range []string{"take over this hive's owners' agent-written PRs in example/testrepo", "take over hand-written author/human PRs in example/other"} {
		if !isWidening(p) || !strings.Contains(p.WideningDisclosure, part) {
			t.Fatalf("disclosure %q lacks %q", p.WideningDisclosure, part)
		}
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(provider.requests) != 1 || bodyJSON(t, provider.requests[0].Body) != want {
		t.Fatalf("executed %+v", provider.requests)
	}
}

// "0" turns the timer off and puts the grace back to its default; with no
// repository listed, turning the timer on takes over nothing new.
func TestStageLabelsOpPersonTimerOffAndDefault(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: stageGovernor()}), WriteOpGovernorStageLabels, map[string]any{"person_after": "48h"})
	if isWidening(p) || bodyJSON(t, p.Request.Body) != `{"stage_adoption_person_after":"48h"}` {
		t.Fatalf("disclosure %q body %s", p.WideningDisclosure, bodyJSON(t, p.Request.Body))
	}
	g := stageGovernor()
	g["stageAdoptionPersonAfter"] = "48h"
	g["stageAdoptionPersonGrace"] = "6h"
	g["stageAdoptionPersonRepos"] = []string{"example/testrepo"}
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpGovernorStageLabels, map[string]any{"person_after": "0", "person_grace": "0"})
	if got := bodyJSON(t, p.Request.Body); got != `{"stage_adoption_person_after":"","stage_adoption_person_grace":""}` {
		t.Fatalf("off body = %s", got)
	}
	if isWidening(p) {
		t.Fatalf("turning the timer off read as widening: %q", p.WideningDisclosure)
	}
	// Shortening the timer or the grace where a repository is listed brings
	// takeovers sooner.
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpGovernorStageLabels, map[string]any{"person_after": "12h"})
	if !isWidening(p) || !strings.Contains(p.WideningDisclosure, "after 12h instead of 48h") {
		t.Fatalf("shorter timer disclosure = %q", p.WideningDisclosure)
	}
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpGovernorStageLabels, map[string]any{"person_grace": "1h"})
	if !isWidening(p) || !strings.Contains(p.WideningDisclosure, "after 1h instead of 6h") {
		t.Fatalf("shorter grace disclosure = %q", p.WideningDisclosure)
	}
}

// A hive whose settings lack the timer keys is refused rather than guessed at.
func TestStageLabelsOpNeedsThePersonTimerSettings(t *testing.T) {
	g := stageGovernor()
	delete(g, "stageAdoptionPersonRepos")
	if _, err := surfaceHandler(&surfaceProvider{governor: g}).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorStageLabels, "args": map[string]any{"person_after": "1h"}}); err == nil || !strings.Contains(err.Error(), "stageAdoptionPersonRepos") {
		t.Fatalf("missing key: err = %v", err)
	}
}
