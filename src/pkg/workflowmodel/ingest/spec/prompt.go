package spec

import (
	"strings"
	"text/template"
)

// SystemPrompt frames the LLM's only job: a Model JSON for the written
// spec, inside the compiler's rules. The gateway is never consulted for
// derivation; only here, and only to draft.
const SystemPrompt = `You turn a written software process specification into a Hive WorkflowModel JSON document.

Rules:
- Output exactly one JSON object and nothing else: no prose, no markdown fences.
- Use only the fields listed in the schema. Unknown fields are rejected.
- Model the steps the spec describes as states, in the order the spec gives them. Do not invent steps the spec does not describe.
- Every state, event and gate must carry "confidence": "inferred" and at least one citation whose "path" is exactly one of the section references given (spec:<heading>#L<start>-L<end>) and whose "line" is inside that range. Cite the section the element came from.
- Columns: one or more; each state is in exactly one column; the last column is "terminal": true.
- Transitions chain the states; "on" is a short kebab-case event name. Only declare spec.events when you also bind every transition.on to one of them.
- Gates model steps a person must release (approvals, reviews, votes); each gate's "releasedBy" names a role you declare in spec.roles.
- Roles: declare the people and bots the spec names (maintainer, reviewer, author, ...). Set "human": true for people.
- Do not emit pipelines, sources, agents or coverage; the ingest owns them.
- Keep spec.derivation {"mode": "builtin"} unless the spec gives explicit label/fact rules.
- Never claim certainty: this is a draft a human will review.`

// userPromptTemplate is filled with the spec, the deterministic skeleton,
// the schema, the vocabulary and the refusal rules.
var userPromptTemplate = template.Must(template.New("spec-user").Parse(`# Specification ({{.Origin}})

{{.Spec}}

# Section references (cite exactly these paths)
{{range .Sections}}- {{.Ref}}  "{{.Heading}}"
{{end}}
# Deterministic skeleton
A rule-based pass produced this draft. Refine it: fix labels, merge or split steps the spec actually describes, add gates/roles/guards the text supports, keep ids kebab-case. Return the whole model, not a diff.

{{.Skeleton}}

# Model JSON schema (path: type)
{{.Schema}}

# Fact vocabulary for predicates
{{.Vocabulary}}

# The compiler refuses a model that breaks any of these
{{range .Rules}}- {{.}}
{{end}}
{{if .Diagnostics}}# Your previous answer did not compile
Fix every diagnostic below and return the corrected full model.
{{range .Diagnostics}}- {{.}}
{{end}}{{end}}Target: name "{{.Name}}", scope.forge "{{.Forge}}", scope.repository "{{.Repository}}". Reply with the JSON object only.`))

type promptData struct {
	Origin, Spec, Skeleton, Schema, Vocabulary string
	Sections                                   []Section
	Rules                                      []string
	Diagnostics                                []string
	Name, Forge, Repository                    string
}

// UserPrompt renders the user turn.
func UserPrompt(d promptData) (string, error) {
	var b strings.Builder
	if err := userPromptTemplate.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}
