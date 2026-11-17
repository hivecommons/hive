package main

import (
	"context"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailallow"
	"github.com/hivecommons/hive/pkg/mailin/wire"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

// The customers discovery sweep (hivecommons/hive#18742). It reads only
// what the hive process already holds: the access list, proven calendar
// emails, the owner's connected GitHub account, the last GitHub
// enumeration the governor made, the claims ledger, and the local Inbox,
// Calendar and meeting stores. It is never handed a GitHub client and makes
// no network call.

const (
	// customersSweepInterval is how often discovery runs. Everything it
	// reads is local, so the cost of a pass is a few file reads.
	customersSweepInterval = 10 * time.Minute
	// githubUserPrefix is the optional provider prefix of an access-list
	// entry the hub authenticated with GitHub ("github:alice").
	githubUserPrefix = "github:"
)

// Sources the sweep names on the edges and suggestions it writes.
const (
	customersSourceAccessList = "access_list"
	customersSourceOwner      = "owner_identity"
	customersSourceProof      = "calendar_proof"
	customersSourcePoll       = "github_poll"
	customersSourceClaims     = "claims_ledger"
	customersSourceUploads    = "inbox_uploads"
	customersSourceMail       = "inbox_mail"
	customersSourceCalendar   = "calendar"
	customersSourceMeetings   = "meetings"
	customersSourceAudit      = "audit_log"
)

const (
	// customersAuditWindow is how many recent audit entries one pass reads.
	customersAuditWindow = 5000
	// customersLoginActionMarker marks an audit action that is a sign-in.
	customersLoginActionMarker = "login"
	// customersAuditDayLayout is the day an audit reference names.
	customersAuditDayLayout = "2006-01-02"
)

// customersSources is one sweep's reading of the hive's local state.
type customersSources struct {
	cfg *config.Config
	// last is the governor's last issue and pull-request enumeration.
	last *github.ActionableResult
	// ownerLogin is the owner's connected GitHub account, "" when none.
	ownerLogin string
	proven     []mailallow.Entry
	claims     []claims.Claim
	uploads    []inbox.UploadMeta
	// mail is the email spool; items awaiting approval are skipped.
	mail     []inbox.Item
	events   []schedule.Event
	meetings []meetings.Meeting
	// audit is the recent dashboard audit log (step D).
	audit []dashboard.AuditEntry
}

// customersSources reads the local stores. A store that is off or fails to
// list contributes nothing this pass.
func (b *boot) customersSources(ctx context.Context, cfg *config.Config) customersSources {
	src := customersSources{cfg: cfg, last: b.lastActionable.Load()}
	if b.dashSrv != nil {
		if log := b.dashSrv.GetAudit(); log != nil {
			src.audit = log.Recent(customersAuditWindow)
		}
	}
	if b.ownerIdentity != nil {
		src.ownerLogin = b.ownerIdentity.Status().Login
	}
	if b.mailAllow != nil {
		src.proven = b.mailAllow.All()
	}
	if b.issueClaims != nil {
		src.claims = b.issueClaims.List()
	}
	if b.inboxSvc != nil {
		if up := b.inboxSvc.Uploads(); up != nil && up.Store != nil {
			if metas, err := up.Store.List(); err == nil {
				src.uploads = metas
			}
		}
		if em := b.inboxSvc.Email(); em != nil {
			if items, err := em.List(ctx); err == nil {
				src.mail = items
			}
		}
	}
	if b.meetingsSched != nil {
		src.events = b.meetingsSched.List()
	}
	if b.meetingsMgr != nil {
		if ms, err := b.meetingsMgr.List(); err == nil {
			src.meetings = ms
		}
	}
	return src
}

// runCustomersSweep is one discovery pass.
func (b *boot) runCustomersSweep(ctx context.Context, cfg *config.Config) {
	if b.customers == nil || cfg == nil || !cfg.CustomersEnabled() {
		return
	}
	res, err := b.customers.svc.Sweep(customersSweepInput(b.customersSources(ctx, cfg)))
	if err != nil {
		if b.logger != nil {
			b.logger.Warn("[customers] discovery sweep failed", "error", err)
		}
		return
	}
	if b.logger != nil && (res.Linked > 0 || res.TagsChanged > 0 || res.PrunedEdges > 0 || res.ExpiredSuggestions > 0 || res.DroppedEvidence > 0) {
		b.logger.Info("[customers] discovery sweep", "users", res.Users, "linked", res.Linked, "edges", res.Edges,
			"suggestions", res.Suggestions, "tags_changed", res.TagsChanged, "pruned_edges", res.PrunedEdges,
			"expired_suggestions", res.ExpiredSuggestions, "dropped_evidence", res.DroppedEvidence)
	}
}

// customersSweepInput turns the local state into what the sweep consumes.
// It is pure: the same sources give the same input.
func customersSweepInput(src customersSources) customers.SweepInput {
	var in customers.SweepInput
	if src.cfg == nil {
		return in
	}
	in.OwnAddresses = customersOwnAddresses(src.cfg)
	in.Users, in.Links = customersUsers(src)
	in.Sightings = append(in.Sightings, customersGitHubSightings(src)...)
	in.Sightings = append(in.Sightings, customersLocalSightings(src)...)
	in.Sightings = append(in.Sightings, customersAuditSightings(src)...)
	return in
}

// customersUsers is step A: dashboard users from the access list, and the
// identities a trusted source proved are theirs.
func customersUsers(src customersSources) ([]customers.HiveUser, []customers.ProvenLink) {
	var users []customers.HiveUser
	var links []customers.ProvenLink
	var owners []string
	for i, entry := range src.cfg.Dashboard.AuthorizedUsers {
		name, role := dashboard.AuthorizedUserEntry(entry)
		if name == "" {
			continue
		}
		if role == "" {
			role = config.RoleRead
			if i == 0 {
				role = config.RoleOwner
			}
		}
		users = append(users, customers.HiveUser{
			Name: name, Rule: role,
			Maintainer: role == config.RoleOwner || role == config.RoleMerger,
		})
		if role == config.RoleOwner {
			owners = append(owners, name)
		}
		// The hub writes a GitHub-authenticated user as "github:<login>",
		// and a device-flow login is the bare login: the access list
		// treats the two as one user. Either way the name is a GitHub
		// login, as long as it is shaped like one.
		if login := customersGitHubLogin(name); login != "" {
			links = append(links, customers.ProvenLink{User: name, Identity: customers.Identity{
				Kind: customers.KindGitHub, Value: login, Source: customersSourceAccessList,
			}})
		}
	}
	// The owner's connected GitHub account belongs to the owner, when
	// there is exactly one owner to give it to.
	if src.ownerLogin != "" && len(owners) == 1 {
		links = append(links, customers.ProvenLink{User: owners[0], Identity: customers.Identity{
			Kind: customers.KindGitHub, Value: src.ownerLogin, Source: customersSourceOwner,
		}})
	}
	for _, e := range src.proven {
		if !e.Proven() || e.Login == "" || e.Email == "" {
			continue
		}
		links = append(links, customers.ProvenLink{User: e.Login, Identity: customers.Identity{
			Kind: customers.KindEmail, Value: e.Email, Source: customersSourceProof,
		}})
	}
	return users, links
}

// customersGitHubLogin is the GitHub login an access-list name stands for, or
// "" when it is not one: a name with another provider prefix, an address or
// anything else a login cannot be.
func customersGitHubLogin(name string) string {
	login := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), githubUserPrefix)
	if !customers.LooksLikeGitHubLogin(login) {
		return ""
	}
	return login
}

// customersOwnAddresses are this hive's own mail addresses: hive@<dashboard
// host>. Mail and invites sent to it are never a person.
func customersOwnAddresses(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(cfg.Hub.DashboardURL))
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return []string{wire.LocalPart + "@" + strings.ToLower(u.Hostname())}
}

// customersIsPerson reports whether a GitHub login is a person rather than
// a bot or this hive's own author account.
func customersIsPerson(cfg *config.Config, login string) bool {
	if customers.IsBotLogin(login) {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(login), strings.TrimSpace(cfg.Project.AIAuthor))
}

// customersMaintainerAssociation reports whether GitHub's author
// association marks a project member.
func customersMaintainerAssociation(assoc string) bool {
	switch strings.ToUpper(strings.TrimSpace(assoc)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

func customersSighting(kind customers.IdentityKind, value, predicate, artefact string, at time.Time, source string) customers.Sighting {
	return customers.Sighting{Source: source, Observation: customers.Observation{
		Kind: kind, Value: value, Predicate: predicate, Artefact: artefact, At: at,
	}}
}

// customersGitHubSightings is step C: authors, assignees, requested
// reviewers and mentioned logins from the enumeration the governor already
// made. It costs no GitHub call.
func customersGitHubSightings(src customersSources) []customers.Sighting {
	var out []customers.Sighting
	if src.last == nil {
		return out
	}
	org := src.cfg.Project.Org
	seenLogin := func(login, predicate, ref string, at time.Time) {
		if customersIsPerson(src.cfg, login) {
			out = append(out, customersSighting(customers.KindGitHub, login, predicate, ref, at, customersSourcePoll))
		}
	}
	for _, is := range src.last.Issues.Items {
		if is.Number <= 0 || is.Repo == "" {
			continue // a Linear or Jira item has no GitHub number
		}
		ref := customers.IssueRef(qualifyClaimRepo(org, is.Repo), is.Number)
		seen := is.UpdatedAt
		if seen.IsZero() {
			seen = is.CreatedAt
		}
		if is.AuthorIsHuman {
			seenLogin(is.Author, customers.PredReported, ref, is.CreatedAt)
		}
		for _, a := range is.Assignees {
			seenLogin(a, customers.PredAssigned, ref, seen)
		}
		for _, m := range customers.Mentions(is.Body) {
			seenLogin(m, customers.PredMentionedIn, ref, seen)
		}
	}
	prs := append(append([]github.PullRequest{}, src.last.PRs.Items...), src.last.PRs.Attributed...)
	for _, pr := range prs {
		if pr.Number <= 0 || pr.Repo == "" {
			continue
		}
		ref := customers.PRRef(qualifyClaimRepo(org, pr.Repo), pr.Number)
		seen := pr.UpdatedAt
		if seen.IsZero() {
			seen = pr.CreatedAt
		}
		if !pr.AppAuthored && !pr.AuthorNonHuman && customersIsPerson(src.cfg, pr.Author) {
			sg := customersSighting(customers.KindGitHub, pr.Author, customers.PredAuthored, ref, pr.CreatedAt, customersSourcePoll)
			sg.Maintainer = customersMaintainerAssociation(pr.AuthorAssociation)
			out = append(out, sg)
		}
		for _, r := range pr.RequestedReviewers {
			seenLogin(r, customers.PredReviewRequested, ref, seen)
		}
	}
	for _, c := range src.claims {
		if c.Kind == claims.KindAgent || c.Issue <= 0 || c.Repo == "" || !customersIsPerson(src.cfg, c.Holder) {
			continue
		}
		out = append(out, customersSighting(customers.KindGitHub, c.Holder, customers.PredClaimed,
			customers.IssueRef(qualifyClaimRepo(org, c.Repo), c.Issue), c.ClaimedAt, customersSourceClaims))
	}
	return out
}

// customersAddress pulls the address out of a From value ("Ada <a@b.c>" or
// "a@b.c"). It returns "" when there is none.
func customersAddress(from string) string {
	from = strings.TrimSpace(from)
	if from == "" {
		return ""
	}
	if a, err := mail.ParseAddress(from); err == nil {
		return a.Address
	}
	return ""
}

// customersWho is the identity behind a "who asked for this" field, which
// holds a dashboard user for something added in the dashboard and an email
// address for something that arrived by mail.
func customersWho(v string) (customers.IdentityKind, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	if strings.Contains(v, "@") {
		return customers.KindEmail, customersAddress(v)
	}
	return customers.KindHiveUser, v
}

// customersPlatformKind is the identity kind of a meeting platform's own
// user id.
func customersPlatformKind(platform string) customers.IdentityKind {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "discord":
		return customers.KindDiscord
	case "teams":
		return customers.KindTeams
	case "zoom":
		return customers.KindZoom
	case "google_meet":
		return customers.KindMeet
	}
	return ""
}

// customersLocalSightings is step B: the Inbox, the Calendar and meetings.
// Mail awaiting approval creates nothing, a purged meeting creates nothing,
// and a display name is never read as an identity.
func customersLocalSightings(src customersSources) []customers.Sighting {
	var out []customers.Sighting
	add := func(kind customers.IdentityKind, value, predicate, ref string, at time.Time, source string) {
		if kind != "" && strings.TrimSpace(value) != "" {
			out = append(out, customersSighting(kind, value, predicate, ref, at, source))
		}
	}
	for _, u := range src.uploads {
		add(customers.KindHiveUser, u.AddedBy, customers.PredAdded, customers.InboxRef(inbox.UploadType, u.ID), u.AddedAt, customersSourceUploads)
	}
	for _, it := range src.mail {
		if it.AwaitingApproval || it.ID == "" {
			continue
		}
		add(customers.KindEmail, customersAddress(it.Sender), customers.PredSent, customers.InboxRef(inbox.EmailType, it.ID), it.RecordedAt, customersSourceMail)
	}
	for _, ev := range src.events {
		if ev.ID == "" {
			continue
		}
		ref := customers.EventRef(ev.ID)
		at := ev.CreatedAt
		if at.IsZero() {
			at = ev.Start
		}
		if ev.Organizer != nil {
			add(customers.KindEmail, customersAddress(ev.Organizer.Email), customers.PredOrganised, ref, at, customersSourceCalendar)
		}
		for _, a := range ev.Attendees {
			add(customers.KindEmail, customersAddress(a.Email), customers.PredInvited, ref, at, customersSourceCalendar)
		}
		kind, who := customersWho(ev.InvitedBy)
		add(kind, who, customers.PredAdded, ref, at, customersSourceCalendar)
	}
	for _, m := range src.meetings {
		if m.ID == "" || m.State == meetings.StatePurged {
			continue
		}
		ref := customers.MeetingRef(m.ID)
		kind, who := customersWho(m.InvitedBy)
		add(kind, who, customers.PredAdded, ref, m.CreatedAt, customersSourceMeetings)
		if m.RequestedBy != nil {
			add(customersPlatformKind(m.Platform), m.RequestedBy.ID, customers.PredAdded, ref, m.CreatedAt, customersSourceMeetings)
		}
	}
	return out
}

// customersResolvers answers, for each kind of artefact the sweep
// references, whether it still exists and who may see it. The stores are
// read at call time, so a store wired after this is still consulted. A
// store that is off cannot say an artefact is gone, so its edges are kept.
func (b *boot) customersResolvers() customers.Resolvers {
	return customers.Resolvers{
		customers.RefMeeting: customersMeetingResolver{b: b},
		customers.RefEvent:   customersEventResolver{b: b},
		customers.RefInbox:   customersInboxResolver{b: b},
		customers.RefAudit:   customersAuditResolver{now: time.Now},
	}
}

func customersRefID(ref, kind string) string {
	return strings.TrimPrefix(ref, kind+":")
}

// customersMeetingResolver: a meeting exists until it is deleted or its
// transcript is purged by retention. Meetings are the owner's to see.
type customersMeetingResolver struct{ b *boot }

func (r customersMeetingResolver) Exists(ref string) bool {
	mgr := r.b.meetingsMgr
	if mgr == nil {
		return true
	}
	m, err := mgr.Get(customersRefID(ref, customers.RefMeeting))
	return err == nil && m.State != meetings.StatePurged
}

func (r customersMeetingResolver) Visible(_ string, v customers.Viewer) bool {
	return v.Full && v.Owner
}

// customersEventResolver: a Calendar event exists while the schedule holds
// it. The Calendar is for the owner and read-write users.
type customersEventResolver struct{ b *boot }

func (r customersEventResolver) Exists(ref string) bool {
	sched := r.b.meetingsSched
	if sched == nil {
		return true
	}
	_, err := sched.Get(customersRefID(ref, customers.RefEvent))
	return err == nil
}

func (r customersEventResolver) Visible(_ string, v customers.Viewer) bool { return v.Full }

// customersInboxResolver: an Inbox item exists while its source holds it.
// Inbox content is for the owner and read-write users.
type customersInboxResolver struct{ b *boot }

func (r customersInboxResolver) Exists(ref string) bool {
	source, id, ok := strings.Cut(customersRefID(ref, customers.RefInbox), ":")
	if !ok || id == "" {
		return false
	}
	svc := r.b.inboxSvc
	if svc == nil {
		return true
	}
	switch source {
	case inbox.UploadType:
		up := svc.Uploads()
		if up == nil || up.Store == nil {
			return true
		}
		_, err := up.Store.Get(id)
		return err == nil
	case inbox.EmailType:
		em := svc.Email()
		if em == nil {
			return true
		}
		f, meta, err := em.Open(id)
		if err != nil {
			return false
		}
		_ = f.Close()
		// Mail that lost its approval is treated as gone.
		return meta.Approved
	}
	return true
}

func (r customersInboxResolver) Visible(_ string, v customers.Viewer) bool { return v.Full }

// customersAuditSightings is step D: what a dashboard user did, as one edge
// per person per day. The audit log carries the user and the day; the
// action's detail is never read, so nothing a person typed is copied.
// Pseudo-users such as the system and an unproxied local session match no
// customer and are dropped by the store.
func customersAuditSightings(src customersSources) []customers.Sighting {
	type key struct{ user, pred, day string }
	seen := map[key]bool{}
	var out []customers.Sighting
	for _, e := range src.audit {
		user := strings.TrimSpace(e.User)
		if user == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		day := at.UTC().Format(customersAuditDayLayout)
		pred := customers.PredActed
		if strings.Contains(strings.ToLower(e.Action), customersLoginActionMarker) {
			pred = customers.PredLoggedIn
		}
		k := key{strings.ToLower(user), pred, day}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, customersSighting(customers.KindHiveUser, user, pred, customers.AuditRef(day), at, customersSourceAudit))
	}
	return out
}

// customersAuditResolver: an audit day exists while it is inside the audit
// log's retention. An audit edge is for the full view only: it says when
// someone worked.
type customersAuditResolver struct{ now func() time.Time }

func (r customersAuditResolver) Exists(ref string) bool {
	day, err := time.Parse(customersAuditDayLayout, customersRefID(ref, customers.RefAudit))
	if err != nil {
		return false
	}
	retention := time.Duration(compliance.BuiltinAuditRetentionDays) * 24 * time.Hour
	return r.now().Sub(day) <= retention+24*time.Hour
}

func (r customersAuditResolver) Visible(_ string, v customers.Viewer) bool { return v.Full }
