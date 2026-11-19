package spec

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Schema renders the Model's JSON shape from the Go types so the prompt
// never drifts from the struct definitions. Each line is
// "path: type" with enum values where the type is a named string.
func Schema() string {
	var lines []string
	seen := map[reflect.Type]bool{}
	walk(reflect.TypeOf(workflowmodel.Model{}), "", seen, &lines)
	return strings.Join(lines, "\n")
}

var enums = map[string][]string{
	"Owner":          {string(workflowmodel.OwnerHuman), string(workflowmodel.OwnerRegisteredAgent), string(workflowmodel.OwnerExternalService), string(workflowmodel.OwnerExternalObservation)},
	"StateType":      {"\"\" (sequential)", string(workflowmodel.StateParallel), string(workflowmodel.StateFinal)},
	"Origin":         {string(workflowmodel.OriginIngested)},
	"GuardKind":      {string(workflowmodel.GuardRequiredChecks), string(workflowmodel.GuardReviewPolicy), string(workflowmodel.GuardEnvironmentApproval), string(workflowmodel.GuardManual), string(workflowmodel.GuardExternal)},
	"Confidence":     {string(workflowmodel.ConfidenceInferred)},
	"ItemKind":       {string(workflowmodel.KindIssue), string(workflowmodel.KindPR)},
	"DerivationMode": {string(workflowmodel.DerivationBuiltin), string(workflowmodel.DerivationRules)},
	"Op":             {string(workflowmodel.OpIs), string(workflowmodel.OpNot), string(workflowmodel.OpHas), string(workflowmodel.OpLacks), string(workflowmodel.OpTrue), string(workflowmodel.OpFalse)},
	"RoleKind":       {string(workflowmodel.RolePermission), string(workflowmodel.RoleFrozenSet), string(workflowmodel.RoleAuthorKind), string(workflowmodel.RoleBot)},
	"MetaSource":     {string(workflowmodel.SourceSpec)},
	"Provenance":     {string(workflowmodel.ProvenanceIngestedUnreviewed)},
	"Severity":       {string(workflowmodel.SeverityUnsupported), string(workflowmodel.SeverityUnknown), string(workflowmodel.SeverityWarning)},
}

// omitted are Model fields the LLM must not emit: they are owned by the
// ingest (pipelines, file sources, agent grants) and stripped anyway.
var omitted = map[string]bool{"pipelines": true, "sources": true, "agents": true, "coverage": true}

func walk(t reflect.Type, prefix string, seen map[reflect.Type]bool, lines *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" || name == "" {
			continue
		}
		if prefix == "spec" && omitted[name] {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		suffix := ""
		for ft.Kind() == reflect.Slice || ft.Kind() == reflect.Pointer {
			if ft.Kind() == reflect.Slice {
				suffix += "[]"
			}
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct:
			if seen[ft] {
				*lines = append(*lines, fmt.Sprintf("%s: %s%s (same shape as above)", path, ft.Name(), suffix))
				continue
			}
			seen[ft] = true
			*lines = append(*lines, fmt.Sprintf("%s: object%s", path, suffix))
			walk(ft, path, seen, lines)
			delete(seen, ft)
		case ft.Kind() == reflect.Map:
			*lines = append(*lines, fmt.Sprintf("%s: map[string]string%s", path, suffix))
		default:
			typ := ft.Kind().String()
			if vals, ok := enums[ft.Name()]; ok {
				typ = "one of " + strings.Join(vals, " | ")
			}
			*lines = append(*lines, fmt.Sprintf("%s: %s%s", path, typ, suffix))
		}
	}
}

// Vocabulary lists the fact names a predicate may use, with their types.
func Vocabulary() string {
	keys := make([]string, 0, len(workflowmodel.Facts))
	for k := range workflowmodel.Facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s (%s)", k, workflowmodel.Facts[k]))
	}
	return strings.Join(parts, ", ")
}

// RefusalRules are the compiler's refusals, stated for the prompt. They
// mirror the error codes in workflowmodel/compile.go.
var RefusalRules = []string{
	workflowmodel.CodeNoVersion + ": spec.meta.version must be set",
	workflowmodel.CodeNoColumn + ": every top-level state must be collected by exactly one column (spec.columns[].states)",
	workflowmodel.CodeColumnEmpty + ": every column must collect at least one state",
	workflowmodel.CodeNoTerminalColumn + ": at least one column must be terminal: true",
	workflowmodel.CodeNoExit + ": every non-terminal state needs a transition out",
	workflowmodel.CodeDuplicateNumber + ": lifecycle.number must be unique per item kind",
	workflowmodel.CodeGateNoOwner + ": every gate needs releasedBy naming a role in spec.roles",
	workflowmodel.CodeUnknownRole + ": event actors and gate releasers must be declared roles",
	workflowmodel.CodeUnknownGate + ": transition gateRef must name a declared gate",
	workflowmodel.CodeUnknownEvent + ": if any spec.events are declared, every transition.on must name one",
	workflowmodel.CodeDuplicateEvent + ": two events may not bind the same fact predicate",
	workflowmodel.CodeUnknownFact + "/" + workflowmodel.CodeBadOp + ": predicates use only vocabulary facts with the right operator (bool: true/false; scalar: is/not; set: has/lacks)",
	workflowmodel.CodeNoRules + ": derivation.mode rules needs at least one rule; prefer mode builtin",
	"invalid: transitions and guardRefs must reference declared states and guards; initial must be a state; every state has an owner",
}
