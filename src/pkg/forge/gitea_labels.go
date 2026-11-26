package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// Label application on Gitea and Forgejo (hivecommons/hive#19910).
//
// POST /repos/{owner}/{repo}/issues/{index}/labels answers HTTP 200 for a
// label name the repo does not have and applies nothing: the response is the
// issue's label list, without the missing name. GitHub instead creates a
// missing label on the same call. So AddLabels reads the returned list, and
// for every requested name missing from it it finds the repo label of that
// name (case-insensitively, as GitHub matches) or creates it, applies it by
// id, and returns ErrLabelsMissing naming any label that still did not land.
// A gate label (ready-for-review, decision-needed) is created with the
// colour and description EnsureHiveGateLabels uses on GitHub.

// ErrLabelsMissing is returned, wrapped, when the forge accepted an
// add-labels call but the issue does not carry every requested label
// afterwards.
var ErrLabelsMissing = errors.New("forge: labels were not applied")

// giteaDefaultLabelColor is the colour of a non-gate label the adapter
// creates, GitHub's colour for a label it creates on an add-labels call.
const giteaDefaultLabelColor = "ededed"

// gtLabelRef is a repo label with its id, as the label list and create calls
// return it.
type gtLabelRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AddLabels adds labels to an issue or pull request by name and verifies that
// each one is on it afterwards, creating a label the repo lacks. It is a
// no-op when labels is empty.
func (f *giteaForge) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	labels = nonBlankLabels(labels)
	if len(labels) == 0 {
		return nil
	}
	owner, name := f.ownerRepo(repo)
	slug := fmt.Sprintf("%s/%s#%d", owner, name, number)
	// `lgtm` / `approved` approves a PR for merging: an open change request
	// from a writer refuses it (#20067). Other labels pass straight through.
	if approvesForMerge(labels) {
		if err := f.gateChangesRequested(ctx, owner, name, number, "label"); err != nil {
			return fmt.Errorf("%s: add labels to %s: %w", f.label, slug, err)
		}
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", url.PathEscape(owner), url.PathEscape(name), number)

	applied, err := f.postIssueLabels(ctx, endpoint, labels)
	if err != nil {
		return fmt.Errorf("%s: add labels to %s: %w", f.label, slug, err)
	}
	missing := labelsMissingFrom(labels, applied)
	if len(missing) == 0 {
		return nil
	}

	ids, err := f.repoLabelIDs(ctx, owner, name, missing)
	if err != nil {
		// The underlying error keeps its status, so a 5xx or 429 while
		// listing or creating labels is retried like any other.
		return fmt.Errorf("%s: add labels to %s: applying %s: %w", f.label, slug, strings.Join(missing, ", "), err)
	}
	refs := make([]any, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, id)
	}
	applied, err = f.postIssueLabels(ctx, endpoint, refs)
	if err != nil {
		return fmt.Errorf("%s: add labels to %s: %w", f.label, slug, err)
	}
	if still := labelsMissingFrom(missing, applied); len(still) > 0 {
		return fmt.Errorf("%s: add labels to %s: %w: %s", f.label, slug, ErrLabelsMissing, strings.Join(still, ", "))
	}
	return nil
}

// postIssueLabels sends one add-labels call (names or ids) and returns the
// names of the labels the issue carries afterwards.
func (f *giteaForge) postIssueLabels(ctx context.Context, endpoint string, labels any) ([]string, error) {
	var out []gtLabel
	if err := f.doWriteJSON(ctx, http.MethodPost, endpoint, map[string]any{"labels": labels}, &out); err != nil {
		return nil, err
	}
	return labelNames(out), nil
}

// repoLabelIDs returns the id of each named repo label, creating the ones the
// repo lacks.
func (f *giteaForge) repoLabelIDs(ctx context.Context, owner, name string, labels []string) ([]int64, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/labels", url.PathEscape(owner), url.PathEscape(name))
	var existing []gtLabelRef
	err := f.paginate(ctx, endpoint, nil, func(body []byte) (int, error) {
		var page []gtLabelRef
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding labels: %w", err)
		}
		existing = append(existing, page...)
		return len(page), nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing labels: %w", err)
	}
	ids := make([]int64, 0, len(labels))
	for _, want := range labels {
		id, found := int64(0), false
		for _, l := range existing {
			if strings.EqualFold(strings.TrimSpace(l.Name), want) && l.ID > 0 {
				id, found = l.ID, true
				break
			}
		}
		if !found {
			created, err := f.createRepoLabel(ctx, endpoint, want)
			if err != nil {
				return nil, err
			}
			id = created
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// createRepoLabel creates one repo label and returns its id.
func (f *giteaForge) createRepoLabel(ctx context.Context, endpoint, label string) (int64, error) {
	color, desc, ok := github.HiveGateLabelDefinition(label)
	if !ok {
		color, desc = giteaDefaultLabelColor, ""
	}
	payload := map[string]string{"name": label, "color": "#" + color, "description": desc}
	var out gtLabelRef
	if err := f.doWriteJSON(ctx, http.MethodPost, endpoint, payload, &out); err != nil {
		return 0, fmt.Errorf("creating label %q: %w", label, err)
	}
	if out.ID <= 0 {
		return 0, fmt.Errorf("creating label %q: the forge returned no label id", label)
	}
	return out.ID, nil
}

// labelsMissingFrom returns the labels in want that have is missing,
// compared case-insensitively.
func labelsMissingFrom(want, have []string) []string {
	var missing []string
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(strings.TrimSpace(h), w) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
}

// nonBlankLabels trims labels and drops blanks and case-insensitive
// duplicates, keeping the first spelling.
func nonBlankLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" || len(labelsMissingFrom([]string{l}, out)) == 0 {
			continue
		}
		out = append(out, l)
	}
	return out
}

// giteaStatusError is a non-2xx answer from the Gitea/Forgejo API. Its text
// is the adapter's usual "<label> API <endpoint> returned status <code>:
// <body>" so callers that read the status from the text keep working.
type giteaStatusError struct {
	label    string
	endpoint string
	status   int
	body     string
}

func (e *giteaStatusError) Error() string {
	return fmt.Sprintf("%s API %s returned status %d: %s", e.label, e.endpoint, e.status, e.body)
}

// isGiteaStatus reports whether err carries the HTTP status code.
func isGiteaStatus(err error, code int) bool {
	var se *giteaStatusError
	return errors.As(err, &se) && se.status == code
}
