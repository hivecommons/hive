package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Completer is the one LLM seam of the workflow-model stack: a single
// system+user turn returning text. The dashboard adapts its configured
// model gateway to this; nothing in this package knows a provider.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// CompleterFunc adapts a function to Completer.
type CompleterFunc func(ctx context.Context, system, user string) (string, error)

// Complete implements Completer.
func (f CompleterFunc) Complete(ctx context.Context, system, user string) (string, error) {
	return f(ctx, system, user)
}

// MaxAttempts is how many times the LLM is asked before falling back.
const MaxAttempts = 2

// Finding codes from the LLM stage.
const (
	// CodeLLMFellBack says the LLM answer never compiled and the
	// deterministic skeleton is returned instead.
	CodeLLMFellBack = "spec-llm-fell-back"
	// CodeLLMCitationReplaced says the LLM cited a section that does not
	// exist; the whole-document reference was substituted.
	CodeLLMCitationReplaced = "spec-llm-citation-replaced"
)

// Errors.
var (
	ErrEmpty    = errors.New("spec is empty")
	ErrTooLarge = fmt.Errorf("spec exceeds %d bytes", MaxBytes)
)

// Options drives one ingest.
type Options struct {
	// Origin names where the text came from (pasted, repo path, URL).
	Origin string
	// Name is the model name; defaults to the first heading's slug.
	Name  string
	Scope workflowmodel.Scope
	// Completer is the gateway; nil disables the LLM stage.
	Completer Completer
	// ModelID is recorded in the LLM report and annotation.
	ModelID string
}

// LLMReport says whether and how the LLM stage ran.
type LLMReport struct {
	Used     bool   `json:"used"`
	Model    string `json:"model,omitempty"`
	Attempts int    `json:"attempts"`
	// Accepted is true when the returned model is the LLM's answer.
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

// Result is one spec ingest.
type Result struct {
	Model       *workflowmodel.Model       `json:"model"`
	Compiled    bool                       `json:"compiled"`
	Profile     *workflowmodel.Profile     `json:"profile,omitempty"`
	Diagnostics []workflowmodel.Diagnostic `json:"diagnostics"`
	Sections    []Section                  `json:"sections"`
	Source      string                     `json:"source"`
	LLM         LLMReport                  `json:"llm"`
}

// Ingest runs both stages. The deterministic skeleton is always built;
// when opts.Completer is set the LLM is asked to refine it and its answer
// is accepted only if it parses strictly and compiles (one retry with the
// diagnostics). The returned model always carries the spec-ingest marks.
func Ingest(ctx context.Context, text string, opts Options) (*Result, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmpty
	}
	if len(text) > MaxBytes {
		return nil, ErrTooLarge
	}
	if opts.Origin == "" {
		opts.Origin = "pasted"
	}
	secs := Split(text)
	skeleton := Extract(opts.Origin, opts.Name, opts.Scope, secs)
	Mark(skeleton, opts.Origin, opts.Name, opts.Scope, secs)
	res := &Result{Sections: secs, Source: "spec:" + opts.Origin}
	if opts.Completer != nil {
		res.LLM = LLMReport{Used: true, Model: opts.ModelID}
		m, attempts, err := refine(ctx, text, skeleton, secs, opts)
		res.LLM.Attempts = attempts
		if err != nil {
			res.LLM.Error = err.Error()
			skeleton.Spec.Coverage.Findings = append(skeleton.Spec.Coverage.Findings, workflowmodel.Finding{Path: res.Source, Code: CodeLLMFellBack, Severity: workflowmodel.SeverityWarning, Detail: "LLM refinement rejected after " + fmt.Sprint(attempts) + " attempt(s): " + err.Error()})
		} else {
			res.LLM.Accepted = true
			skeleton = m
			skeleton.Metadata.Annotations[AnnotationLLMAssisted] = "LLM-assisted (model " + opts.ModelID + ")"
		}
	}
	res.Model = skeleton
	profile, diags, err := workflowmodel.Compile(skeleton)
	res.Diagnostics = diags
	if err == nil {
		res.Compiled = true
		res.Profile = profile
	}
	return res, nil
}

// refine asks the completer for a model, up to MaxAttempts times.
func refine(ctx context.Context, text string, skeleton *workflowmodel.Model, secs []Section, opts Options) (*workflowmodel.Model, int, error) {
	skel, err := json.MarshalIndent(skeleton, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	data := promptData{Origin: opts.Origin, Spec: text, Skeleton: string(skel), Schema: Schema(), Vocabulary: Vocabulary(), Sections: secs, Rules: RefusalRules, Name: skeleton.Metadata.Name, Forge: opts.Scope.Forge, Repository: opts.Scope.Repository}
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		user, err := UserPrompt(data)
		if err != nil {
			return nil, attempt - 1, err
		}
		raw, err := opts.Completer.Complete(ctx, SystemPrompt, user)
		if err != nil {
			return nil, attempt, fmt.Errorf("gateway: %w", err)
		}
		m, diags, err := Validate(raw, opts.Origin, skeleton.Metadata.Name, opts.Scope, secs)
		if err == nil {
			return m, attempt, nil
		}
		lastErr = err
		data.Diagnostics = data.Diagnostics[:0]
		for _, d := range diags {
			if d.Severity == workflowmodel.DiagError {
				data.Diagnostics = append(data.Diagnostics, d.Code+" "+d.Subject+": "+d.Detail)
			}
		}
		if len(data.Diagnostics) == 0 {
			data.Diagnostics = []string{err.Error()}
		}
	}
	return nil, MaxAttempts, lastErr
}

// Validate parses an LLM answer strictly, applies the spec-ingest marks
// and compiles it. The compile diagnostics are returned with the error so
// a retry can quote them.
func Validate(raw, origin, name string, scope workflowmodel.Scope, secs []Section) (*workflowmodel.Model, []workflowmodel.Diagnostic, error) {
	body := extractJSON(raw)
	if body == "" {
		return nil, nil, errors.New("answer contains no JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	var m workflowmodel.Model
	if err := dec.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("answer is not a Model: %w", err)
	}
	replaced := Mark(&m, origin, name, scope, secs)
	if replaced > 0 {
		m.Spec.Coverage.Findings = append(m.Spec.Coverage.Findings, workflowmodel.Finding{Path: "spec:" + origin, Code: CodeLLMCitationReplaced, Severity: workflowmodel.SeverityWarning, Detail: fmt.Sprintf("%d citation(s) named no section in the spec and were replaced with the whole-document reference", replaced)})
	}
	if _, diags, err := workflowmodel.Compile(&m); err != nil {
		return nil, diags, err
	}
	return &m, nil, nil
}

// extractJSON strips code fences and surrounding prose, returning the
// outermost {...}.
func extractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.LastIndex(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		s = strings.TrimSpace(rest)
	}
	start, end := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}
