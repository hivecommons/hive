package dashboard

import (
	"sort"
	"strconv"
	"strings"

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
	// Dispatch maps a stage number (string key, as Replies) to the agent
	// profile the model hands an item in that state to (cncf/techdocs#395).
	// The tracker offers an owner "Send to <profile>" on such rows (POST
	// /api/workflows/dispatch). Absent when the model dispatches nothing.
	Dispatch map[string]StageTrackerWorkflowDispatch `json:"dispatch,omitempty"`
	// Handoffs maps an item number (string key) to the hand-off of that
	// item an agent declined or did not act on (hivecommons/hive#18749).
	// The row wears it, so an item that looks handed off but is not being
	// worked is not silent. Absent when every hand-off started.
	Handoffs map[string]StageTrackerWorkflowHandoff `json:"handoffs,omitempty"`
	Error    string                                 `json:"error,omitempty"`
}

// StageTrackerWorkflowDispatch is one state the model hands to a profile.
type StageTrackerWorkflowDispatch struct {
	Profile string   `json:"profile"`
	State   string   `json:"state"`
	Kinds   []string `json:"kinds,omitempty"`
	// Reply is the reply that starts it automatically ("" = on entry).
	Reply string `json:"reply,omitempty"`
	// Agent is the hive agent that fills the profile for this repository;
	// Problem says why none does (hivecommons/hive#18690). The row wears
	// Problem so a hand-off that cannot happen is not silent. Both are set
	// for bound repositories only.
	Agent   string `json:"agent,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// StageTrackerWorkflowColumn is a column with the stage numbers it anchors,
// so the SPA can place an item by its stage/NNN number without knowing the
// model's state ids.
type StageTrackerWorkflowColumn struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	Icon     string   `json:"icon,omitempty"`
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
		col := StageTrackerWorkflowColumn{ID: c.ID, Label: c.Label, Hint: c.Hint, Icon: strings.TrimSpace(c.Icon), Terminal: c.Terminal, States: append([]string{}, c.States...), Numbers: []int{}}
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
	if p.Name != hivedefault.Name {
		for sid, t := range p.Dispatch {
			st := p.States[sid]
			if st == nil || st.Lifecycle.Number <= 0 {
				continue
			}
			kinds := make([]string, 0, len(t.Kinds))
			for _, k := range t.Kinds {
				kinds = append(kinds, string(k))
			}
			if out.Dispatch == nil {
				out.Dispatch = map[string]StageTrackerWorkflowDispatch{}
			}
			out.Dispatch[strconv.Itoa(st.Lifecycle.Number)] = StageTrackerWorkflowDispatch{Profile: t.Profile, State: sid, Kinds: kinds, Reply: t.Reply}
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
		block := stageTrackerWorkflowBlock(p)
		annotateWorkflowDispatch(cfg, b.Repo, p, &block)
		out.Repos[repo] = block
	}
	return out
}
