package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const externalTestToken = "provider-bearer-token"

// externalProvider is a fake provider shim. It serves the two
// hive.worksource/v1 endpoints and records what Hive sent, which is how the
// auth-header, contract-header, and redirect tests observe the client.
type externalProvider struct {
	t *testing.T

	// source controls GET /v1/source.
	sourceStatus int
	sourceBody   map[string]any

	// issues controls GET /v1/issues. pages are served in call order; raw, when
	// set, is written verbatim so malformed-JSON and oversize-body cases can be
	// expressed.
	issuesStatus int
	pages        []map[string]any
	raw          string

	// recorded request state
	authHeaders     []string
	contractHeaders []string
	acceptHeaders   []string
	cursors         []string
	sourceCalls     int
	issuesCalls     int
}

func (p *externalProvider) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.authHeaders = append(p.authHeaders, r.Header.Get("Authorization"))
		p.contractHeaders = append(p.contractHeaders, r.Header.Get(ExternalContractHeader))
		p.acceptHeaders = append(p.acceptHeaders, r.Header.Get("Accept"))
		switch r.URL.Path {
		case externalSourcePath:
			p.sourceCalls++
			body := p.sourceBody
			if body == nil {
				body = map[string]any{"contract": ExternalContract, "source_type": "acme", "display_name": "Acme Tracker"}
			}
			w.Header().Set("Content-Type", "application/json")
			if p.sourceStatus != 0 {
				w.WriteHeader(p.sourceStatus)
			}
			_ = json.NewEncoder(w).Encode(body)
		case externalIssuesPath:
			p.cursors = append(p.cursors, r.URL.Query().Get("cursor"))
			idx := p.issuesCalls
			p.issuesCalls++
			w.Header().Set("Content-Type", "application/json")
			if p.issuesStatus != 0 {
				w.WriteHeader(p.issuesStatus)
			}
			if p.raw != "" {
				_, _ = w.Write([]byte(p.raw))
				return
			}
			if idx >= len(p.pages) {
				p.t.Errorf("unexpected extra /v1/issues call %d", idx)
				_ = json.NewEncoder(w).Encode(map[string]any{"contract": ExternalContract})
				return
			}
			_ = json.NewEncoder(w).Encode(p.pages[idx])
		default:
			p.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

// externalItem builds a wire item with every field populated, then lets a case
// break exactly one thing.
func externalItem(repo, id string, mutate func(map[string]any)) map[string]any {
	item := map[string]any{
		"repo":        repo,
		"external_id": id,
		"title":       "Retry webhook delivery " + id,
		"body":        "details",
		"author":      "jdoe",
		"labels":      []string{"bug"},
		"assignees":   []string{"carol"},
		"is_tracker":  false,
		"priority":    "high",
		"state":       "Todo",
		"created_at":  "2026-09-30T12:00:00Z",
		"updated_at":  "2026-10-01T08:00:00Z",
		"url":         "https://acme.example/issues/" + id,
	}
	if mutate != nil {
		mutate(item)
	}
	return item
}

func externalPage(cursor string, items ...map[string]any) map[string]any {
	return map[string]any{"contract": ExternalContract, "items": items, "next_cursor": cursor}
}

// newExternalTestSource starts the fake provider and builds the adapter
// against it. The returned source is the concrete type so tests can read the
// dropped-item counter.
func newExternalTestSource(t *testing.T, p *externalProvider, mutate func(*ExternalConfig)) (*externalSource, *httptest.Server) {
	t.Helper()
	p.t = t
	srv := httptest.NewServer(p.handler())
	t.Cleanup(srv.Close)

	cfg := ExternalConfig{
		Name:        "acme",
		DisplayName: "Acme Tracker",
		BaseURL:     srv.URL,
		AuthToken:   externalTestToken,
		Repos:       []string{"your-org/app", "your-org/platform"},
		Timeout:     10 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ws, err := NewExternalSource(cfg)
	if err != nil {
		t.Fatalf("NewExternalSource: %v", err)
	}
	src, ok := ws.(*externalSource)
	if !ok {
		t.Fatalf("NewExternalSource returned %T, want *externalSource", ws)
	}
	return src, srv
}

// TestExternalSourceHappyPathAndPagination is the positive control: two pages
// are followed to an empty cursor, every field maps across, and the handshake
// happens once rather than once per cycle.
func TestExternalSourceHappyPathAndPagination(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{
		externalPage("page-2", externalItem("your-org/app", "ACME-123", func(m map[string]any) {
			m["depends_on"] = []map[string]any{{"repo": "your-org/app", "external_id": "ACME-100", "resolved": false}}
		})),
		externalPage("", externalItem("your-org/platform", "ACME-7", nil)),
		// A second ListIssues replays both pages.
		externalPage("page-2", externalItem("your-org/app", "ACME-123", nil)),
		externalPage("", externalItem("your-org/platform", "ACME-7", nil)),
	}}
	src, _ := newExternalTestSource(t, p, nil)

	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(got), got)
	}
	first := got[0]
	if first.SourceType != "acme" {
		t.Errorf("SourceType = %q, want the configured name", first.SourceType)
	}
	if first.Number != 0 || first.Stage != "" {
		t.Errorf("Number/Stage = %d/%q, want 0 and empty (Hive owns both)", first.Number, first.Stage)
	}
	if first.Repo != "your-org/app" || first.ExternalID != "ACME-123" {
		t.Errorf("identity = %q/%q", first.Repo, first.ExternalID)
	}
	if first.Priority != "high" || first.State != "Todo" || first.Author != "jdoe" {
		t.Errorf("mapped fields = %+v", first)
	}
	if first.URL != "https://acme.example/issues/ACME-123" {
		t.Errorf("URL = %q", first.URL)
	}
	if !first.CreatedAt.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v", first.CreatedAt)
	}
	if len(first.DependsOn) != 1 || first.DependsOn[0].Ref.Key() != "your-org/app!ACME-100" {
		t.Errorf("DependsOn = %+v", first.DependsOn)
	}
	if got[1].ExternalID != "ACME-7" {
		t.Errorf("second page item = %+v", got[1])
	}
	if p.cursors[0] != "" || p.cursors[1] != "page-2" {
		t.Errorf("cursors = %v, want the empty cursor then page-2", p.cursors)
	}
	if src.DroppedItems() != 0 {
		t.Errorf("DroppedItems() = %d, want 0", src.DroppedItems())
	}

	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("second ListIssues: %v", err)
	}
	if p.sourceCalls != 1 {
		t.Errorf("/v1/source calls = %d, want exactly 1 (never per cycle)", p.sourceCalls)
	}
}

// TestExternalSourceSendsAuthAndContractHeaders checks the request side of the
// contract: the provider's own bearer token, nothing else, on every call.
func TestExternalSourceSendsAuthAndContractHeaders(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{externalPage("", externalItem("your-org/app", "ACME-1", nil))}}
	src, _ := newExternalTestSource(t, p, nil)
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(p.authHeaders) != 2 {
		t.Fatalf("recorded %d requests, want 2 (source + issues)", len(p.authHeaders))
	}
	for i := range p.authHeaders {
		if p.authHeaders[i] != "Bearer "+externalTestToken {
			t.Errorf("request %d Authorization = %q", i, p.authHeaders[i])
		}
		if p.contractHeaders[i] != ExternalContract {
			t.Errorf("request %d %s = %q, want %q", i, ExternalContractHeader, p.contractHeaders[i], ExternalContract)
		}
		if p.acceptHeaders[i] != "application/json" {
			t.Errorf("request %d Accept = %q", i, p.acceptHeaders[i])
		}
	}
}

// TestExternalSourceDropsRejectedItems covers the identity and attribution
// rules. Each case pairs one rejected item with one valid item, so the test
// also proves a bad item is withheld rather than failing the whole listing.
func TestExternalSourceDropsRejectedItems(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
	}{
		{"repo outside the allow-list", externalItem("other-org/app", "ACME-9", nil)},
		{"empty repo", externalItem("", "ACME-9", nil)},
		{"empty external_id", externalItem("your-org/app", "", nil)},
		{"external_id with the key separator", externalItem("your-org/app", "ACME!9", nil)},
		{"external_id with a hash", externalItem("your-org/app", "ACME#9", nil)},
		{"external_id with a colon", externalItem("your-org/app", "run-1:spec", nil)},
		{"external_id with a slash", externalItem("your-org/app", "ACME/9", nil)},
		{"external_id with whitespace", externalItem("your-org/app", "ACME 9", nil)},
		{"external_id too long", externalItem("your-org/app", strings.Repeat("a", 129), nil)},
		{"wire source_type", externalItem("your-org/app", "ACME-9", func(m map[string]any) { m["source_type"] = "github" })},
		{"wire number", externalItem("your-org/app", "ACME-9", func(m map[string]any) { m["number"] = 42 })},
		{"wire stage", externalItem("your-org/app", "ACME-9", func(m map[string]any) { m["stage"] = "spec" })},
		{"non-https url", externalItem("your-org/app", "ACME-9", func(m map[string]any) { m["url"] = "http://acme.example/9" })},
		{"javascript url", externalItem("your-org/app", "ACME-9", func(m map[string]any) { m["url"] = "javascript:alert(1)" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keep := externalItem("your-org/app", "ACME-OK", nil)
			p := &externalProvider{pages: []map[string]any{externalPage("", tc.item, keep)}}
			src, _ := newExternalTestSource(t, p, nil)

			got, err := src.ListIssues(context.Background())
			if err != nil {
				t.Fatalf("ListIssues: %v", err)
			}
			if len(got) != 1 || got[0].ExternalID != "ACME-OK" {
				t.Fatalf("items = %+v, want only the valid one", got)
			}
			if src.DroppedItems() != 1 {
				t.Errorf("DroppedItems() = %d, want 1", src.DroppedItems())
			}
		})
	}
}

// TestExternalSourceAcceptsLoopbackItemURL keeps the sidecar case working: a
// loopback http link is a legitimate provider UI, unlike a plain-http public
// one.
func TestExternalSourceAcceptsLoopbackItemURL(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{externalPage("",
		externalItem("your-org/app", "ACME-1", func(m map[string]any) { m["url"] = "http://localhost:9000/i/1" }),
		externalItem("your-org/app", "ACME-2", func(m map[string]any) { m["url"] = "" }),
	)}}
	src, _ := newExternalTestSource(t, p, nil)
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("items = %+v, want both kept", got)
	}
}

// TestExternalSourceDuplicateKeyFailsWholeCall: keeping either copy would be a
// guess about which item the stored holds and claims belong to.
func TestExternalSourceDuplicateKeyFailsWholeCall(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{externalPage("",
		externalItem("your-org/app", "ACME-1", nil),
		externalItem("your-org/app", "ACME-1", func(m map[string]any) { m["title"] = "a different item" }),
	)}}
	src, _ := newExternalTestSource(t, p, nil)
	got, err := src.ListIssues(context.Background())
	if err == nil {
		t.Fatalf("duplicate key must fail the whole call, got %+v", got)
	}
	if !strings.Contains(err.Error(), "your-org/app!ACME-1") {
		t.Errorf("error = %v, want it to name the colliding key", err)
	}
}

// TestExternalSourceDuplicateKeyAcrossPages proves the duplicate check spans
// pagination, not just one page.
func TestExternalSourceDuplicateKeyAcrossPages(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{
		externalPage("p2", externalItem("your-org/app", "ACME-1", nil)),
		externalPage("", externalItem("your-org/app", "ACME-1", nil)),
	}}
	src, _ := newExternalTestSource(t, p, nil)
	if _, err := src.ListIssues(context.Background()); err == nil {
		t.Fatal("a duplicate key across pages must fail the whole call")
	}
}

func TestExternalSourceHoldLabelsAreApplied(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{externalPage("",
		externalItem("your-org/app", "ACME-1", func(m map[string]any) { m["labels"] = []string{"hold"} }),
		externalItem("your-org/app", "ACME-2", func(m map[string]any) { m["labels"] = []string{"bug"} }),
	)}}
	src, _ := newExternalTestSource(t, p, func(c *ExternalConfig) { c.HoldLabels = []string{"hold", "blocked"} })

	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 || got[0].ExternalID != "ACME-2" {
		t.Fatalf("items = %+v, want the held item filtered out", got)
	}
	if src.DroppedItems() != 0 {
		t.Errorf("a held item is filtered, not dropped as invalid: DroppedItems() = %d", src.DroppedItems())
	}
}

// TestExternalSourceDropsBadDependencyEdgeKeepsItem: Hive never invents a
// resolved dependency, and never withholds work over an unnameable blocker.
func TestExternalSourceDropsBadDependencyEdgeKeepsItem(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{externalPage("",
		externalItem("your-org/app", "ACME-1", func(m map[string]any) {
			m["depends_on"] = []map[string]any{
				{"repo": "elsewhere/app", "external_id": "X-1", "resolved": true},
				{"repo": "your-org/app", "external_id": "bad!id", "resolved": true},
				{"repo": "your-org/app", "external_id": "ACME-0", "resolved": true},
			}
		}),
	)}}
	src, _ := newExternalTestSource(t, p, nil)
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("items = %+v, want the item kept", got)
	}
	if len(got[0].DependsOn) != 1 || got[0].DependsOn[0].Ref.ExternalID != "ACME-0" {
		t.Fatalf("DependsOn = %+v, want only the valid edge", got[0].DependsOn)
	}
	if !got[0].DependsOn[0].Resolved {
		t.Error("Resolved must survive the mapping")
	}
}

func TestExternalSourceNormalizesPriority(t *testing.T) {
	p := &externalProvider{pages: []map[string]any{externalPage("",
		externalItem("your-org/app", "ACME-1", func(m map[string]any) { m["priority"] = "URGENT" }),
		externalItem("your-org/app", "ACME-2", func(m map[string]any) { m["priority"] = "critical-now" }),
	)}}
	src, _ := newExternalTestSource(t, p, nil)
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if got[0].Priority != "urgent" {
		t.Errorf("Priority = %q, want urgent", got[0].Priority)
	}
	if got[1].Priority != "" {
		t.Errorf("off-enum Priority = %q, want empty (never an implicit top priority)", got[1].Priority)
	}
}

// TestExternalSourceTransportFailures covers every way the call fails closed.
func TestExternalSourceTransportFailures(t *testing.T) {
	cases := []struct {
		name     string
		provider *externalProvider
		wantIn   string
	}{
		{
			name:     "non-2xx from /v1/issues",
			provider: &externalProvider{issuesStatus: http.StatusBadGateway, raw: `{"contract":"hive.worksource/v1"}`},
			wantIn:   "502",
		},
		{
			name:     "malformed JSON",
			provider: &externalProvider{raw: `{"contract": "hive.worksource/v1", "items": [`},
			wantIn:   "decode response",
		},
		{
			name:     "contract mismatch on /v1/issues",
			provider: &externalProvider{pages: []map[string]any{{"contract": "hive.worksource/v2", "items": []map[string]any{}}}},
			wantIn:   "hive.worksource/v2",
		},
		{
			name: "contract mismatch on /v1/source",
			provider: &externalProvider{
				sourceBody: map[string]any{"contract": "something-else", "source_type": "acme"},
			},
			wantIn: "something-else",
		},
		{
			name: "source_type mismatch on /v1/source",
			provider: &externalProvider{
				sourceBody: map[string]any{"contract": ExternalContract, "source_type": "other"},
			},
			wantIn: "source_type",
		},
		{
			name: "non-2xx from /v1/source",
			provider: &externalProvider{
				sourceStatus: http.StatusUnauthorized,
				sourceBody:   map[string]any{"error": "bad token"},
			},
			wantIn: "401",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := newExternalTestSource(t, tc.provider, nil)
			got, err := src.ListIssues(context.Background())
			if err == nil {
				t.Fatalf("ListIssues must fail, got %+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
			if !strings.Contains(err.Error(), `"acme"`) {
				t.Errorf("error = %v, want it to name the source", err)
			}
			if strings.Contains(err.Error(), externalTestToken) {
				t.Errorf("error leaked the provider token: %v", err)
			}
		})
	}
}

// TestExternalSourceRedactsEchoedToken: a shim that reflects the Authorization
// header into its error body must not put the credential in Hive's logs.
func TestExternalSourceRedactsEchoedToken(t *testing.T) {
	p := &externalProvider{
		sourceStatus: http.StatusForbidden,
		sourceBody:   map[string]any{"error": "token " + externalTestToken + " is not allowed"},
	}
	src, _ := newExternalTestSource(t, p, nil)
	_, err := src.ListIssues(context.Background())
	if err == nil {
		t.Fatal("a 403 must fail the call")
	}
	if strings.Contains(err.Error(), externalTestToken) {
		t.Fatalf("error leaked the token: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error = %v, want the echoed token redacted", err)
	}
}

// TestExternalSourceLimits exercises the page, item, and body caps.
func TestExternalSourceLimits(t *testing.T) {
	t.Run("page limit", func(t *testing.T) {
		pages := make([]map[string]any, externalMaxPages+1)
		for i := range pages {
			pages[i] = externalPage(fmt.Sprintf("c%d", i+1))
		}
		p := &externalProvider{pages: pages}
		src, _ := newExternalTestSource(t, p, nil)
		_, err := src.ListIssues(context.Background())
		if err == nil || !strings.Contains(err.Error(), "pages") {
			t.Fatalf("err = %v, want a page-limit error", err)
		}
	})

	t.Run("item limit", func(t *testing.T) {
		items := make([]map[string]any, externalMaxItems+1)
		for i := range items {
			items[i] = externalItem("your-org/app", fmt.Sprintf("ACME-%d", i), nil)
		}
		p := &externalProvider{pages: []map[string]any{externalPage("", items...)}}
		src, _ := newExternalTestSource(t, p, nil)
		_, err := src.ListIssues(context.Background())
		if err == nil || !strings.Contains(err.Error(), "items") {
			t.Fatalf("err = %v, want an item-limit error", err)
		}
	})

	t.Run("body limit", func(t *testing.T) {
		oversize := `{"contract":"hive.worksource/v1","items":[],"pad":"` + strings.Repeat("a", externalMaxBodyBytes) + `"}`
		p := &externalProvider{raw: oversize}
		src, _ := newExternalTestSource(t, p, nil)
		_, err := src.ListIssues(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("err = %v, want a body-size error", err)
		}
	})
}

// TestExternalSourceRefusesForeignRedirect: the provider token must never be
// replayed to a host the operator did not configure.
func TestExternalSourceRefusesForeignRedirect(t *testing.T) {
	var foreignAuth string
	foreignCalls := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls++
		foreignAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"contract": ExternalContract, "source_type": "acme"})
	}))
	defer foreign.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	ws, err := NewExternalSource(ExternalConfig{
		Name: "acme", BaseURL: redirector.URL, AuthToken: externalTestToken,
		Repos: []string{"your-org/app"}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewExternalSource: %v", err)
	}
	if _, err := ws.ListIssues(context.Background()); err == nil {
		t.Fatal("a redirect to a foreign host must fail the call")
	}
	if foreignCalls != 0 {
		t.Fatalf("the foreign host was called %d times with Authorization %q", foreignCalls, foreignAuth)
	}
}

// TestExternalSourceKeysAreStableAndSourceNeutral is the identity guard: every
// valid external item produces a "repo!id" key that round-trips through
// ParseKey and is never mistaken for GitHub-backed work or a run stage.
func TestExternalSourceKeysAreStableAndSourceNeutral(t *testing.T) {
	ids := []string{"ACME-123", "123", "a", "A.b_c-9", strings.Repeat("z", 128)}
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, externalItem("your-org/app", id, nil))
	}
	p := &externalProvider{pages: []map[string]any{externalPage("", items...)}}
	src, _ := newExternalTestSource(t, p, nil)

	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("got %d items, want %d", len(got), len(ids))
	}
	for i, issue := range got {
		ref := RefFromIssue(issue)
		want := "your-org/app!" + ids[i]
		if ref.Key() != want {
			t.Errorf("Key() = %q, want %q", ref.Key(), want)
		}
		parsed, ok := ParseKey(ref.Key())
		if !ok {
			t.Fatalf("ParseKey(%q) failed", ref.Key())
		}
		if parsed.Repo != "your-org/app" || parsed.ExternalID != ids[i] {
			t.Errorf("ParseKey round-trip = %+v", parsed)
		}
		if parsed.IsRunStage() {
			t.Errorf("%q must not read as a run-stage key", ref.Key())
		}
		if ref.IsGitHubIssue() || parsed.IsGitHubIssue() {
			t.Errorf("%q must not read as GitHub-backed work", ref.Key())
		}
		if ref.Display() != ids[i] {
			t.Errorf("Display() = %q, want the native id (never #0)", ref.Display())
		}
	}
}

// TestExternalSourceWriteBackIsUnsupported: v1 is read-only, so design mode
// must see the interfaces as absent and show its unsupported message.
func TestExternalSourceWriteBackIsUnsupported(t *testing.T) {
	ws, err := NewExternalSource(ExternalConfig{
		Name: "acme", BaseURL: "https://acme.example", AuthToken: externalTestToken,
		Repos: []string{"your-org/app"},
	})
	if err != nil {
		t.Fatalf("NewExternalSource: %v", err)
	}
	if _, ok := ws.(LabelMutator); ok {
		t.Error("external v1 must not implement LabelMutator")
	}
	if _, ok := ws.(Commenter); ok {
		t.Error("external v1 must not implement Commenter")
	}
	if _, ok := ws.(StatusTransitioner); ok {
		t.Error("external v1 must not implement StatusTransitioner")
	}
}

func TestExternalSourceTimeoutFailsClosed(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()

	ws, err := NewExternalSource(ExternalConfig{
		Name: "acme", BaseURL: slow.URL, AuthToken: externalTestToken,
		Repos: []string{"your-org/app"}, Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewExternalSource: %v", err)
	}
	start := time.Now()
	if _, err := ws.ListIssues(context.Background()); err == nil {
		t.Fatal("a provider that never answers must fail the call")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ListIssues took %v, want it bounded by the configured timeout", elapsed)
	}
}

func TestExternalSourceCABundleMustBePEM(t *testing.T) {
	_, err := NewExternalSource(ExternalConfig{
		Name: "acme", BaseURL: "https://acme.example", AuthToken: externalTestToken,
		Repos: []string{"your-org/app"}, CABundle: "not a certificate",
	})
	if err == nil {
		t.Fatal("a ca_bundle with no PEM certificates must fail construction")
	}
}
