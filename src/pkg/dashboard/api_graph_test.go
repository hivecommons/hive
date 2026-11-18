package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeGraphService is the customers fake plus the graph.
type fakeGraphService struct {
	fakeCustomersService
	queries []string
}

func (f *fakeGraphService) Graph(_ context.Context, v CustomersViewer, req CustomerGraphRequest) (CustomerGraphResult, error) {
	f.note("Graph", v, req.OperationName)
	f.queries = append(f.queries, req.Query)
	if f.err != nil {
		return CustomerGraphResult{}, f.err
	}
	return CustomerGraphResult{Body: []byte(`{"data":{"me":{"name":"` + v.User + `"}}}`), Nodes: 4, Errors: 1}, nil
}

const graphBody = `{"query":"query Timeline { me { name } }","operationName":"Timeline","variables":{"id":"p_x"}}`

// TestCustomersGraphRoleMatrix: every signed-in, named user may query the
// graph, the read role included, because the endpoint only reads. The
// service is handed the viewer, and decides what that viewer reaches.
func TestCustomersGraphRoleMatrix(t *testing.T) {
	s, _ := apiServer(t)
	want := map[string]int{
		"owner": http.StatusOK, "read-write": http.StatusOK, "merger": http.StatusOK, "read": http.StatusOK,
		"unverified owner": http.StatusForbidden, "read with no name": http.StatusForbidden,
	}
	for _, who := range []customersWho{custOwner, custReadWrite, custMerger, custRead, custFakeOwner, custNamelessRO} {
		fake := &fakeGraphService{}
		s.SetCustomersService(fake)
		rec := customersDo(s, who, http.MethodPost, "/api/graph", graphBody)
		if rec.Code != want[who.name] {
			t.Fatalf("POST /api/graph as %s = %d, want %d: %s", who.name, rec.Code, want[who.name], rec.Body.String())
		}
		if rec.Code != http.StatusOK {
			if len(fake.calls) != 0 {
				t.Fatalf("a refused query reached the service: %v", fake.calls)
			}
			continue
		}
		full := who.name != "read"
		wantCall := fmt.Sprintf("Graph user=%s full=%v owner=%v arg=Timeline", who.user, full, who.name == "owner")
		if len(fake.calls) != 1 || fake.calls[0] != wantCall {
			t.Fatalf("service saw %v, want %q", fake.calls, wantCall)
		}
		if rec.Body.String() != `{"data":{"me":{"name":"`+who.user+`"}}}` || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %q %v", rec.Body.String(), rec.Header())
		}
	}
	// The read role's exemption is this exact path and method.
	fake := &fakeGraphService{}
	s.SetCustomersService(fake)
	for _, r := range []customersRoute{
		{http.MethodPost, "/api/graph/", graphBody},
		{http.MethodPost, "/api/graph/x", graphBody},
		{http.MethodPut, "/api/graph", graphBody},
		{http.MethodDelete, "/api/graph", ""},
		{http.MethodPost, "/api/graphql", graphBody},
	} {
		if rec := customersDo(s, custRead, r.method, r.path, r.body); rec.Code == http.StatusOK {
			t.Errorf("%s %s as read = 200", r.method, r.path)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a near-miss reached the service: %v", fake.calls)
	}
}

func TestCustomersGraphErrorsAndAudit(t *testing.T) {
	s, _ := apiServer(t)
	// Off.
	if rec := customersDo(s, custOwner, http.MethodPost, "/api/graph", graphBody); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("graph while customers are off = %d", rec.Code)
	}
	// A service without the graph.
	s.SetCustomersService(&fakeCustomersService{})
	if rec := customersDo(s, custOwner, http.MethodPost, "/api/graph", graphBody); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("graph without an engine = %d", rec.Code)
	}
	fake := &fakeGraphService{}
	s.SetCustomersService(fake)
	for body, want := range map[string]int{
		`{not json`:             http.StatusBadRequest,
		`{"query":""}`:          http.StatusBadRequest,
		`{"variables":{"a":1}}`: http.StatusBadRequest,
		`{"query":"` + strings.Repeat("x", customersMaxBody) + `"}`: http.StatusBadRequest,
	} {
		if rec := customersDo(s, custOwner, http.MethodPost, "/api/graph", body); rec.Code != want {
			t.Errorf("body %.30q = %d, want %d", body, rec.Code, want)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a malformed request reached the service: %v", fake.calls)
	}
	fake.err = ErrCustomerUnavailable
	if rec := customersDo(s, custOwner, http.MethodPost, "/api/graph", graphBody); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("engine unavailable = %d", rec.Code)
	}
	fake.err = nil

	// The audit line carries the operation name (cleaned), the node and
	// error counts and whether the viewer had the full view. Never the query.
	secret := `{"query":"query { person(id: \"p_secret\") { notes } }","operationName":"Peek at <script>; DROP"}`
	if rec := customersDo(s, custRead, http.MethodPost, "/api/graph", secret); rec.Code != http.StatusOK {
		t.Fatalf("query = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := customersDo(s, custOwner, http.MethodPost, "/api/graph", `{"query":"{ me { name } }"}`); rec.Code != http.StatusOK {
		t.Fatalf("anonymous query = %d", rec.Code)
	}
	var lines []string
	for _, e := range s.audit.Recent(50) {
		if e.Action == "customer_graph_query" {
			lines = append(lines, e.User+"|"+e.Detail)
		}
	}
	joined := strings.Join(lines, "\n")
	if len(lines) != 2 ||
		!strings.Contains(joined, "reed|operation=PeekatscriptDROP, nodes=4, errors=1, full=false") ||
		!strings.Contains(joined, "olivia|operation=anonymous, nodes=4, errors=1, full=true") {
		t.Fatalf("audit = %q", joined)
	}
	if strings.Contains(joined, "p_secret") || strings.Contains(joined, "notes") || strings.Contains(joined, "<") {
		t.Fatalf("the audit log holds query text: %q", joined)
	}
	long := customersGraphOperation(strings.Repeat("a", 500))
	if len(long) != customersGraphOperationMax {
		t.Fatalf("operation name bound = %d", len(long))
	}
}
