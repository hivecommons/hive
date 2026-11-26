package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/customers/gql"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings/betty"
	"github.com/hivecommons/hive/pkg/scheduler"
)

const (
	// customersDataDir holds people.json, the truth for the Customers
	// section. It never leaves the hive's volume.
	customersDataDir = "/data/customers"
	// customersGraphDir holds customers.db, the rebuildable edge index,
	// beside the knowledge graph store.
	customersGraphDir = "/data/graph"
)

// customersDashboard implements dashboard.CustomersService over the people
// store (hivecommons/hive#18742). pkg/dashboard does not import
// pkg/customers; this adapter is the only place the two meet.
type customersDashboard struct {
	svc *customers.Service
	// graph is the read-only GraphQL engine; nil if it failed to start.
	graph *gql.Engine
	// mentions is the accept-time GitHub search (step E, #18884); nil when
	// it is not wired. Only Accept and AcceptInto reach it.
	mentions *customers.MentionSearch
	// hubImport is the accept-time hub pull (#19163); nil when not wired.
	// It asks only about the person just accepted and is off unless
	// customers.hub_import.enabled and a hub registration.
	hubImport func(ctx context.Context, person string)
	// profileFill fills profile fields from public forge profiles
	// (#19302); nil when not wired. Off unless
	// customers.profile_fill.enabled.
	profileFill *customers.ProfileFill
	logger      *slog.Logger
	// spawn runs the search; nil means a goroutine. Tests run it inline.
	spawn func(func())
}

// wireCustomers opens the people store and the edge index when
// customers.enabled is on. It returns nil when the feature is off or the
// store cannot be opened; the routes then answer 503 and the section hides.
func wireCustomers(cfg *config.Config, resolvers customers.Resolvers, logger *slog.Logger) (*customersDashboard, func()) {
	if !cfg.CustomersEnabled() {
		return nil, nil
	}
	mode := customers.ParseStoreMode(cfg.CustomersStoreMode())
	return wireCustomersAtMode(customersDataDir, customersGraphDir, mode, resolvers, logger)
}

func wireCustomersAt(dataDir, graphDir string, resolvers customers.Resolvers, logger *slog.Logger) (*customersDashboard, func()) {
	return wireCustomersAtMode(dataDir, graphDir, customers.StoreJSON, resolvers, logger)
}

// wireCustomersAtMode is wireCustomersAt with a customers.store mode
// (#19855). shadow (the spoke default, #19972) and colony open
// /data/customers/colony.db beside people.json; a colony store that cannot be
// opened or imported leaves the section on json, people.json untouched.
func wireCustomersAtMode(dataDir, graphDir string, mode customers.StoreMode, resolvers customers.Resolvers, logger *slog.Logger) (*customersDashboard, func()) {
	store, err := customers.Open(dataDir, nil)
	if err != nil {
		if logger != nil {
			logger.Error("[customers] people store unavailable; the Customers section is off", "error", err)
		}
		return nil, nil
	}
	colony, err := customers.AttachColony(store, mode, logger)
	if err != nil {
		colony = nil
		if logger != nil {
			logger.Error("[customers] colony store unavailable; Customers stays on people.json (customers.store json)",
				"mode", string(mode), "error", err)
		}
	}
	if colony != nil {
		logCustomersColonyParity(store, logger, true)
	}
	// The index holds the edges. Without it the store still works, so a
	// failure here is logged and not fatal.
	index, err := customers.OpenIndex(graphDir)
	if err != nil {
		index = nil
		if logger != nil {
			logger.Warn("[customers] edge index unavailable; profiles show no activity", "error", err)
		}
	}
	if logger != nil {
		logger.Info("[customers] people store opened", "file", store.Path(), "index", index != nil)
	}
	stop := func() {
		if index != nil {
			_ = index.Close()
		}
		_ = colony.Close()
	}
	svc := customers.NewService(store, index, resolvers)
	graph, err := gql.New(svc, gql.Limits{})
	if err != nil {
		graph = nil
		if logger != nil {
			logger.Error("[customers] graph endpoint unavailable", "error", err)
		}
	}
	return &customersDashboard{svc: svc, graph: graph, logger: logger}, stop
}

// logCustomersColonyParity logs the colony parity check: counts only, never
// a value. A clean check after a sweep is logged at debug level.
func logCustomersColonyParity(store *customers.Store, logger *slog.Logger, boot bool) {
	if logger == nil || store == nil || store.Colony() == nil {
		return
	}
	p, err := store.ColonyParity()
	if err != nil {
		logger.Warn("[customers] colony parity check failed", "error", err)
		return
	}
	args := []any{"mode", string(p.Mode), "json_people", p.JSONPeople, "colony_people", p.ColonyPeople,
		"json_identities", p.JSONIdentities, "colony_identities", p.ColonyIdent, "facts_user", p.FactsUser,
		"facts_hub_shared", p.FactsHubShared, "facts_enrichment", p.FactsEnrichment, "mismatches", p.Mismatches,
		"write_failures", p.Failures}
	if boot || p.Mismatches > 0 || p.Failures > 0 {
		logger.Info("[customers] colony parity", args...)
		return
	}
	logger.Debug("[customers] colony parity", args...)
}

// customersError maps a store error onto the dashboard's sentinels, which
// pick the HTTP status.
func customersError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, customers.ErrNotFound):
		return fmt.Errorf("%w: %v", dashboard.ErrCustomerNotFound, err)
	case errors.Is(err, customers.ErrInvalid):
		return fmt.Errorf("%w: %v", dashboard.ErrCustomerInvalid, err)
	case errors.Is(err, customers.ErrConflict):
		return fmt.Errorf("%w: %v", dashboard.ErrCustomerConflict, err)
	case errors.Is(err, customers.ErrForbidden):
		return fmt.Errorf("%w: %v", dashboard.ErrCustomerForbidden, err)
	default:
		return err
	}
}

func customersViewer(v dashboard.CustomersViewer) customers.Viewer {
	return customers.Viewer{User: v.User, Full: v.Full, Owner: v.Owner}
}

func customerResult(view customers.PersonView, err error) (dashboard.CustomerResult, error) {
	if err != nil {
		return dashboard.CustomerResult{}, customersError(err)
	}
	return dashboard.CustomerResult{View: view, ID: view.ID, Me: view.Me}, nil
}

// customersList is GET /api/customers.
type customersList struct {
	Enabled bool `json:"enabled"`
	customers.ListView
}

func (c *customersDashboard) List(v dashboard.CustomersViewer) (any, error) {
	out, err := c.svc.List(customersViewer(v))
	if err != nil {
		return nil, customersError(err)
	}
	return customersList{Enabled: true, ListView: out}, nil
}

func (c *customersDashboard) Get(v dashboard.CustomersViewer, id string) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.Get(customersViewer(v), id))
}

func (c *customersDashboard) Add(v dashboard.CustomersViewer, req dashboard.CustomerAddRequest) (dashboard.CustomerResult, error) {
	ids := make([]customers.Identity, 0, len(req.Identities))
	for _, i := range req.Identities {
		ids = append(ids, customers.Identity{Kind: customers.IdentityKind(i.Kind), Value: i.Value})
	}
	return customerResult(c.svc.Add(customersViewer(v), req.Name, ids))
}

func (c *customersDashboard) Edit(v dashboard.CustomersViewer, id string, req dashboard.CustomerEditRequest) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.Edit(customersViewer(v), id, customers.Edit{
		Op: req.Op, Name: req.Name, Notes: req.Notes, Kind: req.Kind, Value: req.Value, Key: req.Key,
		Tag: req.Tag, State: req.State, GoalID: req.GoalID, Text: req.Text, Level: req.Level, Field: req.Field,
	}))
}

func (c *customersDashboard) Forget(v dashboard.CustomersViewer, id string) (int, error) {
	n, err := c.svc.Forget(customersViewer(v), id)
	return n, customersError(err)
}

func (c *customersDashboard) Accept(v dashboard.CustomersViewer, suggestionID string) (dashboard.CustomerResult, error) {
	ident, found := c.suggestedIdentity(suggestionID)
	res, err := customerResult(c.svc.Accept(customersViewer(v), suggestionID))
	if err == nil {
		c.searchOnAccept(res.ID, ident, found)
		c.hubImportOnAccept(res.ID)
		c.profileFillOnAccept(res.ID)
	}
	return res, err
}

func (c *customersDashboard) Dismiss(v dashboard.CustomersViewer, suggestionID string) error {
	return customersError(c.svc.Dismiss(customersViewer(v), suggestionID))
}

// Graph is POST /api/graph: one read-only GraphQL query as the viewer.
func (c *customersDashboard) Graph(ctx context.Context, v dashboard.CustomersViewer, req dashboard.CustomerGraphRequest) (dashboard.CustomerGraphResult, error) {
	if c.graph == nil {
		return dashboard.CustomerGraphResult{}, dashboard.ErrCustomerUnavailable
	}
	res := c.graph.Exec(ctx, customersViewer(v), req.Query, req.OperationName, req.Variables)
	return dashboard.CustomerGraphResult{Body: res.Body, Nodes: res.Nodes, Errors: res.Errors}, nil
}

func (c *customersDashboard) AnswerHint(v dashboard.CustomersViewer, hintID string, same bool) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.AnswerHint(customersViewer(v), hintID, same))
}

// meetingsPeople holds the people directory the meeting persona reads. The
// persona is built before customers are wired, so it asks at call time.
var meetingsPeople atomic.Pointer[customersPeopleDirectory]

func meetingsPeopleDirectory() betty.PeopleDirectory {
	if d := meetingsPeople.Load(); d != nil {
		return d
	}
	return nil
}

// customersPeopleDirectory answers the persona's "who is this?" from the
// customers service. The persona speaks in a room, so the directory hands
// over only what customers.PeopleBriefs allows: a name, tags and confirmed
// goals.
type customersPeopleDirectory struct{ svc *customers.Service }

// Find implements betty.PeopleDirectory.
func (d *customersPeopleDirectory) Find(terms []string, limit int) []betty.PersonBrief {
	var out []betty.PersonBrief
	for _, b := range d.svc.PeopleBriefs(terms, limit) {
		out = append(out, betty.PersonBrief{Name: b.Name, Tags: b.Tags, Goals: b.Goals})
	}
	return out
}

// installCustomersAgentContext hands the customers service to the two
// places agents and the persona read it: the kick prompt and the persona's
// retrievers. Neither can query the store; they receive a bounded slice.
func installCustomersAgentContext(c *customersDashboard, setKickLookup func(scheduler.CustomerContextLookup)) {
	if c == nil {
		meetingsPeople.Store(nil)
		if setKickLookup != nil {
			setKickLookup(nil)
		}
		return
	}
	meetingsPeople.Store(&customersPeopleDirectory{svc: c.svc})
	if setKickLookup != nil {
		setKickLookup(c.svc.KickContext)
	}
}

func (c *customersDashboard) AcceptInto(v dashboard.CustomersViewer, suggestionID, into string) (dashboard.CustomerResult, error) {
	ident, found := c.suggestedIdentity(suggestionID)
	res, err := customerResult(c.svc.AcceptInto(customersViewer(v), suggestionID, into))
	if err == nil {
		c.searchOnAccept(res.ID, ident, found)
		c.hubImportOnAccept(res.ID)
		c.profileFillOnAccept(res.ID)
	}
	return res, err
}

func (c *customersDashboard) Merge(v dashboard.CustomersViewer, id, from string) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.Merge(customersViewer(v), id, from))
}

func (c *customersDashboard) Unmerge(v dashboard.CustomersViewer, id, from string) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.Unmerge(customersViewer(v), id, from))
}

func (c *customersDashboard) Split(v dashboard.CustomersViewer, id string, keys []string, name string) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.Split(customersViewer(v), id, keys, name))
}

func (c *customersDashboard) Export(v dashboard.CustomersViewer, id string) (any, error) {
	out, err := c.svc.Export(customersViewer(v), id)
	if err != nil {
		return nil, customersError(err)
	}
	return out, nil
}
