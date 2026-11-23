package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/github"
)

// Customers step E (hivecommons/hive#18884): the budgeted GitHub mention
// search that runs when a person accepts a suggested GitHub login. It is
// reached only from customersDashboard.Accept and AcceptInto. The discovery
// sweep (customers_sweep.go) is never handed this, nor a GitHub client.

// customersMentionTimeout bounds one accept's background search, including
// any wait for a pacing slot.
const customersMentionTimeout = 45 * time.Second

// customersMentionSearcher adapts the shared GitHub client to
// customers.MentionSearcher. The client is read at call time so a credential
// rebuild is followed.
type customersMentionSearcher struct {
	client func() *github.Client
}

func (s customersMentionSearcher) SearchMentions(ctx context.Context, kind, login string, repos []string, limit int) ([]customers.MentionHit, error) {
	c := s.client()
	if c == nil {
		return nil, github.ErrNoGitHubClient
	}
	ghKind := github.MentionSearchKindMentions
	if kind == customers.MentionKindCommenter {
		ghKind = github.MentionSearchKindCommenter
	}
	hits, err := c.SearchMentions(ctx, ghKind, login, repos, limit)
	if err != nil {
		if errors.Is(err, github.ErrMentionSearchRateLimited) {
			return nil, customers.ErrMentionQuotaLow
		}
		return nil, err
	}
	out := make([]customers.MentionHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, customers.MentionHit{Repo: h.Repo, Number: h.Number, IsPR: h.IsPR, At: h.UpdatedAt})
	}
	return out, nil
}

// newCustomersMentionSearch builds the accept-time search over the people
// store's index. cfg and client are read on every call.
func newCustomersMentionSearch(svc *customers.Service, cfg func() *config.Config, client func() *github.Client) *customers.MentionSearch {
	m := customers.NewMentionSearch(customersMentionSearcher{client: client}, svc.Index())
	m.Enabled = func() bool { return cfg().CustomersMentionSearchEnabled() }
	m.HourlyLimit = func() int {
		if c := cfg(); c != nil {
			return c.Customers.MentionSearch.RequestsPerHour()
		}
		return config.DefaultCustomersMentionSearchMaxPerHour
	}
	m.Repos = func() []string {
		if c := cfg(); c != nil {
			return append([]string(nil), c.Project.Repos...)
		}
		return nil
	}
	m.QuotaLow = func() string { return client().OptionalSearchStandDown() }
	return m
}

// suggestedIdentity returns the identity of a suggestion before an accept
// consumes it.
func (c *customersDashboard) suggestedIdentity(suggestionID string) (customers.Identity, bool) {
	for _, sg := range c.svc.Store().Suggestions() {
		if sg.ID == suggestionID {
			return customers.Identity{Kind: sg.Kind, Value: sg.Value}, true
		}
	}
	return customers.Identity{}, false
}

// searchOnAccept runs the budgeted search for the identity just accepted. It
// never delays or fails the accept: it runs in the background and logs the
// person id and counts only (never the login, a query or a token).
func (c *customersDashboard) searchOnAccept(person string, ident customers.Identity, found bool) {
	if c.mentions == nil || !found || person == "" {
		return
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), customersMentionTimeout)
		defer cancel()
		// The profile fields are filled by the profile fill
		// (customers.profile_fill, #19302), not by this search.
		res := c.mentions.Run(ctx, person, ident)
		if c.logger == nil || res.Skipped == customers.SkipDisabled {
			return
		}
		c.logger.Info("[customers] accept-time github search", slog.String("person", res.Person),
			slog.Int("requests", res.Requests), slog.Int("found", res.Found), slog.Int("edges", res.Edges),
			slog.Bool("truncated", res.Truncated), slog.String("skipped", res.Skipped))
	}
	if c.spawn != nil {
		c.spawn(run)
		return
	}
	go run()
}

// customersMentionConfig is the config the accept-time search reads on every
// call: the immutable snapshot the governor publishes after each reload,
// falling back to the boot config before the first one.
func (b *boot) customersMentionConfig() *config.Config {
	if cfg := b.sweepConfig.Load(); cfg != nil {
		return cfg
	}
	return b.cfg
}
