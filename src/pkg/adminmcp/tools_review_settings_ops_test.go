package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// reviewSettingsProvider answers settings_read sections review and governor
// with a JSON-decoded copy of its maps, as the endpoints answer, and records
// writes. onWrite stands in for the endpoint saving the change; writeErr for
// it refusing.
type reviewSettingsProvider struct {
	review   map[string]any
	governor map[string]any
	readErr  error
	writeErr error
	onWrite  func(body any)
	reads    int
	requests []WriteRequest
}

func (p *reviewSettingsProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead {
		return map[string]any{}, nil
	}
	p.reads++
	if p.readErr != nil {
		return nil, p.readErr
	}
	src := p.review
	if args["section"] == SettingsSectionDefault {
		src = p.governor
	}
	b, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *reviewSettingsProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	if p.writeErr != nil {
		return nil, p.writeErr
	}
	p.requests = append(p.requests, req)
	if p.onWrite != nil {
		p.onWrite(req.Body)
	}
	return map[string]any{"status": "updated"}, nil
}

// reviewSettingsFixture is GET /api/config/review for a hive with the review
// gate on, two named verdict labels and autoscale configured but off. Keys
// at their zero value are left out, as the endpoint's omitempty does.
func reviewSettingsFixture() map[string]any {
	return map[string]any{
		"require_approval": true,
		"fan_out":          true,
		"reviewer_agents":  []string{"reviewer"},
		"severity":         map[string]any{"block_at": "P2"},
		"verdict_labels":   map[string]any{"reviewing": "hive/reviewing", "approve": "hive/approved", "changes_requested": "hive/changes", "requires_human": "hive/human"},
		"reviewers":        map[string]any{"autoscale": map[string]any{"min": 1, "max": 4}},
		"builtin_perspectives": []map[string]string{
			{"name": "correctness", "focus": "bugs"},
		},
	}
}

func reviewSettingsTestProvider() *reviewSettingsProvider {
	return &reviewSettingsProvider{review: reviewSettingsFixture(), governor: map[string]any{"review_bots": map[string]any{"min_priority": "P2"}}}
}

func reviewSettingsHandler(p *reviewSettingsProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func reviewSettingsPreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpReviewSettings, "args": args})
	if err != nil {
		t.Fatalf("preview %v: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func reviewSettingsChanges(t *testing.T, preview WritePreview) map[string][2]string {
	t.Helper()
	rows, _ := preview.Details["changes"].([]map[string]any)
	out := map[string][2]string{}
	for _, row := range rows {
		out[row["field"].(string)] = [2]string{compactJSON(row["before"]), compactJSON(row["after"])}
	}
	return out
}

func TestReviewSettingsRegistered(t *testing.T) {
	op, ok := DefaultWriteRegistry().Get(WriteOpReviewSettings)
	if !ok {
		t.Fatalf("%s is not registered", WriteOpReviewSettings)
	}
	props, _ := op.InputSchema()["properties"].(map[string]any)
	autoscale, _ := props["reviewer_autoscale"].(map[string]any)
	sub, _ := autoscale["properties"].(map[string]any)
	if _, ok := sub["max"]; !ok || autoscale["additionalProperties"] != false {
		t.Fatalf("reviewer_autoscale schema = %#v", autoscale)
	}
}

// Naming two autoscale fields shows exactly those two changing and sends
// nothing else: no other autoscale field, no other review setting.
func TestReviewSettingsPreviewShowsOnlyTheNamedFields(t *testing.T) {
	p := reviewSettingsTestProvider()
	preview, _ := reviewSettingsPreview(t, reviewSettingsHandler(p), map[string]any{"reviewer_autoscale": map[string]any{"enabled": true, "max": 2}})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/review" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if got, want := compactJSON(preview.Request.Body), `{"reviewer_autoscale":{"enabled":true,"max":2}}`; got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	changes := reviewSettingsChanges(t, preview)
	want := map[string][2]string{
		"reviewer_autoscale.enabled": {"false", "true"},
		"reviewer_autoscale.max":     {"4", "2"},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want exactly %v", changes, want)
	}
	for field, w := range want {
		if changes[field] != w {
			t.Fatalf("%s = %v, want %v (all changes %v)", field, changes[field], w, changes)
		}
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening:") || !strings.Contains(preview.WideningDisclosure, "reviewer_autoscale turns on") {
		t.Fatalf("turning autoscale on must be disclosed: %s", preview.WideningDisclosure)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a preview wrote: %+v", p.requests)
	}
}

// The confirm sends the raw body with only the named fields, even when the
// dashboard changed another review setting in between (nothing is read and
// sent back whole), and reports what the hive holds after the write.
func TestReviewSettingsConfirmSendsOnlyNamedFieldsAndReportsAfter(t *testing.T) {
	p := reviewSettingsTestProvider()
	p.onWrite = func(body any) {
		ra := body.(map[string]any)["reviewer_autoscale"].(map[string]any)
		as := p.review["reviewers"].(map[string]any)["autoscale"].(map[string]any)
		for k, v := range ra {
			as[k] = v
		}
	}
	h := reviewSettingsHandler(p)
	_, id := reviewSettingsPreview(t, h, map[string]any{"reviewer_autoscale": map[string]any{"enabled": true, "max": 2}})
	p.review["fan_out"] = false // a dashboard edit to another field meanwhile

	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("executed %+v", p.requests)
	}
	raw, err := json.Marshal(p.requests[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"reviewer_autoscale":{"enabled":true,"max":2}}`; got != want {
		t.Fatalf("PUT body = %s\nwant      %s", got, want)
	}
	res := out.(map[string]any)
	if res["type"] != "write_result" || res["after_error"] != nil {
		t.Fatalf("result = %#v", res)
	}
	if got, want := compactJSON(res["after"]), `{"reviewer_autoscale.enabled":true,"reviewer_autoscale.max":2}`; got != want {
		t.Fatalf("after = %s, want %s", got, want)
	}
	if p.review["fan_out"] != false {
		t.Fatal("the confirm touched fan_out")
	}
}

// A field the endpoint does not take is refused before anything is read,
// minted or written, at the top level and inside a sub-object, and the
// refusal lists the accepted names.
func TestReviewSettingsRefusesUnknownFields(t *testing.T) {
	for name, tc := range map[string]struct {
		args map[string]any
		want []string
	}{
		"top-level typo":       {map[string]any{"reviewer_autoscal": map[string]any{"max": 2}}, []string{`unsupported field "reviewer_autoscal" in review.settings`, "reviewer_autoscale", "require_approval"}},
		"nested typo":          {map[string]any{"reviewer_autoscale": map[string]any{"maxx": 2}}, []string{`unsupported field "maxx" in review.settings.reviewer_autoscale`, "max", "cooldown"}},
		"whole-object typo":    {map[string]any{"verdict_labels": map[string]any{"approved": "x"}}, []string{`unsupported field "approved" in review.settings.verdict_labels`, "approve"}},
		"dotted top-level key": {map[string]any{"reviewer_autoscale.max": 2}, []string{`unsupported field "reviewer_autoscale.max"`}},
		"read-only field":      {map[string]any{"builtin_perspectives": []any{}}, []string{`unsupported field "builtin_perspectives"`}},
		"null value":           {map[string]any{"fan_out": nil}, []string{"null is not accepted"}},
		"wrong type":           {map[string]any{"reviewer_autoscale": map[string]any{"max": "two"}}, []string{"reviewer_autoscale.max must be a whole number"}},
		"empty sub-object":     {map[string]any{"severity": map[string]any{}}, []string{"severity must be an object naming at least one of", "block_at"}},
		"no arguments":         {map[string]any{}, []string{"at least one review setting"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := reviewSettingsTestProvider()
			h := reviewSettingsHandler(p)
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpReviewSettings, "args": tc.args})
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

// The endpoint is owner only. Its refusal of a caller who is not the owner
// reaches the caller as a hive refusal, and nothing is read back as if the
// write had happened. (pkg/dashboard runs the same refusal through the real
// endpoint and role checks.)
func TestReviewSettingsRefusesNonOwner(t *testing.T) {
	p := reviewSettingsTestProvider()
	h := reviewSettingsHandler(p)
	_, id := reviewSettingsPreview(t, h, map[string]any{"require_approval": false})
	reads := p.reads
	p.writeErr = &HiveRefusalError{StatusCode: http.StatusForbidden, Message: `{"error":"owner access required","ok":false}`, Body: []byte(`{"error":"owner access required","ok":false}`)}
	_, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	refusal, ok := HiveRefusalFromError(err)
	if !ok || refusal.StatusCode != http.StatusForbidden {
		t.Fatalf("confirm err = %v, want the endpoint's 403", err)
	}
	if body, _ := refusal.Body.(map[string]any); body["error"] != "owner access required" {
		t.Fatalf("refusal body = %#v", refusal.Body)
	}
	if p.reads != reads+1 {
		t.Fatalf("reads after the guard = %d, want only the guard's re-read (%d)", p.reads, reads+1)
	}
}

// The endpoint's own validation is not repeated: a min above max previews,
// and the endpoint's 400 comes back verbatim.
func TestReviewSettingsPassesHandlerRefusalThrough(t *testing.T) {
	p := reviewSettingsTestProvider()
	h := reviewSettingsHandler(p)
	_, id := reviewSettingsPreview(t, h, map[string]any{"reviewer_autoscale": map[string]any{"enabled": true, "min": 3, "max": 2}})
	const msg = "review.reviewers.autoscale: min 3 is greater than max 2"
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

// verdict_labels is replaced whole by the endpoint, so a partial object
// clears the keys it leaves out, and the preview shows each one.
func TestReviewSettingsWholeObjectShowsClearedKeys(t *testing.T) {
	p := reviewSettingsTestProvider()
	p.onWrite = func(body any) { p.review["verdict_labels"] = body.(map[string]any)["verdict_labels"] }
	h := reviewSettingsHandler(p)
	preview, id := reviewSettingsPreview(t, h, map[string]any{"verdict_labels": map[string]any{"approve": "lgtm"}})
	if got := compactJSON(preview.Request.Body); got != `{"verdict_labels":{"approve":"lgtm"}}` {
		t.Fatalf("body = %s", got)
	}
	changes := reviewSettingsChanges(t, preview)
	if len(changes) != 4 || changes["verdict_labels.approve"] != [2]string{`"hive/approved"`, `"lgtm"`} || changes["verdict_labels.reviewing"] != [2]string{`"hive/reviewing"`, `""`} {
		t.Fatalf("changes = %v, want approve changed and the other three keys cleared", changes)
	}
	if !strings.Contains(strings.Join(preview.Effects, "\n"), "replaces verdict_labels whole") {
		t.Fatalf("effects must say the object is replaced whole: %v", preview.Effects)
	}
	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got, want := compactJSON(out.(map[string]any)["after"]), `{"verdict_labels":{"approve":"lgtm","changes_requested":"","requires_human":"","reviewing":""}}`; got != want {
		t.Fatalf("after = %s, want %s", got, want)
	}
}

// A field already at the requested value is not sent; a change that changes
// nothing is refused. event_driven_mode reads review.event_driven (unset is
// auto), and review_bots.min_priority reads the governor section.
func TestReviewSettingsSkipsUnchangedFields(t *testing.T) {
	p := reviewSettingsTestProvider()
	h := reviewSettingsHandler(p)
	preview, _ := reviewSettingsPreview(t, h, map[string]any{"require_approval": true, "event_driven_mode": "on", "review_bots": map[string]any{"min_priority": "P1"}})
	if got := compactJSON(preview.Request.Body); got != `{"event_driven_mode":"on","review_bots":{"min_priority":"P1"}}` {
		t.Fatalf("body = %s", got)
	}
	changes := reviewSettingsChanges(t, preview)
	if changes["event_driven_mode"] != [2]string{`"auto"`, `"on"`} || changes["review_bots.min_priority"] != [2]string{`"P2"`, `"P1"`} || len(changes) != 2 {
		t.Fatalf("changes = %v", changes)
	}
	if unchanged, _ := preview.Details["unchanged"].([]string); len(unchanged) != 1 || unchanged[0] != "require_approval" {
		t.Fatalf("unchanged = %#v", preview.Details["unchanged"])
	}
	if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpReviewSettings, "args": map[string]any{"fan_out": true}}); err == nil || !strings.Contains(err.Error(), "nothing would change") {
		t.Fatalf("a no-op change err = %v", err)
	}
}

// If a named field changes between preview and confirm, the preview's
// before/after no longer holds, so the confirm is refused and nothing is
// written.
func TestReviewSettingsRefusesStalePreview(t *testing.T) {
	p := reviewSettingsTestProvider()
	h := reviewSettingsHandler(p)
	_, id := reviewSettingsPreview(t, h, map[string]any{"reviewer_autoscale": map[string]any{"max": 2}})
	p.review["reviewers"].(map[string]any)["autoscale"].(map[string]any)["max"] = 3
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}
}

// A failed read-back after a successful write is reported next to the
// result, not as a failed write.
func TestReviewSettingsReportsFailedReadBack(t *testing.T) {
	p := reviewSettingsTestProvider()
	p.onWrite = func(any) { p.readErr = errors.New("read refused") }
	h := reviewSettingsHandler(p)
	_, id := reviewSettingsPreview(t, h, map[string]any{"shadow": true})
	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	res := out.(map[string]any)
	if msg, _ := res["after_error"].(string); !strings.Contains(msg, "the write succeeded") || res["after"] != nil || len(p.requests) != 1 {
		t.Fatalf("result = %#v", res)
	}
}
