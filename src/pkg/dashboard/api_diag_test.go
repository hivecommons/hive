package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logread"
)

const diagOwnSecret = "diagOwnSecretValue123456"

// diagGet serves path through the mux with the given role; verified marks the
// request as a hive-verified owner.
func diagGet(s *Server, path, role string, verified bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeDiagText(t *testing.T, rec *httptest.ResponseRecorder) diagTextResult {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() > adminmcp.DiagResultByteCap {
		t.Fatalf("result is %d bytes, over the %d cap", rec.Body.Len(), adminmcp.DiagResultByteCap)
	}
	var res diagTextResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return res
}

func diagRedacted(list []logread.Redaction, kind string) int {
	for _, r := range list {
		if r.Kind == kind {
			return r.Count
		}
	}
	return 0
}

func diagLogServer(t *testing.T, log string) *Server {
	t.Helper()
	s, _ := apiServer(t)
	s.authToken = diagOwnSecret
	s.captureFullLogFn = func(string) (string, error) { return log, nil }
	return s
}

func TestDiagAgentLogBoundsScrubAndDisclose(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 300*1024; i++ {
		fmt.Fprintf(&b, "line %06d some terminal output here\n", i)
	}
	b.WriteString("auth used " + diagOwnSecret + " near the end\n")
	b.WriteString("final line\n")
	s := diagLogServer(t, b.String())

	res := decodeDiagText(t, diagGet(s, "/api/diag/agents/scanner/log", "", false))
	if res.Type != "diag_text" || res.Read != "agent_log" || !strings.Contains(res.Untrusted, "UNTRUSTED") {
		t.Fatalf("shape = %+v", res)
	}
	if !res.Truncated || res.MaxBytes != adminmcp.DiagDefaultTextBytes || res.BytesReturned > adminmcp.DiagDefaultTextBytes || res.BytesReturned != len(res.Text) {
		t.Fatalf("bounds: truncated=%v max=%d returned=%d", res.Truncated, res.MaxBytes, res.BytesReturned)
	}
	if res.BytesTotal <= 300*1024 || !strings.HasPrefix(res.ContentType, "text/plain") {
		t.Fatalf("full size %d, content type %q", res.BytesTotal, res.ContentType)
	}
	if strings.Contains(res.Text, diagOwnSecret) || diagRedacted(res.Redactions, logread.SecretValueKind) != 1 {
		t.Fatalf("own secret not scrubbed: %+v", res.Redactions)
	}
	if !strings.HasSuffix(res.Text, "final line\n") || strings.HasPrefix(res.Text, "ine") {
		t.Fatalf("newest part not kept on a line boundary: %q ... %q", res.Text[:40], res.Text[len(res.Text)-20:])
	}

	clamped := decodeDiagText(t, diagGet(s, "/api/diag/agents/scanner/log?max_bytes=99999999", "", false))
	if !clamped.MaxBytesClamped || clamped.MaxBytes != adminmcp.DiagMaxTextBytes || !clamped.Truncated {
		t.Fatalf("clamp: %+v", clamped.MaxBytes)
	}
	if rec := diagGet(s, "/api/diag/agents/scanner/log?max_bytes=nope", "", false); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad max_bytes status = %d", rec.Code)
	}

	// The existing plain-text endpoint is unchanged.
	plain := doGet(s, "/api/agents/scanner/log")
	if plain.Code != http.StatusOK || !strings.HasPrefix(plain.Header().Get("Content-Type"), "text/plain") || strings.HasPrefix(plain.Body.String(), "{") {
		t.Fatalf("plain log changed: %d %q", plain.Code, plain.Header().Get("Content-Type"))
	}
}

func TestDiagAgentLogSmallIsWhole(t *testing.T) {
	s := diagLogServer(t, "one\ntwo\n")
	res := decodeDiagText(t, diagGet(s, "/api/diag/agents/scanner/log?explain=hide&max_bytes=4096", "", false))
	if res.Truncated || res.BytesReturned != res.BytesTotal || !strings.HasSuffix(res.Text, "one\ntwo\n") || res.MaxBytes != 4096 {
		t.Fatalf("small log: %+v", res)
	}
	if res.Redactions == nil {
		t.Fatal("redactions must be a list, never null")
	}
}

func TestDiagAgentLogRefusalKeepsStatus(t *testing.T) {
	s, _ := apiServer(t)
	s.captureFullLogFn = func(string) (string, error) { return "", errors.New("pane text " + diagOwnSecret) }
	rec := diagGet(s, "/api/diag/agents/scanner/log", "", false)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), diagOwnSecret) || strings.Contains(rec.Body.String(), "pane text") {
		t.Fatalf("refusal = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDiagAgentKicksListAndRead(t *testing.T) {
	s, id := kickHistoryServer(t, "kick started\nused "+diagOwnSecret+"\nkick done\n")
	s.authToken = diagOwnSecret

	rec := diagGet(s, "/api/diag/agents/scanner/kicks", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("kicks status = %d %s", rec.Code, rec.Body.String())
	}
	var list map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if kicks, _ := list["kicks"].([]any); len(kicks) != 1 || list["truncated"] != false || list["agent"] != "scanner" || list["read"] != "agent_kicks" {
		t.Fatalf("kicks = %s", rec.Body.String())
	}

	res := decodeDiagText(t, diagGet(s, "/api/diag/agents/scanner/kicks/"+id, "", false))
	if res.Read != "agent_kick" || !strings.Contains(res.Text, "kick done") || strings.Contains(res.Text, diagOwnSecret) || res.Truncated {
		t.Fatalf("kick read = %+v", res)
	}

	if rec := diagGet(s, "/api/diag/agents/nonexistent/kicks", "", false); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d", rec.Code)
	}
	if rec := diagGet(s, "/api/diag/agents/scanner/kicks/no-such-kick.log", "", false); rec.Code == http.StatusOK {
		t.Fatalf("unknown kick status = %d", rec.Code)
	}
}

func TestDiagRunLogKeepsReadRole(t *testing.T) {
	s, _ := apiServer(t)
	if rec := diagGet(s, "/api/diag/runs/r1/log", "bogus", false); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "role does not allow") {
		t.Fatalf("invalid role = %d %s", rec.Code, rec.Body.String())
	}
	if rec := diagGet(s, "/api/diag/runs/r1/log?stage=..%2Fx", config.RoleRead, false); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid stage = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDiagRunDetailPassesStatusThrough(t *testing.T) {
	s, _ := apiServer(t)
	inner := diagGet(s, "/api/runs/no-such-run-xyz/detail", config.RoleRead, false)
	rec := diagGet(s, "/api/diag/runs/no-such-run-xyz/detail", config.RoleRead, false)
	if inner.Code == http.StatusOK || rec.Code != inner.Code || !strings.Contains(rec.Body.String(), "run_detail") {
		t.Fatalf("inner %d, diag %d %s", inner.Code, rec.Code, rec.Body.String())
	}
}

func TestDiagHealthIsWhole(t *testing.T) {
	s, _ := apiServer(t)
	inner := diagGet(s, "/api/health/deep", "", false)
	rec := diagGet(s, "/api/diag/health", "", false)
	if rec.Code != inner.Code {
		t.Fatalf("diag %d, inner %d", rec.Code, inner.Code)
	}
	if inner.Code != http.StatusOK {
		return
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	health, _ := res["health"].(map[string]any)
	if health["checks"] == nil || res["truncated"] != false || res["read"] != "health_deep" {
		t.Fatalf("health = %s", rec.Body.String())
	}
}

func diagPromptServer(t *testing.T, n, promptBytes int) *Server {
	t.Helper()
	s, _ := apiServer(t)
	s.authToken = diagOwnSecret
	s.promptHistory = newTestPromptHistory()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		prompt := fmt.Sprintf("prompt %d uses %s ", i, diagOwnSecret) + strings.Repeat("p", promptBytes)
		s.promptHistory.ring = append(s.promptHistory.ring, PromptEntry{
			Timestamp: now.Add(time.Duration(i-n) * time.Second).Format(time.RFC3339),
			Agent:     "scanner", Trigger: "kick", Prompt: prompt,
		})
	}
	return s
}

type diagPromptResult struct {
	Entries      []diagPromptEntry   `json:"entries"`
	EntryCount   int                 `json:"entry_count"`
	EntriesTotal int                 `json:"entries_total"`
	Truncated    bool                `json:"truncated"`
	CutEntries   int                 `json:"cut_entries"`
	Redactions   []logread.Redaction `json:"redactions"`
}

func TestDiagPromptHistoryBoundsAndRole(t *testing.T) {
	s := diagPromptServer(t, 40, 20*1024)
	if rec := diagGet(s, "/api/diag/prompt-history", config.RoleRead, false); rec.Code != http.StatusForbidden {
		t.Fatalf("read role status = %d", rec.Code)
	}
	rec := diagGet(s, "/api/diag/prompt-history?agent=scanner&windowHours=48", config.RoleReadWrite, false)
	if rec.Code != http.StatusOK || rec.Body.Len() > adminmcp.DiagResultByteCap {
		t.Fatalf("status = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if strings.Contains(rec.Body.String(), diagOwnSecret) {
		t.Fatal("own secret leaked from prompt history")
	}
	var res diagPromptResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.EntriesTotal != 40 || res.EntryCount == 0 || res.EntryCount >= 40 || res.EntryCount != len(res.Entries) || res.CutEntries != res.EntryCount {
		t.Fatalf("bounds: %+v", res)
	}
	for _, e := range res.Entries {
		if !e.Cut || len(e.Prompt) > adminmcp.DiagPromptEntryBytes || e.FullBytes <= adminmcp.DiagPromptEntryBytes {
			t.Fatalf("entry not cut: cut=%v len=%d full=%d", e.Cut, len(e.Prompt), e.FullBytes)
		}
	}
	if !strings.HasPrefix(res.Entries[0].Prompt, "prompt 39 ") {
		t.Fatalf("newest entry not first: %q", res.Entries[0].Prompt[:20])
	}
	if diagRedacted(res.Redactions, logread.SecretValueKind) != res.EntryCount {
		t.Fatalf("redactions = %+v", res.Redactions)
	}
}

func TestDiagPromptHistorySmallIsWhole(t *testing.T) {
	s := diagPromptServer(t, 3, 100)
	rec := diagGet(s, "/api/diag/prompt-history", config.RoleOwner, true)
	var res diagPromptResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.EntryCount != 3 || res.CutEntries != 0 {
		t.Fatalf("small history: %+v", res)
	}
}

const diagSeedYAML = `project:
  org: acme
  repos: [r1, r2]
hub:
  url: https://ops:hubpass-123456@hub.example.com
dashboard:
  auth_token: seed-dash-auth-value-987654
governor:
  backup_key: gov-backup-key-value-55555
agents:
  scanner:
    backend: claude
    model: sonnet
`

func diagConfigRead(t *testing.T, s *Server, path string) (diagConfigResult, string) {
	t.Helper()
	rec := diagGet(s, path, config.RoleOwner, true)
	if rec.Code != http.StatusOK || rec.Body.Len() > adminmcp.DiagResultByteCap {
		t.Fatalf("status = %d, %d bytes: %s", rec.Code, rec.Body.Len(), rec.Body.String())
	}
	var res diagConfigResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res, rec.Body.String()
}

func diagKey(t *testing.T, res diagConfigResult, key string) diagConfigKey {
	t.Helper()
	for _, k := range res.Keys {
		if k.Key == key {
			return k
		}
	}
	t.Fatalf("key %q not listed", key)
	return diagConfigKey{}
}

func diagEnv(t *testing.T, res diagConfigResult, name string) diagEnvName {
	t.Helper()
	for _, e := range res.Env {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("env %q not listed", name)
	return diagEnvName{}
}

func TestDiagConfigValuesOnlyForListedKeys(t *testing.T) {
	writeProvenanceFixture(t, diagSeedYAML, "")
	t.Setenv("FOO_PASSPHRASE", "env-pass-value-abcdef12")
	t.Setenv("TZ", "UTC")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", "")
	s, _ := apiServer(t)

	res, body := diagConfigRead(t, s, "/api/diag/config")
	for _, leak := range []string{"hubpass-123456", "seed-dash-auth-value-987654", "gov-backup-key-value-55555", "env-pass-value-abcdef12"} {
		if strings.Contains(body, leak) {
			t.Fatalf("secret %q in result", leak)
		}
	}
	if strings.Contains(body, "backup_key: ") || strings.Contains(body, "project:\n") {
		t.Fatal("raw configuration text returned")
	}
	if res.Type != "config_effective" || res.KeysTotal != res.KeyCount || res.HasMore || res.Note == "" {
		t.Fatalf("shape: %+v", res)
	}
	for _, k := range res.Keys {
		if k.Layer == "" || (k.Set && k.Layer == config.LayerUnset.String()) {
			t.Fatalf("key %q has no layer", k.Key)
		}
		if k.Value != "" && !diagConfigValueKey(k.Key) {
			t.Fatalf("value shown for unlisted key %q", k.Key)
		}
	}
	if org := diagKey(t, res, "project.org"); org.Value != "acme" || !org.ValueShown || org.Layer != config.LayerSeed.String() {
		t.Fatalf("project.org = %+v", org)
	}
	if repo := diagKey(t, res, "project.repos[1]"); repo.Value != "r2" {
		t.Fatalf("project.repos[1] = %+v", repo)
	}
	if auth := diagKey(t, res, "dashboard.auth_token"); !auth.Set || auth.Value != "" || auth.Hidden != diagHiddenNotListed {
		t.Fatalf("dashboard.auth_token = %+v", auth)
	}
	if hub := diagKey(t, res, "hub.url"); hub.Value != "" || hub.Hidden != diagHiddenURLLogin {
		t.Fatalf("hub.url = %+v", hub)
	}
	if extra := diagKey(t, res, "governor.backup_key"); !extra.Set || extra.Layer != config.LayerSeed.String() || extra.Value != "" {
		t.Fatalf("governor.backup_key = %+v", extra)
	}
	if model := diagKey(t, res, "agents.scanner.model"); model.Value != "sonnet" {
		t.Fatalf("agents.scanner.model = %+v", model)
	}
	if e := diagEnv(t, res, "FOO_PASSPHRASE"); !e.Set || e.Value != "" || e.Hidden != diagHiddenNotListed {
		t.Fatalf("FOO_PASSPHRASE = %+v", e)
	}
	if e := diagEnv(t, res, "TZ"); e.Value != "UTC" || !e.ValueShown {
		t.Fatalf("TZ = %+v", e)
	}
	if res.Provenance.OverlayPresent || res.Provenance.OverlayRejected || len(res.Provenance.Layers) == 0 || res.Provenance.SeedPath == "" {
		t.Fatalf("provenance = %+v", res.Provenance)
	}
	if len(res.ValueList.ConfigKeys) == 0 || len(res.ValueList.EnvNames) == 0 {
		t.Fatal("value list not documented in the result")
	}
}

func TestDiagConfigOverlayProvenance(t *testing.T) {
	writeProvenanceFixture(t, diagSeedYAML, "project:\n  org: overlay-org\nagents:\n  scanner: {backend: claude}\n")
	s, _ := apiServer(t)
	res, _ := diagConfigRead(t, s, "/api/diag/config")
	org := diagKey(t, res, "project.org")
	if org.Value != "overlay-org" || org.Layer != config.LayerDashboardOverlay.String() || len(org.AlsoSetBy) != 1 || org.AlsoSetBy[0] != config.LayerSeed.String() {
		t.Fatalf("project.org = %+v", org)
	}
	if !res.Provenance.OverlayPresent || res.Provenance.OverlayRejected {
		t.Fatalf("provenance = %+v", res.Provenance)
	}
	// A seed-only key the accepted overlay dropped is listed as not set.
	if extra := diagKey(t, res, "governor.backup_key"); extra.Set || extra.Layer != config.LayerUnset.String() {
		t.Fatalf("governor.backup_key = %+v", extra)
	}
}

func TestDiagConfigRejectedOverlay(t *testing.T) {
	writeProvenanceFixture(t, diagSeedYAML, "project: [unclosed-secret-text\n")
	s, _ := apiServer(t)
	res, body := diagConfigRead(t, s, "/api/diag/config")
	if !res.Provenance.OverlayRejected || res.Provenance.OverlayRejectReason == "" {
		t.Fatalf("provenance = %+v", res.Provenance)
	}
	if strings.Contains(body, "unclosed-secret-text") {
		t.Fatal("overlay text returned")
	}
	if org := diagKey(t, res, "project.org"); org.Layer != config.LayerSeed.String() || org.Value != "acme" {
		t.Fatalf("project.org = %+v", org)
	}
}

func TestDiagConfigNeedsVerifiedOwner(t *testing.T) {
	writeProvenanceFixture(t, diagSeedYAML, "")
	s, _ := apiServer(t)
	for _, tc := range []struct {
		role     string
		verified bool
	}{{config.RoleReadWrite, false}, {config.RoleOwner, false}, {config.RoleReadWrite, true}, {"", false}} {
		if rec := diagGet(s, "/api/diag/config", tc.role, tc.verified); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q verified=%v: status = %d", tc.role, tc.verified, rec.Code)
		}
	}
	if rec := diagGet(s, "/api/diag/config?offset=-1", config.RoleOwner, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad offset status = %d", rec.Code)
	}
	t.Setenv("HIVE_CONFIG", "/nonexistent/hive.yaml")
	if rec := diagGet(s, "/api/diag/config", config.RoleOwner, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing seed status = %d", rec.Code)
	}
}

func TestBuildDiagConfigPagesKeys(t *testing.T) {
	var b strings.Builder
	b.WriteString("extra:\n")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "  key_%05d_%s: v\n", i, strings.Repeat("x", 80))
	}
	in := diagConfigInput{seedYAML: []byte(b.String()), environ: []string{"PATH=/bin"}}
	first := buildDiagConfig(in)
	if !first.HasMore || first.KeysTotal != 3000 || first.NextOffset != first.KeyCount || first.KeyCount == 0 {
		t.Fatalf("first page: total=%d count=%d more=%v next=%d", first.KeysTotal, first.KeyCount, first.HasMore, first.NextOffset)
	}
	if size := diagJSONSize(first); size > adminmcp.DiagResultByteCap {
		t.Fatalf("first page is %d bytes", size)
	}
	in.offset = first.NextOffset
	second := buildDiagConfig(in)
	if second.Keys[0].Key == first.Keys[0].Key || second.Offset != first.NextOffset {
		t.Fatal("second page did not continue")
	}
	in.offset = 99999
	if last := buildDiagConfig(in); last.HasMore || last.KeyCount != 0 || last.Keys == nil {
		t.Fatalf("past the end: %+v", last.KeyCount)
	}
}

func TestDiagConfigValueKey(t *testing.T) {
	for key, want := range map[string]bool{
		"project.org":               true,
		"project.repos[3]":          true,
		"agents.scanner.model":      true,
		"agents..model":             false,
		"agents.scanner.env.X":      false,
		"github.private_key":        false,
		"dashboard.auth_token":      false,
		"hub.url":                   true,
		"hub.url.x":                 false,
		"notifications.slack.hooks": false,
	} {
		if got := diagConfigValueKey(key); got != want {
			t.Errorf("diagConfigValueKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestDiagShownValueAndZero(t *testing.T) {
	scrub := func(s string) string { return logread.ScrubText(s, nil).Text }
	if v, cut, hidden := diagShownValue("https://u:p@x.example", scrub); v != "" || cut || hidden != diagHiddenURLLogin {
		t.Fatalf("url login: %q %v %q", v, cut, hidden)
	}
	if v, cut, hidden := diagShownValue(strings.Repeat("a", 5000), scrub); len(v) != diagConfigValueBytes || !cut || hidden != "" {
		t.Fatalf("long value: %d %v %q", len(v), cut, hidden)
	}
	for _, v := range []string{"", "0", "false", "null", "~", "[]", "{}"} {
		if !diagZeroValue(v) {
			t.Errorf("%q not zero", v)
		}
	}
	if diagZeroValue("x") {
		t.Error("x is not zero")
	}
}

func TestDiagYAMLLeaves(t *testing.T) {
	got := diagYAMLLeaves([]byte("a:\n  b: 1\n  l: [x, y]\nbase: &anchor {k: v}\nref: *anchor\nempty: {}\n"))
	for k, want := range map[string]string{"a.b": "1", "a.l[0]": "x", "a.l[1]": "y", "base.k": "v", "ref.k": "v"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q (%v)", k, got[k], want, got)
		}
	}
	if _, ok := got["empty"]; ok {
		t.Error("an empty map is not a leaf")
	}
	if len(diagYAMLLeaves(nil)) != 0 || len(diagYAMLLeaves([]byte("a: [unclosed\n"))) != 0 {
		t.Error("empty or unparsable documents have no leaves")
	}
}

func TestDiagEnvNames(t *testing.T) {
	scrub := func(s string) string { return s }
	got := diagEnvNames([]string{"TZ=UTC", "TZ=dup", "=bad", "GITHUB_TOKEN=x", "HOME=/root", "ZED=1"}, scrub)
	byName := map[string]diagEnvName{}
	for _, e := range got {
		if _, dup := byName[e.Name]; dup {
			t.Fatalf("duplicate %q", e.Name)
		}
		byName[e.Name] = e
	}
	if byName["TZ"].Value != "UTC" || byName["HOME"].Value != "/root" {
		t.Fatalf("listed values: %+v", got)
	}
	if e := byName["GITHUB_TOKEN"]; e.Value != "" || e.Hidden != diagHiddenNotListed || !e.Set {
		t.Fatalf("secret env: %+v", e)
	}
	if e := byName["ZED"]; e.Value != "" || e.Hidden != diagHiddenNotListed {
		t.Fatalf("unlisted env: %+v", e)
	}
	if e, ok := byName["HIVE_CONFIG"]; !ok || e.Set {
		t.Fatalf("unset listed name: %+v", e)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Fatal("not sorted")
		}
	}
}

func TestFitDiagHelpers(t *testing.T) {
	items := make([]any, 2000)
	for i := range items {
		items[i] = map[string]any{"i": i, "pad": strings.Repeat("k", 100)}
	}
	res := map[string]any{"kicks": items}
	trim, ok := fitDiagList(res, res, "kicks", true)
	kept := res["kicks"].([]any)
	if !ok || trim.Total != 2000 || trim.Kept != len(kept) || diagExcess(res) > 0 || kept[0].(map[string]any)["i"] != 0 {
		t.Fatalf("newest-first trim: %+v", trim)
	}
	res = map[string]any{"events": append([]any(nil), items...)}
	if _, ok := fitDiagList(res, res, "events", false); !ok || res["events"].([]any)[len(res["events"].([]any))-1].(map[string]any)["i"] != 1999 {
		t.Fatal("oldest-first trim did not keep the newest")
	}
	if _, ok := fitDiagList(res, res, "missing", true); ok {
		t.Fatal("missing list trimmed")
	}

	res = map[string]any{"raw_log": strings.Repeat("line\n", 60000) + "last\n"}
	trim, ok = fitDiagString(res, res, "raw_log")
	if !ok || trim.Unit != "bytes" || !strings.HasSuffix(res["raw_log"].(string), "last\n") || diagExcess(res) > 0 {
		t.Fatalf("string trim: %+v", trim)
	}
	if _, ok := fitDiagString(res, res, "raw_log"); ok {
		t.Fatal("a fitting string was trimmed again")
	}

	res = map[string]any{"detail": map[string]any{"blob": strings.Repeat("z", 300*1024)}}
	fitDiagFallback(res, "detail")
	if _, ok := res["detail"]; ok || res["truncated"] != true || diagExcess(res) > 0 {
		t.Fatalf("fallback did not replace detail: %v", res["truncated"])
	}
	small := map[string]any{"detail": map[string]any{"a": 1}}
	fitDiagFallback(small, "detail")
	if _, ok := small["detail"]; !ok {
		t.Fatal("fallback changed a fitting result")
	}
}

func TestBuildDiagTextFitsCapWithEscapes(t *testing.T) {
	// Control characters expand when JSON-encoded; the result must still fit.
	body := []byte(strings.Repeat("\x01\x02<>&\n", 60*1024))
	res := buildDiagText("agent_log", "text/plain", body, adminmcp.DiagMaxTextBytes, false, nil)
	if size := diagJSONSize(res); size > adminmcp.DiagResultByteCap || !res.Truncated || res.BytesReturned != len(res.Text) {
		t.Fatalf("size %d truncated %v", size, res.Truncated)
	}
}

func TestDiagThroughInHiveProvider(t *testing.T) {
	writeProvenanceFixture(t, diagSeedYAML, "")
	s := diagLogServer(t, "hello\n"+diagOwnSecret+"\n")
	s.promptHistory = newTestPromptHistory()
	ctx := context.Background()

	reader := dashboardAdminMCPProvider{server: s, role: config.RoleRead}
	data, err := reader.Read(ctx, adminmcp.ToolAgentLogRead, map[string]any{"agent": "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := data.(map[string]any); m["type"] != "diag_text" || m["_admin_mcp"] != nil || strings.Contains(fmt.Sprint(m["text"]), diagOwnSecret) {
		t.Fatalf("agent_log_read = %#v", data)
	}
	if _, err := reader.Read(ctx, adminmcp.ToolPromptHistoryRead, nil); !errors.Is(err, adminmcp.ErrForbidden) {
		t.Fatalf("prompt history as read: %v", err)
	}
	if _, err := reader.Read(ctx, adminmcp.ToolAgentKickRead, map[string]any{"agent": "scanner"}); err == nil {
		t.Fatal("missing kick_id accepted")
	}

	// A claimed owner role without this hive's verification is refused before
	// any read, with the typed error.
	claimed := dashboardAdminMCPProvider{server: s, role: config.RoleOwner}
	if _, err := claimed.Read(ctx, adminmcp.ToolConfigEffectiveRead, nil); !errors.Is(err, adminmcp.ErrOwnerUnverified) {
		t.Fatalf("claimed owner: %v", err)
	}
	writer := dashboardAdminMCPProvider{server: s, role: config.RoleReadWrite, ownerVerified: false}
	if _, err := writer.Read(ctx, adminmcp.ToolConfigEffectiveRead, nil); !errors.Is(err, adminmcp.ErrOwnerUnverified) {
		t.Fatalf("read-write: %v", err)
	}
	owner := dashboardAdminMCPProvider{server: s, role: config.RoleOwner, ownerVerified: true}
	data, err = owner.Read(ctx, adminmcp.ToolConfigEffectiveRead, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := data.(map[string]any); m["type"] != "config_effective" {
		t.Fatalf("config_effective_read = %#v", data)
	}
	if _, err := owner.Read(ctx, adminmcp.ToolPromptHistoryRead, nil); err != nil {
		t.Fatalf("prompt history as owner: %v", err)
	}
	if _, err := owner.Read(ctx, adminmcp.ToolHealthDeepRead, nil); err != nil && errors.Is(err, adminmcp.ErrForbidden) {
		t.Fatalf("health refused: %v", err)
	}
}

func TestAdminMCPOwnerVerified(t *testing.T) {
	for _, tc := range []struct {
		role, marker string
		want         bool
	}{
		{config.RoleOwner, "true", true},
		{config.RoleOwner, "", false},
		{config.RoleReadWrite, "true", false},
		{"", "true", false},
	} {
		req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, nil)
		req.Header.Set("X-Hive-Role", tc.role)
		req.Header.Set(ownerRoleVerifiedHeader, tc.marker)
		if got := adminMCPOwnerVerified(req); got != tc.want {
			t.Errorf("role %q marker %q = %v", tc.role, tc.marker, got)
		}
	}
}
