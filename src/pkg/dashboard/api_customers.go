package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Customers: a profile per person (hivecommons/hive#18742). The store and
// its rules live in pkg/customers, which this package does not import; the
// service below is installed by cmd/hive.
//
// Who may do what:
//   - the verified owner and read-write users see every profile and every
//     suggestion, and may add, edit, accept and dismiss;
//   - only the verified owner may forget a person;
//   - anyone else who is signed in sees only their own profile ("me") and
//     may edit its name, identities, tags and goals, through
//     POST /api/customers/me/edit and no other route. They may also query
//     the read-only graph (POST /api/graph), where the same rule holds.
//
// The service enforces the same rules again on the viewer it is handed, so
// a mistake here cannot widen what a read-only user reaches.

// Service errors. The implementation wraps these so the handlers can pick a
// status code.
var (
	ErrCustomerNotFound  = errors.New("customer not found")
	ErrCustomerInvalid   = errors.New("invalid customer request")
	ErrCustomerConflict  = errors.New("customer request conflicts with the current state")
	ErrCustomerForbidden = errors.New("you do not have access to this customer")
	// ErrCustomerUnavailable is a part of the feature that is not running
	// (the graph engine failed to start).
	ErrCustomerUnavailable = errors.New("this part of customers is not available on this hive")
)

const (
	// customersMaxBody bounds a customers request body.
	customersMaxBody = 16 << 10
	// customersSelfEditPath is the one write a read-only user may make: an
	// edit to their own profile. roleEnforcement lets exactly this through.
	customersSelfEditPath = "/api/customers/me/edit"
	// customersMeID is the id alias for the viewer's own profile.
	customersMeID = "me"
	// customersAuditIDMax bounds an id copied into an audit line.
	customersAuditIDMax = 40
)

// CustomersViewer is who is asking. Full is the owner and read-write view;
// Owner adds forget.
type CustomersViewer struct {
	User  string
	Full  bool
	Owner bool
}

// CustomerIdentityInput is one identity on an add request.
type CustomerIdentityInput struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// CustomerAddRequest is the body of POST /api/customers.
type CustomerAddRequest struct {
	Name       string                  `json:"name"`
	Identities []CustomerIdentityInput `json:"identities,omitempty"`
}

// CustomerEditRequest is the body of POST /api/customers/{id}/edit: one
// change, named by Op (name, notes, profile, affiliation_dismiss, identity_add, identity_remove, tag,
// goal_add, goal_edit, goal_confirm, goal_reject, satisfaction_set,
// satisfaction_confirm, satisfaction_reject).
type CustomerEditRequest struct {
	Op     string `json:"op"`
	Name   string `json:"name,omitempty"`
	Notes  string `json:"notes,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Value  string `json:"value,omitempty"`
	Key    string `json:"key,omitempty"`
	Tag    string `json:"tag,omitempty"`
	State  string `json:"state,omitempty"`
	GoalID string `json:"goal_id,omitempty"`
	Text   string `json:"text,omitempty"`
	Level  string `json:"level,omitempty"`
	// Field names the profile field an "profile" op sets (real_name,
	// organization, location, country); Value is its text and State "auto" hands it
	// back to the automatic source.
	Field string `json:"field,omitempty"`
}

// CustomerResult is one profile as the viewer sees it. View is JSON-encoded
// as returned; ID and Me are for the audit line.
type CustomerResult struct {
	View any
	ID   string
	Me   bool
}

// CustomersService is the people store as the dashboard sees it. Every
// method takes the viewer and applies the access rules itself. Errors wrap
// the ErrCustomer* sentinels.
type CustomersService interface {
	// List returns the viewer's own profile and, with the full view, every
	// customer and suggestion.
	List(v CustomersViewer) (any, error)
	// Get returns one profile; id may be "me".
	Get(v CustomersViewer, id string) (CustomerResult, error)
	Add(v CustomersViewer, req CustomerAddRequest) (CustomerResult, error)
	Edit(v CustomersViewer, id string, req CustomerEditRequest) (CustomerResult, error)
	// Forget deletes a person and returns how many edges went with them.
	Forget(v CustomersViewer, id string) (edges int, err error)
	Accept(v CustomersViewer, suggestionID string) (CustomerResult, error)
	// AcceptInto attaches a suggestion to the existing customer into
	// (an id or "me") instead of making a new one; Unmerge undoes it.
	AcceptInto(v CustomersViewer, suggestionID, into string) (CustomerResult, error)
	Dismiss(v CustomersViewer, suggestionID string) error
	// AnswerHint answers a "Same person?" question: same true makes the
	// suggested identity the customer's own, false closes the question for
	// good.
	AnswerHint(v CustomersViewer, hintID string, same bool) (CustomerResult, error)
	// Merge folds the customer "from" into id; Unmerge undoes it.
	Merge(v CustomersViewer, id, from string) (CustomerResult, error)
	Unmerge(v CustomersViewer, id, from string) (CustomerResult, error)
	// Split moves the named identity keys (kind:value) off id onto a new
	// customer, which it returns.
	Split(v CustomersViewer, id string, keys []string, name string) (CustomerResult, error)
	// Export returns everything held about one customer (owner only),
	// JSON-encoded as returned.
	Export(v CustomersViewer, id string) (any, error)
}

// CustomersProfileRefresher is the optional "Refresh from profiles" part of
// the service (#19302): the profile fill run on demand. A service without
// it, or with the fill switched off, answers 503.
type CustomersProfileRefresher interface {
	// RefreshProfile looks one customer (an id or "me") up now and returns
	// the profile after the lookup and the pass counts.
	RefreshProfile(ctx context.Context, v CustomersViewer, id string) (CustomerResult, any, error)
	// RefreshAllProfiles makes every customer due and starts a pass in the
	// background.
	RefreshAllProfiles(v CustomersViewer) (any, error)
}

// CustomerMergeRequest is the body of POST /api/customers/{id}/merge and
// /unmerge: From is the customer folded in (or restored).
type CustomerMergeRequest struct {
	From string `json:"from"`
}

// CustomerAcceptRequest is the optional body of
// POST /api/customers/suggestions/{sid}/accept: Into is an existing customer
// (an id, or "me") to attach the suggestion to instead of making a new one.
type CustomerAcceptRequest struct {
	Into string `json:"into,omitempty"`
}

// CustomerSplitRequest is the body of POST /api/customers/{id}/split.
type CustomerSplitRequest struct {
	Identities []string `json:"identities"`
	Name       string   `json:"name,omitempty"`
}

// CustomerHintAnswer is the body of POST /api/customers/hints/{hid}/answer.
type CustomerHintAnswer struct {
	Same bool `json:"same"`
}

type customersHolder struct{ svc CustomersService }

// SetCustomersService installs the people store (nil removes it).
func (s *Server) SetCustomersService(svc CustomersService) {
	s.customers.Store(&customersHolder{svc: svc})
}

func (s *Server) customersService(w http.ResponseWriter) (CustomersService, bool) {
	h := s.customers.Load()
	if h == nil || h.svc == nil {
		jsonError(w, "customers are turned off on this hive (Settings, Customers)", http.StatusServiceUnavailable)
		return nil, false
	}
	return h.svc, true
}

func (s *Server) registerCustomersRoutes() {
	s.mux.HandleFunc("GET /api/customers", s.handleCustomersList)
	s.mux.HandleFunc("POST /api/customers", s.handleCustomersAdd)
	s.mux.HandleFunc("GET /api/customers/{id}", s.handleCustomersGet)
	s.mux.HandleFunc("POST /api/customers/{id}/edit", s.handleCustomersEdit)
	s.mux.HandleFunc("DELETE /api/customers/{id}", s.handleCustomersForget)
	s.mux.HandleFunc("POST /api/customers/suggestions/{sid}/accept", s.handleCustomersAccept)
	s.mux.HandleFunc("POST /api/customers/suggestions/{sid}/dismiss", s.handleCustomersDismiss)
	s.mux.HandleFunc("POST /api/customers/hints/{hid}/answer", s.handleCustomersHintAnswer)
	s.mux.HandleFunc("POST /api/customers/{id}/merge", s.handleCustomersMerge)
	s.mux.HandleFunc("POST /api/customers/{id}/unmerge", s.handleCustomersUnmerge)
	s.mux.HandleFunc("POST /api/customers/{id}/split", s.handleCustomersSplit)
	s.mux.HandleFunc("GET /api/customers/{id}/export", s.handleCustomersExport)
	s.mux.HandleFunc("POST /api/customers/{id}/refresh-profile", s.handleCustomersRefreshProfile)
	s.mux.HandleFunc("POST /api/customers/refresh-profiles", s.handleCustomersRefreshProfiles)
	s.registerCustomersGraphRoutes()
	s.registerCustomersEnrichmentRoutes()
}

// readOnlySelfService reports whether a write from a read-only user is one
// of the few a read-only user may make to their own data. It is consulted
// by roleEnforcement, which refuses every other write from the read role.
func readOnlySelfService(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	// The graph endpoint is a POST that only reads: its schema has no
	// mutations.
	return r.URL.Path == customersSelfEditPath || r.URL.Path == customersGraphPath
}

// customersViewer works out who is asking. The owner role counts only when
// authenticate verified it; a read-only viewer must be a named user, since
// their profile is keyed on the name.
func customersViewer(w http.ResponseWriter, r *http.Request) (CustomersViewer, bool) {
	role := r.Header.Get("X-Hive-Role")
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if isOwnerRole(role) {
		if r.Header.Get(ownerRoleVerifiedHeader) != "true" {
			jsonError(w, "sign in to see your customer profile", http.StatusForbidden)
			return CustomersViewer{}, false
		}
		return CustomersViewer{User: requestUser(r), Full: true, Owner: true}, true
	}
	if config.RoleAtLeast(role, config.RoleReadWrite) {
		return CustomersViewer{User: requestUser(r), Full: true}, true
	}
	if user == "" {
		jsonError(w, "sign in to see your customer profile", http.StatusForbidden)
		return CustomersViewer{}, false
	}
	return CustomersViewer{User: user}, true
}

// customersFullViewer admits only the full view (verified owner or
// read-write).
func customersFullViewer(w http.ResponseWriter, r *http.Request) (CustomersViewer, bool) {
	v, ok := customersViewer(w, r)
	if !ok {
		return v, false
	}
	if !v.Full {
		jsonError(w, "owner or read-write access required", http.StatusForbidden)
		return v, false
	}
	return v, true
}

func customersErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrCustomerNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrCustomerInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrCustomerConflict):
		return http.StatusConflict
	case errors.Is(err, ErrCustomerForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrCustomerUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// customersError answers a failed call. A store failure is logged; its text
// is not sent to the browser.
func (s *Server) customersError(w http.ResponseWriter, r *http.Request, err error) {
	status := customersErrorStatus(err)
	if status >= http.StatusInternalServerError {
		if s.logger != nil {
			s.logger.Log(r.Context(), slog.LevelWarn, "[customers] call failed",
				"method", r.Method, "path", r.URL.Path, "error", err.Error())
		}
		jsonError(w, "the customers store could not complete the request", status)
		return
	}
	jsonError(w, err.Error(), status)
}

func decodeCustomersBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, customersMaxBody)).Decode(into); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// handleCustomersList answers the Customers section. With the feature off
// it says so with a 200, so the page can hide the section without an error.
func (s *Server) handleCustomersList(w http.ResponseWriter, r *http.Request) {
	h := s.customers.Load()
	if h == nil || h.svc == nil {
		jsonResponse(w, map[string]any{"enabled": false})
		return
	}
	v, ok := customersViewer(w, r)
	if !ok {
		return
	}
	out, err := h.svc.List(v)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	jsonResponse(w, s.withCustomersAvatars(out))
}

// withCustomersAvatars adds the live avatars switch to the list answer, so
// the page knows whether it may ask github.com for profile pictures. It is
// read per request, so turning it off in Settings takes effect on the next
// refresh, and the answer is unchanged if the list cannot be re-encoded.
func (s *Server) withCustomersAvatars(out any) any {
	raw, err := json.Marshal(out)
	if err != nil {
		return out
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return out
	}
	on := true
	if s.deps != nil && s.deps.Config != nil {
		on = s.deps.Config.CustomersAvatarsEnabled()
	}
	m["avatars"] = on
	return m
}

func (s *Server) handleCustomersGet(w http.ResponseWriter, r *http.Request) {
	v, ok := customersViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	res, err := svc.Get(v, r.PathValue("id"))
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	if !res.Me {
		s.auditFromRequest(r, "customer_profile_open", auditDetail("person", res.ID), "")
	}
	jsonResponse(w, res.View)
}

func (s *Server) handleCustomersAdd(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	var req CustomerAddRequest
	if !decodeCustomersBody(w, r, &req) {
		return
	}
	res, err := svc.Add(v, req)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_add", auditDetail("person", res.ID, "identities", strconv.Itoa(len(req.Identities))), "")
	jsonResponse(w, res.View)
}

// handleCustomersEdit applies one change to a profile. A read-only user
// reaches it only as /api/customers/me/edit; the service then limits them
// to their own name, identities, tags and goals.
func (s *Server) handleCustomersEdit(w http.ResponseWriter, r *http.Request) {
	v, ok := customersViewer(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !v.Full && id != customersMeID {
		// Never reached through roleEnforcement, which lets a read-only
		// user post to the "me" path only. Kept so the rule does not
		// depend on that middleware alone.
		jsonError(w, ErrCustomerNotFound.Error(), http.StatusNotFound)
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	var req CustomerEditRequest
	if !decodeCustomersBody(w, r, &req) {
		return
	}
	res, err := svc.Edit(v, id, req)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	// The audit line names the person and the kind of change, never the
	// value: the log outlives a forget.
	s.auditFromRequest(r, "customer_edit", auditDetail("person", res.ID, "op", req.Op, "self", boolString(res.Me)), "")
	jsonResponse(w, res.View)
}

func (s *Server) handleCustomersForget(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	v, ok := customersViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	edges, err := svc.Forget(v, id)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_forget", auditDetail("person", id, "edges", strconv.Itoa(edges)), "")
	jsonResponse(w, map[string]any{"forgotten": true, "edges_removed": edges})
}

func (s *Server) handleCustomersAccept(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	var body CustomerAcceptRequest
	if r.ContentLength != 0 && !decodeCustomersBody(w, r, &body) {
		return
	}
	if into := strings.TrimSpace(body.Into); into != "" {
		res, err := svc.AcceptInto(v, r.PathValue("sid"), into)
		if err != nil {
			s.customersError(w, r, err)
			return
		}
		s.auditFromRequest(r, "customer_attach", auditDetail("person", res.ID), "")
		jsonResponse(w, res.View)
		return
	}
	res, err := svc.Accept(v, r.PathValue("sid"))
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_accept", auditDetail("person", res.ID), "")
	jsonResponse(w, res.View)
}

func (s *Server) handleCustomersDismiss(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	sid := r.PathValue("sid")
	if err := svc.Dismiss(v, sid); err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_dismiss", auditDetail("suggestion", sid), "")
	jsonResponse(w, map[string]any{"dismissed": true})
}

func (s *Server) handleCustomersHintAnswer(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	var body CustomerHintAnswer
	if !decodeCustomersBody(w, r, &body) {
		return
	}
	res, err := svc.AnswerHint(v, r.PathValue("hid"), body.Same)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_hint_answer", auditDetail("person", res.ID, "same", boolString(body.Same)), "")
	jsonResponse(w, res.View)
}

func (s *Server) handleCustomersMerge(w http.ResponseWriter, r *http.Request) {
	s.customersMergeOrUnmerge(w, r, "customer_merge", func(svc CustomersService, v CustomersViewer, id, from string) (CustomerResult, error) {
		return svc.Merge(v, id, from)
	})
}

func (s *Server) handleCustomersUnmerge(w http.ResponseWriter, r *http.Request) {
	s.customersMergeOrUnmerge(w, r, "customer_unmerge", func(svc CustomersService, v CustomersViewer, id, from string) (CustomerResult, error) {
		return svc.Unmerge(v, id, from)
	})
}

func (s *Server) customersMergeOrUnmerge(w http.ResponseWriter, r *http.Request, action string, do func(CustomersService, CustomersViewer, string, string) (CustomerResult, error)) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	var body CustomerMergeRequest
	if !decodeCustomersBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.From) == "" {
		jsonError(w, "from is required", http.StatusBadRequest)
		return
	}
	res, err := do(svc, v, r.PathValue("id"), body.From)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, action, auditDetail("person", res.ID, "from", auditPersonID(body.From)), "")
	jsonResponse(w, res.View)
}

// auditPersonID keeps only what an id may hold, so a hand-made request
// cannot put free text in the audit log.
func auditPersonID(id string) string {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id) && len(out) < customersAuditIDMax; i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}

func (s *Server) handleCustomersSplit(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	var body CustomerSplitRequest
	if !decodeCustomersBody(w, r, &body) {
		return
	}
	if len(body.Identities) == 0 {
		jsonError(w, "name at least one identity to split off", http.StatusBadRequest)
		return
	}
	res, err := svc.Split(v, r.PathValue("id"), body.Identities, body.Name)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_split", auditDetail("from", auditPersonID(r.PathValue("id")), "person", res.ID, "identities", strconv.Itoa(len(body.Identities))), "")
	jsonResponse(w, res.View)
}

// handleCustomersExport gives the owner everything held about one person as
// a JSON download. It is audited, since it moves personal data off the page.
func (s *Server) handleCustomersExport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	v, ok := customersViewer(w, r)
	if !ok {
		return
	}
	svc, ok := s.customersService(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	out, err := svc.Export(v, id)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_export", auditDetail("person", auditPersonID(id)), "")
	w.Header().Set("Content-Disposition", `attachment; filename="customer-export.json"`)
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, out)
}

// customersRefresher returns the profile refresher, or answers 503.
func (s *Server) customersRefresher(w http.ResponseWriter) (CustomersProfileRefresher, bool) {
	svc, ok := s.customersService(w)
	if !ok {
		return nil, false
	}
	r, ok := svc.(CustomersProfileRefresher)
	if !ok {
		jsonError(w, ErrCustomerUnavailable.Error(), http.StatusServiceUnavailable)
		return nil, false
	}
	return r, true
}

// handleCustomersRefreshProfile is "Refresh from profiles" on one customer:
// the profile fill looks them up now (full view only).
func (s *Server) handleCustomersRefreshProfile(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	ref, ok := s.customersRefresher(w)
	if !ok {
		return
	}
	res, fill, err := ref.RefreshProfile(r.Context(), v, r.PathValue("id"))
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_profile_refresh", auditDetail("person", res.ID), "")
	jsonResponse(w, map[string]any{"customer": res.View, "fill": fill})
}

// handleCustomersRefreshProfiles is "Refresh from profiles" for everyone:
// every customer becomes due and a bounded pass starts (full view only).
func (s *Server) handleCustomersRefreshProfiles(w http.ResponseWriter, r *http.Request) {
	v, ok := customersFullViewer(w, r)
	if !ok {
		return
	}
	ref, ok := s.customersRefresher(w)
	if !ok {
		return
	}
	out, err := ref.RefreshAllProfiles(v)
	if err != nil {
		s.customersError(w, r, err)
		return
	}
	s.auditFromRequest(r, "customer_profile_refresh_all", "", "")
	jsonResponse(w, out)
}
