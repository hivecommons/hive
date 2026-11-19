package worksource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/prosefilter"
)

// LinearAdvisoryMarker is the stable footer every hive-authored digest comment
// on Linear carries. It is how the poster finds its own comment on the next
// cycle (the same idea as the pinned GitHub comment's fixed prefix), so the
// digest stays ONE comment rewritten in place instead of a new comment per
// governor eval. Kept visible rather than an HTML comment because Linear's
// markdown renderer does not reliably preserve HTML comments.
const LinearAdvisoryMarker = "_Hive advisory digest (hive-advisory-digest) - rewritten every governor cycle._"

// legacyLinearAdvisoryMarker is the footer digests carried before outbound
// text lost its em-dashes (hivecommons/hive#19103); a comment carrying it is
// still the hive's digest and is updated in place.
const legacyLinearAdvisoryMarker = "_Hive advisory digest (hive-advisory-digest) \u2014 rewritten every governor cycle._"

// ErrLinearAdvisoryIssueUnset is returned when the Linear target is selected
// but governor.advisory.linear_issue is empty. It names the missing key so
// the log line an operator sees is actionable, and it is a hard error rather
// than a GitHub fallback: an operator who chose Linear must never find the
// digest silently living somewhere else.
var ErrLinearAdvisoryIssueUnset = errors.New("governor.advisory.linear_issue is required when governor.advisory.target is linear — digest not posted")

// ErrLinearAdvisoryAPIKeyUnset is returned when no Linear credential is
// available to post with. The poster reuses the work source's key
// (governor.work_source.linear.api_key) rather than introducing a second one.
var ErrLinearAdvisoryAPIKeyUnset = errors.New("governor.work_source.linear.api_key is required to post the advisory digest to Linear — digest not posted")

// LinearAdvisoryPoster maintains the advisory digest as a single comment on a
// designated Linear issue.
type LinearAdvisoryPoster struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewLinearAdvisoryPoster builds a poster that authenticates exactly like
// LinearSource (bare API key in Authorization). baseURL may be empty for
// production; tests point it at an httptest server.
func NewLinearAdvisoryPoster(apiKey, baseURL string, httpClient *http.Client) *LinearAdvisoryPoster {
	return &LinearAdvisoryPoster{apiKey: apiKey, baseURL: baseURL, client: httpClient}
}

const linearAdvisoryIssueQuery = `query($id: String!) {
  issue(id: $id) {
    id
    identifier
    comments(first: 100) {
      nodes { id body }
    }
  }
}`

const linearAdvisoryCommentCreate = `mutation($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id }
  }
}`

const linearAdvisoryCommentUpdate = `mutation($id: String!, $body: String!) {
  commentUpdate(id: $id, input: { body: $body }) {
    success
  }
}`

// PostDigest writes digest as the hive's one advisory comment on the Linear
// issue named by identifier (e.g. "ONB-123"): it updates the existing comment
// carrying LinearAdvisoryMarker when there is one and creates it otherwise.
// Fails closed — an empty identifier or API key is an error, never a reason
// to post elsewhere.
func (p *LinearAdvisoryPoster) PostDigest(ctx context.Context, identifier, digest string) error {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return ErrLinearAdvisoryIssueUnset
	}
	if p == nil || strings.TrimSpace(p.apiKey) == "" {
		return ErrLinearAdvisoryAPIKeyUnset
	}
	// Filter before the compare below so the skip-if-unchanged guard sees
	// the text Linear will actually hold (hivecommons/hive#19103).
	body := prosefilter.Sanitize(strings.TrimRight(digest, "\n") + "\n\n" + LinearAdvisoryMarker)

	raw, err := linearGraphQL(ctx, p.client, p.baseURL, p.apiKey, linearAdvisoryIssueQuery, map[string]interface{}{"id": identifier})
	if err != nil {
		return fmt.Errorf("lookup linear issue %s: %w", identifier, err)
	}
	var lookup struct {
		Data struct {
			Issue *struct {
				ID       string `json:"id"`
				Comments struct {
					Nodes []struct {
						ID   string `json:"id"`
						Body string `json:"body"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &lookup); err != nil {
		return fmt.Errorf("decode linear issue %s: %w", identifier, err)
	}
	if lookup.Data.Issue == nil || lookup.Data.Issue.ID == "" {
		return fmt.Errorf("linear issue %s not found (check governor.advisory.linear_issue and the api key's workspace)", identifier)
	}

	for _, c := range lookup.Data.Issue.Comments.Nodes {
		if !strings.Contains(c.Body, LinearAdvisoryMarker) && !strings.Contains(c.Body, legacyLinearAdvisoryMarker) {
			continue
		}
		if c.Body == body {
			// Byte-identical: skip the write, mirroring the GitHub poster's
			// skip-if-unchanged guard so a quiet digest costs no API writes.
			return nil
		}
		raw, err = linearGraphQL(ctx, p.client, p.baseURL, p.apiKey, linearAdvisoryCommentUpdate, map[string]interface{}{"id": c.ID, "body": body})
		if err != nil {
			return fmt.Errorf("update linear advisory comment on %s: %w", identifier, err)
		}
		var upd struct {
			Data struct {
				CommentUpdate struct {
					Success bool `json:"success"`
				} `json:"commentUpdate"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &upd); err != nil {
			return fmt.Errorf("decode commentUpdate on %s: %w", identifier, err)
		}
		if !upd.Data.CommentUpdate.Success {
			return fmt.Errorf("commentUpdate on %s reported success=false", identifier)
		}
		return nil
	}

	raw, err = linearGraphQL(ctx, p.client, p.baseURL, p.apiKey, linearAdvisoryCommentCreate, map[string]interface{}{"issueId": lookup.Data.Issue.ID, "body": body})
	if err != nil {
		return fmt.Errorf("create linear advisory comment on %s: %w", identifier, err)
	}
	var cre struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &cre); err != nil {
		return fmt.Errorf("decode commentCreate on %s: %w", identifier, err)
	}
	if !cre.Data.CommentCreate.Success {
		return fmt.Errorf("commentCreate on %s reported success=false", identifier)
	}
	return nil
}
