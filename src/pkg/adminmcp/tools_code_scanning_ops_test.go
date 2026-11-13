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

// Tests for the code_scanning.settings write operation and the code_scanning
// settings_read section. Before them the admin MCP had no way to read or
// change the code scanning sweep settings.

// codeScanningSettingsProvider answers settings_read section code_scanning
// with a copy of settings, decoded from JSON as the endpoint's answer is, and
// records writes.
type codeScanningSettingsProvider struct {
	settings map[string]any
	readErr  error
	requests []WriteRequest
}

func (p *codeScanningSettingsProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != codeScanningSettingsSection {
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

func (p *codeScanningSettingsProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"ok": true}, nil
}

// codeScanningSettingsFixture is GET /api/config/code-scanning for a hive
// under org "org" with three project repositories: the sweep off hive-wide,
// on in org/a by an override that also sets a severity, a tool and a cap, and
// pinned off in org/b (stored under its bare name).
func codeScanningSettingsFixture() map[string]any {
	return map[string]any{
		"org":                              "org",
		"enabled":                          false,
		"interval":                         "",
		"interval_effective":               "1h0m0s",
		"min_severity":                     "",
		"min_severity_effective":           "high",
		"include_tools":                    []string{},
		"exclude_rules":                    []string{},
		"max_new_issues_per_run":           0,
		"max_new_issues_per_run_effective": 5,
		"repos": map[string]any{
			"org/a": map[string]any{"enabled": true, "min_severity": "medium", "include_tools": []string{"CodeQL"}, "exclude_rules": []string{}, "max_new_issues_per_run": 2},
			"b":     map[string]any{"enabled": false, "min_severity": "", "include_tools": []string{}, "exclude_rules": []string{}, "max_new_issues_per_run": 0},
		},
		"project_repos": []any{
			map[string]any{"repo": "org/a", "enabled": true, "enabled_from": "repo", "alerts_access": "not_checked"},
			map[string]any{"repo": "org/b", "enabled": false, "enabled_from": "repo", "alerts_access": "off"},
			map[string]any{"repo": "org/c", "enabled": false, "enabled_from": "default", "alerts_access": "off"},
		},
	}
}

func codeScanningSettingsHandler(p *codeScanningSettingsProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func codeScanningSettingsPreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpCodeScanningSettings, "args": args})
	if err != nil {
		t.Fatalf("preview %v: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

// The write operation is registered, and the settings are readable as a
// settings_read section through the endpoint the dashboard reads.
func TestCodeScanningSettingsRegisteredAndReadable(t *testing.T) {
	if _, ok := DefaultWriteRegistry().Get(WriteOpCodeScanningSettings); !ok {
		t.Fatalf("%s is not registered", WriteOpCodeScanningSettings)
	}
	path, err := SettingsReadPath(map[string]any{"section": codeScanningSettingsSection})
	if err != nil || path != codeScanningSettingsPath {
		t.Fatalf("settings_read section %s reads %q, %v; want %s", codeScanningSettingsSection, path, err, codeScanningSettingsPath)
	}
	top, repo := CodeScanningSettingsBodyFields()
	props, _ := (codeScanningSettingsOp{}).InputSchema()["properties"].(map[string]any)
	if len(props) != len(top) {
		t.Fatalf("the schema offers %d settings, the op takes %d", len(props), len(top))
	}
	for _, f := range top {
		if _, ok := props[f]; !ok {
			t.Errorf("the schema does not offer %q", f)
		}
	}
	if len(repo) != 5 {
		t.Fatalf("a repos entry takes %v, want its five override fields", repo)
	}
}

// A malformed argument, and a value hive.yaml loading would refuse, are both
// refused at write_preview, before a confirmation is minted or anything is
// written. The second kind carries hive.yaml loading's own message.
func TestCodeScanningSettingsRefusesBadArgumentsAtPreview(t *testing.T) {
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"no arguments":               {map[string]any{}, "at least one code scanning setting"},
		"top-level typo":             {map[string]any{"enable": true}, `unknown field "enable"`},
		"enabled not a boolean":      {map[string]any{"enabled": "yes"}, "unsupported or malformed code scanning setting"},
		"empty repos":                {map[string]any{"repos": map[string]any{}}, "repos takes"},
		"repo value not an object":   {map[string]any{"repos": map[string]any{"org/a": true}}, "takes the override fields to set"},
		"repo value an empty object": {map[string]any{"repos": map[string]any{"org/a": map[string]any{}}}, "takes the override fields to set"},
		"unknown repo field":         {map[string]any{"repos": map[string]any{"org/a": map[string]any{"llm": true}}}, `unknown field "llm"`},
		"bad repo name":              {map[string]any{"repos": map[string]any{"a/b/c": map[string]any{"enabled": true}}}, "is not an owner/repo name"},
		"same repository twice":      {map[string]any{"repos": map[string]any{"org/a": map[string]any{"enabled": true}, "A": map[string]any{"enabled": false}}}, "the same repository twice"},
		"interval not a duration":    {map[string]any{"interval": "soon"}, "interval must be a duration"},
		"interval under ten minutes": {map[string]any{"interval": "5m"}, "code_scanning.interval: must be at least 10m0s, got 5m0s"},
		"unknown severity":           {map[string]any{"min_severity": "severe"}, `code_scanning.min_severity: "severe" is not one of critical, high, medium, low, note (or error, warning)`},
		"negative cap":               {map[string]any{"max_new_issues_per_run": -1}, "code_scanning.max_new_issues_per_run: must not be negative, got -1"},
		"malformed rule pattern":     {map[string]any{"exclude_rules": []any{"cpp/["}}, "code_scanning.exclude_rules: bad rule pattern cpp/[: syntax error in pattern"},
		"repository not in project":  {map[string]any{"repos": map[string]any{"org/zzz": map[string]any{"enabled": true}}}, `code_scanning.repos: "org/zzz" is not listed in project.repos`},
		"per-repository severity":    {map[string]any{"repos": map[string]any{"org/c": map[string]any{"min_severity": "x"}}}, `code_scanning.repos[org/c].min_severity: "x" is not one of`},
		"nothing to change":          {map[string]any{"enabled": false, "repos": map[string]any{"org/b": map[string]any{"enabled": false}}}, "nothing would change"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &codeScanningSettingsProvider{settings: codeScanningSettingsFixture()}
			h := codeScanningSettingsHandler(p)
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpCodeScanningSettings, "args": tc.args})
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
// sends every other repository's override back unchanged, and a repository
// set back to "inherit" keeps its severity, tool and cap overrides. Nothing
// is written until write_confirm, which sends exactly the previewed body.
func TestCodeScanningSettingsOneRepoChangeKeepsTheOthersAndWritesOnlyOnConfirm(t *testing.T) {
	p := &codeScanningSettingsProvider{settings: codeScanningSettingsFixture()}
	h := codeScanningSettingsHandler(p)
	preview, id := codeScanningSettingsPreview(t, h, map[string]any{"repos": map[string]any{
		"c":     map[string]any{"enabled": true},
		"org/a": map[string]any{"enabled": nil},
	}})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/code-scanning" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	want := `{"repos":{` +
		`"b":{"enabled":false,"exclude_rules":[],"include_tools":[],"max_new_issues_per_run":0,"min_severity":""},` +
		`"c":{"enabled":true,"exclude_rules":[],"include_tools":[],"max_new_issues_per_run":0,"min_severity":""},` +
		`"org/a":{"enabled":null,"exclude_rules":[],"include_tools":["CodeQL"],"max_new_issues_per_run":2,"min_severity":"medium"}}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	changes, _ := preview.Details["changes"].([]map[string]any)
	if len(changes) != 2 ||
		changes[0]["field"] != "repos[c]" || changes[0]["before"] != "no override" || changes[0]["after"] != "enabled=true" ||
		changes[1]["field"] != "repos[org/a]" || changes[1]["before"] != `enabled=true min_severity=medium include_tools=["CodeQL"] max_new_issues_per_run=2` ||
		changes[1]["after"] != `enabled=inherit min_severity=medium include_tools=["CodeQL"] max_new_issues_per_run=2` {
		t.Fatalf("changes = %+v", changes)
	}
	// org/c is turned on; org/a now follows the hive-wide switch, which is off.
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening: the code scanning sweep is turned on for org/c.") {
		t.Fatalf("widening disclosure = %q", preview.WideningDisclosure)
	}
	if preview.StateFingerprint == "" {
		t.Fatal("the preview carries no state fingerprint, so a stale confirm could not be refused")
	}
	if len(p.requests) != 0 {
		t.Fatalf("write_preview wrote: %+v", p.requests)
	}

	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || p.requests[0].Method != http.MethodPut || p.requests[0].Path != "/api/config/code-scanning" || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want one PUT of the previewed body", p.requests)
	}
	// A confirmation is one-use.
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil || len(p.requests) != 1 {
		t.Fatalf("a second confirm went through: err=%v requests=%d", err, len(p.requests))
	}
}

// Only what changes is sent: a top-level key already at the requested value
// is left out, null drops a repository's whole override, and a cleared list
// goes as [] (null would leave it alone).
func TestCodeScanningSettingsSendsOnlyWhatChanges(t *testing.T) {
	p := &codeScanningSettingsProvider{settings: codeScanningSettingsFixture()}
	h := codeScanningSettingsHandler(p)
	p.settings["include_tools"] = []string{"Trivy"}
	preview, _ := codeScanningSettingsPreview(t, h, map[string]any{
		"enabled":       false, // already off
		"interval":      "30m",
		"min_severity":  "low",
		"include_tools": []any{},
		"repos":         map[string]any{"b": nil},
	})
	want := `{"include_tools":[],"interval":"30m0s","min_severity":"low",` +
		`"repos":{"org/a":{"enabled":true,"exclude_rules":[],"include_tools":["CodeQL"],"max_new_issues_per_run":2,"min_severity":"medium"}}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	// The sweep stays on in org/a and the severity floor drops, so it may file more there.
	if !strings.HasPrefix(preview.WideningDisclosure, "Possible widening:") {
		t.Fatalf("widening disclosure = %q", preview.WideningDisclosure)
	}

	// Turning the only enabled repository off widens nothing.
	off, _ := codeScanningSettingsPreview(t, h, map[string]any{"repos": map[string]any{"org/a": map[string]any{"enabled": false}}})
	if !strings.HasPrefix(off.WideningDisclosure, "No widening:") {
		t.Fatalf("turning the sweep off disclosed %q", off.WideningDisclosure)
	}
}

// write_confirm reads the settings again. A setting changed on the hive after
// the preview makes the confirmation stale and nothing is written; a sweep
// pass that only changes what the hive reports about access does not.
func TestCodeScanningSettingsRefusesAStaleConfirmation(t *testing.T) {
	p := &codeScanningSettingsProvider{settings: codeScanningSettingsFixture()}
	h := codeScanningSettingsHandler(p)
	args := map[string]any{"repos": map[string]any{"org/c": map[string]any{"enabled": true}}}

	_, id := codeScanningSettingsPreview(t, h, args)
	p.settings["min_severity"] = "low" // changed by someone else after the preview
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm after the settings moved: err = %v, want %v", err, ErrConfirmationStale)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}

	_, id = codeScanningSettingsPreview(t, h, args)
	p.settings["project_repos"] = []any{
		map[string]any{"repo": "org/a", "enabled": true, "enabled_from": "repo", "alerts_access": "readable", "checked_at": "2026-11-13T15:00:00Z"},
		map[string]any{"repo": "org/b", "enabled": false, "enabled_from": "repo", "alerts_access": "off"},
		map[string]any{"repo": "org/c", "enabled": false, "enabled_from": "default", "alerts_access": "off"},
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm after a sweep pass only updated the access state: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("executed %+v, want one write", p.requests)
	}
}

// The settings are owner only. A caller the hive does not verify as owner
// cannot read them, so the preview is refused with no confirmation minted;
// and a confirmation whose re-read is refused writes nothing.
func TestCodeScanningSettingsRefusesNonOwner(t *testing.T) {
	ownerOnly := fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
	p := &codeScanningSettingsProvider{settings: codeScanningSettingsFixture(), readErr: ownerOnly}
	h := codeScanningSettingsHandler(p)
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpCodeScanningSettings, "args": map[string]any{"enabled": true}})
	if err == nil || !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "verified owner") {
		t.Fatalf("non-owner preview err = %v", err)
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a non-owner preview minted a confirmation: %+v", pending)
	}

	p.readErr = nil
	_, id := codeScanningSettingsPreview(t, h, map[string]any{"enabled": true})
	p.readErr = ownerOnly
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil {
		t.Fatal("a confirm whose re-read was refused went through")
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", p.requests)
	}
}

// With writes off, the operation is refused like every other write.
func TestCodeScanningSettingsRefusedWhileWritesAreOff(t *testing.T) {
	p := &codeScanningSettingsProvider{settings: codeScanningSettingsFixture()}
	h := NewHandler(p, WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpCodeScanningSettings, "args": map[string]any{"enabled": true}})
	if !errors.Is(err, ErrWritesDisabled) {
		t.Fatalf("preview with writes off: err = %v, want %v", err, ErrWritesDisabled)
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote with writes off: %+v", p.requests)
	}
}
