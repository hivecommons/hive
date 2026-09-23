package wavefront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	// SourceType is the adapter's own identity for dashboard badges and log
	// fields. The ITEMS it lists carry worksource.SourceTypeRun: a migration
	// node is a run-stage work item, not a new item kind.
	SourceType = "wavefront"
	// LabelWavefront tags every listed node so prompts and holds can tell a
	// migration node from a plain run stage.
	LabelWavefront = "wavefront"
	// LabelRevisionPrefix carries the graph revision the item was minted at
	// ("graph-rev/<revision>"). Whoever completes the node hands that revision
	// back to Complete, which is how the stale-plan guard knows what the
	// worker was working from.
	LabelRevisionPrefix = "graph-rev/"
	// labelRunStage and labelStagePrefix mirror the run-stage source's labels
	// so a migration node is filtered exactly like any other run stage.
	labelRunStage    = "hive-run"
	labelStagePrefix = "stage/"
	// defaultHTTPTimeout bounds one graph fetch from a Wavefront endpoint.
	defaultHTTPTimeout = 10 * time.Second
	// maxGraphBytes caps a graph document read from disk or the network. A
	// graph of a few thousand nodes is well under a megabyte; the cap only
	// stops a runaway endpoint from exhausting memory.
	maxGraphBytes = 16 << 20
	// nodePriority is the priority every migration node is listed with. The
	// graph carries no priority; ordering is by wave, not by urgency.
	nodePriority = "medium"
	// itemState is the state string of every listed node.
	itemState = "open"
)

// Sentinel errors returned by Complete and Verify.
var (
	// ErrStaleRevision means the item was minted at a graph revision that is
	// no longer current. The item is refused and must be re-listed from the
	// current graph; it is never re-derived.
	ErrStaleRevision = errors.New("wavefront: item revision does not match the current graph")
	// ErrUnknownNode means the item names a graph or node the current graph
	// does not define.
	ErrUnknownNode = errors.New("wavefront: unknown graph or node")
	// ErrNotReady means the node's dependencies are not all satisfied, so a
	// completion for it cannot be accepted.
	ErrNotReady = errors.New("wavefront: node is not ready")
)

// Loader returns the current graph. It is the single injection seam: file,
// HTTP, or a test stub.
type Loader func(ctx context.Context) (Graph, error)

// Options configure a Source.
type Options struct {
	// Repo is the owner/name repository every node key is scoped to.
	Repo string
	// Path or URL selects where the graph is read from. Exactly one is used;
	// Loader, when set, overrides both.
	Path string
	URL  string
	// Loader overrides Path and URL.
	Loader Loader
	// ReceiptsDir persists receipts; empty keeps them in memory.
	ReceiptsDir string
	// HTTPClient is used for URL loads. Nil uses a client with
	// defaultHTTPTimeout. It is never used for anything but the graph URL.
	HTTPClient *http.Client
	// Now supplies the clock for receipts. Nil uses time.Now.
	Now func() time.Time
}

// Source lists the ready nodes of a Wavefront graph as run-stage work items.
type Source struct {
	repo       string
	load       Loader
	provenance string
	receipts   *ReceiptStore
	now        func() time.Time
}

type Burndown struct {
	Scope     int
	Satisfied int
	Remaining int
	Unknown   int
}

// New constructs a Source. It performs no network or GitHub call; the graph is
// read lazily on each ListIssues.
func New(opts Options) (*Source, error) {
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" {
		return nil, fmt.Errorf("wavefront: repo is required")
	}
	receipts, err := NewReceiptStore(opts.ReceiptsDir)
	if err != nil {
		return nil, err
	}
	s := &Source{repo: repo, receipts: receipts, now: opts.Now}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	switch {
	case opts.Loader != nil:
		s.load = opts.Loader
		s.provenance = "wavefront-loader"
	case strings.TrimSpace(opts.Path) != "":
		path := strings.TrimSpace(opts.Path)
		s.load = FileLoader(path)
		s.provenance = "wavefront-file@" + path
	case strings.TrimSpace(opts.URL) != "":
		url := strings.TrimSpace(opts.URL)
		s.load = HTTPLoader(url, opts.HTTPClient)
		s.provenance = "wavefront-url@" + url
	default:
		return nil, fmt.Errorf("wavefront: one of Path, URL, or Loader is required")
	}
	return s, nil
}

// FileLoader reads the graph from a JSON file on every call.
func FileLoader(path string) Loader {
	return func(context.Context) (Graph, error) {
		f, err := os.Open(path)
		if err != nil {
			return Graph{}, fmt.Errorf("wavefront: open graph: %w", err)
		}
		defer f.Close()
		return readGraph(f)
	}
}

// HTTPLoader fetches the graph with a GET on every call. The client is used
// for this URL only.
func HTTPLoader(url string, client *http.Client) Loader {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return func(ctx context.Context) (Graph, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return Graph{}, fmt.Errorf("wavefront: build graph request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return Graph{}, fmt.Errorf("wavefront: fetch graph: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return Graph{}, fmt.Errorf("wavefront: fetch graph: status %d", resp.StatusCode)
		}
		return readGraph(resp.Body)
	}
}

func readGraph(r io.Reader) (Graph, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxGraphBytes+1))
	if err != nil {
		return Graph{}, fmt.Errorf("wavefront: read graph: %w", err)
	}
	if len(data) > maxGraphBytes {
		return Graph{}, fmt.Errorf("wavefront: graph exceeds %d bytes", maxGraphBytes)
	}
	return ParseGraph(data)
}

// SourceType returns the adapter identity ("wavefront").
func (s *Source) SourceType() string { return SourceType }

// Receipts exposes the receipt store, for the runs API and tests.
func (s *Source) Receipts() *ReceiptStore { return s.receipts }

// Graph returns the current graph as Wavefront publishes it.
func (s *Source) Graph(ctx context.Context) (Graph, error) {
	if s == nil || s.load == nil {
		return Graph{}, fmt.Errorf("wavefront: source is not configured")
	}
	return s.load(ctx)
}

// Ready returns the current graph and the nodes Hive may hand out right now:
// pending nodes whose every dependency is satisfied and that hold no
// completion receipt at the current revision. Blocked and done nodes are
// withheld. Nodes come back in wave order, sorted by ID within a wave.
func (s *Source) Ready(ctx context.Context) (Graph, []Node, error) {
	g, err := s.Graph(ctx)
	if err != nil {
		return Graph{}, nil, err
	}
	waves, err := g.Waves()
	if err != nil {
		return Graph{}, nil, err
	}
	var ready []Node
	for _, wave := range waves {
		for _, n := range wave {
			if s.isReady(g, n) {
				ready = append(ready, n)
			}
		}
	}
	return g, ready, nil
}

// Burndown returns graph-level progress for the run key that names one node in
// this source, or ok=false when the key belongs to another source/graph.
func (s *Source) Burndown(ctx context.Context, key string) (Burndown, bool, error) {
	if s == nil {
		return Burndown{}, false, nil
	}
	ref, ok := worksource.ParseKey(key)
	if !ok || ref.Repo != s.repo {
		return Burndown{}, false, nil
	}
	graphName, nodeID, ok := SplitExternalID(ref.ExternalID)
	if !ok {
		return Burndown{}, false, nil
	}
	g, err := s.Graph(ctx)
	if err != nil {
		return Burndown{}, false, err
	}
	if g.Name != graphName {
		return Burndown{}, false, nil
	}
	if _, ok := g.Node(nodeID); !ok {
		return Burndown{}, false, nil
	}
	out := Burndown{Scope: len(g.Nodes)}
	for _, n := range g.Nodes {
		if s.satisfied(g, n) {
			out.Satisfied++
		} else if s.receipts.Unknown(g.Name, n.ID, g.Revision) {
			out.Unknown++
		}
	}
	out.Remaining = out.Scope - out.Satisfied - out.Unknown
	return out, true, nil
}

// satisfied reports whether a node counts as complete for its dependents:
// Wavefront says done, or Hive holds a completion receipt at this revision.
func (s *Source) satisfied(g Graph, n Node) bool {
	return n.Status == StatusDone || s.receipts.Completed(g.Name, n.ID, g.Revision)
}

func (s *Source) isReady(g Graph, n Node) bool {
	if n.Status == StatusBlocked || n.Status == StatusDone || s.receipts.terminal(g.Name, n.ID, g.Revision) {
		return false
	}
	for _, dep := range n.DependsOn {
		d, ok := g.Node(dep)
		if !ok || !s.satisfied(g, d) {
			return false
		}
	}
	return true
}

// MarkUnknown records that an in-flight node's owner disappeared before Hive
// could observe a normal completion. Unknown receipts are terminal for this
// graph revision, but do not satisfy dependents.
func (s *Source) MarkUnknown(ctx context.Context, externalID, reason string, startedAt time.Time) (Receipt, error) {
	g, n, err := s.verifyCurrentNode(ctx, externalID)
	if err != nil {
		return Receipt{}, err
	}
	if existing, ok := s.receipts.Get(g.Name, n.ID); ok && existing.Revision == g.Revision {
		return existing, nil
	}
	endedAt := s.now()
	if startedAt.IsZero() || startedAt.After(endedAt) {
		startedAt = endedAt
	}
	provenance := s.provenance
	if reason = strings.TrimSpace(reason); reason != "" {
		provenance += " " + reason
	}
	r := buildReceiptWithResult(s.repo, g, n, provenance, nil, startedAt, endedAt, outputschema.ReceiptResultUnknown)
	if err := s.receipts.Put(r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// ListIssues lists every ready node as a run-stage work item.
func (s *Source) ListIssues(ctx context.Context) ([]worksource.Issue, error) {
	g, ready, err := s.Ready(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]worksource.Issue, 0, len(ready))
	for _, n := range ready {
		out = append(out, s.issue(g, n))
	}
	return out, nil
}

func (s *Source) issue(g Graph, n Node) worksource.Issue {
	title := strings.TrimSpace(n.Title)
	if title == "" {
		title = n.ID
	}
	labels := []string{labelRunStage, labelStagePrefix + worksource.RunStageImplement, LabelWavefront, LabelRevisionPrefix + g.Revision}
	if n.Kind != "" {
		labels = append(labels, LabelWavefront+"/"+n.Kind)
	}
	issue := worksource.Issue{
		SourceType: worksource.SourceTypeRun,
		Repo:       s.repo,
		ExternalID: ExternalID(g.Name, n.ID),
		Number:     0,
		Title:      worksource.RunStageImplement + ": " + title,
		Author:     Engine,
		Labels:     labels,
		Stage:      worksource.RunStageImplement,
		Priority:   nodePriority,
		State:      itemState,
	}
	for _, dep := range n.DependsOn {
		issue.DependsOn = append(issue.DependsOn, worksource.Dependency{
			Ref: worksource.Ref{
				SourceType: worksource.SourceTypeRun,
				Repo:       s.repo,
				ExternalID: ExternalID(g.Name, dep),
			},
			// Only ready nodes are listed, so every edge is resolved by
			// construction. Admission still sees the edge and can re-check it.
			Resolved: true,
		})
	}
	return issue
}

// RevisionFromLabels recovers the graph revision an item was minted at.
func RevisionFromLabels(labels []string) (string, bool) {
	for _, l := range labels {
		if rev, ok := strings.CutPrefix(l, LabelRevisionPrefix); ok && rev != "" {
			return rev, true
		}
	}
	return "", false
}

// Verify is the stale-plan guard. It checks that externalID names a node of
// the current graph and that revision is the current graph revision. A
// mismatch is refused with ErrStaleRevision: the worker's plan is out of date
// and Hive does not try to map the old node onto the new graph.
func (s *Source) Verify(ctx context.Context, externalID, revision string) (Graph, Node, error) {
	g, n, err := s.verifyCurrentNode(ctx, externalID)
	if err != nil {
		return Graph{}, Node{}, err
	}
	if g.Revision != revision {
		return Graph{}, Node{}, fmt.Errorf("%w: item at %q, graph at %q", ErrStaleRevision, revision, g.Revision)
	}
	return g, n, nil
}

func (s *Source) verifyCurrentNode(ctx context.Context, externalID string) (Graph, Node, error) {
	graphName, nodeID, ok := SplitExternalID(externalID)
	if !ok {
		return Graph{}, Node{}, fmt.Errorf("%w: malformed external id %q", ErrUnknownNode, externalID)
	}
	g, err := s.Graph(ctx)
	if err != nil {
		return Graph{}, Node{}, err
	}
	if g.Name != graphName {
		return Graph{}, Node{}, fmt.Errorf("%w: graph %q is not %q", ErrUnknownNode, graphName, g.Name)
	}
	n, ok := g.Node(nodeID)
	if !ok {
		return Graph{}, Node{}, fmt.Errorf("%w: node %q", ErrUnknownNode, nodeID)
	}
	return g, n, nil
}

// Complete records that a node finished under the given graph revision and
// returns its receipt. It refuses a stale revision (ErrStaleRevision), an
// unknown node (ErrUnknownNode), and a node whose dependencies are not all
// satisfied (ErrNotReady). Completing an already-completed node at the same
// revision returns the existing receipt unchanged, so a retry is idempotent.
// artifacts are the progress anchors the stage produced; nil records the
// default anchor path for the node. startedAt is when the stage began.
func (s *Source) Complete(ctx context.Context, externalID, revision string, artifacts []outputschema.Artifact, startedAt time.Time) (Receipt, error) {
	g, n, err := s.Verify(ctx, externalID, revision)
	if err != nil {
		return Receipt{}, err
	}
	if existing, ok := s.receipts.Get(g.Name, n.ID); ok && existing.Revision == g.Revision {
		return existing, nil
	}
	if n.Status == StatusDone {
		return Receipt{}, fmt.Errorf("%w: node %q is already done in the graph", ErrNotReady, n.ID)
	}
	for _, dep := range n.DependsOn {
		d, ok := g.Node(dep)
		if !ok || !s.satisfied(g, d) {
			return Receipt{}, fmt.Errorf("%w: node %q waits on %q", ErrNotReady, n.ID, dep)
		}
	}
	endedAt := s.now()
	if startedAt.IsZero() || startedAt.After(endedAt) {
		startedAt = endedAt
	}
	r := buildReceipt(s.repo, g, n, s.provenance, artifacts, startedAt, endedAt)
	if err := s.receipts.Put(r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}
