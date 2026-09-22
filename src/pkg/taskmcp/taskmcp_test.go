package taskmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProvider struct {
	scope        Scope
	items        []RelatedItem
	checks       []CheckHealth
	conventions  []RepoConvention
	deps         DependenciesData
	history      []RelatedItem
	knowledge    []KnowledgeItem
	scopeErr     error
	contextErr   error
	relatedErr   error
	ciErr        error
	convErr      error
	depErr       error
	historyErr   error
	knowledgeErr error
}

func (f fakeProvider) Scope(r *http.Request, args map[string]any) (Scope, error) {
	if f.scopeErr != nil {
		return Scope{}, f.scopeErr
	}
	repo, _ := args["repo"].(string)
	if repo != "" && repo != f.scope.Repo {
		return Scope{}, ErrForbidden
	}
	if task := r.Header.Get(HeaderTaskID); task != "" && task != f.scope.TaskID {
		return Scope{}, ErrForbidden
	}
	return f.scope, nil
}
func (f fakeProvider) TaskContext(context.Context, Scope) (TaskContextData, error) {
	if f.contextErr != nil {
		return TaskContextData{}, f.contextErr
	}
	return TaskContextData{Assignment: AssignmentData{TaskID: f.scope.TaskID, Repo: f.scope.Repo, Number: f.scope.Number}, Data: ServedText{Title: "title", Body: "body"}, Policies: PolicyData{CacheOnly: true, HubLaunchedOnly: true, StreamableHTTP: true, RoutePrefix: "/api/contribute/", ServedTextSchema: "data", RepoScoped: true}}, nil
}
func (f fakeProvider) RelatedWork(_ context.Context, _ Scope, p PageRequest) (RelatedWorkData, PageInfo, error) {
	if f.relatedErr != nil {
		return RelatedWorkData{}, PageInfo{}, f.relatedErr
	}
	data, info := PaginateRelated(f.items, p)
	return data, info, nil
}
func (f fakeProvider) CIHealth(_ context.Context, _ Scope, p PageRequest) (CIHealthData, PageInfo, error) {
	if f.ciErr != nil {
		return CIHealthData{}, PageInfo{}, f.ciErr
	}
	data, info := PaginateChecks(f.checks, p)
	return data, info, nil
}
func (f fakeProvider) RepoConventions(_ context.Context, s Scope, p PageRequest) (RepoConventionsData, PageInfo, error) {
	if f.convErr != nil {
		return RepoConventionsData{}, PageInfo{}, f.convErr
	}
	items, info := PaginateConventions(f.conventions, p)
	source := "human"
	if len(items) == 0 {
		source = "none"
	}
	return RepoConventionsData{Repo: s.Repo, Source: source, Conventions: items}, info, nil
}
func (f fakeProvider) Dependencies(_ context.Context, _ Scope, p PageRequest) (DependenciesData, PageInfo, error) {
	if f.depErr != nil {
		return DependenciesData{}, PageInfo{}, f.depErr
	}
	data, info := PaginateDependencies(f.deps, p)
	return data, info, nil
}
func (f fakeProvider) History(_ context.Context, _ Scope, p PageRequest) (HistoryData, PageInfo, error) {
	if f.historyErr != nil {
		return HistoryData{}, PageInfo{}, f.historyErr
	}
	data, info := PaginateHistory(f.history, p)
	return data, info, nil
}
func (f fakeProvider) Knowledge(_ context.Context, _ Scope, _ string, p PageRequest) (KnowledgeData, PageInfo, error) {
	if f.knowledgeErr != nil {
		return KnowledgeData{}, PageInfo{}, f.knowledgeErr
	}
	data, info := PaginateKnowledge(f.knowledge, p)
	return data, info, nil
}

func TestHandlerProtocolMethods(t *testing.T) {
	h := NewHandler(fakeProvider{})
	for _, tc := range []struct {
		name   string
		body   string
		assert func(*testing.T, rpcResponse)
	}{
		{"initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, func(t *testing.T, resp rpcResponse) {
			m := asMap(t, resp.Result)
			if asMap(t, m["serverInfo"])["name"] != ServerName {
				t.Fatalf("initialize result = %#v", m)
			}
		}},
		{"tools list", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, func(t *testing.T, resp rpcResponse) {
			tools := asSlice(t, asMap(t, resp.Result)["tools"])
			if len(tools) != 8 {
				t.Fatalf("tools = %#v", tools)
			}
		}},
		{"initialized notification", `{"jsonrpc":"2.0","id":3,"method":"notifications/initialized"}`, func(t *testing.T, resp rpcResponse) {
			if len(asMap(t, resp.Result)) != 0 {
				t.Fatalf("initialized result = %#v", resp.Result)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := serveRPC(t, h, tc.body)
			if resp.Error != nil {
				t.Fatalf("error = %#v", resp.Error)
			}
			tc.assert(t, resp)
		})
	}
}

func TestHandlerRejectsBadRequests(t *testing.T) {
	h := NewHandler(fakeProvider{})
	t.Run("method", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, EndpointPath, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("status=%d allow=%q", rec.Code, rec.Header().Get("Allow"))
		}
	})
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"malformed json", `{`, -32700},
		{"bad jsonrpc", `{"jsonrpc":"1.0","id":1,"method":"initialize"}`, -32600},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"bogus"}`, -32601},
		{"missing tool params", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, -32602},
		{"invalid tool params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`, -32602},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bogus","arguments":{}}}`, -32602},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := serveRPC(t, h, tc.body)
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Fatalf("error = %#v, want code %d", resp.Error, tc.code)
			}
		})
	}
}

func TestHandlerScopesWrongRepo(t *testing.T) {
	h := NewHandler(fakeProvider{scope: Scope{TaskID: "t1", Repo: "owner/repo", Number: 7}})
	resp := serveRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_context","arguments":{"repo":"other/repo"}}}`)
	if resp.Error == nil || resp.Error.Code != -32003 {
		t.Fatalf("error = %#v, want forbidden", resp.Error)
	}
}

func TestHandlerReturnsTypedRefusalInDataEnvelope(t *testing.T) {
	h := NewHandler(fakeProvider{scopeErr: RefusalError{Err: fmt.Errorf("%w: no", ErrForbidden), Data: RefusalData{Code: "outside_lease_scope", LeaseID: "lease-1", Repo: "owner/other"}}})
	env := resultEnvelope(t, serveRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"related_work","arguments":{"repo":"owner/other"}}}`))
	data := asMap(t, env["data"])
	if data["type"] != "refusal" || data["code"] != "outside_lease_scope" || data["lease_id"] != "lease-1" || data["reason"] == "" {
		t.Fatalf("refusal data = %#v", data)
	}
}

func TestToolCallsReturnDataEnvelopes(t *testing.T) {
	items := make([]RelatedItem, MaxPageSize+5)
	checks := make([]CheckHealth, MaxPageSize+3)
	for i := range items {
		items[i] = RelatedItem{Kind: "pull_request", Repo: "owner/repo", Number: i + 1, Data: ServedText{Title: "x"}}
	}
	for i := range checks {
		checks[i] = CheckHealth{Name: fmt.Sprintf("check-%02d", i), State: "success"}
	}
	h := NewHandler(fakeProvider{scope: Scope{TaskID: "t1", Repo: "owner/repo", Number: 7}, items: items, checks: checks})
	for _, tc := range []struct {
		tool   string
		limit  any
		cursor any
		check  func(*testing.T, map[string]any)
	}{
		{"task_context", nil, nil, func(t *testing.T, env map[string]any) {
			data := asMap(t, env["data"])
			if asMap(t, data["data"])["body"] != "body" || strings.Contains(fmt.Sprint(data), "instructions") {
				t.Fatalf("task context = %#v", data)
			}
		}},
		{"related_work", 99, nil, func(t *testing.T, env map[string]any) {
			if got := len(asSlice(t, asMap(t, env["data"])["items"])); got != MaxPageSize {
				t.Fatalf("related items = %d", got)
			}
			if asMap(t, env["page"])["more"] != true {
				t.Fatalf("page = %#v", env["page"])
			}
		}},
		{"ci_health", "2", "1", func(t *testing.T, env map[string]any) {
			checks := asSlice(t, asMap(t, env["data"])["checks"])
			if len(checks) != 2 || asMap(t, checks[0])["name"] != "check-01" {
				t.Fatalf("checks = %#v", checks)
			}
		}},
		{"context_bundle", 99, nil, func(t *testing.T, env map[string]any) {
			data := asMap(t, env["data"])
			if asMap(t, data["task_context"])["assignment"] == nil || asMap(t, data["related_work"])["items"] == nil || asMap(t, data["ci_health"])["checks"] == nil {
				t.Fatalf("bundle = %#v", data)
			}
			if asMap(t, env["page"])["more"] != true {
				t.Fatalf("bundle page = %#v", env["page"])
			}
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			args := `{"repo":"owner/repo"`
			if tc.limit != nil {
				switch v := tc.limit.(type) {
				case int:
					args += fmt.Sprintf(`,"limit":%d`, v)
				case string:
					args += fmt.Sprintf(`,"limit":%q`, v)
				}
			}
			if tc.cursor != nil {
				args += fmt.Sprintf(`,"cursor":%q`, tc.cursor)
			}
			args += `}`
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tc.tool, args)
			env := resultEnvelope(t, serveRPC(t, h, body))
			tc.check(t, env)
		})
	}
}

func TestContextBundleComposesEmptySlots(t *testing.T) {
	h := NewHandler(fakeProvider{scope: Scope{TaskID: "t1", Repo: "owner/repo", Number: 7}})
	env := resultEnvelope(t, serveRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_bundle","arguments":{"repo":"owner/repo"}}}`))
	data := asMap(t, env["data"])
	if got := len(asSlice(t, asMap(t, data["related_work"])["items"])); got != 0 {
		t.Fatalf("related slot len = %d", got)
	}
	if got := len(asSlice(t, asMap(t, data["ci_health"])["checks"])); got != 0 {
		t.Fatalf("ci slot len = %d", got)
	}
}

func TestPhase3ToolCallsAreCappedAndDataScoped(t *testing.T) {
	fp := fakeProvider{scope: Scope{TaskID: "t1", Repo: "owner/repo", Number: 7}}
	for i := 0; i < MaxPageSize+2; i++ {
		fp.conventions = append(fp.conventions, RepoConvention{Slug: fmt.Sprintf("c%d", i), Source: "human", Data: ServedText{Title: "conv", Body: "text"}})
		fp.history = append(fp.history, RelatedItem{Kind: "pull_request", Repo: "owner/repo", Number: i + 10, Data: ServedText{Title: "hist", Body: "text"}})
		fp.knowledge = append(fp.knowledge, KnowledgeItem{Repo: "owner/repo", Slug: fmt.Sprintf("k%d", i), Data: ServedText{Title: "know", Body: "text"}})
		fp.deps.BlockedBy = append(fp.deps.BlockedBy, DependencyNode{ID: fmt.Sprintf("b%d", i), Title: "blocker"})
	}
	h := NewHandler(fp)
	for _, tc := range []struct{ tool, key string }{{ToolRepoConventions, "conventions"}, {ToolDependencies, "blocked_by"}, {ToolHistory, "items"}, {ToolKnowledge, "items"}} {
		t.Run(tc.tool, func(t *testing.T) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"repo":"owner/repo","limit":99,"query":"text"}}}`, tc.tool)
			env := resultEnvelope(t, serveRPC(t, h, body))
			data := asMap(t, env["data"])
			if got := len(asSlice(t, data[tc.key])); got != MaxPageSize {
				t.Fatalf("%s len = %d", tc.key, got)
			}
			if asMap(t, env["page"])["more"] != true {
				t.Fatalf("page = %#v", env["page"])
			}
			if strings.Contains(fmt.Sprint(env), "instructions") {
				t.Fatalf("served text leaked outside data-shaped payload: %#v", env)
			}
		})
	}
}

func TestContextBundleIncludeOptInPhase3(t *testing.T) {
	h := NewHandler(fakeProvider{scope: Scope{TaskID: "t1", Repo: "owner/repo", Number: 7}, conventions: []RepoConvention{{Source: "human", Data: ServedText{Title: "conv"}}}, knowledge: []KnowledgeItem{{Repo: "owner/repo", Data: ServedText{Title: "know"}}}})
	env := resultEnvelope(t, serveRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_bundle","arguments":{"repo":"owner/repo","include":["task_context","repo_conventions","knowledge"],"query":"know"}}}`))
	data := asMap(t, env["data"])
	if data["related_work"] != nil || data["ci_health"] != nil {
		t.Fatalf("default-only slots should be omitted when include is explicit: %#v", data)
	}
	if asMap(t, data["repo_conventions"])["conventions"] == nil || asMap(t, data["knowledge"])["items"] == nil {
		t.Fatalf("phase3 slots missing: %#v", data)
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    fakeProvider
		tool string
		code int
	}{
		{"provider missing", fakeProvider{}, "task_context", -32000},
		{"scope generic", fakeProvider{scopeErr: errors.New("boom")}, "task_context", -32000},
		{"scope forbidden", fakeProvider{scopeErr: ErrForbidden}, "task_context", -32003},
		{"context forbidden", fakeProvider{contextErr: ErrForbidden}, "task_context", -32003},
		{"context generic", fakeProvider{contextErr: errors.New("boom")}, "task_context", -32000},
		{"related generic", fakeProvider{relatedErr: errors.New("boom")}, "related_work", -32000},
		{"ci generic", fakeProvider{ciErr: errors.New("boom")}, "ci_health", -32000},
		{"conventions generic", fakeProvider{convErr: errors.New("boom")}, "repo_conventions", -32000},
		{"dependencies generic", fakeProvider{depErr: errors.New("boom")}, "dependencies", -32000},
		{"history generic", fakeProvider{historyErr: errors.New("boom")}, "history", -32000},
		{"knowledge generic", fakeProvider{knowledgeErr: errors.New("boom")}, "knowledge", -32000},
		{"bundle phase3 forbidden", fakeProvider{convErr: ErrForbidden}, "context_bundle", -32003},
		{"bundle related generic", fakeProvider{relatedErr: errors.New("boom")}, "context_bundle", -32000},
		{"bundle ci generic", fakeProvider{ciErr: errors.New("boom")}, "context_bundle", -32000},
		{"bundle deps generic", fakeProvider{depErr: errors.New("boom")}, "context_bundle", -32000},
		{"bundle history generic", fakeProvider{historyErr: errors.New("boom")}, "context_bundle", -32000},
		{"bundle knowledge generic", fakeProvider{knowledgeErr: errors.New("boom")}, "context_bundle", -32000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *Handler
			if tc.name == "provider missing" {
				h = NewHandler(nil)
			} else {
				h = NewHandler(tc.p)
			}
			args := `{}`
			if strings.HasPrefix(tc.name, "bundle ") {
				args = `{"include":["repo_conventions"]}`
			}
			if tc.name == "bundle related generic" {
				args = `{"include":["related_work"]}`
			}
			if tc.name == "bundle ci generic" {
				args = `{"include":["ci_health"]}`
			}
			if tc.name == "bundle deps generic" {
				args = `{"include":["dependencies"]}`
			}
			if tc.name == "bundle history generic" {
				args = `{"include":["history"]}`
			}
			if tc.name == "bundle knowledge generic" {
				args = `{"include":["knowledge"]}`
			}
			resp := serveRPC(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tc.tool, args))
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Fatalf("error = %#v, want %d", resp.Error, tc.code)
			}

		})
	}
}

func TestRefusalError(t *testing.T) {
	base := errors.New("denied")
	err := RefusalError{Data: RefusalData{Reason: "outside"}, Err: base}
	if err.Error() != "denied" || !errors.Is(err, base) {
		t.Fatalf("wrapped refusal = %v", err)
	}
	err = RefusalError{Data: RefusalData{Reason: "outside"}}
	if err.Error() != "outside" || err.Unwrap() != nil {
		t.Fatalf("data refusal = %v unwrap=%v", err, err.Unwrap())
	}
}

func TestHelpers(t *testing.T) {
	items := []RelatedItem{{Repo: "b", Number: 2}, {Repo: "a", Number: 9}, {Repo: "a", Number: 1}}
	SortRelated(items)
	if items[0].Repo != "a" || items[0].Number != 1 {
		t.Fatalf("sorted = %#v", items)
	}
	if err := RequireRepo(Scope{Repo: "owner/repo"}, "owner/repo"); err != nil {
		t.Fatalf("RequireRepo same = %v", err)
	}
	if err := RequireRepo(Scope{Repo: "owner/repo"}, "other/repo"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("RequireRepo wrong = %v", err)
	}
	if cursorIndex("bad") != 0 || cursorIndex("-1") != 0 || cursorIndex("3") != 3 {
		t.Fatalf("cursorIndex failed")
	}
	if queryFromArgs(nil) != "" || queryFromArgs(map[string]any{"query": "  q  "}) != "q" {
		t.Fatalf("queryFromArgs failed")
	}
	inc := includeSet(map[string]any{"include": "task_context,knowledge,bogus"})
	if !inc[ToolTaskContext] || !inc[ToolKnowledge] || inc[ToolRelatedWork] {
		t.Fatalf("includeSet string = %#v", inc)
	}
	inc = includeSet(map[string]any{"include": []any{ToolDependencies, 7}})
	if !inc[ToolDependencies] || inc[ToolTaskContext] {
		t.Fatalf("includeSet slice = %#v", inc)
	}
	if got := combinePages(2, PageInfo{Limit: 2, More: true, NextCursor: "2"}); !got.More || got.NextCursor != "2" || got.Limit != 2 {
		t.Fatalf("combinePages = %#v", got)
	}
}

func TestPhase3PaginationEmptyAndCursorPastEnd(t *testing.T) {
	if items, page := PaginateConventions(nil, PageRequest{Limit: 2, Cursor: "9"}); len(items) != 0 || page.More {
		t.Fatalf("conventions=%#v page=%#v", items, page)
	}
	if data, page := PaginateKnowledge(nil, PageRequest{Limit: 2}); len(data.Items) != 0 || page.More {
		t.Fatalf("knowledge=%#v page=%#v", data, page)
	}
	deps, page := PaginateDependencies(DependenciesData{BlockedBy: []DependencyNode{{ID: "a"}, {ID: "b"}}, Blocks: []DependencyNode{{ID: "c"}}}, PageRequest{Limit: 2, Cursor: "1"})
	if len(deps.BlockedBy) != 1 || len(deps.Blocks) != 1 || page.More {
		t.Fatalf("deps=%#v page=%#v", deps, page)
	}
}

func serveRPC(t *testing.T, h *Handler, body string) rpcResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(body)))
	var resp rpcResponse
	decodeJSON(t, rec.Body.String(), &resp)
	return resp
}

func resultEnvelope(t *testing.T, resp rpcResponse) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("rpc error: %#v", resp.Error)
	}
	content := asSlice(t, asMap(t, resp.Result)["content"])
	if len(content) != 1 {
		t.Fatalf("content = %#v", content)
	}
	text, ok := asMap(t, content[0])["text"].(string)
	if !ok {
		t.Fatalf("content text = %#v", content[0])
	}
	var env map[string]any
	decodeJSON(t, text, &env)
	return env
}

func decodeJSON(t *testing.T, raw string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%T %#v is not map", v, v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%T %#v is not slice", v, v)
	}
	return s
}
