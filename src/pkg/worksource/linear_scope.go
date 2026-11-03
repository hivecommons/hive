package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
)

// LinearScopeContractMaxBytes caps the Linear issue text inlined into a
// review kick as its scope contract (hivecommons/hive#14906).
const LinearScopeContractMaxBytes = 10 << 10

// linearScopeIssueComments is how many of the newest comments are inlined.
const linearScopeIssueComments = 5

// linearScopeRecheck is how long a cached issue is reused without asking
// Linear whether it changed: the review planner runs every eval cycle, and
// one updatedAt probe per issue per cycle would spend the API budget for
// nothing.
const linearScopeRecheck = 5 * time.Minute

type linearScopeCacheEntry struct {
	issue     LinearScopeIssue
	checkedAt time.Time
}

const linearScopeIssueQuery = `query($id: String!) {
  issue(id: $id) {
    identifier
    title
    updatedAt
    description
    state { name }
    comments(last: 5) {
      nodes { body createdAt user { displayName } }
    }
  }
}`

const linearScopeIssueUpdatedQuery = `query($id: String!) {
  issue(id: $id) { updatedAt }
}`

// LinearScopeIssue is a Linear issue rendered for a review kick's scope
// contract: the identifier, its workflow state, and the issue text
// (identifier, title, state, description, last few comments) capped at
// LinearScopeContractMaxBytes.
type LinearScopeIssue struct {
	Identifier string
	State      string
	Text       string
	UpdatedAt  time.Time
}

// ScopeContractIssue reads the Linear issue identifier (e.g. "ONB-3872")
// read-only for a review kick. The text is cached per identifier and
// updatedAt: a repeat call within linearScopeRecheck asks Linear nothing,
// a later one asks only for updatedAt, and the text is refetched only when
// the issue changed.
func (s *LinearSource) ScopeContractIssue(ctx context.Context, identifier string) (LinearScopeIssue, error) {
	identifier = strings.ToUpper(strings.TrimSpace(identifier))
	if identifier == "" {
		return LinearScopeIssue{}, fmt.Errorf("linear: issue identifier is required")
	}
	now := time.Now()
	s.scopeMu.Lock()
	cached, ok := s.scopeIssues[identifier]
	s.scopeMu.Unlock()
	if ok && now.Sub(cached.checkedAt) < linearScopeRecheck {
		return cached.issue, nil
	}
	if ok {
		raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearScopeIssueUpdatedQuery, map[string]interface{}{"id": identifier})
		if err == nil {
			var resp struct {
				Data struct {
					Issue *struct {
						UpdatedAt time.Time `json:"updatedAt"`
					} `json:"issue"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &resp) == nil && resp.Data.Issue != nil && resp.Data.Issue.UpdatedAt.Equal(cached.issue.UpdatedAt) {
				s.storeScopeIssue(identifier, cached.issue, now)
				return cached.issue, nil
			}
		}
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearScopeIssueQuery, map[string]interface{}{"id": identifier})
	if err != nil {
		if ok {
			// The cached text is still the last known good scope; a Linear
			// outage must not turn it into a fetch failure. Refresh checkedAt
			// so the outage costs one probe+fetch per recheck interval.
			s.storeScopeIssue(identifier, cached.issue, now)
			return cached.issue, nil
		}
		return LinearScopeIssue{}, fmt.Errorf("linear: read %s: %w", identifier, err)
	}
	var resp struct {
		Data struct {
			Issue *struct {
				Identifier  string    `json:"identifier"`
				Title       string    `json:"title"`
				UpdatedAt   time.Time `json:"updatedAt"`
				Description string    `json:"description"`
				State       struct {
					Name string `json:"name"`
				} `json:"state"`
				Comments struct {
					Nodes []struct {
						Body      string    `json:"body"`
						CreatedAt time.Time `json:"createdAt"`
						User      *struct {
							DisplayName string `json:"displayName"`
						} `json:"user"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return LinearScopeIssue{}, fmt.Errorf("linear: read %s: decode response: %w", identifier, err)
	}
	n := resp.Data.Issue
	if n == nil {
		return LinearScopeIssue{}, fmt.Errorf("linear: issue %s not found", identifier)
	}
	if n.Identifier == "" {
		n.Identifier = identifier
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\nState: %s\n", n.Identifier, strings.TrimSpace(n.Title), n.State.Name)
	if d := strings.TrimSpace(n.Description); d != "" {
		fmt.Fprintf(&b, "\n%s\n", d)
	}
	comments := n.Comments.Nodes
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	if len(comments) > linearScopeIssueComments {
		comments = comments[len(comments)-linearScopeIssueComments:]
	}
	if len(comments) > 0 {
		b.WriteString("\nRecent comments:\n")
	}
	for _, c := range comments {
		who := "unknown"
		if c.User != nil && strings.TrimSpace(c.User.DisplayName) != "" {
			who = strings.TrimSpace(c.User.DisplayName)
		}
		fmt.Fprintf(&b, "\n--- %s (%s):\n%s\n", who, c.CreatedAt.UTC().Format("2006-01-02"), strings.TrimSpace(c.Body))
	}
	issue := LinearScopeIssue{
		Identifier: n.Identifier,
		State:      n.State.Name,
		Text:       capLinearScopeText(b.String(), LinearScopeContractMaxBytes),
		UpdatedAt:  n.UpdatedAt,
	}
	s.storeScopeIssue(identifier, issue, now)
	return issue, nil
}

func (s *LinearSource) storeScopeIssue(identifier string, issue LinearScopeIssue, checkedAt time.Time) {
	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	if s.scopeIssues == nil {
		s.scopeIssues = map[string]linearScopeCacheEntry{}
	}
	s.scopeIssues[identifier] = linearScopeCacheEntry{issue: issue, checkedAt: checkedAt}
}

// LinearScopeSource builds the Linear source the review planner reads scope
// contracts through: the primary Linear client from the work-source config
// only, never narrowed by assigned_only or wrapped in additive sources (the
// same narrowing as reviewBacklogWorkSource).
func LinearScopeSource(ws config.WorkSourceConfig, logger *slog.Logger) (*LinearSource, error) {
	ws.Type = "linear"
	ws.RunStages = false
	ws.Wavefront.Enabled = false
	ws.Linear.AssignedOnly = false
	src, err := FromConfig(ws, nil, "", "", logger)
	if err != nil {
		return nil, err
	}
	l, ok := src.(*LinearSource)
	if !ok {
		return nil, fmt.Errorf("work_source.linear did not build a Linear source")
	}
	return l, nil
}

// capLinearScopeText cuts text to at most limit bytes on a rune boundary and
// says so, so the reviewer knows the issue goes on past what it was shown.
func capLinearScopeText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], "\n") + fmt.Sprintf("\n\n[truncated: the issue text exceeds %d bytes; acceptance criteria past this point were not shown]\n", limit)
}
