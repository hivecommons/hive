package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// #9587 phase 2: the owner-only write-surface allowlist editor.

const writeSurfacePath = "/api/config/write-surface"

func decodeWriteSurface(t *testing.T, body []byte) writeSurfaceSectionResponse {
	t.Helper()
	var got writeSurfaceSectionResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func TestWriteSurfaceGet_DefaultRestrictsNothing(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, writeSurfacePath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rec.Code)
	}
	// The client contract: allowlist and warnings are always present (never
	// null), so the UI needs no null checks.
	if !strings.Contains(rec.Body.String(), `"allowlist":{}`) || !strings.Contains(rec.Body.String(), `"warnings":[]`) {
		t.Fatalf("default body must carry an empty allowlist object and warnings array: %s", rec.Body.String())
	}
	got := decodeWriteSurface(t, rec.Body.Bytes())
	wantOps := append(append([]string{}, config.KnownWriteOps...), config.WriteSurfaceAllowAll)
	if !reflect.DeepEqual(got.Ops, wantOps) {
		t.Fatalf("ops = %v, want %v", got.Ops, wantOps)
	}
}

func TestWriteSurfacePut_RoundTripNarrowsOnlyListedLanes(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config

	rec := doPut(s, writeSurfacePath, map[string]any{"allowlist": map[string][]string{
		"scanner": {"Comment", "create_issue", "comment"},
		"ghost":   {"*", "merge_pr"},
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeWriteSurface(t, rec.Body.Bytes())
	want := map[string][]string{"scanner": {"create_issue", "comment"}, "ghost": {config.WriteSurfaceAllowAll}}
	if !reflect.DeepEqual(got.Allowlist, want) {
		t.Fatalf("PUT echoed %v, want canonical %v", got.Allowlist, want)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], `"ghost"`) {
		t.Fatalf("a lane naming no configured agent must be warned about: %v", got.Warnings)
	}

	// In force for the very next relay check, and only for the listed lane.
	if cfg.AgentMayWrite("scanner", "merge_pr") || !cfg.AgentMayWrite("scanner", "comment") {
		t.Fatal("the saved allowlist is not what the relays enforce")
	}
	if !cfg.AgentMayWrite("reviewer", "merge_pr") {
		t.Fatal("an unlisted lane must stay unrestricted")
	}

	// GET returns what PUT stored.
	back := decodeWriteSurface(t, doOwnerGet(s, writeSurfacePath).Body.Bytes())
	if !reflect.DeepEqual(back.Allowlist, want) {
		t.Fatalf("GET after PUT = %v, want %v", back.Allowlist, want)
	}

	// The change is audited without the operation lists.
	entries := s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "config_write_surface")
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "lanes=2") || !strings.Contains(entries[0].Detail, "restricted=ghost+scanner") {
		t.Fatalf("config change not audited as expected: %+v", entries)
	}

	// An empty object clears it: nothing restricted again.
	if rec := doPut(s, writeSurfacePath, map[string]any{"allowlist": map[string][]string{}}); rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d", rec.Code)
	}
	if cfg.WriteSurfaceAllowlist() != nil || !cfg.AgentMayWrite("scanner", "merge_pr") {
		t.Fatal("clearing did not restore unrestricted behaviour")
	}
}

// An allow-nothing lane is expressible through the API and enforced.
func TestWriteSurfacePut_EmptyListAllowsNothing(t *testing.T) {
	s := covApiServer(t)
	rec := doPutRaw(s, writeSurfacePath, `{"allowlist":{"scanner":[]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"scanner":[]`) {
		t.Fatalf("an allow-nothing lane must echo as an empty list: %s", rec.Body.String())
	}
	for _, op := range config.KnownWriteOps {
		if s.deps.Config.AgentMayWrite("scanner", op) {
			t.Errorf("allow-nothing lane may still %s", op)
		}
	}
}

func TestWriteSurfacePut_ValidatesBeforeMutating(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.SetWriteSurfaceAllowlist(map[string][]string{"scanner": {"comment"}})
	before := cfg.WriteSurfaceAllowlist()

	cases := []struct {
		name, body, wantErr string
	}{
		{"malformed", `{nope`, "invalid body"},
		{"missing key", `{}`, "allowlist is required"},
		{"null allowlist", `{"allowlist":null}`, "allowlist is required"},
		{"unknown op", `{"allowlist":{"scanner":["delete_repo"]}}`, "unknown operation"},
		{"bad lane", `{"allowlist":{"<img src=x>":["comment"]}}`, "may contain only"},
		{"wrong type", `{"allowlist":{"scanner":"comment"}}`, "invalid body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPutRaw(s, writeSurfacePath, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", rec.Body.String(), tc.wantErr)
			}
			if got := cfg.WriteSurfaceAllowlist(); !reflect.DeepEqual(got, before) {
				t.Fatalf("a rejected PUT changed the allowlist: %v", got)
			}
		})
	}
	if entries := s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "config_write_surface"); len(entries) != 0 {
		t.Fatalf("a rejected PUT must not be audited as a change: %+v", entries)
	}
}

func TestWriteSurfaceOwnerGate(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, writeSurfacePath); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, writeSurfacePath, `{"allowlist":{"scanner":[]}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.WriteSurfaceAllowlist() != nil {
		t.Fatal("a refused PUT still changed the allowlist")
	}
}

func TestWriteSurfaceSection_WarnsUnknownOpFromYAML(t *testing.T) {
	cfg := &config.Config{
		Agents:       map[string]config.AgentConfig{"scanner": {}},
		WriteSurface: config.WriteSurfaceConfig{Allowlist: map[string][]string{"scanner": {"comennt"}, "scanner-2": {"comment"}}},
	}
	got := writeSurfaceSection(cfg)
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "comennt") {
		t.Fatalf("want exactly the unknown-op warning (a replica of a configured agent is fine): %v", got.Warnings)
	}
}

func TestWriteSurfaceLaneConfigured(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"scanner": {}}}
	for lane, want := range map[string]bool{"scanner": true, "scanner-3": true, "reviewer": false, "reviewer-2": false} {
		if got := writeSurfaceLaneConfigured(cfg, lane); got != want {
			t.Errorf("writeSurfaceLaneConfigured(%q) = %v, want %v", lane, got, want)
		}
	}
	if writeSurfaceLaneConfigured(nil, "scanner") {
		t.Error("nil config configures nothing")
	}
}

func TestWriteSurfaceNoConfig(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config = nil
	if rec := doOwnerGet(s, writeSurfacePath); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET without config: expected 503, got %d", rec.Code)
	}
	if rec := doPutRaw(s, writeSurfacePath, `{"allowlist":{}}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT without config: expected 503, got %d", rec.Code)
	}
}

// The Security tab renders the editor from its own endpoint, with text-only
// DOM rendering: no function in the editor may assign innerHTML, and every
// helper it calls must be defined (the renderAll()/hiveToast() bug class).
func TestWriteSurfaceUIIsTextOnlyAndWired(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, snippet := range []string{
		`${renderSecuritySection('Write Surface', writeSurface)}`,
		`id="gov-write-surface-host"`,
		`fetch('/api/config/write-surface')`,
		`fetch('/api/config/write-surface', {`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	for _, fn := range []string{
		"async function loadWriteSurfaceAllowlist()",
		"function writeSurfaceNode(tag, style, text)",
		"function writeSurfaceAllowlistToText(allowlist)",
		"function parseWriteSurfaceText(text)",
		"function renderWriteSurfacePanel(host, d)",
	} {
		body := jsFunctionBody(t, html, fn)
		if strings.Contains(body, "innerHTML") || strings.Contains(body, "insertAdjacentHTML") {
			t.Errorf("%s must render text only, but uses HTML injection", fn)
		}
	}
	for _, fn := range []string{"_inceptionAuthHeaders", "saveErrorMessage", "showToast", "renderSecuritySection"} {
		defined := regexp.MustCompile(`function\s+` + regexp.QuoteMeta(fn) + `\s*\(`)
		if !defined.MatchString(html) {
			t.Errorf("the write-surface editor calls %s() but index.html never defines it", fn)
		}
	}
}
