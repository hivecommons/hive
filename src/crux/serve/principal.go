package serve

import (
	"context"
	"net/http"
	"strings"
)

// Caller identity on serve routes (hivecommons/crux#114 S0). Every request
// that passes authenticate carries a Principal in its context: the shared
// control token (or the unauthenticated unix socket of a token-less
// server) is the operator; a lane project credential (lanes.Authenticator)
// is a project principal, accepted only on the routes a work-service
// family registered through workService. Agent RPCs keep refusing project
// credentials exactly as before — the work services are the only surface
// where a non-operator caller exists, because they own records per caller
// (S1 checkouts) and gate answers by caller (S3).

const (
	// KindOperator is the principal behind the shared control token. It may
	// act on every record regardless of owner.
	KindOperator = "operator"
	// KindProject is a lane project principal (lanes.Config Principals); its
	// Name is the project id.
	KindProject = "project"
	// DefaultOperatorName is Principal.Name for the operator when the
	// server was given none.
	DefaultOperatorName = "operator"
)

// Principal is who made an authenticated request.
type Principal struct {
	// Kind is KindOperator or KindProject.
	Kind string `json:"kind"`
	// Name is the operator name or the lane project id.
	Name string `json:"name"`
}

// IsOperator reports whether p holds the shared control credential.
func (p Principal) IsOperator() bool { return p.Kind == KindOperator }

// String renders kind:name for logs and error messages.
func (p Principal) String() string { return p.Kind + ":" + p.Name }

// principalKey carries the Principal through the request context.
type principalKey struct{}

// CallerFrom returns the Principal authenticate attached to ctx. ok is
// false on a context that did not pass through the server's auth gate.
func CallerFrom(ctx context.Context) (p Principal, ok bool) {
	p, ok = ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// withCaller returns r with p as its caller.
func withCaller(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// operator is the principal for the shared control credential.
func (s *Server) operator() Principal {
	name := s.OperatorName
	if name == "" {
		name = DefaultOperatorName
	}
	return Principal{Kind: KindOperator, Name: name}
}

// workService marks every route under prefix (the path itself and its
// subpaths) as a work-services route: project principals are accepted there
// and nowhere else. Handler calls it for each family before the mux is
// wrapped in authenticate.
func (s *Server) workService(prefix string) {
	s.workServices = append(s.workServices, prefix)
}

// isWorkService reports whether path belongs to a work-services family.
func (s *Server) isWorkService(path string) bool {
	for _, prefix := range s.workServices {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
