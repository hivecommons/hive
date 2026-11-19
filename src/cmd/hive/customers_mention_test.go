package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
)

type countingMentionSearcher struct {
	mu    sync.Mutex
	calls int
	login string
}

func (c *countingMentionSearcher) SearchMentions(_ context.Context, kind, login string, _ []string, _ int) ([]customers.MentionHit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.login = login
	if kind == customers.MentionKindCommenter {
		return nil, nil
	}
	return []customers.MentionHit{{Repo: "acme/widgets", Number: 5, IsPR: true, At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}}, nil
}

func (c *countingMentionSearcher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// wireMentionTest returns an adapter whose accept-time search is on, runs
// inline, and logs into buf.
func wireMentionTest(t *testing.T, enabled bool) (*customersDashboard, *countingMentionSearcher, *bytes.Buffer) {
	t.Helper()
	c := wireTestCustomers(t)
	fake := &countingMentionSearcher{}
	m := customers.NewMentionSearch(fake, c.svc.Index())
	m.Enabled = func() bool { return enabled }
	m.HourlyLimit = func() int { return 10 }
	m.Repos = func() []string { return []string{"acme/widgets"} }
	c.mentions = m
	c.spawn = func(f func()) { f() }
	var buf bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&buf, nil))
	return c, fake, &buf
}

// suggest makes the sweep see a login and an address nobody owns, which
// leaves one suggestion for each, and returns their ids.
func suggest(t *testing.T, c *customersDashboard) (loginID, emailID string) {
	t.Helper()
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	_, err := c.svc.Sweep(customers.SweepInput{Sightings: []customers.Sighting{
		{Source: customersSourcePoll, Observation: customers.Observation{Kind: customers.KindGitHub, Value: "zed", Predicate: customers.PredReported, Artefact: "gh:issue:acme/widgets#7", At: at}},
		{Source: customersSourceMail, Observation: customers.Observation{Kind: customers.KindEmail, Value: "yara@example.org", Predicate: customers.PredSent, Artefact: "inbox:mail:1", At: at}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sg := range c.svc.Store().Suggestions() {
		switch sg.Kind {
		case customers.KindGitHub:
			loginID = sg.ID
		case customers.KindEmail:
			emailID = sg.ID
		}
	}
	if loginID == "" || emailID == "" {
		t.Fatalf("suggestions = %+v", c.svc.Store().Suggestions())
	}
	return loginID, emailID
}

// The search is reached from the accept path only: a sweep that sees a new
// login, with the search wired and on, makes no call.
func TestCustomersMentionSearchNeverRunsInTheSweep(t *testing.T) {
	c, fake, _ := wireMentionTest(t, true)
	suggest(t, c)
	if _, err := c.svc.Sweep(customers.SweepInput{}); err != nil {
		t.Fatal(err)
	}
	if n := fake.count(); n != 0 {
		t.Fatalf("the sweep made %d GitHub searches", n)
	}
	// And the sweep's source cannot reach it: it is handed no client and
	// names neither the search nor its adapter.
	src, err := os.ReadFile("customers_sweep.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"MentionSearch", "SearchMentions", "mentions"} {
		if strings.Contains(string(src), "."+banned) || strings.Contains(string(src), banned+"(") {
			t.Errorf("customers_sweep.go references %q; the search belongs on the accept path only", banned)
		}
	}
	if !strings.Contains(string(src), "never handed a GitHub client") {
		t.Error("the sweep's no-network comment is gone")
	}
}

func TestCustomersMentionSearchRunsOnAccept(t *testing.T) {
	c, fake, buf := wireMentionTest(t, true)
	loginID, _ := suggest(t, c)
	owner := dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}
	res, err := c.Accept(owner, loginID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.count() != customers.MentionSearchMaxRequests {
		t.Fatalf("accept made %d searches, want %d", fake.count(), customers.MentionSearchMaxRequests)
	}
	edges, _, err := c.svc.Index().ByPerson(res.ID, customers.PredMentionedIn, "", 10)
	if err != nil || len(edges) == 0 {
		t.Fatalf("edges = %+v, %v", edges, err)
	}
	var found bool
	for _, e := range edges {
		if e.Artefact == "gh:pr:acme/widgets#5" && e.Source == customers.MentionSearchSource {
			found = true
		}
	}
	if !found {
		t.Fatalf("the search result did not land as an edge: %+v", edges)
	}
	// Ids and counts only in the log.
	out := buf.String()
	if !strings.Contains(out, "requests=2") || !strings.Contains(out, "person="+res.ID) {
		t.Fatalf("log line missing ids and counts: %q", out)
	}
	for _, bad := range []string{"zed", "acme", "widgets", "mentions:", "token"} {
		if strings.Contains(out, bad) {
			t.Errorf("log line leaks %q: %q", bad, out)
		}
	}
}

func TestCustomersMentionSearchAcceptIntoAlsoSearches(t *testing.T) {
	c, fake, _ := wireMentionTest(t, true)
	loginID, _ := suggest(t, c)
	owner := dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}
	if _, err := c.AcceptInto(owner, loginID, "me"); err != nil {
		t.Fatal(err)
	}
	if fake.count() != customers.MentionSearchMaxRequests || fake.login != "zed" {
		t.Fatalf("calls = %d, login %q", fake.count(), fake.login)
	}
}

func TestCustomersMentionSearchSkippedWhenOffOrNotALogin(t *testing.T) {
	owner := dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}
	c, fake, _ := wireMentionTest(t, false)
	loginID, _ := suggest(t, c)
	if _, err := c.Accept(owner, loginID); err != nil || fake.count() != 0 {
		t.Fatalf("off: err %v, calls %d", err, fake.count())
	}
	c, fake, _ = wireMentionTest(t, true)
	_, emailID := suggest(t, c)
	if _, err := c.Accept(owner, emailID); err != nil || fake.count() != 0 {
		t.Fatalf("an email suggestion must never be searched: err %v, calls %d", err, fake.count())
	}
}

// A refused accept (read-only viewer, unknown suggestion) searches nothing.
func TestCustomersMentionSearchNotRunOnRefusedAccept(t *testing.T) {
	c, fake, _ := wireMentionTest(t, true)
	loginID, _ := suggest(t, c)
	if _, err := c.Accept(dashboard.CustomersViewer{User: "reed"}, loginID); err == nil {
		t.Fatal("a read-only accept must be refused")
	}
	if _, err := c.Accept(dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}, "s_missing"); err == nil {
		t.Fatal("an unknown suggestion must be refused")
	}
	if fake.count() != 0 {
		t.Fatalf("%d searches after refused accepts", fake.count())
	}
}

// The accept does not wait for GitHub: by default the search is a goroutine.
func TestCustomersMentionSearchDefaultsToBackground(t *testing.T) {
	c, fake, _ := wireMentionTest(t, true)
	c.spawn = nil
	loginID, _ := suggest(t, c)
	if _, err := c.Accept(dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}, loginID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for fake.count() < customers.MentionSearchMaxRequests && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.count() != customers.MentionSearchMaxRequests {
		t.Fatalf("background search made %d calls", fake.count())
	}
}
