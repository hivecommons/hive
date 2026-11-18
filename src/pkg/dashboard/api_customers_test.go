package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeCustomersService records the viewer each call was made with, so the
// tests can check what the handlers hand the service.
type fakeCustomersService struct {
	err   error
	calls []string
}

func (f *fakeCustomersService) note(method string, v CustomersViewer, arg string) {
	f.calls = append(f.calls, fmt.Sprintf("%s user=%s full=%v owner=%v arg=%s", method, v.User, v.Full, v.Owner, arg))
}

func (f *fakeCustomersService) List(v CustomersViewer) (any, error) {
	f.note("List", v, "")
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"enabled": true, "full": v.Full}, nil
}

func (f *fakeCustomersService) result(id string, v CustomersViewer) (CustomerResult, error) {
	if f.err != nil {
		return CustomerResult{}, f.err
	}
	me := id == customersMeID
	if me {
		id = "p_" + v.User
	}
	return CustomerResult{View: map[string]any{"id": id, "me": me}, ID: id, Me: me}, nil
}

func (f *fakeCustomersService) Get(v CustomersViewer, id string) (CustomerResult, error) {
	f.note("Get", v, id)
	return f.result(id, v)
}

func (f *fakeCustomersService) Add(v CustomersViewer, req CustomerAddRequest) (CustomerResult, error) {
	f.note("Add", v, req.Name)
	return f.result("p_new", v)
}

func (f *fakeCustomersService) Edit(v CustomersViewer, id string, req CustomerEditRequest) (CustomerResult, error) {
	f.note("Edit", v, id+":"+req.Op)
	return f.result(id, v)
}

func (f *fakeCustomersService) Forget(v CustomersViewer, id string) (int, error) {
	f.note("Forget", v, id)
	return 3, f.err
}

func (f *fakeCustomersService) Accept(v CustomersViewer, sid string) (CustomerResult, error) {
	f.note("Accept", v, sid)
	return f.result("p_accepted", v)
}

func (f *fakeCustomersService) AcceptInto(v CustomersViewer, sid, into string) (CustomerResult, error) {
	f.note("AcceptInto", v, sid+"->"+into)
	return f.result(into, v)
}

func (f *fakeCustomersService) AnswerHint(v CustomersViewer, hid string, same bool) (CustomerResult, error) {
	f.note("AnswerHint", v, fmt.Sprintf("%s:%v", hid, same))
	return f.result("p_hinted", v)
}

func (f *fakeCustomersService) Merge(v CustomersViewer, id, from string) (CustomerResult, error) {
	f.note("Merge", v, id+"<-"+from)
	return f.result(id, v)
}

func (f *fakeCustomersService) Unmerge(v CustomersViewer, id, from string) (CustomerResult, error) {
	f.note("Unmerge", v, id+"<-"+from)
	return f.result(id, v)
}

func (f *fakeCustomersService) Split(v CustomersViewer, id string, keys []string, name string) (CustomerResult, error) {
	f.note("Split", v, id+":"+strings.Join(keys, ",")+":"+name)
	return f.result("p_split", v)
}

func (f *fakeCustomersService) Export(v CustomersViewer, id string) (any, error) {
	f.note("Export", v, id)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"person": map[string]any{"id": id}, "edges": []any{}}, nil
}

func (f *fakeCustomersService) Dismiss(v CustomersViewer, sid string) error {
	f.note("Dismiss", v, sid)
	return f.err
}

// customersWho is one kind of caller.
type customersWho struct {
	name     string
	user     string
	role     string
	verified bool
}

var (
	custOwner      = customersWho{"owner", "olivia", "owner", true}
	custReadWrite  = customersWho{"read-write", "rita", "read-write", false}
	custMerger     = customersWho{"merger", "max", "merger", false}
	custRead       = customersWho{"read", "reed", "read", false}
	custFakeOwner  = customersWho{"unverified owner", "mallory", "owner", false}
	custNamelessRO = customersWho{"read with no name", "", "read", false}
)

// customersDo sends a request through roleEnforcement and the mux, the two
// layers that decide what a role may reach.
func customersDo(s *Server, who customersWho, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if who.user != "" {
		req.Header.Set("X-Hive-User", who.user)
	}
	if who.role != "" {
		req.Header.Set("X-Hive-Role", who.role)
	}
	if who.verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.roleEnforcement(s.mux).ServeHTTP(rec, req)
	return rec
}

type customersRoute struct {
	method, path, body string
}

var (
	custList      = customersRoute{http.MethodGet, "/api/customers", ""}
	custAdd       = customersRoute{http.MethodPost, "/api/customers", `{"name":"Ada"}`}
	custGetMe     = customersRoute{http.MethodGet, "/api/customers/me", ""}
	custGetOther  = customersRoute{http.MethodGet, "/api/customers/p_other", ""}
	custEditMe    = customersRoute{http.MethodPost, "/api/customers/me/edit", `{"op":"name","name":"N"}`}
	custEditOther = customersRoute{http.MethodPost, "/api/customers/p_other/edit", `{"op":"name","name":"N"}`}
	custForget    = customersRoute{http.MethodDelete, "/api/customers/p_other", ""}
	custAccept    = customersRoute{http.MethodPost, "/api/customers/suggestions/s_1/accept", ""}
	custAttach    = customersRoute{http.MethodPost, "/api/customers/suggestions/s_1/accept", `{"into":"p_other"}`}
	custDismiss   = customersRoute{http.MethodPost, "/api/customers/suggestions/s_1/dismiss", ""}
	custMerge     = customersRoute{http.MethodPost, "/api/customers/p_other/merge", `{"from":"p_src"}`}
	custUnmerge   = customersRoute{http.MethodPost, "/api/customers/p_other/unmerge", `{"from":"p_src"}`}
	custSplit     = customersRoute{http.MethodPost, "/api/customers/p_other/split", `{"identities":["email:a@b.c"],"name":"Split"}`}
	custExport    = customersRoute{http.MethodGet, "/api/customers/p_other/export", ""}
	custHint      = customersRoute{http.MethodPost, "/api/customers/hints/h_1/answer", `{"same":true}`}
)

// TestCustomersRoleMatrix walks every route as every kind of caller. The
// read role reaches the list, its own profile and an edit of its own
// profile, and nothing else.
func TestCustomersRoleMatrix(t *testing.T) {
	const ok, forbidden = http.StatusOK, http.StatusForbidden
	cases := []struct {
		route customersRoute
		want  map[string]int
	}{
		{custList, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": ok, "unverified owner": forbidden, "read with no name": forbidden}},
		{custGetMe, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": ok, "unverified owner": forbidden, "read with no name": forbidden}},
		{custEditMe, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": ok, "unverified owner": forbidden, "read with no name": forbidden}},
		// The fake answers any id, so a 200 here for the read role would
		// mean the handler asked on its behalf. The real service answers
		// 404; see pkg/customers and cmd/hive.
		{custGetOther, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": ok, "unverified owner": forbidden, "read with no name": forbidden}},
		{custEditOther, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custAdd, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custAccept, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		// Attaching a suggestion to an existing customer is the accept route
		// with a body: the same roles, and a read-only user, who cannot see
		// suggestions, is refused before the service is asked.
		{custAttach, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custDismiss, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custHint, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custMerge, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custUnmerge, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custSplit, map[string]int{"owner": ok, "read-write": ok, "merger": ok, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custExport, map[string]int{"owner": ok, "read-write": forbidden, "merger": forbidden, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
		{custForget, map[string]int{"owner": ok, "read-write": forbidden, "merger": forbidden, "read": forbidden, "unverified owner": forbidden, "read with no name": forbidden}},
	}
	s, _ := apiServer(t)
	for _, tc := range cases {
		for _, who := range []customersWho{custOwner, custReadWrite, custMerger, custRead, custFakeOwner, custNamelessRO} {
			t.Run(tc.route.method+" "+tc.route.path+" as "+who.name, func(t *testing.T) {
				fake := &fakeCustomersService{}
				s.SetCustomersService(fake)
				rec := customersDo(s, who, tc.route.method, tc.route.path, tc.route.body)
				if rec.Code != tc.want[who.name] {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want[who.name], rec.Body.String())
				}
				if rec.Code != ok && len(fake.calls) != 0 {
					t.Fatalf("a refused request reached the service: %v", fake.calls)
				}
				for _, call := range fake.calls {
					wantFull := who.name == "owner" || who.name == "read-write" || who.name == "merger"
					if strings.Contains(call, "full=true") != wantFull || strings.Contains(call, "owner=true") != (who.name == "owner") {
						t.Fatalf("service saw the wrong viewer: %s", call)
					}
					if !strings.Contains(call, "user="+who.user+" ") {
						t.Fatalf("service saw the wrong user: %s", call)
					}
				}
			})
		}
	}
}

// TestCustomersReadOnlyWriteExemptionIsExact: the read role's one write is
// POST /api/customers/me/edit. Nothing near it gets through.
func TestCustomersReadOnlyWriteExemptionIsExact(t *testing.T) {
	s, _ := apiServer(t)
	fake := &fakeCustomersService{}
	s.SetCustomersService(fake)
	refused := []customersRoute{
		{http.MethodPost, "/api/customers/me/edit/", `{"op":"name"}`},
		{http.MethodPost, "/api/customers/me/edit/x", `{"op":"name"}`},
		{http.MethodPut, "/api/customers/me/edit", `{"op":"name"}`},
		{http.MethodDelete, "/api/customers/me/edit", ""},
		{http.MethodDelete, "/api/customers/me", ""},
		{http.MethodPost, "/api/customers/me", `{"name":"x"}`},
		{http.MethodPost, "/api/customers/p_reed/edit", `{"op":"name"}`},
		{http.MethodPost, "/api/customers/ME/edit", `{"op":"name"}`},
		{http.MethodPost, "/api/customers/me/editor", `{"op":"name"}`},
		{http.MethodPost, "/api/customers/suggestions/me/accept", ""},
		{http.MethodPost, "/api/customers/suggestions/me/dismiss", ""},
	}
	for _, r := range refused {
		rec := customersDo(s, custRead, r.method, r.path, r.body)
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s as read = 200, want a refusal", r.method, r.path)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a refused read-only write reached the service: %v", fake.calls)
	}
	if rec := customersDo(s, custRead, http.MethodPost, customersSelfEditPath, `{"op":"goal_add","text":"ship it"}`); rec.Code != http.StatusOK {
		t.Fatalf("the self edit itself = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 1 || !strings.Contains(fake.calls[0], "Edit user=reed full=false owner=false arg=me:goal_add") {
		t.Fatalf("calls = %v", fake.calls)
	}
	// The exemption does not open other read-only writes.
	if rec := customersDo(s, custRead, http.MethodPost, "/api/meetings/schedule", `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("another write as read = %d", rec.Code)
	}
	// The handler holds the rule by itself too, without the middleware.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/customers/p_other/edit", strings.NewReader(`{"op":"name","name":"x"}`))
	req.Header.Set("X-Hive-User", "reed")
	req.Header.Set("X-Hive-Role", "read")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || len(fake.calls) != 1 {
		t.Fatalf("edit of another profile straight at the handler = %d, calls %v", rec.Code, fake.calls)
	}
}

func TestCustomersOffAndErrors(t *testing.T) {
	s, _ := apiServer(t)
	// Off: the list says so with a 200 so the page can hide the section;
	// everything else is 503.
	rec := customersDo(s, custRead, http.MethodGet, "/api/customers", "")
	var off map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &off) != nil || off["enabled"] != false {
		t.Fatalf("list while off = %d %s", rec.Code, rec.Body.String())
	}
	for _, r := range []customersRoute{custGetMe, custEditMe, custAdd, custForget, custAccept, custDismiss, custHint, custMerge, custUnmerge, custSplit, custExport} {
		if rec := customersDo(s, custOwner, r.method, r.path, r.body); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s while off = %d, want 503", r.method, r.path, rec.Code)
		}
	}
	s.SetCustomersService(nil)
	if rec := customersDo(s, custOwner, http.MethodGet, "/api/customers/me", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil service = %d", rec.Code)
	}

	fake := &fakeCustomersService{}
	s.SetCustomersService(fake)
	statuses := map[error]int{
		fmt.Errorf("%w: p_x", ErrCustomerNotFound):    http.StatusNotFound,
		fmt.Errorf("%w: bad tag", ErrCustomerInvalid): http.StatusBadRequest,
		fmt.Errorf("%w: taken", ErrCustomerConflict):  http.StatusConflict,
		fmt.Errorf("%w", ErrCustomerForbidden):        http.StatusForbidden,
		fmt.Errorf("disk full at /data/customers"):    http.StatusInternalServerError,
	}
	for err, want := range statuses {
		fake.err = err
		for _, r := range []customersRoute{custList, custGetOther, custEditOther, custAdd, custForget, custAccept, custDismiss} {
			rec := customersDo(s, custOwner, r.method, r.path, r.body)
			if rec.Code != want {
				t.Errorf("%s %s with %v = %d, want %d", r.method, r.path, err, rec.Code, want)
			}
			if want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "/data/customers") {
				t.Errorf("a store failure leaked its text: %s", rec.Body.String())
			}
		}
	}
	fake.err = nil
	for _, r := range []customersRoute{custAdd, custEditOther} {
		if rec := customersDo(s, custOwner, r.method, r.path, `{not json`); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s with a bad body = %d", r.method, r.path, rec.Code)
		}
		big := `{"name":"` + strings.Repeat("x", customersMaxBody) + `"}`
		if rec := customersDo(s, custOwner, r.method, r.path, big); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s with an oversize body = %d", r.method, r.path, rec.Code)
		}
	}
}

func TestCustomersAudit(t *testing.T) {
	s, _ := apiServer(t)
	s.SetCustomersService(&fakeCustomersService{})
	steps := []struct {
		who   customersWho
		route customersRoute
	}{
		{custOwner, custGetMe},    // your own profile is not a "profile open"
		{custOwner, custGetOther}, // someone else's is
		{custOwner, custAdd},
		{custRead, custEditMe},
		{custReadWrite, custEditOther},
		{custReadWrite, custAccept},
		{custReadWrite, custAttach},
		{custReadWrite, custDismiss},
		{custReadWrite, custHint},
		{custReadWrite, custMerge},
		{custReadWrite, custUnmerge},
		{custReadWrite, custSplit},
		{custOwner, custExport},
		{custOwner, custForget},
	}
	for _, st := range steps {
		if rec := customersDo(s, st.who, st.route.method, st.route.path, st.route.body); rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", st.route.method, st.route.path, rec.Code)
		}
	}
	got := map[string]string{}
	count := 0
	for _, e := range s.audit.Recent(50) {
		if strings.HasPrefix(e.Action, "customer_") {
			got[e.Action] += e.User + "|" + e.Detail + ";"
			count++
		}
	}
	if count != 13 {
		t.Fatalf("audited %d customer actions, want 13: %v", count, got)
	}
	want := map[string]string{
		"customer_profile_open": "olivia|person=p_other;",
		"customer_add":          "olivia|person=p_new, identities=0;",
		"customer_forget":       "olivia|person=p_other, edges=3;",
		"customer_accept":       "rita|person=p_accepted;",
		// Ids only: never the suggested person's login or address.
		"customer_attach":      "rita|person=p_other;",
		"customer_dismiss":     "rita|suggestion=s_1;",
		"customer_hint_answer": "rita|person=p_hinted, same=true;",
		"customer_merge":       "rita|person=p_other, from=p_src;",
		"customer_unmerge":     "rita|person=p_other, from=p_src;",
		"customer_split":       "rita|from=p_other, person=p_split, identities=1;",
		"customer_export":      "olivia|person=p_other;",
	}
	for action, detail := range want {
		if got[action] != detail {
			t.Errorf("audit %s = %q, want %q", action, got[action], detail)
		}
	}
	edits := got["customer_edit"]
	if !strings.Contains(edits, "reed|person=p_reed, op=name, self=true;") || !strings.Contains(edits, "rita|person=p_other, op=name, self=false;") {
		t.Errorf("audit customer_edit = %q", edits)
	}
	// The audit line names the person and the kind of change, never the
	// value that was written.
	if strings.Contains(edits, "N;") || strings.Contains(got["customer_add"], "Ada") {
		t.Errorf("audit carries a written value: %v", got)
	}
}

func TestCustomersMergeSplitExportBodiesAndAudit(t *testing.T) {
	s, _ := apiServer(t)
	fake := &fakeCustomersService{}
	s.SetCustomersService(fake)
	for _, r := range []customersRoute{
		{http.MethodPost, "/api/customers/p_a/merge", `{}`},
		{http.MethodPost, "/api/customers/p_a/merge", `{not json`},
		{http.MethodPost, "/api/customers/p_a/unmerge", `{"from":"  "}`},
		{http.MethodPost, "/api/customers/p_a/split", `{"identities":[]}`},
		{http.MethodPost, "/api/customers/p_a/split", `{}`},
	} {
		if rec := customersDo(s, custOwner, r.method, r.path, r.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s %s = %d, want 400", r.method, r.path, r.body, rec.Code)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a malformed request reached the service: %v", fake.calls)
	}
	// An id copied into the audit line is cleaned and bounded.
	rec := customersDo(s, custOwner, http.MethodPost, "/api/customers/p_a/merge", `{"from":"p_x\n<script> `+strings.Repeat("a", 100)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge = %d: %s", rec.Code, rec.Body.String())
	}
	for _, e := range s.audit.Recent(10) {
		if e.Action == "customer_merge" && (strings.ContainsAny(e.Detail, "<>\n") || len(e.Detail) > 120) {
			t.Fatalf("audit detail not cleaned: %q", e.Detail)
		}
	}
	rec = customersDo(s, custOwner, http.MethodGet, "/api/customers/p_other/export", "")
	if rec.Header().Get("Content-Disposition") == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export headers = %v", rec.Header())
	}
	fake.err = ErrCustomerConflict
	if rec := customersDo(s, custOwner, http.MethodPost, "/api/customers/p_a/merge", `{"from":"p_b"}`); rec.Code != http.StatusConflict {
		t.Fatalf("a conflicting merge = %d", rec.Code)
	}
}

// The list answer carries the live avatars switch, so the page asks github.com
// for pictures only while the hive allows it.
func TestCustomersListCarriesAvatarsSwitch(t *testing.T) {
	t.Setenv(config.EnvCustomersAvatarsEnabled, "")
	s, _ := apiServer(t)
	s.SetCustomersService(&fakeCustomersService{})
	if s.deps.Config == nil {
		s.deps.Config = &config.Config{}
	}
	get := func() map[string]any {
		rec := customersDo(s, custOwner, http.MethodGet, "/api/customers", "")
		var m map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		return m
	}
	if got := get(); got["avatars"] != true || got["enabled"] != true {
		t.Fatalf("default list = %v, want avatars on", got)
	}
	off := false
	s.deps.Config.Customers.Avatars.Enabled = &off
	if got := get(); got["avatars"] != false {
		t.Fatalf("avatars off = %v", got)
	}
	t.Setenv(config.EnvCustomersAvatarsEnabled, "true")
	if got := get(); got["avatars"] != true {
		t.Fatalf("env on must win = %v", got)
	}
}
