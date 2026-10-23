package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/codeindex"
	"github.com/hivecommons/hive/pkg/priorart"
)

// Prior-art docs and code sources (hivecommons/hive#13878): a pkg/codeindex
// snapshot over the local checkouts of every registered repo replaces GitHub
// code search, so findings cover all repos and never spend the code-search
// quota. A repo whose checkout is missing, dirty or only partly indexed is
// reported as a priorart.CoverageError gap, never read as "no prior art".
const (
	// DefaultPriorArtCodeIndexTTL is how long a snapshot answers before a
	// background rebuild picks up synced checkouts.
	DefaultPriorArtCodeIndexTTL = 30 * time.Minute
	// priorArtCodeIndexBuildTimeout bounds one snapshot build.
	priorArtCodeIndexBuildTimeout = 10 * time.Minute
	// priorArtCodeIndexSearchLimit is the files asked of the index per
	// source before path filtering; priorArtCodeIndexPerSource caps what a
	// source returns to the ranker.
	priorArtCodeIndexSearchLimit = 40
	priorArtCodeIndexPerSource   = 10
)

// PriorArtCheckout is one registered repo ("owner/name") and its host-local
// checkout root; Root is "" when no checkout is configured.
type PriorArtCheckout struct {
	Repo string
	Root string
}

// PriorArtCodeIndex owns the codeindex snapshot behind the docs and code
// sources. The first search waits (up to its context) for the initial build;
// later searches answer from the current snapshot while a stale one is
// rebuilt in the background.
type PriorArtCodeIndex struct {
	checkouts func() []PriorArtCheckout
	ttl       time.Duration
	now       func() time.Time

	mu       sync.Mutex
	idx      *codeindex.Index
	gaps     []string
	builtAt  time.Time
	building chan struct{}
}

// NewPriorArtCodeIndex returns an index over checkouts, resolved at every
// build so config changes are picked up. ttl <= 0 means
// DefaultPriorArtCodeIndexTTL.
func NewPriorArtCodeIndex(checkouts func() []PriorArtCheckout, ttl time.Duration) *PriorArtCodeIndex {
	if ttl <= 0 {
		ttl = DefaultPriorArtCodeIndexTTL
	}
	return &PriorArtCodeIndex{checkouts: checkouts, ttl: ttl, now: time.Now}
}

// Sources returns the gate's "docs" (README, CHANGELOG, docs/) and "code"
// sources. Neither needs a GitHub client.
func (p *PriorArtCodeIndex) Sources() []priorart.Source {
	if p == nil {
		return nil
	}
	return []priorart.Source{
		priorart.SourceFunc{SourceName: "docs", Fn: p.searchDocs},
		priorart.SourceFunc{SourceName: "code", Fn: p.searchCode},
	}
}

// searchDocs asks for the identifiers and the top plain terms.
func (p *PriorArtCodeIndex) searchDocs(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
	terms := append([]string(nil), q.Identifiers...)
	for _, t := range q.TopTerms(priorArtSearchTerms) {
		// Query terms are singularized; the index keeps words verbatim.
		terms = append(terms, t, t+"s")
	}
	return p.search(ctx, terms, priorart.KindDoc, func(m codeindex.Match) bool { return m.Doc })
}

// searchCode asks for the identifiers only: plain words in code are noise.
func (p *PriorArtCodeIndex) searchCode(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
	if len(q.Identifiers) == 0 {
		return nil, nil
	}
	return p.search(ctx, q.Identifiers, priorart.KindCode, func(m codeindex.Match) bool {
		low := strings.ToLower(m.Path)
		return !m.Doc && !strings.HasSuffix(low, ".md") && !strings.Contains(low, "_test.") && !strings.Contains("/"+low, "/testdata/")
	})
}

func (p *PriorArtCodeIndex) search(ctx context.Context, terms []string, kind priorart.Kind, keep func(codeindex.Match) bool) ([]priorart.Candidate, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	idx, gaps := p.snapshot(ctx)
	var out []priorart.Candidate
	if idx != nil {
		matches, err := idx.Search(ctx, terms, priorArtCodeIndexSearchLimit)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if !keep(m) {
				continue
			}
			k := kind
			if kind == priorart.KindCode && looksLikeSettingPath(m.Path) {
				k = priorart.KindSetting
			}
			out = append(out, priorart.Candidate{
				Kind:  k,
				Title: m.Repo + ": " + m.Path,
				// The ranker needs the file's matched terms, not only its
				// best line; an HTML-comment line is never quoted.
				Text: m.Text + "\n<!-- terms: " + strings.Join(m.Terms, " ") + " -->",
				URL:  m.URL(),
				Ref:  m.Repo + "/" + m.Path,
			})
			if len(out) == priorArtCodeIndexPerSource {
				break
			}
		}
	}
	if len(gaps) > 0 {
		return out, &priorart.CoverageError{Gaps: gaps}
	}
	return out, nil
}

// snapshot returns the current index and its coverage gaps, starting a
// rebuild when the snapshot is stale. Only when no snapshot exists yet does
// it wait for the build, and then only until ctx ends.
func (p *PriorArtCodeIndex) snapshot(ctx context.Context) (*codeindex.Index, []string) {
	p.mu.Lock()
	if p.building == nil && (p.builtAt.IsZero() || p.now().Sub(p.builtAt) >= p.ttl) {
		p.building = make(chan struct{})
		go p.rebuild(p.building)
	}
	done, built := p.building, !p.builtAt.IsZero()
	p.mu.Unlock()
	if done != nil && !built {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	gaps := append([]string(nil), p.gaps...)
	if p.builtAt.IsZero() {
		gaps = append(gaps, "code index still building: no registered checkout searched yet")
	}
	return p.idx, gaps
}

func (p *PriorArtCodeIndex) rebuild(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), priorArtCodeIndexBuildTimeout)
	defer cancel()
	var checkouts []PriorArtCheckout
	if p.checkouts != nil {
		checkouts = p.checkouts()
	}
	sort.Slice(checkouts, func(i, j int) bool { return checkouts[i].Repo < checkouts[j].Repo })

	var gaps []string
	var repos []codeindex.Repo
	for _, c := range checkouts {
		if c.Repo == "" {
			continue
		}
		if c.Root == "" {
			gaps = append(gaps, fmt.Sprintf("checkout missing for %s: no checkouts_dir configured", c.Repo))
			continue
		}
		if info, err := os.Stat(c.Root); err != nil || !info.IsDir() {
			gaps = append(gaps, fmt.Sprintf("checkout missing for %s: %s", c.Repo, c.Root))
			continue
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", c.Root, "status", "--porcelain", "--untracked-files=no").Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			gaps = append(gaps, fmt.Sprintf("checkout partial for %s: uncommitted changes, indexed text may differ from the linked revision", c.Repo))
		}
		repos = append(repos, codeindex.Repo{Name: c.Repo, Root: c.Root})
	}
	idx, errs := codeindex.Build(ctx, repos)
	for _, r := range repos {
		if err := errs[r.Name]; err != nil {
			gaps = append(gaps, fmt.Sprintf("checkout partial for %s: %v", r.Name, err))
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if idx != nil {
		p.idx = idx
	} else {
		gaps = append(gaps, "code index build interrupted; answering from the previous snapshot")
	}
	p.gaps = gaps
	p.builtAt = p.now()
	p.building = nil
	close(done)
}
