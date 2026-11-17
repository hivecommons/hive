package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
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
}

// wireCustomers opens the people store and the edge index when
// customers.enabled is on. It returns nil when the feature is off or the
// store cannot be opened; the routes then answer 503 and the section hides.
func wireCustomers(cfg *config.Config, resolvers customers.Resolvers, logger *slog.Logger) (*customersDashboard, func()) {
	if !cfg.CustomersEnabled() {
		return nil, nil
	}
	return wireCustomersAt(customersDataDir, customersGraphDir, resolvers, logger)
}

func wireCustomersAt(dataDir, graphDir string, resolvers customers.Resolvers, logger *slog.Logger) (*customersDashboard, func()) {
	store, err := customers.Open(dataDir, nil)
	if err != nil {
		if logger != nil {
			logger.Error("[customers] people store unavailable; the Customers section is off", "error", err)
		}
		return nil, nil
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
	}
	return &customersDashboard{svc: customers.NewService(store, index, resolvers)}, stop
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
		Tag: req.Tag, State: req.State, GoalID: req.GoalID, Text: req.Text, Level: req.Level,
	}))
}

func (c *customersDashboard) Forget(v dashboard.CustomersViewer, id string) (int, error) {
	n, err := c.svc.Forget(customersViewer(v), id)
	return n, customersError(err)
}

func (c *customersDashboard) Accept(v dashboard.CustomersViewer, suggestionID string) (dashboard.CustomerResult, error) {
	return customerResult(c.svc.Accept(customersViewer(v), suggestionID))
}

func (c *customersDashboard) Dismiss(v dashboard.CustomersViewer, suggestionID string) error {
	return customersError(c.svc.Dismiss(customersViewer(v), suggestionID))
}
