// Package wavefront is the additive work source for an imported, versioned
// code-migration graph (Crustify's C/C++ to Rust migration driven by
// Wavefront, or any equivalent fixture). It is the second proving workload of
// the long-running-run model (hivecommons/hive#8362, #7620 question 5).
//
// The contract, in the words of the #7620 thread: Wavefront stays
// authoritative for its semantic graph. It already owns dependency closure,
// cycle handling, waves, and batching. Hive reads that graph, lists each READY
// node as a run-stage work item, withholds blocked nodes, and records a receipt
// when a node completes. Hive never re-derives the graph with an LLM, never
// fabricates GitHub issues or pull requests for nodes, and makes zero GitHub
// calls in this package. Publication of results is out of scope (#8353).
package wavefront

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Node statuses as written by Wavefront. Anything else is treated as pending.
const (
	// StatusPending is a node with work still to do. Empty status means pending.
	StatusPending = "pending"
	// StatusReady is a pending node Wavefront has already marked runnable. It
	// is admitted only when Hive's own dependency check agrees.
	StatusReady = "ready"
	// StatusDone is a node Wavefront considers complete. Its dependents are
	// unblocked and it is never listed.
	StatusDone = "done"
	// StatusBlocked is a node Wavefront has explicitly withheld, regardless of
	// what its dependency edges say.
	StatusBlocked = "blocked"
)

// Node is one migration unit in the graph.
type Node struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Kind      string   `json:"kind,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Status    string   `json:"status,omitempty"`
}

// Graph is the versioned migration plan as Wavefront publishes it.
type Graph struct {
	// Name identifies the graph; it is the "<graph>" half of every node's
	// external ID ("<graph>:<node>").
	Name string `json:"graph"`
	// Revision is Wavefront's version of this plan. Work items and receipts
	// are bound to it: an item minted at one revision is refused once the
	// graph moves to another (the stale-plan guard).
	Revision string `json:"revision"`
	Nodes    []Node `json:"nodes"`
}

// externalIDSeparator joins the graph name and the node ID in an item's
// ExternalID. It is ":" so the key shape matches run-stage items
// ("<runKey>:<stage>") and survives worksource.ParseKey unchanged.
const externalIDSeparator = ":"

// reservedIdentifierChars can never appear in a graph name or node ID: "#" and
// "!" are the worksource key separators, ":" is this package's own, "/" would
// escape the receipt directory, and whitespace breaks the plan-list line
// format DecomposeFromOutput parses.
const reservedIdentifierChars = "#!:/ \t\r\n"

// Structural defects Validate reports. Undefined edges are checked before
// cycles, so a dangling edge is never misreported as a cycle.
var (
	// ErrUndefinedEdge means a node depends on an id the graph does not define.
	ErrUndefinedEdge = errors.New("wavefront: edge to undefined node")
	// ErrCycle means the dependency graph is not a DAG.
	ErrCycle = errors.New("wavefront: dependency cycle")
)

// ExternalID returns the work item identifier for node in graph.
func ExternalID(graph, node string) string {
	return graph + externalIDSeparator + node
}

// SplitExternalID recovers the graph name and node ID from an item ID.
func SplitExternalID(externalID string) (graph, node string, ok bool) {
	graph, node, found := strings.Cut(externalID, externalIDSeparator)
	if !found || graph == "" || node == "" {
		return "", "", false
	}
	return graph, node, true
}

// ParseGraph decodes and validates a graph document. It rejects a document
// with no name, no revision, duplicate or malformed node IDs, an edge to a
// node the graph does not define, or a dependency cycle. Validation is
// structural only: Hive never rewrites the graph it was given.
func ParseGraph(data []byte) (Graph, error) {
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return Graph{}, fmt.Errorf("wavefront: decode graph: %w", err)
	}
	if err := g.Validate(); err != nil {
		return Graph{}, err
	}
	return g, nil
}

// Validate checks the structural invariants ParseGraph relies on.
func (g Graph) Validate() error {
	if err := validIdentifier("graph name", g.Name); err != nil {
		return err
	}
	if strings.TrimSpace(g.Revision) == "" {
		return fmt.Errorf("wavefront: graph %q has no revision", g.Name)
	}
	if len(g.Nodes) == 0 {
		return fmt.Errorf("wavefront: graph %q has no nodes", g.Name)
	}
	ids := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if err := validIdentifier("node id", n.ID); err != nil {
			return err
		}
		if ids[n.ID] {
			return fmt.Errorf("wavefront: graph %q defines node %q twice", g.Name, n.ID)
		}
		ids[n.ID] = true
	}
	for _, n := range g.Nodes {
		for _, dep := range n.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("%w: node %q depends on %q", ErrUndefinedEdge, n.ID, dep)
			}
			if dep == n.ID {
				return fmt.Errorf("wavefront: node %q depends on itself", n.ID)
			}
		}
	}
	if _, err := g.Waves(); err != nil {
		return err
	}
	return nil
}

func validIdentifier(what, id string) error {
	if id == "" {
		return fmt.Errorf("wavefront: %s is empty", what)
	}
	if strings.ContainsAny(id, reservedIdentifierChars) {
		return fmt.Errorf("wavefront: %s %q contains a reserved character (one of %q)", what, id, reservedIdentifierChars)
	}
	return nil
}

// Digest is a stable content hash of the graph (name, revision, and every
// node in declared order). It is the "graph digest" a receipt's InputRevision
// carries, so a receipt can be tied to exactly the plan it was produced under.
func (g Graph) Digest() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write(g.Name, g.Revision)
	for _, n := range g.Nodes {
		write(n.ID, n.Title, n.Kind, n.Status)
		write(n.DependsOn...)
		write("")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Node returns the node with the given ID.
func (g Graph) Node(id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Waves returns the nodes grouped into dependency waves: wave 0 holds every
// node with no dependencies, wave k holds nodes whose deepest dependency is in
// wave k-1. Within a wave nodes are sorted by ID. It reports ErrCycle when the
// graph is not a DAG, which is the one structural defect Hive refuses rather
// than repairs: cycle handling belongs to Wavefront. An edge to an id the
// graph does not define is ignored here (Validate reports it separately as
// ErrUndefinedEdge), so a dangling edge can never masquerade as a cycle.
func (g Graph) Waves() ([][]Node, error) {
	byID := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	indegree := make(map[string]int, len(g.Nodes))
	dependents := make(map[string][]string, len(g.Nodes))
	for _, n := range g.Nodes {
		for _, dep := range n.DependsOn {
			if _, known := byID[dep]; !known {
				continue
			}
			indegree[n.ID]++
			dependents[dep] = append(dependents[dep], n.ID)
		}
	}
	var frontier []string
	for _, n := range g.Nodes {
		if indegree[n.ID] == 0 {
			frontier = append(frontier, n.ID)
		}
	}
	var waves [][]Node
	placed := 0
	for len(frontier) > 0 {
		sort.Strings(frontier)
		wave := make([]Node, 0, len(frontier))
		var next []string
		for _, id := range frontier {
			wave = append(wave, byID[id])
			placed++
			for _, child := range dependents[id] {
				indegree[child]--
				if indegree[child] == 0 {
					next = append(next, child)
				}
			}
		}
		waves = append(waves, wave)
		frontier = next
	}
	if placed != len(g.Nodes) {
		return nil, fmt.Errorf("%w: graph %q", ErrCycle, g.Name)
	}
	return waves, nil
}
