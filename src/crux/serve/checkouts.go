package serve

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/checkout"
)

// Pinned checkouts (hivecommons/crux#114 S1) are the first work-services
// family: the routes accept project principals (authenticate admits them
// because registerCheckouts marks the prefix with workService), and
// ownership is enforced from the caller principal — a project may only
// create checkouts it owns, list its own, and delete its own; the operator
// may do anything.

// registerCheckouts adds the checkout routes and marks them a work service.
func (s *Server) registerCheckouts(mux *http.ServeMux) {
	mux.HandleFunc("POST "+api.PathCheckouts, s.handleCheckoutCreate)
	mux.HandleFunc("GET "+api.PathCheckouts, s.handleCheckoutList)
	mux.HandleFunc("GET "+api.PathCheckouts+"/{id}", s.handleCheckoutGet)
	mux.HandleFunc("DELETE "+api.PathCheckouts+"/{id}", s.handleCheckoutDelete)
	s.workService(api.PathCheckouts)
}

// checkoutService answers 501 when the server has no checkout service.
func (s *Server) checkoutService(w http.ResponseWriter) (*checkout.Service, bool) {
	if s.Checkouts == nil {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "this server has no checkout service; start it with --checkout-allow")
		return nil, false
	}
	return s.Checkouts, true
}

func (s *Server) handleCheckoutCreate(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.checkoutService(w)
	if !ok {
		return
	}
	var in api.CheckoutRequest
	if !s.readJSON(w, r, &in) {
		return
	}
	caller, _ := CallerFrom(r.Context())
	owner := in.Owner
	switch {
	case owner == "":
		owner = caller.Name
	case !caller.IsOperator() && owner != caller.Name:
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, fmt.Sprintf("principal %s may only create checkouts owned by %q", caller, caller.Name))
		return
	}
	req := checkout.Request{Repo: in.Repo, Commit: in.Commit, Owner: owner, Branches: in.Branches, MaxBytes: in.MaxBytes}
	if in.TTL != "" {
		ttl, err := time.ParseDuration(in.TTL)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "ttl: "+err.Error())
			return
		}
		req.TTL = ttl
	}
	rec, err := svc.Create(r.Context(), req)
	if err != nil {
		s.writeCheckoutError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, checkoutToAPI(rec))
}

func (s *Server) handleCheckoutList(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.checkoutService(w)
	if !ok {
		return
	}
	caller, _ := CallerFrom(r.Context())
	owner := r.URL.Query().Get("owner")
	if !caller.IsOperator() {
		// A project sees only its own checkouts, whatever it asked for.
		owner = caller.Name
	}
	recs, err := svc.List(owner)
	if err != nil {
		s.writeCheckoutError(w, err)
		return
	}
	out := api.CheckoutList{Checkouts: make([]api.Checkout, 0, len(recs))}
	for _, rec := range recs {
		out.Checkouts = append(out.Checkouts, checkoutToAPI(rec))
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCheckoutGet(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.checkoutService(w)
	if !ok {
		return
	}
	rec, ok := s.ownedCheckout(w, r, svc)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, checkoutToAPI(rec))
}

func (s *Server) handleCheckoutDelete(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.checkoutService(w)
	if !ok {
		return
	}
	rec, ok := s.ownedCheckout(w, r, svc)
	if !ok {
		return
	}
	caller, _ := CallerFrom(r.Context())
	if err := svc.Delete(r.Context(), rec.ID, caller.String()); err != nil {
		s.writeCheckoutError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownedCheckout loads {id} and enforces that the caller owns it or is the
// operator; on failure the response has been written.
func (s *Server) ownedCheckout(w http.ResponseWriter, r *http.Request, svc *checkout.Service) (checkout.Record, bool) {
	rec, err := svc.Get(r.PathValue("id"))
	if err != nil {
		s.writeCheckoutError(w, err)
		return checkout.Record{}, false
	}
	caller, _ := CallerFrom(r.Context())
	if !checkout.CanDelete(rec, caller.Name, caller.IsOperator()) {
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, fmt.Sprintf("checkout %s is owned by %q, not by principal %s", rec.ID, rec.Owner, caller))
		return checkout.Record{}, false
	}
	return rec, true
}

// writeCheckoutError maps the package's sentinels to typed codes.
func (s *Server) writeCheckoutError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, api.CodeRuntime
	switch {
	case errors.Is(err, checkout.ErrBadRequest):
		status, code = http.StatusBadRequest, api.CodeBadRequest
	case errors.Is(err, checkout.ErrForbidden):
		status, code = http.StatusForbidden, api.CodeForbidden
	case errors.Is(err, checkout.ErrNotFound):
		status, code = http.StatusNotFound, api.CodeNotFound
	case errors.Is(err, checkout.ErrInUse):
		status, code = http.StatusConflict, api.CodeConflict
	case errors.Is(err, checkout.ErrMismatch):
		status, code = http.StatusBadGateway, api.CodeUpstream
	case errors.Is(err, checkout.ErrTooLarge):
		status, code = http.StatusRequestEntityTooLarge, api.CodeTooLarge
	case errors.Is(err, checkout.ErrUnsupported):
		status, code = http.StatusNotImplemented, api.CodeUnsupported
	}
	s.writeError(w, status, code, err.Error())
}

func checkoutToAPI(rec checkout.Record) api.Checkout {
	return api.Checkout{ID: rec.ID, Path: rec.Path, Repo: rec.Repo, Commit: rec.Commit, Owner: rec.Owner, Branches: rec.Branches,
		MaxBytes: rec.MaxBytes, Bytes: rec.Bytes, TTL: rec.TTL, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt}
}
