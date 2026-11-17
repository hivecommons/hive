package gql

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/customers"
)

var (
	ownerV = customers.Viewer{User: "olivia", Full: true, Owner: true}
	rwV    = customers.Viewer{User: "rita", Full: true}
	readV  = customers.Viewer{User: "reed"}
)

// ownersOnly lets only the owner see the artefacts in it.
type ownersOnly map[string]bool

func (o ownersOnly) Exists(string) bool { return true }

func (o ownersOnly) Visible(ref string, v customers.Viewer) bool { return !o[ref] || v.Owner }

type fixture struct {
	svc     *customers.Service
	engine  *Engine
	reed    customers.PersonView
	ada     customers.PersonView
	private string
	shared  string
}

func newFixture(t *testing.T, limits Limits) fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := customers.Open(filepath.Join(dir, "customers"), nil)
	if err != nil {
		t.Fatal(err)
	}
	index, err := customers.OpenIndex(filepath.Join(dir, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	f := fixture{private: customers.MeetingRef("board"), shared: customers.IssueRef("o/r", 1)}
	f.svc = customers.NewService(store, index, customers.Resolvers{customers.RefMeeting: ownersOnly{f.private: true}})
	if f.reed, err = f.svc.Get(readV, customers.MeID); err != nil {
		t.Fatal(err)
	}
	if f.ada, err = f.svc.Add(ownerV, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}, {Kind: customers.KindEmail, Value: "ada@example.com"}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []customers.Edit{
		{Op: customers.OpTag, Tag: customers.TagStakeholder, State: customers.TagOn},
		{Op: customers.OpGoalAdd, Text: "Get exports working"},
		{Op: customers.OpSatisfactionSet, Level: string(customers.LevelAtRisk)},
		{Op: customers.OpNotes, Notes: "private note about Ada"},
	} {
		if _, err := f.svc.Edit(ownerV, f.ada.ID, e); err != nil {
			t.Fatalf("edit %s: %v", e.Op, err)
		}
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := index.Put([]customers.Edge{
		{Person: f.reed.ID, Predicate: customers.PredReported, Artefact: f.shared, Via: "hive_user:reed", Source: "github_poll", At: at},
		{Person: f.reed.ID, Predicate: customers.PredAttended, Artefact: f.private, Via: "hive_user:reed", Source: "meetings", At: at.Add(time.Hour)},
		{Person: f.ada.ID, Predicate: customers.PredAssigned, Artefact: f.shared, Via: "github:ada", Source: "github_poll", At: at.Add(2 * time.Hour)},
		{Person: f.ada.ID, Predicate: customers.PredAttended, Artefact: f.private, Via: "github:ada", Source: "meetings", At: at.Add(3 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	if f.engine, err = New(f.svc, limits); err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

type response struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func exec(t *testing.T, f fixture, v customers.Viewer, query string, vars map[string]any) (response, Result) {
	t.Helper()
	res := f.engine.Exec(context.Background(), v, query, "", vars)
	var out response
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, res.Body)
	}
	return out, res
}

func mustOK(t *testing.T, f fixture, v customers.Viewer, query string, vars map[string]any) string {
	t.Helper()
	out, res := exec(t, f, v, query, vars)
	if len(out.Errors) != 0 {
		t.Fatalf("query failed: %+v\n%s", out.Errors, query)
	}
	return string(res.Body)
}

func TestSchemaParsesAndIsReadOnly(t *testing.T) {
	f := newFixture(t, Limits{})
	if f.engine.Limits() != DefaultLimits {
		t.Fatalf("zero limits must select the defaults: %+v", f.engine.Limits())
	}
	if strings.Contains(Schema, "type Mutation") || strings.Contains(Schema, "mutation:") || strings.Contains(Schema, "subscription:") {
		t.Fatal("the customer graph must have no mutations and no subscriptions")
	}
	out, _ := exec(t, f, ownerV, `mutation { forget(id: "x") }`, nil)
	if len(out.Errors) == 0 {
		t.Fatal("a mutation was accepted")
	}
	out, _ = exec(t, f, ownerV, `{ __schema { types { name } } }`, nil)
	if len(out.Errors) == 0 && strings.Contains(string(out.Data["__schema"]), "Person") {
		t.Fatal("introspection is on")
	}
	if _, err := New(nil, Limits{}); err == nil {
		t.Fatal("New without a service must fail")
	}
}

func TestMeForEveryRole(t *testing.T) {
	f := newFixture(t, Limits{})
	for _, v := range []customers.Viewer{ownerV, rwV, readV} {
		body := mustOK(t, f, v, `{ me { id name origin me identities { kind value claimed } tags { name on source } goals { id text state source evidence } counts { predicate count } } }`, nil)
		if !strings.Contains(body, `"me":true`) || !strings.Contains(body, `"value":"`+v.User+`"`) || !strings.Contains(body, `"origin":"hive_user"`) {
			t.Fatalf("%s: me = %s", v.User, body)
		}
	}
	out, _ := exec(t, f, customers.Viewer{}, `{ me { id } }`, nil)
	if len(out.Errors) == 0 {
		t.Fatal("a viewer with no name got a profile")
	}
}

func TestFullViewReadsAPersonAndWalksTwoHops(t *testing.T) {
	f := newFixture(t, Limits{})
	vars := map[string]any{"id": f.ada.ID}
	body := mustOK(t, f, ownerV, `query($id: ID!) { person(id: $id) { name me notes satisfaction { level source evidence suggestedLevel suggestedEvidence }
		tags { name on source rule } identities { kind value source manual claimed } goals { text state source }
		counts { predicate count } edges(first: 10) { nodes { predicate at source artefact { ref kind } } next } } }`, vars)
	for _, want := range []string{
		`"name":"Ada"`, `"me":false`, `"notes":"private note about Ada"`, `"level":"at_risk"`,
		`"name":"stakeholder","on":true,"source":"manual"`, `"value":"ada@example.com"`, `"text":"Get exports working","state":"confirmed","source":"manual"`,
		`"predicate":"assigned","count":1`, `"ref":"gh:issue:o/r#1","kind":"gh"`, `"ref":"meeting:board","kind":"meeting"`, `"at":"2026-11-01T03:00:00Z"`, `"next":null`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner person query missing %s in %s", want, body)
		}
	}
	// Two hops: Ada -> the issue -> everyone on it.
	body = mustOK(t, f, ownerV, `query($id: ID!) { person(id: $id) { edges(predicate: "assigned") { nodes { artefact { ref people { predicate person { name me } } } } } } }`, vars)
	if !strings.Contains(body, `"predicate":"reported","person":{"name":"reed","me":false}`) || !strings.Contains(body, `"predicate":"assigned","person":{"name":"Ada","me":false}`) {
		t.Fatalf("two-hop walk = %s", body)
	}
	// Read-write sees Ada but not the artefact only the owner may open.
	body = mustOK(t, f, rwV, `query($id: ID!) { person(id: $id) { name notes edges { nodes { artefact { ref } } } } }`, vars)
	if !strings.Contains(body, `"name":"Ada"`) || strings.Contains(body, "meeting:board") {
		t.Fatalf("read-write person query = %s", body)
	}
	if got := mustOK(t, f, rwV, `{ artefact(ref: "meeting:board") { ref } }`, nil); !strings.Contains(got, `"artefact":null`) {
		t.Fatalf("an artefact the viewer may not see = %s", got)
	}
	// people: every customer, filtered by tag, paged.
	body = mustOK(t, f, ownerV, `{ people(first: 1) { nodes { name } next } }`, nil)
	if !strings.Contains(body, `"next":"1"`) {
		t.Fatalf("first page = %s", body)
	}
	body = mustOK(t, f, ownerV, `{ people(tag: "Stakeholder") { nodes { name } next } }`, nil)
	if !strings.Contains(body, `"nodes":[{"name":"Ada"}]`) || !strings.Contains(body, `"next":null`) {
		t.Fatalf("tag filter = %s", body)
	}
	if out, _ := exec(t, f, ownerV, `{ people(after: "x") { nodes { name } } }`, nil); len(out.Errors) == 0 {
		t.Fatal("a garbage cursor was accepted")
	}
	if got := mustOK(t, f, ownerV, `{ person(id: "p_doesnotexist") { name } }`, nil); !strings.Contains(got, `"person":null`) {
		t.Fatalf("missing person = %s", got)
	}
}

// TestReadOnlyCannotReachAnotherPersonByAnyPath walks every field that can
// return a person as a read-only user. The only person that ever comes back
// is the viewer, and nothing about Ada appears anywhere in any response.
func TestReadOnlyCannotReachAnotherPersonByAnyPath(t *testing.T) {
	f := newFixture(t, Limits{})
	vars := map[string]any{"id": f.ada.ID}
	person := `id name origin me notes satisfaction { level } tags { name } identities { kind value } goals { text } counts { predicate count }`
	short := `id name origin me notes`
	queries := []string{
		// Directly, by id.
		`query($id: ID!) { person(id: $id) { ` + person + ` edges { nodes { artefact { ref people { person { ` + short + ` } } } } } } }`,
		// Through the list, with and without a filter and a cursor.
		`{ people { nodes { ` + person + ` } next } }`,
		`{ people(tag: "stakeholder") { nodes { ` + person + ` } next } }`,
		`{ people(first: 100, after: "0") { nodes { ` + person + ` } next } }`,
		// Through an artefact they share with Ada.
		`{ artefact(ref: "gh:issue:o/r#1") { ref people { predicate person { ` + person + ` edges { nodes { artefact { ref } } } } } } }`,
		// Through their own edges, two hops out.
		`{ me { ` + person + ` edges { nodes { predicate artefact { ref people { predicate person { ` + short + ` } } } } } } }`,
		// Aliases and fragments do not change who is asking.
		`query($id: ID!) { a: person(id: $id) { ...P } b: me { ...P } } fragment P on Person { ` + person + ` }`,
	}
	for _, q := range queries {
		out, res := exec(t, f, readV, q, vars)
		if len(out.Errors) != 0 {
			t.Fatalf("query errored (it should answer with nulls and the viewer only): %+v\n%s", out.Errors, q)
		}
		body := string(res.Body)
		for _, leak := range []string{f.ada.ID, "Ada", "ada@example.com", `"ada"`, "private note", "at_risk", "Get exports working", "stakeholder", "meeting:board", "olivia", "rita"} {
			if strings.Contains(body, leak) {
				t.Fatalf("read-only response leaks %q:\n%s\n%s", leak, q, body)
			}
		}
	}
	// What a read-only user does get: themselves, and their own visible edge.
	body := mustOK(t, f, readV, `{ me { name me notes satisfaction { level } edges { nodes { predicate artefact { ref people { person { name me } } } } } } }`, nil)
	for _, want := range []string{`"name":"reed"`, `"me":true`, `"notes":null`, `"satisfaction":null`, `"ref":"gh:issue:o/r#1"`, `"people":[{"person":{"name":"reed","me":true}}]`} {
		if !strings.Contains(body, want) {
			t.Errorf("read-only me query missing %s in %s", want, body)
		}
	}
	if got := mustOK(t, f, readV, `query($id: ID!) { person(id: $id) { name } }`, vars); !strings.Contains(got, `"person":null`) {
		t.Fatalf("read-only person(ada) = %s", got)
	}
	// Their own id works as well as "me".
	if got := mustOK(t, f, readV, `query($id: ID!) { person(id: $id) { name me } }`, map[string]any{"id": f.reed.ID}); !strings.Contains(got, `"me":true`) {
		t.Fatalf("read-only person(self) = %s", got)
	}
}

func TestSuggestedPeopleAreNeverReturned(t *testing.T) {
	f := newFixture(t, Limits{})
	if _, err := f.svc.Store().Observe([]customers.Observation{{Kind: customers.KindGitHub, Value: "stranger", Predicate: customers.PredReported, Artefact: f.shared}}); err != nil {
		t.Fatal(err)
	}
	sid := f.svc.Store().Suggestions()[0].ID
	for _, q := range []string{
		`{ people(first: 100) { nodes { id name identities { value } } } }`,
		`{ artefact(ref: "gh:issue:o/r#1") { people { person { id name identities { value } } } } }`,
		`{ person(id: "` + sid + `") { id name } }`,
	} {
		body := mustOK(t, f, ownerV, q, nil)
		if strings.Contains(body, "stranger") || strings.Contains(body, sid+`","name"`) {
			t.Fatalf("a suggested person was returned:\n%s\n%s", q, body)
		}
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t, Limits{MaxDepth: 4, MaxQueryBytes: 200, MaxPage: 2, MaxNodes: 3, Timeout: time.Second})

	// Depth.
	out, _ := exec(t, f, ownerV, `{ me { edges { nodes { artefact { people { person { name } } } } } } }`, nil)
	if len(out.Errors) == 0 {
		t.Fatal("an over-deep query was accepted")
	}
	// Size.
	out, _ = exec(t, f, ownerV, `{ me { name } } #`+strings.Repeat("x", 300), nil)
	if len(out.Errors) == 0 {
		t.Fatal("an over-long query was accepted")
	}
	// Page size is clamped, not refused.
	body := mustOK(t, f, ownerV, `{ people(first: 100) { nodes { name } next } }`, nil)
	if strings.Count(body, `"name"`) != 2 || !strings.Contains(body, `"next":"2"`) {
		t.Fatalf("page clamp = %s", body)
	}
	// Node budget: three people fit, their edges do not.
	tight := newFixture(t, Limits{MaxDepth: 8, MaxQueryBytes: 1000, MaxPage: 10, MaxNodes: 3, Timeout: time.Second})
	out, res := exec(t, tight, ownerV, `{ people { nodes { name edges { nodes { predicate } } } } }`, nil)
	if len(out.Errors) == 0 || !strings.Contains(out.Errors[0].Message, "too many nodes") {
		t.Fatalf("node budget not enforced: %+v (%d nodes)", out.Errors, res.Nodes)
	}
	if res.Errors == 0 {
		t.Fatal("Result.Errors must count the errors")
	}
	// A small query stays within it and reports what it touched.
	if _, res := exec(t, f, ownerV, `{ me { name } }`, nil); res.Nodes != 1 || res.Errors != 0 {
		t.Fatalf("Result = %+v", res)
	}

	// Timeout: a query that cannot finish in time is cut off.
	slow := newFixture(t, Limits{MaxDepth: 8, MaxQueryBytes: 1000, MaxPage: 10, MaxNodes: 100, Timeout: time.Nanosecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res = slow.engine.Exec(ctx, ownerV, `{ me { name } }`, "", nil)
	if len(res.Body) == 0 {
		t.Fatal("a cancelled query must still answer")
	}
}
