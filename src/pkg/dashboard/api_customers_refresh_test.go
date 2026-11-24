package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeCustomersRefresher is the fake service plus "Refresh from profiles".
type fakeCustomersRefresher struct {
	fakeCustomersService
}

func (f *fakeCustomersRefresher) RefreshProfile(_ context.Context, v CustomersViewer, id string) (CustomerResult, any, error) {
	f.note("RefreshProfile", v, id)
	res, err := f.result(id, v)
	return res, map[string]any{"filled": 1}, err
}

func (f *fakeCustomersRefresher) RefreshAllProfiles(v CustomersViewer) (any, error) {
	f.note("RefreshAllProfiles", v, "")
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"queued": true}, nil
}

var (
	custRefreshOne = customersRoute{http.MethodPost, "/api/customers/p_other/refresh-profile", `{}`}
	custRefreshAll = customersRoute{http.MethodPost, "/api/customers/refresh-profiles", `{}`}
)

// "Refresh from profiles" (#19302) is for the full view only: a read-only
// user is refused before the service is asked, even on their own profile.
func TestCustomersRefreshProfileRoles(t *testing.T) {
	const ok, forbidden = http.StatusOK, http.StatusForbidden
	want := map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}
	s, _ := apiServer(t)
	for _, route := range []customersRoute{custRefreshOne, custRefreshAll, {http.MethodPost, "/api/customers/me/refresh-profile", `{}`}} {
		for _, who := range []customersWho{custOwner, custReadWrite, custMerger, custRead, custFakeOwner, custNamelessRO} {
			t.Run(route.path+" as "+who.name, func(t *testing.T) {
				fake := &fakeCustomersRefresher{}
				s.SetCustomersService(fake)
				rec := customersDo(s, who, route.method, route.path, route.body)
				if rec.Code != want[who.name] {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, want[who.name], rec.Body.String())
				}
				if rec.Code != ok && len(fake.calls) != 0 {
					t.Fatalf("a refused request reached the service: %v", fake.calls)
				}
			})
		}
	}
}

func TestCustomersRefreshProfileAnswersAndAudits(t *testing.T) {
	s, _ := apiServer(t)
	fake := &fakeCustomersRefresher{}
	s.SetCustomersService(fake)
	rec := customersDo(s, custOwner, custRefreshOne.method, custRefreshOne.path, custRefreshOne.body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"customer"`) || !strings.Contains(rec.Body.String(), `"fill"`) {
		t.Fatalf("refresh one = %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 1 || !strings.HasPrefix(fake.calls[0], "RefreshProfile") || !strings.HasSuffix(fake.calls[0], "arg=p_other") {
		t.Fatalf("calls = %v", fake.calls)
	}
	rec = customersDo(s, custReadWrite, custRefreshAll.method, custRefreshAll.path, custRefreshAll.body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"queued":true`) {
		t.Fatalf("refresh all = %d %s", rec.Code, rec.Body.String())
	}
	// A service that is off answers 503 with its reason.
	fake.err = fmt.Errorf("%w: the profile fill is off", ErrCustomerUnavailable)
	rec = customersDo(s, custOwner, custRefreshAll.method, custRefreshAll.path, custRefreshAll.body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("off = %d %s", rec.Code, rec.Body.String())
	}
}

// A service without the refresher answers 503, never 500.
func TestCustomersRefreshProfileWithoutRefresher(t *testing.T) {
	s, _ := apiServer(t)
	s.SetCustomersService(&fakeCustomersService{})
	for _, route := range []customersRoute{custRefreshOne, custRefreshAll} {
		if rec := customersDo(s, custOwner, route.method, route.path, route.body); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d %s", route.path, rec.Code, rec.Body.String())
		}
	}
}
