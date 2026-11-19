// Package gql serves the customer graph over GraphQL, read-only. It is the
// only package that imports the GraphQL library. Every access decision is
// made by customers.Service on the viewer of the request; the resolvers
// here only shape what the service returns.
package gql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	graphql "github.com/graph-gophers/graphql-go"

	"github.com/hivecommons/hive/pkg/customers"
)

// Limits bound one query.
type Limits struct {
	// MaxDepth is the deepest field nesting a query may use.
	MaxDepth int
	// MaxQueryBytes is the longest query text accepted.
	MaxQueryBytes int
	// MaxPage is the largest page a list field returns.
	MaxPage int
	// MaxNodes is how many people, edges and artefacts one query may
	// materialise in total.
	MaxNodes int
	// Timeout bounds the execution of one query.
	Timeout time.Duration
}

// DefaultLimits are the limits of POST /api/graph.
var DefaultLimits = Limits{
	MaxDepth:      8,
	MaxQueryBytes: 8 << 10,
	MaxPage:       100,
	MaxNodes:      5000,
	Timeout:       5 * time.Second,
}

// defaultPage is the page size when a query does not ask for one.
const defaultPage = 25

// maxSelections bounds how far fragment spreads may expand one operation.
const maxSelections = 2000

// ErrNodeLimit is the resolver error once a query has materialised more
// nodes than Limits.MaxNodes.
var ErrNodeLimit = errors.New("the query touches too many nodes; narrow it or page through it")

// Engine executes queries against the customer graph.
type Engine struct {
	schema *graphql.Schema
	svc    *customers.Service
	limits Limits
}

// New builds the engine. The zero Limits selects DefaultLimits.
func New(svc *customers.Service, limits Limits) (*Engine, error) {
	if svc == nil {
		return nil, errors.New("gql: a customers service is required")
	}
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	e := &Engine{svc: svc, limits: limits}
	schema, err := graphql.ParseSchema(Schema, &query{e: e},
		graphql.MaxDepth(limits.MaxDepth),
		graphql.MaxQueryLength(limits.MaxQueryBytes),
		graphql.MaxSelections(maxSelections),
		// The schema is published in the docs. Introspection is off so the
		// endpoint answers only what the limits above were sized for.
		graphql.RestrictIntrospection(func(context.Context) bool { return false }),
	)
	if err != nil {
		return nil, fmt.Errorf("gql: parsing schema: %w", err)
	}
	e.schema = schema
	return e, nil
}

// Limits reports the engine's limits.
func (e *Engine) Limits() Limits { return e.limits }

// Result is the outcome of one query.
type Result struct {
	// Body is the GraphQL response, JSON-encoded: {"data": ..., "errors": ...}.
	Body []byte
	// Nodes is how many people, edges and artefacts the query materialised.
	Nodes int
	// Errors is the number of errors in the response.
	Errors int
}

// request is the per-query state the resolvers share.
type request struct {
	viewer customers.Viewer
	nodes  atomic.Int64
	limit  int64
}

type requestKey struct{}

// take charges n nodes to the query and fails once the budget is spent.
func (r *request) take(n int) error {
	if r.nodes.Add(int64(n)) > r.limit {
		return ErrNodeLimit
	}
	return nil
}

func requestFrom(ctx context.Context) *request {
	r, _ := ctx.Value(requestKey{}).(*request)
	if r == nil {
		// No viewer: nothing is visible and nothing may be materialised.
		return &request{}
	}
	return r
}

// Exec runs one query as the viewer. It never returns an error: a failure
// is a GraphQL error in the response body.
func (e *Engine) Exec(ctx context.Context, viewer customers.Viewer, queryText, operation string, variables map[string]any) Result {
	req := &request{viewer: viewer, limit: int64(e.limits.MaxNodes)}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, requestKey{}, req), e.limits.Timeout)
	defer cancel()
	resp := e.schema.Exec(ctx, queryText, operation, variables)
	body, err := json.Marshal(resp)
	if err != nil {
		body = []byte(`{"errors":[{"message":"the response could not be encoded"}]}`)
		return Result{Body: body, Nodes: int(req.nodes.Load()), Errors: 1}
	}
	return Result{Body: body, Nodes: int(req.nodes.Load()), Errors: len(resp.Errors)}
}
