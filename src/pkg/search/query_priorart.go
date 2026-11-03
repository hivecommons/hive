package search

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/priorart"
)

// Prior-art gate source over the portfolio index (#14639, RFC #14491
// Decision 4: lexical only). Past review verdicts, findings, sentinel hits
// and beads become candidates the gate ranks like every other source; the
// gate keeps the thresholds and the park. Knowledge facts are left to the
// gate's own knowledge source, complexity snapshots are not prior art.
const (
	// PriorArtSourceName is the gate source's name.
	PriorArtSourceName = "search"
	// PriorArtTimeout bounds one query so a slow index never eats the
	// gate's budget for the sources after it.
	PriorArtTimeout = 5 * time.Second
	// priorArtTerms is how many top plain terms join the identifiers.
	priorArtTerms = 4
	// priorArtLimit caps the candidates handed to the ranker.
	priorArtLimit = 10
)

var priorArtKinds = []string{KindEvidence, KindFinding, KindSentinel, KindBead}

// PriorArtSource returns the gate's "search" source. backend is resolved per
// search, so the index can come up after the gate is wired; a nil backend
// answers nothing. Query errors and timeouts are returned as the source's
// error, which the gate counts and otherwise ignores.
func PriorArtSource(backend func() Backend) priorart.Source {
	return priorart.SourceFunc{SourceName: PriorArtSourceName, Fn: func(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
		return priorArtSearch(ctx, backend, q, PriorArtTimeout)
	}}
}

func priorArtSearch(ctx context.Context, backend func() Backend, q priorart.Query, timeout time.Duration) ([]priorart.Candidate, error) {
	if backend == nil {
		return nil, nil
	}
	b := backend()
	if b == nil {
		return nil, nil
	}
	terms := append([]string(nil), q.Identifiers...)
	for _, t := range q.TopTerms(priorArtTerms) {
		// Query terms are singularized; the index keeps words verbatim.
		terms = append(terms, t, t+"s")
	}
	text := strings.TrimSpace(strings.Join(terms, " "))
	if text == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type answer struct {
		res Result
		err error
	}
	// The index is portfolio-wide and its documents are private to the
	// spoke, while the gate comments on one repository's issue. Scoping the
	// query to that repository keeps another repository's review output out
	// of the comment, and keeps "#N" from naming a different repository's PR.
	filters := map[string][]string{FieldKind: priorArtKinds}
	if repo := strings.TrimSpace(q.Repo); repo != "" {
		filters[FieldRepo] = []string{repo}
	}
	done := make(chan answer, 1)
	go func() {
		res, err := b.Query(ctx, Query{Text: text, Filters: filters, Limit: priorArtLimit})
		done <- answer{res, err}
	}()
	var res Result
	select {
	case a := <-done:
		if a.err != nil {
			return nil, a.err
		}
		res = a.res
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]priorart.Candidate, 0, len(res.Hits))
	for _, h := range res.Hits {
		d := h.Document
		if beadOfIssue(d, q) {
			continue
		}
		title := d.Title
		if strings.TrimSpace(title) == "" {
			title = d.Repo + ": " + d.Kind
		}
		// Index text is past agent and reviewer output; it reaches the
		// gate's comments and agent prompts, so it is scanned like any
		// other untrusted input. A blocked document is dropped: its
		// matched terms would still rank a redaction marker.
		title, tv := ioscan.EnforceInput(title)
		body, bv := ioscan.EnforceInput(d.Text)
		if tv.Blocked || bv.Blocked {
			continue
		}
		out = append(out, priorart.Candidate{
			Kind:  priorart.KindReview,
			Title: title,
			Text:  body,
			Terms: append([]string(nil), h.Terms...),
			URL:   d.URL,
			Ref:   priorArtRef(d),
		})
	}
	return out, nil
}

// beadOfIssue reports whether d is a bead filed against the gated issue: an
// epic or advisory whose external_ref names it (`owner/repo#N`, the
// planning `gh-owner/repo#N` form, or the issue URL). Such a bead echoes the
// issue's own title back and would rank ~100% against it. Beads with an
// `owner/repo#N` ref already carry the issue URL, which the gate's own check
// catches; the other forms fall back to a `bead://` URL and are caught here.
func beadOfIssue(d Document, q priorart.Query) bool {
	if q.Number <= 0 || d.Kind != KindBead {
		return false
	}
	ref := strings.TrimSpace(d.Fields["external_ref"])
	if ref == "" {
		return false
	}
	ref = strings.TrimPrefix(ref, "gh-")
	n := strconv.Itoa(q.Number)
	if i := strings.Index(ref, "://"); i >= 0 {
		// Issue URL: the path must name the gated repo when it is known.
		path := strings.TrimSuffix(ref[i+3:], "/")
		for _, seg := range []string{"/issues/", "/pull/"} {
			if repo, ok := strings.CutSuffix(path, seg+n); ok {
				_, repo, _ = strings.Cut(repo, "/")
				return q.Repo == "" || strings.EqualFold(repo, q.Repo)
			}
		}
		return false
	}
	repo, num, ok := strings.Cut(ref, "#")
	if !ok {
		// Bare "gh-7" style refs name an issue in the bead's own repo.
		return ref == n && (q.Repo == "" || strings.EqualFold(d.Repo, q.Repo))
	}
	if num != n {
		return false
	}
	return repo == "" || q.Repo == "" || strings.EqualFold(repo, q.Repo)
}

// priorArtRef is the candidate's short human handle: "#<pr>" for review
// evidence, findings and sentinel hits (the ingester records the PR number
// in the "number" field), the bead's own ID for beads, else the document ID.
func priorArtRef(d Document) string {
	if n := d.Fields["number"]; n != "" {
		return "#" + n
	}
	if d.Kind == KindBead {
		if rest, ok := strings.CutPrefix(d.ID, KindBead+"/"); ok {
			if _, id, ok := strings.Cut(rest, "/"); ok && id != "" {
				return id
			}
		}
	}
	return d.ID
}
