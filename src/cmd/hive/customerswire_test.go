package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
)

func wireTestCustomers(t *testing.T) *customersDashboard {
	t.Helper()
	dir := t.TempDir()
	svc, stop := wireCustomersAt(filepath.Join(dir, "customers"), filepath.Join(dir, "graph"), nil)
	if svc == nil {
		t.Fatal("wireCustomersAt returned no service")
	}
	t.Cleanup(stop)
	return svc
}

func TestWireCustomersHonoursTheSwitch(t *testing.T) {
	t.Setenv(config.EnvCustomersEnabled, "false")
	if svc, stop := wireCustomers(&config.Config{}, nil); svc != nil || stop != nil {
		t.Fatal("customers.enabled off must open no store")
	}
	// A store that cannot be opened turns the feature off instead of
	// failing the boot.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if svc, _ := wireCustomersAt(filepath.Join(blocked, "customers"), filepath.Join(blocked, "graph"), nil); svc != nil {
		t.Fatal("an unusable data dir must yield no service")
	}
}

func TestCustomersErrorMapsToDashboardSentinels(t *testing.T) {
	cases := map[error]error{
		customers.ErrNotFound:  dashboard.ErrCustomerNotFound,
		customers.ErrInvalid:   dashboard.ErrCustomerInvalid,
		customers.ErrConflict:  dashboard.ErrCustomerConflict,
		customers.ErrForbidden: dashboard.ErrCustomerForbidden,
	}
	for in, want := range cases {
		if got := customersError(fmt.Errorf("%w: detail", in)); !errors.Is(got, want) {
			t.Errorf("customersError(%v) = %v, want %v", in, got, want)
		}
	}
	if customersError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	plain := errors.New("disk full")
	if got := customersError(plain); got != plain {
		t.Fatalf("an unknown error must pass through, got %v", got)
	}
}

// TestCustomersAdapterRoleMatrix drives the real store through the adapter
// the dashboard calls, as each kind of viewer.
func TestCustomersAdapterRoleMatrix(t *testing.T) {
	c := wireTestCustomers(t)
	owner := dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}
	rw := dashboard.CustomersViewer{User: "rita", Full: true}
	read := dashboard.CustomersViewer{User: "reed"}

	ada, err := c.Add(rw, dashboard.CustomerAddRequest{Name: "Ada", Identities: []dashboard.CustomerIdentityInput{{Kind: "github", Value: "@Ada"}}})
	if err != nil || ada.ID == "" || ada.Me {
		t.Fatalf("Add = %+v, %v", ada, err)
	}

	// Every role has a "me".
	for _, v := range []dashboard.CustomersViewer{owner, rw, read} {
		me, err := c.Get(v, "me")
		if err != nil || !me.Me || me.ID == ada.ID {
			t.Fatalf("%s me = %+v, %v", v.User, me, err)
		}
	}

	// The read role: only itself, by any method.
	if _, err := c.Get(read, ada.ID); !errors.Is(err, dashboard.ErrCustomerNotFound) {
		t.Fatalf("read Get other = %v, want not found", err)
	}
	if _, err := c.Edit(read, ada.ID, dashboard.CustomerEditRequest{Op: "name", Name: "Hacked"}); !errors.Is(err, dashboard.ErrCustomerNotFound) {
		t.Fatalf("read Edit other = %v, want not found", err)
	}
	if _, err := c.Add(read, dashboard.CustomerAddRequest{Name: "Sneaky"}); !errors.Is(err, dashboard.ErrCustomerForbidden) {
		t.Fatalf("read Add = %v", err)
	}
	if _, err := c.Forget(read, ada.ID); !errors.Is(err, dashboard.ErrCustomerForbidden) {
		t.Fatalf("read Forget = %v", err)
	}
	if _, err := c.Accept(read, "s_x"); !errors.Is(err, dashboard.ErrCustomerForbidden) {
		t.Fatalf("read Accept = %v", err)
	}
	if err := c.Dismiss(read, "s_x"); !errors.Is(err, dashboard.ErrCustomerForbidden) {
		t.Fatalf("read Dismiss = %v", err)
	}
	if _, err := c.Edit(read, "me", dashboard.CustomerEditRequest{Op: "notes", Notes: "x"}); !errors.Is(err, dashboard.ErrCustomerForbidden) {
		t.Fatalf("read self notes = %v", err)
	}
	mine, err := c.Edit(read, "me", dashboard.CustomerEditRequest{Op: "goal_add", Text: "Get exports working"})
	if err != nil || !mine.Me {
		t.Fatalf("read self goal = %+v, %v", mine, err)
	}

	// The list is JSON the page reads: enabled, the viewer's own profile
	// and, for the read role, nobody else.
	for _, tc := range []struct {
		v      dashboard.CustomersViewer
		people int
		full   bool
	}{{read, 0, false}, {rw, 3, true}, {owner, 3, true}} {
		raw, err := c.List(tc.v)
		if err != nil {
			t.Fatalf("%s List: %v", tc.v.User, err)
		}
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Enabled bool `json:"enabled"`
			Full    bool `json:"full"`
			Me      struct {
				ID string `json:"id"`
				Me bool   `json:"me"`
			} `json:"me"`
			People []struct {
				ID string `json:"id"`
			} `json:"people"`
			Suggestions []any    `json:"suggestions"`
			DefaultTags []string `json:"default_tags"`
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("list is not the expected JSON: %v\n%s", err, data)
		}
		if !got.Enabled || got.Full != tc.full || !got.Me.Me || got.Me.ID == "" || len(got.People) != tc.people || len(got.DefaultTags) != 4 {
			t.Fatalf("%s list = %s", tc.v.User, data)
		}
	}

	// Read-write edits anyone; only the owner forgets.
	if res, err := c.Edit(rw, ada.ID, dashboard.CustomerEditRequest{Op: "tag", Tag: "stakeholder", State: "on"}); err != nil || res.Me {
		t.Fatalf("rw Edit = %+v, %v", res, err)
	}
	if _, err := c.Edit(rw, ada.ID, dashboard.CustomerEditRequest{Op: "tag", Tag: "bad!", State: "on"}); !errors.Is(err, dashboard.ErrCustomerInvalid) {
		t.Fatalf("invalid edit = %v", err)
	}
	if _, err := c.Add(rw, dashboard.CustomerAddRequest{Name: "Ada 2", Identities: []dashboard.CustomerIdentityInput{{Kind: "github", Value: "ada"}}}); !errors.Is(err, dashboard.ErrCustomerConflict) {
		t.Fatalf("conflicting add = %v", err)
	}
	if _, err := c.Forget(rw, ada.ID); !errors.Is(err, dashboard.ErrCustomerForbidden) {
		t.Fatalf("rw Forget = %v", err)
	}
	if n, err := c.Forget(owner, ada.ID); err != nil || n != 0 {
		t.Fatalf("owner Forget = %d, %v", n, err)
	}
	if _, err := c.Get(owner, ada.ID); !errors.Is(err, dashboard.ErrCustomerNotFound) {
		t.Fatalf("Get after forget = %v", err)
	}
	if err := c.Dismiss(owner, "s_missing"); !errors.Is(err, dashboard.ErrCustomerNotFound) {
		t.Fatalf("Dismiss missing = %v", err)
	}
	if _, err := c.Accept(owner, "s_missing"); !errors.Is(err, dashboard.ErrCustomerNotFound) {
		t.Fatalf("Accept missing = %v", err)
	}
}
