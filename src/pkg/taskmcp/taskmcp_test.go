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
	scope      Scope
	items      []RelatedItem
	checks     []CheckHealth
	scopeErr   error
	contextErr error
	relatedErr error
	ciErr      error
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
			if len(tools) != 4 {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *Handler
			if tc.name == "provider missing" {
				h = NewHandler(nil)
			} else {
				h = NewHandler(tc.p)
			}
			resp := serveRPC(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tc.tool))
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Fatalf("error = %#v, want %d", resp.Error, tc.code)
			}
		})
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
