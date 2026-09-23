package publish

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/convergence/outcome"
)

// Routes a prediction assigns to each finding.
const (
	// RouteIssue: one public issue is expected for the finding.
	RouteIssue = "issue"
	// RoutePrivate: one private disclosure is expected for the finding.
	RoutePrivate = "private"
	// RouteNone: nothing is expected (duplicate, rejected, or invalid).
	RouteNone = "none"
)

// OutcomeProject is the hive-instance scope every campaign outcome is
// declared under; the outcome ledger's positive control uses one fixed value.
const OutcomeProject = "default"

// outcomeSlugPrefix keeps campaign outcomes distinguishable from any other
// outcome declared on the same repository.
const outcomeSlugPrefix = "audit-"

// outcomeSlugInvalid matches every character the outcome slug pattern
// forbids, so a campaign key of any spelling maps to one stable slug.
var outcomeSlugInvalid = regexp.MustCompile(`[^a-z0-9._-]+`)

// Predicted is one finding's predicted publication.
type Predicted struct {
	ContentHash string
	BeadID      string
	Route       string
	DuplicateOf string
}

// Prediction is the campaign's predicted repository end state: one issue per
// validated public finding, one private disclosure per validated sensitive
// finding, nothing for duplicates and rejections. It replaces the campaign's
// ad-hoc counting: the outcome ledger records this prediction as the desired
// generation and Satisfied checks the actual publications against it.
type Prediction struct {
	Campaign string
	Repo     string
	Findings []Predicted
}

// Plan aggregates the campaign's findings into a Prediction. Findings are
// sorted by content hash so the same input always yields the same Spec.
func Plan(campaign, repo string, findings []Finding, classify Classifier) Prediction {
	if classify == nil {
		classify = ConservativeClassifier
	}
	pred := Prediction{Campaign: campaign, Repo: repo}
	for _, f := range findings {
		entry := Predicted{ContentHash: f.ContentHash, BeadID: f.BeadID, Route: RouteNone, DuplicateOf: f.DuplicateOf}
		if f.Validate() == nil {
			if classify(f) == SensitivitySensitive {
				entry.Route = RoutePrivate
			} else {
				entry.Route = RouteIssue
			}
		}
		pred.Findings = append(pred.Findings, entry)
	}
	sort.Slice(pred.Findings, func(i, j int) bool { return pred.Findings[i].ContentHash < pred.Findings[j].ContentHash })
	return pred
}

// Expected counts the findings the prediction expects to publish.
func (p Prediction) Expected() int {
	n := 0
	for _, f := range p.Findings {
		if f.Route != RouteNone {
			n++
		}
	}
	return n
}

// Spec renders the prediction as the outcome ledger's desired-state text:
// one line per finding, deterministic, so an unchanged prediction never
// supersedes the current generation.
func (p Prediction) Spec() string {
	var b strings.Builder
	b.WriteString("campaign " + p.Campaign + " in " + p.Repo + "\n")
	for _, f := range p.Findings {
		b.WriteString(f.Route + " " + f.ContentHash)
		if f.DuplicateOf != "" {
			b.WriteString(" duplicate_of=" + f.DuplicateOf)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Ref is the outcome identity of the campaign on its repository.
func (p Prediction) Ref() outcome.Ref {
	return outcome.Ref{Project: OutcomeProject, Repo: p.Repo, Outcome: OutcomeSlug(p.Campaign)}
}

// Satisfied reports whether the publications satisfy the prediction: every
// finding routed to an issue is published or existing, every finding routed
// privately is private, and nothing routed to none was published.
func (p Prediction) Satisfied(pubs map[string]Publication) bool {
	for _, f := range p.Findings {
		pub, ok := pubs[f.ContentHash]
		switch f.Route {
		case RouteIssue:
			if !ok || (pub.State != StatePublished && pub.State != StateExisting) {
				return false
			}
		case RoutePrivate:
			if !ok || pub.State != StatePrivate {
				return false
			}
		default:
			if ok && (pub.State == StatePublished || pub.State == StatePrivate) {
				return false
			}
		}
	}
	return true
}

// OutcomeSlug maps a campaign key to a stable outcome slug.
func OutcomeSlug(campaign string) string {
	slug := outcomeSlugInvalid.ReplaceAllString(strings.ToLower(strings.TrimSpace(campaign)), "-")
	slug = strings.Trim(slug, "._-")
	if slug == "" {
		slug = "campaign"
	}
	return outcomeSlugPrefix + slug
}

// Book records the prediction on the outcome ledger as the campaign's
// desired generation: Create on first sight, Supersede on expectedGeneration
// when the prediction changed, nothing when it is unchanged. A retired
// outcome is left alone. It returns the current record.
func Book(ledger *outcome.Ledger, pred Prediction, actor string) (outcome.Record, error) {
	if ledger == nil {
		return outcome.Record{}, fmt.Errorf("outcome ledger is required to book a prediction")
	}
	ref := pred.Ref()
	spec := pred.Spec()
	current, ok := ledger.Get(ref)
	if !ok {
		return ledger.Create(ref, spec, nil, actor)
	}
	if current.State == outcome.StateRetired || current.Spec == spec {
		return current, nil
	}
	return ledger.Supersede(ref, current.Generation, spec, nil, actor)
}

// Accept marks the CURRENT desired generation accepted once the publications
// satisfy the prediction. An already-accepted generation is left as is; an
// unsatisfied prediction records nothing.
func Accept(ledger *outcome.Ledger, pred Prediction, pubs map[string]Publication, actor string) (outcome.Record, bool, error) {
	if ledger == nil {
		return outcome.Record{}, false, fmt.Errorf("outcome ledger is required to accept a prediction")
	}
	current, ok := ledger.Get(pred.Ref())
	if !ok {
		return outcome.Record{}, false, fmt.Errorf("%s: %w", pred.Ref().Key(), outcome.ErrOutcomeNotFound)
	}
	if current.State != outcome.StateProposed || !pred.Satisfied(pubs) {
		return current, false, nil
	}
	rec, err := ledger.Accept(pred.Ref(), current.Generation, actor)
	if err != nil {
		return current, false, err
	}
	return rec, true, nil
}

// Retire terminally closes the campaign's outcome, for example when the
// campaign scope is withdrawn. Retiring an unknown or already retired
// outcome is a no-op.
func Retire(ledger *outcome.Ledger, pred Prediction, actor string) (outcome.Record, error) {
	if ledger == nil {
		return outcome.Record{}, fmt.Errorf("outcome ledger is required to retire a prediction")
	}
	current, ok := ledger.Get(pred.Ref())
	if !ok || current.State == outcome.StateRetired {
		return current, nil
	}
	rec, err := ledger.Retire(pred.Ref(), current.Generation, actor)
	if errors.Is(err, outcome.ErrOutcomeRetired) {
		return current, nil
	}
	return rec, err
}

// Campaign identifies one campaign run for PublishCampaign.
type Campaign struct {
	Key    string
	Repo   string
	RunKey string
	RunURL string
}

// CampaignResult is what PublishCampaign records.
type CampaignResult struct {
	Prediction   Prediction
	Publications map[string]Publication
	// Outcome is the campaign's outcome record after booking and, when the
	// prediction was satisfied, acceptance. Zero when no ledger is wired.
	Outcome outcome.Record
	// Accepted reports that this run accepted the current generation.
	Accepted bool
}

// PublishCampaign is the campaign-level aggregation: it books the predicted
// end state, publishes each expected finding under the campaign's grant, and
// accepts the outcome generation when the actual publications satisfy the
// prediction. Findings that are withheld or refused keep the generation
// proposed, so "issue filed" and "repository outcome satisfied" remain
// separate statuses.
func (p *Publisher) PublishCampaign(ctx context.Context, c Campaign, findings []Finding, grant Grant, ledger *outcome.Ledger) (CampaignResult, error) {
	pred := Plan(c.Key, c.Repo, findings, p.Classify)
	res := CampaignResult{Prediction: pred, Publications: map[string]Publication{}}
	if ledger != nil {
		rec, err := Book(ledger, pred, p.Actor)
		if err != nil {
			return res, err
		}
		res.Outcome = rec
	}
	var firstErr error
	for _, f := range findings {
		if f.Validate() != nil {
			res.Publications[f.ContentHash] = Publication{State: StateNone}
			continue
		}
		f.RunKey, f.RunURL = c.RunKey, c.RunURL
		pub, err := p.Publish(ctx, f, grant)
		res.Publications[f.ContentHash] = pub
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if ledger != nil {
		rec, accepted, err := Accept(ledger, pred, res.Publications, p.Actor)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		res.Outcome, res.Accepted = rec, accepted
	}
	return res, firstErr
}
