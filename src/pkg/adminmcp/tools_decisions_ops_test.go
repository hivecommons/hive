package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// delegateSettingsProvider answers settings_read section decisions_delegate
// with a copy of settings, decoded from JSON as the endpoint's answer is, and
// records writes.
type delegateSettingsProvider struct {
	settings map[string]any
	readErr  error
	requests []WriteRequest
}

func (p *delegateSettingsProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != delegateSettingsSection {
		return map[string]any{}, nil
	}
	if p.readErr != nil {
		return nil, p.readErr
	}
	b, err := json.Marshal(p.settings)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *delegateSettingsProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"ok": true}, nil
}

// delegateSettingsFixture is GET /api/config/decisions/delegate for a hive
// with three project repositories: the delegate on hive-wide, off in org/a by
// override and pinned on in org/b, and the LLM step off.
func delegateSettingsFixture() map[string]any {
	return map[string]any{
		"enabled":     true,
		"enabled_set": false,
		"repos": map[string]any{
			"org/a": map[string]any{"enabled": false},
			"org/b": map[string]any{"enabled": true},
		},
		"llm":                      map[string]any{"enabled": false, "model": "", "model_masked": false},
		"llm_reviewer_ready":       true,
		"llm_model_effective":      "reviewer-model-1",
		"effect_timeout":           "",
		"effect_timeout_effective": "30m0s",
		"project_repos":            []string{"org/a", "org/b", "org/c"},
	}
}

func delegateSettingsHandler(p *delegateSettingsProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func delegateSettingsPreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpDecisionsDelegateSettings, "args": args})
	if err != nil {
		t.Fatalf("preview %v: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func TestDecisionsDelegateSettingsRegistered(t *testing.T) {
	if _, ok := DefaultWriteRegistry().Get(WriteOpDecisionsDelegateSettings); !ok {
		t.Fatalf("%s is not registered", WriteOpDecisionsDelegateSettings)
	}
}

// An argument the endpoint does not take is refused before anything is read
// or minted, at the top level and inside llm and repos. A key pasted as the
// model is refused the same way, so it never reaches a stored confirmation.
func TestDecisionsDelegateSettingsRejectsUnknownFields(t *testing.T) {
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"top-level typo":       {map[string]any{"enable": true}, `unsupported Let hive decide setting "enable"`},
		"dotted top-level":     {map[string]any{"llm.enabled": true}, `unsupported Let hive decide setting "llm.enabled"`},
		"unknown llm field":    {map[string]any{"llm": map[string]any{"enabled": true, "api_key_env": "KEY"}}, `unsupported Let hive decide setting "llm.api_key_env"`},
		"repo value not bool":  {map[string]any{"repos": map[string]any{"org/a": "yes"}}, "must be true, false or null"},
		"repo value as object": {map[string]any{"repos": map[string]any{"org/a": map[string]any{"enabled": true}}}, "must be true, false or null"},
		"bad repo name":        {map[string]any{"repos": map[string]any{"a/b/c": true}}, "is not an owner/repo name"},
		"key as model":         {map[string]any{"llm": map[string]any{"model": "sk-" + strings.Repeat("q7", 16)}}, "looks like an API key"},
		"bad duration":         {map[string]any{"effect_timeout": "-1h"}, "above zero"},
		"no arguments":         {map[string]any{}, "at least one"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &delegateSettingsProvider{settings: delegateSettingsFixture()}
			h := delegateSettingsHandler(p)
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpDecisionsDelegateSettings, "args": tc.args})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
				t.Fatalf("a refused preview minted a confirmation: %+v", pending)
			}
			if len(p.requests) != 0 {
				t.Fatalf("a refused preview wrote: %+v", p.requests)
			}
		})
	}
}

// The endpoint replaces the override map whole, so a change to one repository
// sends every other repository's override back unchanged. Dropping org/a's
// "off" override lets the delegate act there again (the hive-wide switch is
// on), and the preview says so; adding org/c "on" changes nothing it could do.
func TestDecisionsDelegateSettingsOneRepoChangeKeepsTheOthers(t *testing.T) {
	p := &delegateSettingsProvider{settings: delegateSettingsFixture()}
	h := delegateSettingsHandler(p)
	preview, id := delegateSettingsPreview(t, h, map[string]any{"repos": map[string]any{"c": true, "org/a": nil}})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/decisions/delegate" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	want := `{"repos":{"org/b":{"enabled":true},"org/c":{"enabled":true}}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	changes, _ := preview.Details["changes"].([]map[string]any)
	if len(changes) != 2 || changes[0]["field"] != "repos[org/c]" || changes[0]["before"] != "inherit" || changes[0]["after"] != true ||
		changes[1]["field"] != "repos[org/a]" || changes[1]["before"] != false || changes[1]["after"] != "inherit" {
		t.Fatalf("changes = %+v, want repos[org/c] inherit → true and repos[org/a] false → inherit", changes)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening:") || !strings.Contains(preview.WideningDisclosure, "turned on for org/a.") {
		t.Fatalf("dropping org/a's off override must be disclosed as widening for org/a only: %s", preview.WideningDisclosure)
	}
	if !strings.Contains(preview.WideningDisclosure, "merges anything") {
		t.Fatalf("disclosure must say the change merges nothing: %s", preview.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want body %s", p.requests, want)
	}
}

// Turning the LLM step on sends item text to a model, so the preview names
// the model and what is sent; when no reviewer endpoint is configured it says
// the step cannot run yet. Turning it off is not widening.
func TestDecisionsDelegateSettingsDisclosesLLM(t *testing.T) {
	p := &delegateSettingsProvider{settings: delegateSettingsFixture()}
	preview, _ := delegateSettingsPreview(t, delegateSettingsHandler(p), map[string]any{"llm": map[string]any{"enabled": true}})
	if got := compactJSON(preview.Request.Body); got != `{"llm":{"enabled":true}}` {
		t.Fatalf("body = %s", got)
	}
	d := preview.WideningDisclosure
	if !strings.HasPrefix(d, "Widening:") || !strings.Contains(d, "item's text") || !strings.Contains(d, "reviewer-model-1") || !strings.Contains(d, "merges anything") {
		t.Fatalf("turning on llm must disclose that item text goes to reviewer-model-1 and that nothing merges: %s", d)
	}
	if strings.Contains(d, "No reviewer endpoint") {
		t.Fatalf("the reviewer is ready, so the preview must not say it is missing: %s", d)
	}

	notReady := delegateSettingsFixture()
	notReady["llm_reviewer_ready"] = false
	preview, _ = delegateSettingsPreview(t, delegateSettingsHandler(&delegateSettingsProvider{settings: notReady}), map[string]any{"llm": map[string]any{"enabled": true, "model": "provider/decider-2"}})
	if !strings.Contains(preview.WideningDisclosure, "provider/decider-2") || !strings.Contains(preview.WideningDisclosure, "No reviewer endpoint") {
		t.Fatalf("disclosure must name the new model and the missing reviewer: %s", preview.WideningDisclosure)
	}

	on := delegateSettingsFixture()
	on["llm"] = map[string]any{"enabled": true, "model": "", "model_masked": false}
	preview, _ = delegateSettingsPreview(t, delegateSettingsHandler(&delegateSettingsProvider{settings: on}), map[string]any{"llm": map[string]any{"enabled": false}})
	if !strings.HasPrefix(preview.WideningDisclosure, "No widening:") {
		t.Fatalf("turning llm off is not widening: %s", preview.WideningDisclosure)
	}
}

// A preview is built from the overrides it read. If one changes before
// write_confirm, sending the preview's merged map would undo that change, so
// the confirm is refused and nothing is written. A change to a setting the
// preview does not send does not block it.
func TestDecisionsDelegateSettingsRefusesStalePreview(t *testing.T) {
	p := &delegateSettingsProvider{settings: delegateSettingsFixture()}
	h := delegateSettingsHandler(p)
	args := map[string]any{"repos": map[string]any{"org/c": false}}

	_, id := delegateSettingsPreview(t, h, args)
	p.settings["repos"] = map[string]any{"org/a": map[string]any{"enabled": false}} // someone dropped org/b meanwhile
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}

	preview, id := delegateSettingsPreview(t, h, args)
	want := `{"repos":{"org/a":{"enabled":false},"org/c":{"enabled":false}}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("fresh preview body = %s", got)
	}
	p.settings["effect_timeout"] = "1h0m0s" // not sent, so not stale
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm after an unrelated change: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v", p.requests)
	}
}

// The settings are owner only. A caller the hive does not verify as owner
// cannot read them, so the preview is refused with no confirmation minted;
// and a confirmation whose re-read is refused writes nothing.
func TestDecisionsDelegateSettingsRefusesNonOwner(t *testing.T) {
	ownerOnly := fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
	p := &delegateSettingsProvider{settings: delegateSettingsFixture(), readErr: ownerOnly}
	h := delegateSettingsHandler(p)
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpDecisionsDelegateSettings, "args": map[string]any{"enabled": false}})
	if err == nil || !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "verified owner") {
		t.Fatalf("non-owner preview err = %v", err)
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a non-owner preview minted a confirmation: %+v", pending)
	}

	p.readErr = nil
	_, id := delegateSettingsPreview(t, h, map[string]any{"enabled": false})
	p.readErr = ownerOnly
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil {
		t.Fatal("a confirm whose re-read was refused went through")
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", p.requests)
	}
}
