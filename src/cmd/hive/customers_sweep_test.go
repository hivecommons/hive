package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailallow"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

var customersTestTime = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

func customersTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "acme", AIAuthor: "hive-author"},
		Dashboard: config.DashboardConfig{AuthorizedUsers: []string{
			"github:Olivia:owner", "rita:read-write", "max:merger", "reed", "",
		}},
	}
}

func customersTestSources() customersSources {
	proven := customersTestTime
	return customersSources{
		cfg:        customersTestConfig(),
		ownerLogin: "olivia-gh",
		proven: []mailallow.Entry{
			{Login: "rita", Email: "rita@example.com", ProvenAt: &proven},
			{Login: "reed", Email: "reed@example.com"}, // never proven
		},
		last: &github.ActionableResult{
			Issues: github.IssueResult{Items: []github.Issue{
				{Repo: "widgets", Number: 7, Author: "carol", AuthorIsHuman: true, Assignees: []string{"dave", "dependabot[bot]"},
					Body: "cc @erin, mail carol@example.com, `@notalogin`", CreatedAt: customersTestTime, UpdatedAt: customersTestTime.Add(time.Hour)},
				{Repo: "widgets", Number: 8, Author: "some-app[bot]", AuthorIsHuman: false, CreatedAt: customersTestTime},
				{Repo: "widgets", Number: 0, Author: "linear-user", AuthorIsHuman: true},
			}},
			PRs: github.PRResult{Items: []github.PullRequest{
				{Repo: "acme/widgets", Number: 20, Author: "frank", AuthorAssociation: "MEMBER", RequestedReviewers: []string{"gina"}, CreatedAt: customersTestTime},
				{Repo: "widgets", Number: 21, Author: "hive-author", CreatedAt: customersTestTime},
				{Repo: "widgets", Number: 22, Author: "an-app", AppAuthored: true, CreatedAt: customersTestTime},
				{Repo: "widgets", Number: 23, Author: "robot", AuthorNonHuman: true, CreatedAt: customersTestTime},
			}},
		},
		claims: []claims.Claim{
			{Repo: "acme/widgets", Issue: 7, Holder: "heidi", Kind: claims.KindHuman, ClaimedAt: customersTestTime},
			{Repo: "acme/widgets", Issue: 9, Holder: "scanner", Kind: claims.KindAgent, ClaimedAt: customersTestTime},
		},
		uploads: []inbox.UploadMeta{{ID: "u1", AddedBy: "rita", AddedAt: customersTestTime}},
		mail: []inbox.Item{
			{ID: "m1", Sender: "Ivan <Ivan@Example.com>", RecordedAt: customersTestTime},
			{ID: "m2", Sender: "judy@example.com", AwaitingApproval: true, RecordedAt: customersTestTime},
			{ID: "m3", Sender: "not an address", RecordedAt: customersTestTime},
		},
		events: []schedule.Event{{
			ID: "e1", Organizer: &schedule.Person{Email: "ken@example.com", Name: "Ken"},
			Attendees: []schedule.Person{{Email: "lea@example.com"}, {Name: "No Email"}},
			InvitedBy: "ken@example.com", CreatedAt: customersTestTime,
		}},
		meetings: []meetings.Meeting{
			{ID: "mt1", Platform: "discord", InvitedBy: "olivia", RequestedBy: &meetings.Person{ID: "123456", Name: "Mallory Display"}, CreatedAt: customersTestTime},
			{ID: "mt2", Platform: "zoom", InvitedBy: "rita", State: meetings.StatePurged, CreatedAt: customersTestTime},
		},
	}
}

type sightKey struct {
	kind  customers.IdentityKind
	value string
	pred  string
	ref   string
}

func TestCustomersSweepInputMapsLocalState(t *testing.T) {
	in := customersSweepInput(customersTestSources())

	users := map[string]customers.HiveUser{}
	for _, u := range in.Users {
		users[u.Name] = u
	}
	if len(users) != 4 || !users["github:Olivia"].Maintainer || !users["max"].Maintainer || users["rita"].Maintainer || users["reed"].Maintainer {
		t.Fatalf("users = %+v", in.Users)
	}
	if users["reed"].Rule != config.RoleRead || users["github:Olivia"].Rule != config.RoleOwner {
		t.Fatalf("roles = %+v", in.Users)
	}

	links := map[string]string{}
	for _, l := range in.Links {
		links[l.User+"|"+string(l.Identity.Kind)+"|"+l.Identity.Value] = l.Identity.Source
	}
	wantLinks := map[string]string{
		"github:Olivia|github|olivia":    customersSourceAccessList,
		"github:Olivia|github|olivia-gh": customersSourceOwner,
		"rita|email|rita@example.com":    customersSourceProof,
		// A bare access-list name is the GitHub login of a device-flow user.
		"rita|github|rita": customersSourceAccessList,
		"max|github|max":   customersSourceAccessList,
		"reed|github|reed": customersSourceAccessList,
	}
	if len(links) != len(wantLinks) {
		t.Fatalf("links = %v, want %v (an unproven email must not link)", links, wantLinks)
	}
	for k, src := range wantLinks {
		if links[k] != src {
			t.Errorf("link %s = %q, want %q", k, links[k], src)
		}
	}

	got := map[sightKey]customers.Sighting{}
	for _, s := range in.Sightings {
		got[sightKey{s.Kind, s.Value, s.Predicate, s.Artefact}] = s
	}
	want := []sightKey{
		{customers.KindGitHub, "carol", customers.PredReported, "gh:issue:acme/widgets#7"},
		{customers.KindGitHub, "dave", customers.PredAssigned, "gh:issue:acme/widgets#7"},
		{customers.KindGitHub, "erin", customers.PredMentionedIn, "gh:issue:acme/widgets#7"},
		{customers.KindGitHub, "frank", customers.PredAuthored, "gh:pr:acme/widgets#20"},
		{customers.KindGitHub, "gina", customers.PredReviewRequested, "gh:pr:acme/widgets#20"},
		{customers.KindGitHub, "heidi", customers.PredClaimed, "gh:issue:acme/widgets#7"},
		{customers.KindHiveUser, "rita", customers.PredAdded, "inbox:upload:u1"},
		{customers.KindEmail, "Ivan@Example.com", customers.PredSent, "inbox:email:m1"},
		{customers.KindEmail, "ken@example.com", customers.PredOrganised, "event:e1"},
		{customers.KindEmail, "lea@example.com", customers.PredInvited, "event:e1"},
		{customers.KindEmail, "ken@example.com", customers.PredAdded, "event:e1"},
		{customers.KindHiveUser, "olivia", customers.PredAdded, "meeting:mt1"},
		{customers.KindDiscord, "123456", customers.PredAdded, "meeting:mt1"},
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing sighting %+v", k)
		}
	}
	if len(got) != len(want) {
		for k := range got {
			t.Logf("got %+v", k)
		}
		t.Fatalf("%d sightings, want %d: bots, the hive's own author, agent claims, mail awaiting approval, purged meetings and display names must produce none", len(got), len(want))
	}
	if !got[sightKey{customers.KindGitHub, "frank", customers.PredAuthored, "gh:pr:acme/widgets#20"}].Maintainer {
		t.Error("a MEMBER author association must mark a maintainer")
	}
	if s := got[sightKey{customers.KindGitHub, "carol", customers.PredReported, "gh:issue:acme/widgets#7"}]; s.Source != customersSourcePoll || !s.At.Equal(customersTestTime) {
		t.Errorf("issue author sighting = %+v", s)
	}
	for k := range got {
		if k.kind == customers.KindName {
			t.Errorf("a display name was read as an identity: %+v", k)
		}
	}
	if in := customersSweepInput(customersSources{}); len(in.Users)+len(in.Links)+len(in.Sightings) != 0 {
		t.Fatal("no config must give an empty input")
	}
	src := customersTestSources()
	src.last = nil
	src.cfg.Dashboard.AuthorizedUsers = []string{"olivia", "second:owner"}
	if in := customersSweepInput(src); len(in.Users) != 2 {
		t.Fatalf("users = %+v", in.Users)
	} else {
		for _, l := range in.Links {
			if l.Identity.Source == customersSourceOwner {
				t.Fatal("with two owners the connected GitHub account must not be given to either")
			}
		}
	}
}

func TestCustomersHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"Ada <ada@example.com>": "ada@example.com", "bob@example.com": "bob@example.com", "": "", "nobody": "",
	} {
		if got := customersAddress(in); got != want {
			t.Errorf("customersAddress(%q) = %q, want %q", in, got, want)
		}
	}
	if k, v := customersWho("rita"); k != customers.KindHiveUser || v != "rita" {
		t.Errorf("customersWho(rita) = %s, %s", k, v)
	}
	if k, v := customersWho("Ken <ken@example.com>"); k != customers.KindEmail || v != "ken@example.com" {
		t.Errorf("customersWho(address) = %s, %s", k, v)
	}
	if k, _ := customersWho("  "); k != "" {
		t.Errorf("customersWho(blank) = %s", k)
	}
	for platform, want := range map[string]customers.IdentityKind{
		"discord": customers.KindDiscord, "Teams": customers.KindTeams, "zoom": customers.KindZoom,
		"google_meet": customers.KindMeet, "phone": "",
	} {
		if got := customersPlatformKind(platform); got != want {
			t.Errorf("customersPlatformKind(%q) = %q, want %q", platform, got, want)
		}
	}
	for assoc, want := range map[string]bool{"OWNER": true, "member": true, "COLLABORATOR": true, "CONTRIBUTOR": false, "": false} {
		if got := customersMaintainerAssociation(assoc); got != want {
			t.Errorf("customersMaintainerAssociation(%q) = %v", assoc, got)
		}
	}
}

// TestCustomersSweepMakesNoGitHubCall runs discovery on a boot whose GitHub
// client points at a server that counts every request. A normal sweep reads
// the governor's last enumeration and local stores only.
func TestCustomersSweepMakesNoGitHubCall(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "the customers sweep must not call GitHub", http.StatusTeapot)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := customersTestSources()
	b := &boot{logger: logger, ghClient: github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)}
	b.lastActionable.Store(src.last)
	dir := t.TempDir()
	svc, stop := wireCustomersAt(filepath.Join(dir, "customers"), filepath.Join(dir, "graph"), b.customersResolvers(), logger)
	if svc == nil {
		t.Fatal("no customers service")
	}
	t.Cleanup(stop)
	b.customers = svc

	for i := 0; i < 2; i++ {
		b.runCustomersSweep(context.Background(), src.cfg)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("the customers sweep made %d GitHub requests, want 0", n)
	}

	store := svc.svc.Store()
	people := store.People()
	if len(people) != 4 {
		t.Fatalf("people = %d, want the four dashboard users", len(people))
	}
	olivia, ok := store.Lookup(customers.KindHiveUser, "olivia")
	if !ok || !olivia.HasIdentity("github:olivia") || !olivia.HasTag(customers.TagMaintainer) {
		t.Fatalf("olivia = %+v", olivia)
	}
	// Issue and PR authors the hive has seen are suggestions, not customers.
	suggested := map[string]bool{}
	for _, sg := range store.Suggestions() {
		suggested[string(sg.Kind)+":"+sg.Value] = true
	}
	for _, key := range []string{"github:carol", "github:frank", "github:dave", "github:erin", "github:gina"} {
		if !suggested[key] {
			t.Errorf("%s is not suggested: %v", key, suggested)
		}
	}
	for _, key := range []string{"github:hive-author", "github:an-app", "github:robot", "github:some-app[bot]", "github:dependabot[bot]", "github:linear-user"} {
		if suggested[key] {
			t.Errorf("%s must not be suggested", key)
		}
	}

	// Off: the sweep does nothing.
	off := false
	disabled := customersTestConfig()
	disabled.Customers.Enabled = &off
	disabled.Dashboard.AuthorizedUsers = append(disabled.Dashboard.AuthorizedUsers, "newcomer")
	t.Setenv(config.EnvCustomersEnabled, "")
	b.runCustomersSweep(context.Background(), disabled)
	if len(store.People()) != 4 {
		t.Fatal("a sweep ran with customers.enabled off")
	}
	b.customers = nil
	b.runCustomersSweep(context.Background(), src.cfg) // no store: must not panic
	b.runCustomersSweep(context.Background(), nil)
}

// With the Inbox, the Calendar and meetings off, nothing can say an artefact
// is gone, so nothing is pruned.
func TestCustomersResolversWithStoresOff(t *testing.T) {
	r := (&boot{}).customersResolvers()
	for _, ref := range []string{"meeting:m1", "event:e1", "inbox:upload:u1", "inbox:email:m1", "inbox:plaud:p1", "gh:issue:o/r#1"} {
		if !r.Exists(ref) {
			t.Errorf("Exists(%q) = false with every store off", ref)
		}
	}
	if r.Exists("inbox:malformed") {
		t.Error("a malformed inbox reference must not be kept")
	}
	owner := customers.Viewer{User: "o", Full: true, Owner: true}
	rw := customers.Viewer{User: "r", Full: true}
	read := customers.Viewer{User: "x"}
	visible := []struct {
		ref   string
		v     customers.Viewer
		want  bool
		label string
	}{
		{"meeting:m1", owner, true, "owner sees meetings"},
		{"meeting:m1", rw, false, "meetings are the owner's"},
		{"meeting:m1", read, false, "read-only sees no meeting"},
		{"event:e1", rw, true, "read-write sees the calendar"},
		{"event:e1", read, false, "read-only sees no calendar event"},
		{"inbox:email:m1", rw, true, "read-write sees inbox items"},
		{"inbox:email:m1", read, false, "read-only sees no inbox item"},
		{"gh:issue:o/r#1", read, true, "a GitHub reference is not private"},
	}
	for _, tc := range visible {
		if got := r.Visible(tc.ref, tc.v); got != tc.want {
			t.Errorf("%s: Visible(%q) = %v", tc.label, tc.ref, got)
		}
	}
}

func TestCustomersAuditSightings(t *testing.T) {
	src := customersTestSources()
	src.audit = []dashboard.AuditEntry{
		{Timestamp: "2026-11-03T09:00:00Z", User: "olivia", Action: "config_save", Detail: "secret=hunter2"},
		{Timestamp: "2026-11-03T17:00:00Z", User: "Olivia", Action: "agent_restart"},
		{Timestamp: "2026-11-03T08:00:00Z", User: "olivia", Action: "dashboard_login"},
		{Timestamp: "2026-11-04T01:00:00Z", User: "olivia", Action: "pause"},
		{Timestamp: "2026-11-03T09:00:00Z", User: "reed", Action: "customer_edit"},
		{Timestamp: "not a time", User: "reed", Action: "pause"},
		{Timestamp: "2026-11-03T09:00:00Z", User: "  ", Action: "pause"},
	}
	got := map[sightKey]customers.Sighting{}
	for _, s := range customersAuditSightings(src) {
		got[sightKey{s.Kind, s.Value, s.Predicate, s.Artefact}] = s
	}
	want := []sightKey{
		{customers.KindHiveUser, "olivia", customers.PredActed, "audit:2026-11-03"},
		{customers.KindHiveUser, "olivia", customers.PredLoggedIn, "audit:2026-11-03"},
		{customers.KindHiveUser, "olivia", customers.PredActed, "audit:2026-11-04"},
		{customers.KindHiveUser, "reed", customers.PredActed, "audit:2026-11-03"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d audit sightings, want %d (one per person, kind and day): %v", len(got), len(want), got)
	}
	for _, k := range want {
		if s, ok := got[k]; !ok || s.Source != customersSourceAudit {
			t.Errorf("missing audit sighting %+v", k)
		}
	}
	for k := range got {
		if strings.Contains(k.ref+k.value, "hunter2") {
			t.Fatal("audit detail text was copied")
		}
	}
	if len(customersAuditSightings(customersSources{})) != 0 {
		t.Fatal("no audit log, no sightings")
	}
}

func TestCustomersAuditResolver(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := customersAuditResolver{now: func() time.Time { return now }}
	for ref, want := range map[string]bool{
		"audit:2026-11-17": true, "audit:2026-08-20": true, "audit:2026-08-10": false, "audit:garbage": false, "audit:": false,
	} {
		if got := r.Exists(ref); got != want {
			t.Errorf("Exists(%q) = %v, want %v", ref, got, want)
		}
	}
	if !r.Visible("audit:2026-11-17", customers.Viewer{User: "a", Full: true}) || r.Visible("audit:2026-11-17", customers.Viewer{User: "a"}) {
		t.Fatal("audit edges are for the full view only")
	}
	if (&boot{}).customersResolvers()[customers.RefAudit] == nil {
		t.Fatal("the audit resolver is not registered")
	}
}

// TestCustomersBareAccessListNameIsGitHubLogin pins the root cause of
// hive users showing up as suggested GitHub people: the access list holds a
// device-flow user as the bare login, not "github:<login>", and only the
// prefixed form used to be linked.
func TestCustomersBareAccessListNameIsGitHubLogin(t *testing.T) {
	for name, want := range map[string]string{
		"danathar": "danathar", "GitHub:Alice": "alice", "github:bob-1": "bob-1", " carol ": "carol",
		"ibmid:abc": "", "dave@example.com": "", "has.dot": "", "under_score": "", "-lead": "", "a--b": "", "": "", "github:": "",
		"x23456789012345678901234567890123456789x": "",
	} {
		if got := customersGitHubLogin(name); got != want {
			t.Errorf("customersGitHubLogin(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCustomersOwnAddresses(t *testing.T) {
	cfg := &config.Config{}
	cfg.Hub.DashboardURL = "https://Hosted-X.hive.example.dev/"
	got := customersOwnAddresses(cfg)
	if len(got) != 1 || got[0] != "hive@hosted-x.hive.example.dev" {
		t.Fatalf("own addresses = %v", got)
	}
	if customersOwnAddresses(&config.Config{}) != nil || customersOwnAddresses(nil) != nil {
		t.Fatal("an unknown host must give no own address")
	}
	if in := customersSweepInput(customersSources{cfg: cfg}); len(in.OwnAddresses) != 1 {
		t.Fatalf("the sweep input lacks the own address: %v", in.OwnAddresses)
	}
}
