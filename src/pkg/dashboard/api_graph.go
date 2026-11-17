package dashboard

import (
	"context"
	"net/http"
	"strconv"
)

// POST /api/graph: the customer graph over GraphQL (hivecommons/hive#18742).
// It is read-only: the schema has no mutations, so although the route is a
// POST it changes nothing, which is why a read-only user may call it. Who
// may see what is decided by the customers service on the viewer of the
// request: a read-only user reaches only their own profile by any path.

const (
	// customersGraphPath is the endpoint. roleEnforcement lets a read-only
	// user post to exactly this path.
	customersGraphPath = "/api/graph"
	// customersGraphOperationMax bounds the operation name kept for the
	// audit line.
	customersGraphOperationMax = 64
)

// CustomerGraphRequest is a GraphQL request body.
type CustomerGraphRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// CustomerGraphResult is a GraphQL response. Body is the encoded
// {"data": ..., "errors": ...} document, written as is.
type CustomerGraphResult struct {
	Body   []byte
	Nodes  int
	Errors int
}

// CustomersGraphService executes read-only queries against the customer
// graph as the viewer. An error wraps the ErrCustomer* sentinels; a query
// that fails is not an error but a GraphQL error inside Body.
type CustomersGraphService interface {
	Graph(ctx context.Context, v CustomersViewer, req CustomerGraphRequest) (CustomerGraphResult, error)
}

func (s *Server) registerCustomersGraphRoutes() {
	s.mux.HandleFunc("POST /api/graph", s.handleCustomersGraph)
}

// customersGraphOperation is the operation name as the audit log keeps it:
// letters, digits and underscores only, and bounded. The query text itself
// is never logged.
func customersGraphOperation(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name) && len(out) < customersGraphOperationMax; i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "anonymous"
	}
	return string(out)
}

func (s *Server) handleCustomersGraph(w http.ResponseWriter, r *http.Request) {
	v, ok := customersViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	graph, ok := svc.(CustomersGraphService)
	if !ok {
		jsonError(w, "the customer graph is not available on this hive", http.StatusServiceUnavailable)
		return
	}
	// The body cap covers the query text (the engine caps it at 8 KB) and
	// its variables.
	var req CustomerGraphRequest
	if !decodeCustomersBody(w, r, &req) {
		return
	}
	if req.Query == "" {
		jsonError(w, "a query is required", http.StatusBadRequest)
		return
	}
	res, err := graph.Graph(r.Context(), v, req)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_graph_query", auditDetail(
		"operation", customersGraphOperation(req.OperationName),
		"nodes", strconv.Itoa(res.Nodes), "errors", strconv.Itoa(res.Errors), "full", boolString(v.Full)), "")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(res.Body)
}
