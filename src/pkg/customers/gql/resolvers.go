package gql

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	graphql "github.com/graph-gophers/graphql-go"

	"github.com/hivecommons/hive/pkg/customers"
)

// query is the root resolver.
type query struct {
	e *Engine
}

// page clamps a requested page size to the engine's limits.
func (e *Engine) page(first *int32) int {
	n := defaultPage
	if first != nil {
		n = int(*first)
	}
	if n <= 0 {
		n = defaultPage
	}
	if n > e.limits.MaxPage {
		n = e.limits.MaxPage
	}
	return n
}

// hidden reports whether err means "not there for this viewer", which a
// nullable field answers with null rather than an error.
func hidden(err error) bool {
	return errors.Is(err, customers.ErrNotFound) || errors.Is(err, customers.ErrForbidden)
}

// Me is the viewer's own profile.
func (q *query) Me(ctx context.Context) (*personResolver, error) {
	req := requestFrom(ctx)
	p, err := q.e.svc.Get(req.viewer, customers.MeID)
	if err != nil {
		return nil, err
	}
	if err := req.take(1); err != nil {
		return nil, err
	}
	return &personResolver{e: q.e, p: p}, nil
}

// Person is one profile, or null.
func (q *query) Person(ctx context.Context, args struct{ ID graphql.ID }) (*personResolver, error) {
	req := requestFrom(ctx)
	p, err := q.e.svc.Get(req.viewer, string(args.ID))
	if err != nil {
		if hidden(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := req.take(1); err != nil {
		return nil, err
	}
	return &personResolver{e: q.e, p: p}, nil
}

// People pages through the customers the viewer may see.
func (q *query) People(ctx context.Context, args struct {
	Tag   *string
	First *int32
	After *string
}) (*personConnection, error) {
	req := requestFrom(ctx)
	list, err := q.e.svc.List(req.viewer)
	if err != nil {
		return nil, err
	}
	all := append([]customers.PersonView{list.Me}, list.People...)
	if args.Tag != nil && strings.TrimSpace(*args.Tag) != "" {
		tag := strings.ToLower(strings.TrimSpace(*args.Tag))
		kept := all[:0:0]
		for _, p := range all {
			if p.HasTag(tag) {
				kept = append(kept, p)
			}
		}
		all = kept
	}
	start := 0
	if args.After != nil && *args.After != "" {
		n, err := strconv.Atoi(*args.After)
		if err != nil || n < 0 {
			return nil, errors.New("bad cursor")
		}
		start = n
	}
	if start > len(all) {
		start = len(all)
	}
	end := start + q.e.page(args.First)
	if end > len(all) {
		end = len(all)
	}
	if err := req.take(end - start); err != nil {
		return nil, err
	}
	conn := &personConnection{}
	for _, p := range all[start:end] {
		conn.nodes = append(conn.nodes, &personResolver{e: q.e, p: p})
	}
	if end < len(all) {
		next := strconv.Itoa(end)
		conn.next = &next
	}
	return conn, nil
}

// Artefact is an artefact by reference, or null.
func (q *query) Artefact(ctx context.Context, args struct{ Ref string }) (*artefactResolver, error) {
	req := requestFrom(ctx)
	if !q.e.svc.ArtefactVisible(req.viewer, args.Ref) {
		return nil, nil
	}
	if err := req.take(1); err != nil {
		return nil, err
	}
	return &artefactResolver{e: q.e, ref: args.Ref}, nil
}

type personConnection struct {
	nodes []*personResolver
	next  *string
}

func (c *personConnection) Nodes() []*personResolver {
	if c.nodes == nil {
		return []*personResolver{}
	}
	return c.nodes
}

func (c *personConnection) Next() *string { return c.next }

// personResolver is a profile as the service already cut it for the viewer.
type personResolver struct {
	e *Engine
	p customers.PersonView
}

func (r *personResolver) ID() graphql.ID { return graphql.ID(r.p.ID) }
func (r *personResolver) Name() string   { return r.p.Name }
func (r *personResolver) Origin() string { return string(r.p.Origin) }
func (r *personResolver) Me() bool       { return r.p.Me }

func (r *personResolver) Tags() []*tagResolver {
	out := make([]*tagResolver, 0, len(r.p.Tags))
	for _, t := range r.p.Tags {
		out = append(out, &tagResolver{t: t})
	}
	return out
}

func (r *personResolver) Identities() []*identityResolver {
	out := make([]*identityResolver, 0, len(r.p.Identities))
	for _, i := range r.p.Identities {
		out = append(out, &identityResolver{i: i})
	}
	return out
}

func (r *personResolver) Goals() []*goalResolver {
	out := make([]*goalResolver, 0, len(r.p.Goals))
	for _, g := range r.p.Goals {
		out = append(out, &goalResolver{g: g})
	}
	return out
}

// Satisfaction is null when the service withheld it (no full view).
func (r *personResolver) Satisfaction() *satisfactionResolver {
	if r.p.Satisfaction.Level == "" {
		return nil
	}
	return &satisfactionResolver{s: r.p.Satisfaction}
}

// Notes is null when the service withheld them or there are none.
func (r *personResolver) Notes() *string {
	if r.p.Notes == "" {
		return nil
	}
	notes := r.p.Notes
	return &notes
}

func (r *personResolver) Counts() []*countResolver {
	keys := make([]string, 0, len(r.p.Counts))
	for k := range r.p.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*countResolver, 0, len(keys))
	for _, k := range keys {
		out = append(out, &countResolver{predicate: k, count: int32(r.p.Counts[k])})
	}
	return out
}

// Edges pages through the person's edges the viewer may see, newest first.
func (r *personResolver) Edges(ctx context.Context, args struct {
	Predicate *string
	First     *int32
	After     *string
}) (*edgeConnection, error) {
	req := requestFrom(ctx)
	predicate, after := "", ""
	if args.Predicate != nil {
		predicate = *args.Predicate
	}
	if args.After != nil {
		after = *args.After
	}
	edges, next, err := r.e.svc.Edges(req.viewer, r.p.ID, predicate, after, r.e.page(args.First))
	if err != nil {
		if hidden(err) {
			return &edgeConnection{}, nil
		}
		return nil, err
	}
	if err := req.take(len(edges)); err != nil {
		return nil, err
	}
	conn := &edgeConnection{}
	for _, e := range edges {
		conn.nodes = append(conn.nodes, &edgeResolver{e: r.e, edge: e})
	}
	if next != "" {
		conn.next = &next
	}
	return conn, nil
}

type tagResolver struct{ t customers.Tag }

func (r *tagResolver) Name() string   { return r.t.Name }
func (r *tagResolver) On() bool       { return r.t.State == customers.TagOn }
func (r *tagResolver) Source() string { return string(r.t.Source) }
func (r *tagResolver) Rule() *string  { return optional(r.t.Rule) }

type identityResolver struct{ i customers.Identity }

func (r *identityResolver) Kind() string    { return string(r.i.Kind) }
func (r *identityResolver) Value() string   { return r.i.Value }
func (r *identityResolver) Source() *string { return optional(r.i.Source) }
func (r *identityResolver) Manual() bool    { return r.i.Manual }
func (r *identityResolver) Claimed() bool   { return r.i.Claimed }

type goalResolver struct{ g customers.Goal }

func (r *goalResolver) ID() graphql.ID     { return graphql.ID(r.g.ID) }
func (r *goalResolver) Text() string       { return r.g.Text }
func (r *goalResolver) State() string      { return r.g.State }
func (r *goalResolver) Source() string     { return string(r.g.Source) }
func (r *goalResolver) Evidence() []string { return list(r.g.Evidence) }

type satisfactionResolver struct{ s customers.Satisfaction }

func (r *satisfactionResolver) Level() string      { return string(r.s.Level) }
func (r *satisfactionResolver) Source() *string    { return optional(string(r.s.Source)) }
func (r *satisfactionResolver) Evidence() []string { return list(r.s.Evidence) }

func (r *satisfactionResolver) SuggestedLevel() *string {
	if r.s.Suggestion == nil {
		return nil
	}
	return optional(string(r.s.Suggestion.Level))
}

func (r *satisfactionResolver) SuggestedEvidence() []string {
	if r.s.Suggestion == nil {
		return []string{}
	}
	return list(r.s.Suggestion.Evidence)
}

type countResolver struct {
	predicate string
	count     int32
}

func (r *countResolver) Predicate() string { return r.predicate }
func (r *countResolver) Count() int32      { return r.count }

type edgeConnection struct {
	nodes []*edgeResolver
	next  *string
}

func (c *edgeConnection) Nodes() []*edgeResolver {
	if c.nodes == nil {
		return []*edgeResolver{}
	}
	return c.nodes
}

func (c *edgeConnection) Next() *string { return c.next }

type edgeResolver struct {
	e    *Engine
	edge customers.Edge
}

func (r *edgeResolver) Predicate() string { return r.edge.Predicate }
func (r *edgeResolver) At() string        { return r.edge.At.UTC().Format(time.RFC3339) }
func (r *edgeResolver) Source() *string   { return optional(r.edge.Source) }

func (r *edgeResolver) Artefact() *artefactResolver {
	return &artefactResolver{e: r.e, ref: r.edge.Artefact}
}

// artefactResolver is a reference. It never carries content.
type artefactResolver struct {
	e   *Engine
	ref string
}

func (r *artefactResolver) Ref() string  { return r.ref }
func (r *artefactResolver) Kind() string { return customers.RefKind(r.ref) }

// People walks back from the artefact to the people on it that the viewer
// may see. For a viewer without the full view that is at most themselves.
func (r *artefactResolver) People(ctx context.Context, args struct{ First *int32 }) ([]*artefactPersonResolver, error) {
	req := requestFrom(ctx)
	people, err := r.e.svc.ArtefactPeople(req.viewer, r.ref, r.e.page(args.First))
	if err != nil {
		if hidden(err) {
			return []*artefactPersonResolver{}, nil
		}
		return nil, err
	}
	if err := req.take(len(people)); err != nil {
		return nil, err
	}
	out := make([]*artefactPersonResolver, 0, len(people))
	for _, p := range people {
		out = append(out, &artefactPersonResolver{predicate: p.Predicate, person: &personResolver{e: r.e, p: p.Person}})
	}
	return out, nil
}

type artefactPersonResolver struct {
	predicate string
	person    *personResolver
}

func (r *artefactPersonResolver) Predicate() string       { return r.predicate }
func (r *artefactPersonResolver) Person() *personResolver { return r.person }

// optional turns an empty string into null.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// list turns a nil slice into an empty list, which a non-null list field
// needs.
func list(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
