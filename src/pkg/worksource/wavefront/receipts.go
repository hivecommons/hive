package wavefront

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	// Engine identifies this adapter in the receipts it writes.
	Engine = "hive-wavefront-adapter"
	// EngineVersion is the receipt-producing contract version of the adapter.
	EngineVersion = "v1"
	// ContractRevision is the receipt's contract identifier: what a "completed"
	// receipt for a migration node promises (the node's artifact anchor exists
	// under the recorded graph revision).
	ContractRevision = "wavefront-node/v1"
	// inputRevisionPrefix makes the receipt's InputRevision an artifact-style
	// revision ("<name>@<sha256 hex>") as outputschema requires.
	inputRevisionPrefix = "wavefront-graph@"
	// nodeGeneration is the receipt generation for a node completion. A node
	// completes at most once per graph revision, so there is exactly one
	// generation per (node, revision); a new revision mints a new receipt.
	nodeGeneration uint64 = 1
	// receiptFileMode keeps receipts operator-readable but not world-writable.
	receiptFileMode = 0o644
	// receiptDirMode matches the mode the beads store uses for its own tree.
	receiptDirMode = 0o755
	// receiptFileExt is the on-disk receipt file suffix.
	receiptFileExt = ".json"
	// anchorPathPrefix is the artifact path of the default progress anchor an
	// implementation stage leaves for a node (the "// crustify:todo" idea from
	// #7620 lesson 1, expressed as a repo-relative path).
	anchorPathPrefix = "wavefront/"
	// anchorDescription describes the default progress anchor artifact.
	anchorDescription = "migration node progress anchor"
)

// Receipt is the durable record that a node completed under one graph
// revision. The embedded stage receipt is the #8295 schema; Graph, Node,
// Revision, and Digest are the adapter's lookup keys.
type Receipt struct {
	Graph    string                    `json:"graph"`
	Node     string                    `json:"node"`
	Revision string                    `json:"revision"`
	Digest   string                    `json:"digest"`
	Receipt  outputschema.StageReceipt `json:"stage_receipt"`
}

// ReceiptStore keeps node-completion receipts, in memory and (when dir is set)
// as one JSON file per node under <dir>/<graph>/<node>.json. It is the only
// state Hive keeps for the migration: there is no side database.
type ReceiptStore struct {
	dir string
	mu  sync.RWMutex
	mem map[string]Receipt
}

// NewReceiptStore opens (or creates) a receipt store. An empty dir keeps
// receipts in memory only.
func NewReceiptStore(dir string) (*ReceiptStore, error) {
	s := &ReceiptStore{dir: dir, mem: map[string]Receipt{}}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, receiptDirMode); err != nil {
		return nil, fmt.Errorf("wavefront: receipts dir: %w", err)
	}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ReceiptStore) loadAll() error {
	graphs, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("wavefront: read receipts dir: %w", err)
	}
	for _, g := range graphs {
		if !g.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, g.Name()))
		if err != nil {
			return fmt.Errorf("wavefront: read receipts for %s: %w", g.Name(), err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), receiptFileExt) {
				continue
			}
			path := filepath.Join(s.dir, g.Name(), e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("wavefront: read receipt %s: %w", path, err)
			}
			var r Receipt
			if err := json.Unmarshal(data, &r); err != nil {
				return fmt.Errorf("wavefront: decode receipt %s: %w", path, err)
			}
			if r.Graph == "" || r.Node == "" {
				return fmt.Errorf("wavefront: receipt %s has no graph/node identity", path)
			}
			s.mem[ExternalID(r.Graph, r.Node)] = r
		}
	}
	return nil
}

// Get returns the receipt recorded for a node, at whatever revision it was
// recorded. Callers compare Revision against the current graph themselves.
func (s *ReceiptStore) Get(graph, node string) (Receipt, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.mem[ExternalID(graph, node)]
	return r, ok
}

// Put records a receipt, replacing any earlier receipt for the same node.
func (s *ReceiptStore) Put(r Receipt) error {
	if err := validIdentifier("graph name", r.Graph); err != nil {
		return err
	}
	if err := validIdentifier("node id", r.Node); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		dir := filepath.Join(s.dir, r.Graph)
		if err := os.MkdirAll(dir, receiptDirMode); err != nil {
			return fmt.Errorf("wavefront: receipt dir %s: %w", dir, err)
		}
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return fmt.Errorf("wavefront: encode receipt: %w", err)
		}
		final := filepath.Join(dir, r.Node+receiptFileExt)
		tmp := final + ".tmp"
		if err := os.WriteFile(tmp, data, receiptFileMode); err != nil {
			return fmt.Errorf("wavefront: write receipt: %w", err)
		}
		if err := os.Rename(tmp, final); err != nil {
			return fmt.Errorf("wavefront: commit receipt: %w", err)
		}
	}
	s.mem[ExternalID(r.Graph, r.Node)] = r
	return nil
}

// Completed reports whether the node holds a receipt recorded under exactly
// the given revision. A receipt from another revision is stale evidence and
// does not count: the node must be redone under the current plan.
func (s *ReceiptStore) Completed(graph, node, revision string) bool {
	r, ok := s.Get(graph, node)
	return ok && r.Revision == revision && r.Receipt.ResultClass == outputschema.ReceiptResultCompleted
}

// Unknown reports whether the node holds an unknown-state receipt recorded
// under exactly the given revision.
func (s *ReceiptStore) Unknown(graph, node, revision string) bool {
	r, ok := s.Get(graph, node)
	return ok && r.Revision == revision && r.Receipt.ResultClass == outputschema.ReceiptResultUnknown
}

func (s *ReceiptStore) terminal(graph, node, revision string) bool {
	r, ok := s.Get(graph, node)
	return ok && r.Revision == revision &&
		(r.Receipt.ResultClass == outputschema.ReceiptResultCompleted ||
			r.Receipt.ResultClass == outputschema.ReceiptResultUnknown)
}

// buildReceipt renders the #8295 stage receipt for one node completion.
func buildReceipt(repo string, g Graph, node Node, provenance string, artifacts []outputschema.Artifact, startedAt, endedAt time.Time) Receipt {
	return buildReceiptWithResult(repo, g, node, provenance, artifacts, startedAt, endedAt, outputschema.ReceiptResultCompleted)
}

func buildReceiptWithResult(repo string, g Graph, node Node, provenance string, artifacts []outputschema.Artifact, startedAt, endedAt time.Time, result outputschema.StageReceiptResultClass) Receipt {
	if len(artifacts) == 0 {
		artifacts = []outputschema.Artifact{{Repo: repo, Path: anchorPathPrefix + g.Name + "/" + node.ID, Description: anchorDescription}}
	}
	parts := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		parts = append(parts, strings.Join([]string{a.Repo, a.Path, a.Description}, "\x00"))
	}
	sort.Strings(parts)
	ref := worksource.Ref{SourceType: worksource.SourceTypeRun, Repo: repo, ExternalID: ExternalID(g.Name, node.ID)}
	digest := g.Digest()
	return Receipt{
		Graph:    g.Name,
		Node:     node.ID,
		Revision: g.Revision,
		Digest:   digest,
		Receipt: outputschema.StageReceipt{
			SchemaVersion:    outputschema.StageReceiptSchemaVersion,
			WorkKey:          ref.Key(),
			AssignmentID:     ExternalID(g.Name, node.ID),
			Generation:       nodeGeneration,
			Stage:            worksource.RunStageImplement,
			ContractRevision: ContractRevision,
			ExecutionKey:     mutation.DeriveLogicalID([]string{g.Name, node.ID, g.Revision}, nil),
			Engine:           &outputschema.StageReceiptEngine{Name: Engine, Version: EngineVersion},
			InputRevision:    inputRevisionPrefix + digest,
			OutputDigest:     effects.StableDigest(parts...),
			ResultClass:      result,
			StartedAt:        startedAt.UTC().Format(time.RFC3339Nano),
			EndedAt:          endedAt.UTC().Format(time.RFC3339Nano),
			Provenance:       &proof.Provenance{Query: provenance},
			Artifacts:        artifacts,
		},
	}
}
