package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// Workflow-model versions, diff, confirm and label preview
// (hivecommons/hive#17620, RFC phase 4, backend). These sit beside the
// library routes in api_workflows.go: reads are open, confirm is
// owner-gated and audited as workflow_confirm.
func (s *Server) registerWorkflowVersionRoutes() {
	s.mux.HandleFunc("GET /api/workflows/{name}/versions", s.handleWorkflowVersions)
	s.mux.HandleFunc("GET /api/workflows/{name}/diff", s.handleWorkflowDiff)
	s.mux.HandleFunc("POST /api/workflows/{name}/{version}/confirm", s.handleWorkflowConfirm)
	s.mux.HandleFunc("GET /api/workflows/{name}/{version}/labels", s.handleWorkflowLabels)
}

// handleWorkflowVersions lists every saved version of a model, newest
// save first; hive-default answers its single virtual entry.
func (s *Server) handleWorkflowVersions(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	entries, err := st.Versions(r.PathValue("name"))
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	jsonResponse(w, map[string]interface{}{"versions": entries})
}

// handleWorkflowDiff compares two saved versions. from/to are versions of
// {name}; fromName/toName override the model name on either side so a
// local model can be compared against hive-default.
func (s *Server) handleWorkflowDiff(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" || to == "" {
		jsonError(w, "from and to query parameters are required", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	fromName, toName := q.Get("fromName"), q.Get("toName")
	if fromName == "" {
		fromName = name
	}
	if toName == "" {
		toName = name
	}
	a, ae, err := st.Get(fromName, from)
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	b, be, err := st.Get(toName, to)
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	jsonResponse(w, map[string]interface{}{"from": ae, "to": be, "diff": workflowmodel.Diff(a, b)})
}

// handleWorkflowConfirm saves a copy of {name}@{version} with every
// inferred element confirmed and provenance local, as the next integer
// version. hive-default is read-only and answers 409.
func (s *Server) handleWorkflowConfirm(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	if wfstore.IsBuiltin(name) {
		jsonError(w, fmt.Sprintf("%s is built-in and read-only; save a copy under another name to edit it", name), http.StatusConflict)
		return
	}
	m, _, err := st.Get(name, version)
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	inferred := workflowmodel.Inferred(m)
	next, err := st.NextVersion(name)
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	c := workflowmodel.Confirm(m, requestUser(r))
	c.Spec.Meta.Version = next
	e, err := st.Save(c, requestUser(r), time.Now().UTC())
	if err != nil {
		workflowStoreError(w, err)
		return
	}
	s.auditFromRequest(r, "workflow_confirm", fmt.Sprintf("%s@%s -> %s confirmed=%d compiled=%v", name, version, e.Key(), inferred, e.Compiled), "")
	jsonStatusResponse(w, http.StatusCreated, map[string]interface{}{"entry": e, "confirmed": inferred, "from": name + "@" + version})
}

// workflowStateCommands is the human-gate verb list one state offers.
type workflowStateCommands struct {
	State string   `json:"state"`
	Verbs []string `json:"verbs"`
}

// workflowStageLabel is the built-in stage label a numbered state maps
// onto when the model declares no label of its own.
type workflowStageLabel struct {
	State  string `json:"state"`
	Number int    `json:"number"`
	Label  string `json:"label"`
}

// handleWorkflowLabels previews what binding {name}@{version} regenerates:
// the labels the engine reconciles into a bound repository and the
// commands (human-gate verbs) each state offers. A draft that does not
// compile answers 400 with its diagnostics.
func (s *Server) handleWorkflowLabels(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	p, err := st.Profile(name, version)
	if err != nil {
		if errors.Is(err, wfstore.ErrNotCompiled) {
			_, e, gerr := st.Get(name, version)
			if gerr == nil {
				jsonStatusResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error(), "diagnostics": e.Diagnostics})
				return
			}
		}
		workflowStoreError(w, err)
		return
	}
	jsonResponse(w, workflowLabelsPreview(p))
}

func workflowLabelsPreview(p *workflowmodel.Profile) map[string]interface{} {
	labels := p.LabelDefs(stagelabels.StageColor)
	if labels == nil {
		labels = []workflowmodel.LabelDef{}
	}
	for i := range labels {
		labels[i].Description = stagelabels.ClampLabelDescription(labels[i].Description)
	}
	commands := []workflowStateCommands{}
	stages := []workflowStageLabel{}
	for _, col := range p.Columns {
		for _, sid := range col.States {
			if verbs := p.Replies[sid]; len(verbs) > 0 {
				commands = append(commands, workflowStateCommands{State: sid, Verbs: append([]string(nil), verbs...)})
			}
			st, ok := p.States[sid]
			if !ok || st == nil || st.Lifecycle.Number == 0 {
				continue
			}
			for _, def := range stagelabels.Table {
				if def.Number == st.Lifecycle.Number {
					stages = append(stages, workflowStageLabel{State: sid, Number: def.Number, Label: def.Stage.Label()})
					break
				}
			}
		}
	}
	return map[string]interface{}{
		"name": p.Name, "version": p.Version, "hash": p.Hash,
		"prefix":   p.Labels.Prefix,
		"labels":   labels,
		"commands": commands,
		"stages":   stages,
	}
}
