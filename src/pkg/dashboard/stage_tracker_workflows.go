package dashboard

import (
	"sort"
	"strconv"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// StageTrackerWorkflows is the tracker's `workflows` block
// (hivecommons/hive#17620, RFC phase 2): which model each bound repository
// follows and the column/state layout the SPA renders for it. Absent when no
// repository is bound, so the v5/v6 payload is unchanged for every hive that
// has not opted in.
type StageTrackerWorkflows struct {
	// Default is the hive-default model every unbound repository follows.
	Default StageTrackerWorkflow `json:"default"`
	// Repos maps an org-qualified repository to its bound model. Repos
	// bound to a model that is missing or no longer compiles are listed
	// with Error set and fall back to Default in the SPA.
	Repos map[string]StageTrackerWorkflow `json:"repos"`
}

// StageTrackerWorkflow is one model's identity and column layout.
type StageTrackerWorkflow struct {
	Name    string                       `json:"name"`
	Version string                       `json:"version"`
	Hash    string                       `json:"hash,omitempty"`
	Columns []StageTrackerWorkflowColumn `json:"columns,omitempty"`
	// Replies maps a stage number (as a string key, the JSON object key
	// the SPA indexes by `item.stage`) to the human-gate reply verbs the
	// model declares for the state at that number (hivecommons/hive#17620,
	// RFC phase 4). The gate pill offers them after the built-in verbs;
	// each posts `/hive <verb>`. Absent when the model declares none.
	Replies map[string][]string `json:"replies,omitempty"`
	Error   string              `json:"error,omitempty"`
}

// StageTrackerWorkflowColumn is a column with the stage numbers it anchors,
// so the SPA can place an item by its stage/NNN number without knowing the
// model's state ids.
type StageTrackerWorkflowColumn struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	Terminal bool     `json:"terminal,omitempty"`
	States   []string `json:"states"`
	Numbers  []int    `json:"numbers"`
}

// workflowProfileLookup resolves a bound name@version to its compiled
// profile; the store's Profile method satisfies it.
type workflowProfileLookup func(name, version string) (*workflowmodel.Profile, error)

func stageTrackerWorkflowBlock(p *workflowmodel.Profile) StageTrackerWorkflow {
	out := StageTrackerWorkflow{Name: p.Name, Version: p.Version, Hash: p.Hash}
	for _, c := range p.Columns {
		col := StageTrackerWorkflowColumn{ID: c.ID, Label: c.Label, Hint: c.Hint, Terminal: c.Terminal, States: append([]string{}, c.States...), Numbers: []int{}}
		for _, id := range c.States {
			if st := p.States[id]; st != nil && st.Lifecycle.Number > 0 {
				col.Numbers = append(col.Numbers, st.Lifecycle.Number)
			}
		}
		sort.Ints(col.Numbers)
		out.Columns = append(out.Columns, col)
	}
	// hive-default's replies are dashboard action ids, not comment verbs;
	// its block stays exactly as before.
	for n, id := range p.StateOfNumber {
		if p.Name == hivedefault.Name {
			break
		}
		if verbs := p.RepliesForNumber(n); len(verbs) > 0 && id != "" {
			if out.Replies == nil {
				out.Replies = map[string][]string{}
			}
			out.Replies[strconv.Itoa(n)] = append([]string{}, verbs...)
		}
	}
	return out
}

// workflowProfileForRepo is the compiled model the repository is bound to
// — nil when unbound, bound to hive-default, or when the binding does not
// resolve — so the dashboard applies exactly the vocabulary the stage
// engine does (hivecommons/hive#17620).
func (s *Server) workflowProfileForRepo(repoFull string) *workflowmodel.Profile {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.deps.Config.Workflow.Enabled {
		return nil
	}
	lookup := s.workflowProfileLookup()
	if lookup == nil {
		return nil
	}
	b, ok := s.deps.Config.WorkflowBindingFor(repoFull)
	if !ok {
		return nil
	}
	p, err := lookup(b.Name, b.Version)
	if err != nil || p == nil || p.Name == hivedefault.Name {
		return nil
	}
	return p
}

// stageTrackerWorkflows builds the block from the config's bindings; nil when
// there are none.
func stageTrackerWorkflows(cfg *config.Config, lookup workflowProfileLookup, qualify func(string) string) *StageTrackerWorkflows {
	if cfg == nil || len(cfg.Project.WorkflowBindings) == 0 {
		return nil
	}
	out := &StageTrackerWorkflows{
		Default: stageTrackerWorkflowBlock(hivedefault.Profile()),
		Repos:   map[string]StageTrackerWorkflow{},
	}
	for _, b := range cfg.Project.WorkflowBindings {
		repo := qualify(b.Repo)
		if lookup == nil {
			out.Repos[repo] = StageTrackerWorkflow{Name: b.Name, Version: b.Version, Error: "workflow store not enabled"}
			continue
		}
		p, err := lookup(b.Name, b.Version)
		if err != nil {
			out.Repos[repo] = StageTrackerWorkflow{Name: b.Name, Version: b.Version, Error: err.Error()}
			continue
		}
		out.Repos[repo] = stageTrackerWorkflowBlock(p)
	}
	return out
}
