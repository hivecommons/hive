package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// githubSettingsProvider answers settings_read section github_apps with a
// JSON-decoded copy of its settings, as GET /api/config/github/forge-apps
// answers, and records writes. onWrite stands in for the endpoint saving the
// change; writeErr for it refusing.
type githubSettingsProvider struct {
	settings map[string]any
	readErr  error
	writeErr error
	onWrite  func(body any)
	reads    int
	requests []WriteRequest
}

func (p *githubSettingsProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != "github_apps" {
		return map[string]any{}, nil
	}
	p.reads++
	if p.readErr != nil {
		return nil, p.readErr
	}
	b, err := json.Marshal(map[string]any{"active": map[string]any{"app_id": 1}, "settings": p.settings})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *githubSettingsProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	if p.writeErr != nil {
		return nil, p.writeErr
	}
	p.requests = append(p.requests, req)
	if p.onWrite != nil {
		p.onWrite(req.Body)
	}
	return map[string]any{"status": "updated"}, nil
}

// githubSettingsFixture is the settings object of the Forge App tab for a
// hive on the built-in budget defaults with no ETag or batch overrides.
func githubSettingsFixture() map[string]any {
	return map[string]any{
		"api_reserve":                 800,
		"api_critical":                250,
		"agent_reserve_floor":         400,
		"pr_detail_ttl_s":             0,
		"graphql_pr_batch":            true,
		"graphql_pr_batch_page_size":  0,
		"etag_cache_max_mb":           0,
		"etag_cache_max_entries":      0,
		"agent_github_api_hourly_cap": 300,
		"defaults": map[string]any{
			"agent_reserve_floor":        400,
			"etag_cache_max_mb":          128,
			"etag_cache_max_entries":     8192,
			"pr_detail_ttl_s":            1800,
			"graphql_pr_batch_page_size": 50,
		},
	}
}

func githubSettingsTestProvider() *githubSettingsProvider {
	return &githubSettingsProvider{settings: githubSettingsFixture()}
}

// applyBody saves a PUT body into the fixture the way the endpoint does,
// including writing the default for a cleared reserve or critical floor.
func (p *githubSettingsProvider) applyBody(body any) {
	for k, v := range body.(map[string]any) {
		if n, _ := reviewSettingsNumber(v); n == 0 && (k == "api_reserve" || k == "api_critical") {
			v = map[string]int{"api_reserve": 800, "api_critical": 250}[k]
		}
		p.settings[k] = v
	}
}

func githubSettingsHandler(p *githubSettingsProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func githubSettingsPreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGitHubSettings, "args": args})
	if err != nil {
		t.Fatalf("preview %v: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func githubSettingsChanges(t *testing.T, preview WritePreview) map[string][2]string {
	t.Helper()
	rows, _ := preview.Details["changes"].([]map[string]any)
	out := map[string][2]string{}
	for _, row := range rows {
		out[row["field"].(string)] = [2]string{compactJSON(row["before"]), compactJSON(row["after"])}
	}
	return out
}

func TestGitHubSettingsRegistered(t *testing.T) {
	op, ok := DefaultWriteRegistry().Get(WriteOpGitHubSettings)
	if !ok {
		t.Fatalf("%s is not registered", WriteOpGitHubSettings)
	}
	if _, ok := DefaultWriteRegistry().Get(WriteOpGitHubAgentAPIHourlyCap); !ok {
		t.Fatalf("%s is no longer registered", WriteOpGitHubAgentAPIHourlyCap)
	}
	schema := op.InputSchema()
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 7 || schema["additionalProperties"] != false {
		t.Fatalf("schema = %#v", schema)
	}
	for _, key := range []string{"api_reserve", "api_critical", "agent_reserve_floor", "pr_detail_ttl_s", "graphql_pr_batch_page_size", "etag_cache_max_mb", "etag_cache_max_entries"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema has no %s", key)
		}
	}
}

// Naming two fields shows exactly those two before and after and sends
// nothing else. Raising the ETag cache says how much more memory it may use
// and is not budget widening.
func TestGitHubSettingsPreviewShowsOnlyTheNamedFields(t *testing.T) {
	p := githubSettingsTestProvider()
	preview, _ := githubSettingsPreview(t, githubSettingsHandler(p), map[string]any{"etag_cache_max_mb": float64(512), "pr_detail_ttl_s": float64(600)})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/github" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if got, want := compactJSON(preview.Request.Body), `{"etag_cache_max_mb":512,"pr_detail_ttl_s":600}`; got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	changes := githubSettingsChanges(t, preview)
	want := map[string][2]string{
		"etag_cache_max_mb": {"0", "512"},
		"pr_detail_ttl_s":   {"0", "600"},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want exactly %v", changes, want)
	}
	for field, w := range want {
		if changes[field] != w {
			t.Fatalf("%s = %v, want %v (all changes %v)", field, changes[field], w, changes)
		}
	}
	effects := strings.Join(preview.Effects, "\n")
	if !strings.Contains(effects, "etag_cache_max_mb: 0 (default 128) → 512") || !strings.Contains(effects, "pr_detail_ttl_s: 0 (default 1800) → 600") {
		t.Fatalf("effects must show each field before and after: %v", preview.Effects)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "No widening:") || !strings.Contains(preview.WideningDisclosure, "up to 384 MB more memory (512 MB in all)") {
		t.Fatalf("raising the ETag cache must state the memory delta and not widen the budget: %s", preview.WideningDisclosure)
	}
	// A shorter PR detail TTL costs calls; both fields apply live.
	if !strings.Contains(effects, "pr_detail_ttl_s goes down") || !strings.Contains(effects, "spends more GitHub API calls") {
		t.Fatalf("lowering pr_detail_ttl_s must say it spends more calls: %v", preview.Effects)
	}
	if !strings.Contains(effects, "pr_detail_ttl_s, etag_cache_max_mb apply without a restart") || strings.Contains(effects, "next restart") {
		t.Fatalf("live fields must say they apply without a restart, and nothing here waits for one: %v", preview.Effects)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a preview wrote: %+v", p.requests)
	}
}

// Lowering a floor that guards the shared bucket is widening and names the
// field; raising one is not.
func TestGitHubSettingsDisclosesLoweredFloorsAsWidening(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  map[string]any
		widen []string
	}{
		{"lower reserve", map[string]any{"api_reserve": float64(600)}, []string{"api_reserve drops from 800 to 600", "conserve mode"}},
		{"lower critical", map[string]any{"api_critical": float64(100)}, []string{"api_critical drops from 250 to 100", "critical mode"}},
		{"lower agent floor", map[string]any{"agent_reserve_floor": float64(200)}, []string{"agent_reserve_floor drops from 400 to 200", "API proxy"}},
		{"raise reserve", map[string]any{"api_reserve": float64(1200)}, nil},
		{"raise agent floor", map[string]any{"agent_reserve_floor": float64(600)}, nil},
		{"page size", map[string]any{"graphql_pr_batch_page_size": float64(25)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview, _ := githubSettingsPreview(t, githubSettingsHandler(githubSettingsTestProvider()), tc.args)
			d := preview.WideningDisclosure
			_, atBuild := tc.args["api_reserve"]
			if _, ok := tc.args["api_critical"]; ok {
				atBuild = true
			}
			// The reserve and critical floors apply when the client is
			// rebuilt: on this save for a GitHub App hive, else at a restart.
			effects := strings.Join(preview.Effects, "\n")
			if strings.Contains(effects, "reinit ok") != atBuild || strings.Contains(effects, "after the next restart") != atBuild {
				t.Fatalf("effects = %v, want the rebuild-or-restart note only for api_reserve and api_critical", preview.Effects)
			}
			if len(tc.widen) == 0 {
				if !strings.HasPrefix(d, "No widening:") {
					t.Fatalf("disclosure = %q, want no widening", d)
				}
				return
			}
			if strings.Contains(d, "at the next restart") != atBuild {
				t.Fatalf("disclosure = %q, want when a lower reserve or critical floor takes effect only for those two", d)
			}
			if !strings.HasPrefix(d, "Widening:") || !strings.Contains(d, "App installation") {
				t.Fatalf("disclosure = %q, want widening naming the shared bucket", d)
			}
			for _, w := range tc.widen {
				if !strings.Contains(d, w) {
					t.Fatalf("disclosure = %q, want it to contain %q", d, w)
				}
			}
		})
	}

	// A smaller GraphQL page costs calls, and is not widening.
	preview, _ := githubSettingsPreview(t, githubSettingsHandler(githubSettingsTestProvider()), map[string]any{"graphql_pr_batch_page_size": float64(25)})
	if effects := strings.Join(preview.Effects, "\n"); !strings.Contains(effects, "graphql_pr_batch_page_size goes down") || !strings.Contains(effects, "spends more GitHub API calls") {
		t.Fatalf("lowering graphql_pr_batch_page_size must say it spends more calls: %v", preview.Effects)
	}

	// More ETag entries name the MB bound that still caps the memory.
	preview, _ = githubSettingsPreview(t, githubSettingsHandler(githubSettingsTestProvider()), map[string]any{"etag_cache_max_entries": float64(16384)})
	if d := preview.WideningDisclosure; !strings.Contains(d, "up to 8192 more responses") || !strings.Contains(d, "etag_cache_max_mb bound of 128 MB") {
		t.Fatalf("disclosure = %q, want the entries delta and the MB bound", d)
	}
}

// The confirm sends only the named fields, even when the dashboard changed
// another setting in between, and reports what the hive holds after the
// write.
func TestGitHubSettingsConfirmSendsOnlyNamedFieldsAndReportsAfter(t *testing.T) {
	p := githubSettingsTestProvider()
	p.onWrite = p.applyBody
	h := githubSettingsHandler(p)
	_, id := githubSettingsPreview(t, h, map[string]any{"api_reserve": float64(600), "etag_cache_max_mb": float64(256)})
	p.settings["graphql_pr_batch_page_size"] = 40 // a dashboard edit to another field meanwhile

	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("executed %+v", p.requests)
	}
	if got, want := compactJSON(p.requests[0].Body), `{"api_reserve":600,"etag_cache_max_mb":256}`; got != want {
		t.Fatalf("PUT body = %s\nwant      %s", got, want)
	}
	res := out.(map[string]any)
	if res["type"] != "write_result" || res["after_error"] != nil {
		t.Fatalf("result = %#v", res)
	}
	if got, want := compactJSON(res["after"]), `{"api_reserve":600,"etag_cache_max_mb":256}`; got != want {
		t.Fatalf("after = %s, want %s", got, want)
	}
	if p.settings["graphql_pr_batch_page_size"] != 40 {
		t.Fatal("the confirm touched graphql_pr_batch_page_size")
	}
}

// If a named field changes between preview and confirm, the preview's
// before and after no longer hold, so the confirm is refused and nothing is
// written.
func TestGitHubSettingsRefusesStalePreview(t *testing.T) {
	p := githubSettingsTestProvider()
	h := githubSettingsHandler(p)
	_, id := githubSettingsPreview(t, h, map[string]any{"etag_cache_max_mb": float64(512), "api_reserve": float64(700)})
	p.settings["etag_cache_max_mb"] = 256
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}
}

// The endpoint's range checks are not repeated: an ETag cache above its
// maximum previews, and the endpoint's 400 comes back verbatim.
func TestGitHubSettingsPassesHandlerRefusalThrough(t *testing.T) {
	p := githubSettingsTestProvider()
	h := githubSettingsHandler(p)
	_, id := githubSettingsPreview(t, h, map[string]any{"etag_cache_max_mb": float64(4096)})
	const msg = "etag_cache_max_mb must be between 1 and 2048, or 0 for the default (128)"
	body := []byte(`{"error":"` + msg + `","ok":false}`)
	p.writeErr = &HiveRefusalError{StatusCode: http.StatusBadRequest, Message: string(body), Body: body}
	_, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	refusal, ok := HiveRefusalFromError(err)
	if !ok || refusal.StatusCode != http.StatusBadRequest || refusal.Type != "hive-refusal" {
		t.Fatalf("confirm err = %v, want the endpoint's 400 as a hive refusal", err)
	}
	if got, _ := refusal.Body.(map[string]any); got["error"] != msg {
		t.Fatalf("refusal body = %#v, want error %q", refusal.Body, msg)
	}
}

// A field the op does not take, a null and a fraction are refused before
// anything is read, minted or written. The fleet agent cap points at its own
// op.
func TestGitHubSettingsRefusesBadArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		args map[string]any
		want []string
	}{
		"unknown field": {map[string]any{"api_reserv": float64(600)}, []string{`unsupported field "api_reserv" in github.settings`, "api_reserve", "etag_cache_max_entries"}},
		"toggle":        {map[string]any{"graphql_pr_batch": false}, []string{`unsupported field "graphql_pr_batch"`}},
		"fleet cap":     {map[string]any{"agent_github_api_hourly_cap": float64(600)}, []string{"use the github.agent_api_hourly_cap operation"}},
		"null value":    {map[string]any{"api_reserve": nil}, []string{"null is not accepted"}},
		"fraction":      {map[string]any{"etag_cache_max_mb": 1.5}, []string{"etag_cache_max_mb must be a whole number"}},
		"string":        {map[string]any{"etag_cache_max_mb": "256"}, []string{"etag_cache_max_mb must be a whole number"}},
		"no arguments":  {map[string]any{}, []string{"at least one GitHub API setting"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := githubSettingsTestProvider()
			h := githubSettingsHandler(p)
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGitHubSettings, "args": tc.args})
			if err == nil {
				t.Fatal("preview accepted it")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want it to contain %q", err, want)
				}
			}
			if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
				t.Fatalf("a refused preview minted a confirmation: %+v", pending)
			}
			if p.reads != 0 || len(p.requests) != 0 {
				t.Fatalf("a refused preview read %d times and wrote %+v", p.reads, p.requests)
			}
		})
	}
}

// A field already at the requested value is not sent; a change that changes
// nothing is refused. 0 for api_reserve stores the default, so at the
// default it changes nothing.
func TestGitHubSettingsSkipsUnchangedFields(t *testing.T) {
	p := githubSettingsTestProvider()
	h := githubSettingsHandler(p)
	preview, _ := githubSettingsPreview(t, h, map[string]any{"api_critical": float64(250), "etag_cache_max_entries": float64(16384)})
	if got := compactJSON(preview.Request.Body); got != `{"etag_cache_max_entries":16384}` {
		t.Fatalf("body = %s", got)
	}
	if unchanged, _ := preview.Details["unchanged"].([]string); len(unchanged) != 1 || unchanged[0] != "api_critical" {
		t.Fatalf("unchanged = %#v", preview.Details["unchanged"])
	}
	for _, key := range []string{"api_reserve", "agent_reserve_floor"} {
		if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGitHubSettings, "args": map[string]any{key: float64(0)}}); err == nil || !strings.Contains(err.Error(), "nothing would change") {
			t.Fatalf("%s 0 at its default: err = %v, want nothing would change", key, err)
		}
	}
}

// The settings read reports agent_reserve_floor as its effective value, so a
// cleared floor previews as the default it will read back as, and the body
// still sends the 0.
func TestGitHubSettingsClearedAgentFloorPreviewsAsDefault(t *testing.T) {
	p := githubSettingsTestProvider()
	p.settings["agent_reserve_floor"] = 500
	preview, _ := githubSettingsPreview(t, githubSettingsHandler(p), map[string]any{"agent_reserve_floor": float64(0)})
	if got := compactJSON(preview.Request.Body); got != `{"agent_reserve_floor":0}` {
		t.Fatalf("body = %s", got)
	}
	if changes := githubSettingsChanges(t, preview); changes["agent_reserve_floor"] != [2]string{"500", "400"} {
		t.Fatalf("changes = %v, want 500 to 400", changes)
	}
	if d := preview.WideningDisclosure; !strings.HasPrefix(d, "Widening:") || !strings.Contains(d, "agent_reserve_floor drops from 500 to 400") {
		t.Fatalf("disclosure = %q", d)
	}
}

// A failed read-back after a successful write is reported next to the
// result, not as a failed write.
func TestGitHubSettingsReportsFailedReadBack(t *testing.T) {
	p := githubSettingsTestProvider()
	p.onWrite = func(any) { p.readErr = errors.New("read refused") }
	h := githubSettingsHandler(p)
	_, id := githubSettingsPreview(t, h, map[string]any{"agent_reserve_floor": float64(500)})
	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	res := out.(map[string]any)
	if msg, _ := res["after_error"].(string); !strings.Contains(msg, "the write succeeded") || res["after"] != nil || len(p.requests) != 1 {
		t.Fatalf("result = %#v", res)
	}
}
