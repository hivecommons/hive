package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"gopkg.in/yaml.v3"
)

// Workflow-model editor support (hivecommons/hive#17620, RFC phase 4 UI).
//
// The editor works on an unsaved model in the browser, so the read-only
// computations it needs — YAML round-trip and a what-if derivation — take
// the model in the request body rather than a stored name@version. Like
// POST /api/workflows/compile they need no store and no owner role.
func (s *Server) registerWorkflowEditorRoutes() {
	s.mux.HandleFunc("POST /api/workflows/yaml", s.handleWorkflowYAML)
	s.mux.HandleFunc("POST /api/workflows/simulate", s.handleWorkflowSimulateModel)
}

// workflowYAMLRequest carries either a model (→ yaml) or yaml (→ model).
type workflowYAMLRequest struct {
	Model json.RawMessage `json:"model,omitempty"`
	YAML  string          `json:"yaml,omitempty"`
}

// handleWorkflowYAML converts between the JSON model and its YAML form.
// Field names are the json tags on both sides: the JSON document is parsed
// as YAML (JSON is a YAML subset) and re-encoded, so a round-trip changes
// nothing but whitespace. A YAML parse or schema error answers 200 with
// `error` set so the editor can show it inline next to the text.
func (s *Server) handleWorkflowYAML(w http.ResponseWriter, r *http.Request) {
	var req workflowYAMLRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case len(req.Model) > 0 && strings.TrimSpace(string(req.Model)) != "null":
		var m workflowmodel.Model
		if err := json.Unmarshal(req.Model, &m); err != nil {
			jsonError(w, "model: "+err.Error(), http.StatusBadRequest)
			return
		}
		out, err := workflowModelToYAML(&m)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]interface{}{"yaml": out})
	case strings.TrimSpace(req.YAML) != "":
		m, err := workflowModelFromYAML(req.YAML)
		if err != nil {
			jsonResponse(w, map[string]interface{}{"error": err.Error()})
			return
		}
		jsonResponse(w, map[string]interface{}{"model": m})
	default:
		jsonError(w, "set model (to get yaml) or yaml (to get a model)", http.StatusBadRequest)
	}
}

func workflowModelToYAML(m *workflowmodel.Model) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return "", err
	}
	// The JSON decoder leaves flow style on every mapping; clear it so the
	// encoder writes block YAML.
	workflowYAMLBlockStyle(&node)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func workflowYAMLBlockStyle(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style = 0
	}
	if n.Kind == yaml.ScalarNode && n.Style == yaml.DoubleQuotedStyle {
		n.Style = 0
	}
	for _, c := range n.Content {
		workflowYAMLBlockStyle(c)
	}
}

func workflowModelFromYAML(text string) (*workflowmodel.Model, error) {
	var generic interface{}
	if err := yaml.Unmarshal([]byte(text), &generic); err != nil {
		return nil, err
	}
	generic = workflowYAMLToJSONValue(generic)
	b, err := json.Marshal(generic)
	if err != nil {
		return nil, err
	}
	var m workflowmodel.Model
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// workflowYAMLToJSONValue turns yaml.v3's map[string]interface{} and
// []interface{} trees into JSON-marshalable values (yaml.v3 already uses
// string keys for string-keyed maps; non-string keys are stringified).
func workflowYAMLToJSONValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = workflowYAMLToJSONValue(val)
		}
		return out
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = workflowYAMLToJSONValue(val)
		}
		return out
	case []interface{}:
		for i := range t {
			t[i] = workflowYAMLToJSONValue(t[i])
		}
		return t
	}
	return v
}

// workflowSimulateModelRequest is the editor's variant of the stored-model
// simulate request: the model travels in the body and facts are required
// (no forge fetch — the canvas may hold something that was never saved).
type workflowSimulateModelRequest struct {
	Model *workflowmodel.Model `json:"model"`
	Kind  string               `json:"kind"`
	Facts *SimulateFacts       `json:"facts"`
}

// handleWorkflowSimulateModel answers POST /api/workflows/simulate with the
// same SimulateResponse as the stored-model variant. 422 with diagnostics
// when the posted model does not compile, so the editor can point at the
// offending element instead of a bare "does not compile".
func (s *Server) handleWorkflowSimulateModel(w http.ResponseWriter, r *http.Request) {
	var req workflowSimulateModelRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == nil {
		jsonError(w, "model is required", http.StatusBadRequest)
		return
	}
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	if req.Kind == "" {
		jsonError(w, "kind must be issue or pr", http.StatusBadRequest)
		return
	}
	if req.Facts == nil {
		jsonError(w, "facts are required when simulating an unsaved model", http.StatusBadRequest)
		return
	}
	profile, diags, cerr := workflowmodel.Compile(req.Model)
	if cerr != nil {
		if diags == nil {
			diags = []workflowmodel.Diagnostic{}
		}
		jsonStatusResponse(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"error":       fmt.Sprintf("model does not compile: %d diagnostics", len(diags)),
			"diagnostics": diags,
		})
		return
	}
	ref, err := stageengine.SimulateRef(req.Kind, "sim", "sim", 0)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var notes []string
	norm := normalizeSimulateFacts(req.Kind, *req.Facts)
	ef := norm.toEngine(ref)
	if ref.Kind == stagelabels.PRItem && len(ef.PR.Lanes) == 0 {
		lane := stageengine.SimulatedLane(ef)
		ef.PR.Lanes = []stageengine.LaneFact{lane}
		norm.PR.Lane = &SimulateLane{Lane: lane.Lane, Reason: lane.Reason}
		notes = append(notes, fmt.Sprintf("no lane observation supplied; simulated lane %s with reason %q from the head facts", lane.Lane, lane.Reason))
	}
	dec, trace := stageengine.Explain(profile, ref.Kind, ef)
	if trace == nil {
		trace = []stageengine.RuleTrace{}
	}
	jsonResponse(w, SimulateResponse{
		Decision: simulateDecision(profile, dec),
		Trace:    trace,
		Facts:    norm,
		Filled:   []string{},
		Unknown:  []string{},
		Notes:    notes,
	})
}
