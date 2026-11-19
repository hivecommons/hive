package spec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

const sample = `# Contribution process

Anyone may open an issue. The TOC decides on proposals.

## Steps

1. Request: a contributor files an issue describing the change.
2. Triage: maintainers label the issue within a week.
3. Implement the change in a pull request.
4. Review by two reviewers. If changes are requested, the PR goes back to implement.
5. Approval: a maintainer approves when CI is green.
6. Merge
7. Release: the release manager publishes a tag.

## Notes

A bot closes stale issues after 90 days.
`

var scope = workflowmodel.Scope{Forge: "github.com", Repository: "acme/widgets"}

func TestSplitSections(t *testing.T) {
	secs := Split(sample)
	var got []string
	for _, s := range secs {
		got = append(got, s.Ref())
	}
	if want := "spec:Contribution process#L1-L4 spec:Steps#L5-L14 spec:Notes#L15-L18"; strings.Join(got, " ") != want {
		t.Fatalf("%v", got)
	}
	if secs[1].Level != 2 || len(secs[1].Lines) != 9 {
		t.Fatalf("%+v", secs[1])
	}

	setext := "Title\n=====\n\nbody\n\nSub\n---\n- a\r\n- b\n"
	secs = Split(setext)
	if len(secs) != 2 || secs[0].Heading != "Title" || secs[0].Level != 1 || secs[1].Heading != "Sub" || secs[1].Level != 2 || secs[1].StartLine != 6 {
		t.Fatalf("%+v", secs)
	}
	plain := Split("just text\nmore")
	if len(plain) != 1 || plain[0].Heading != "Document" || plain[0].EndLine != 2 {
		t.Fatalf("%+v", plain)
	}
	pre := Split("intro\n# H\nx")
	if len(pre) != 2 || pre[0].Heading != "Preamble" || pre[1].StartLine != 2 {
		t.Fatalf("%+v", pre)
	}
	// An empty section before a heading is dropped; a heading-only section is kept.
	empty := Split("\n\n# Only\n")
	if len(empty) != 1 || empty[0].Heading != "Only" {
		t.Fatalf("%+v", empty)
	}
}

func TestExtractSkeleton(t *testing.T) {
	secs := Split(sample)
	m := Extract("pasted", "", scope, secs)
	if m.Metadata.Name != "contribution-process" || m.Spec.Meta.Source != workflowmodel.SourceSpec || m.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed || m.Spec.Meta.Version != "1" {
		t.Fatalf("meta %+v name %q", m.Spec.Meta, m.Metadata.Name)
	}
	var ids, hints []string
	for i, s := range m.Spec.States {
		ids = append(ids, s.ID)
		hints = append(hints, m.Spec.Columns[i].Hint)
		if s.Lifecycle.Confidence != workflowmodel.ConfidenceInferred || s.Origin != workflowmodel.OriginIngested {
			t.Errorf("%s not inferred/ingested", s.ID)
		}
		if len(s.Lifecycle.Citations) != 1 || s.Lifecycle.Citations[0].Path != "spec:Steps#L5-L14" || s.Lifecycle.Citations[0].Line != 6+i {
			t.Errorf("%s citations %+v", s.ID, s.Lifecycle.Citations)
		}
		if i < len(m.Spec.States)-1 && s.Lifecycle.Number != (i+1)*stageStep {
			t.Errorf("%s number %d", s.ID, s.Lifecycle.Number)
		}
	}
	if want := "request triage implement-the-change-in-a-pull-request review-by-two-reviewers approval merge release"; strings.Join(ids, " ") != want {
		t.Fatalf("ids %v", ids)
	}
	if want := "intake triage work review approval merge release"; strings.Join(hints, " ") != want {
		t.Fatalf("hints %v", hints)
	}
	last := m.Spec.States[len(m.Spec.States)-1]
	if last.Type != workflowmodel.StateFinal || !m.Spec.Columns[len(m.Spec.Columns)-1].Terminal || last.Owner != workflowmodel.OwnerExternalObservation {
		t.Fatalf("last %+v", last)
	}
	if m.Spec.States[5].Owner != workflowmodel.OwnerExternalObservation || m.Spec.States[0].Owner != workflowmodel.OwnerHuman {
		t.Fatalf("owners %+v", m.Spec.States)
	}
	var edges []string
	for _, tr := range m.Spec.Transitions {
		edges = append(edges, tr.From+">"+tr.To+"["+tr.GuardRef+"/"+tr.GateRef+"]")
	}
	want := []string{
		"review-by-two-reviewers>approval[/gate-review-by-two-reviewers]",
		"approval>merge[condition-2/gate-approval]",
		"review-by-two-reviewers>implement-the-change-in-a-pull-request[condition-1/]",
	}
	for _, w := range want {
		found := false
		for _, e := range edges {
			found = found || e == w
		}
		if !found {
			t.Errorf("missing edge %s in %v", w, edges)
		}
	}
	if len(m.Spec.Guards) != 2 || m.Spec.Guards[0].Kind != workflowmodel.GuardManual || !strings.HasPrefix(m.Spec.Guards[0].Params["condition"], "If changes are requested") || m.Spec.Guards[0].Source != "spec:Steps#L5-L14" {
		t.Fatalf("guards %+v", m.Spec.Guards)
	}
	gates := map[string]string{}
	for _, g := range m.Spec.Gates {
		gates[g.ID] = g.ReleasedBy
		if g.Confidence != workflowmodel.ConfidenceInferred || len(g.Citations) != 1 {
			t.Errorf("gate %+v", g)
		}
	}
	if gates["gate-review-by-two-reviewers"] != "reviewer" || gates["gate-approval"] != "maintainer" {
		t.Fatalf("gates %v", gates)
	}
	roles := map[string]workflowmodel.RoleDef{}
	for _, r := range m.Spec.Roles {
		roles[r.ID] = r
	}
	for _, id := range []string{"maintainer", "reviewer", "toc", "release-manager", "contributor", "bot"} {
		if _, ok := roles[id]; !ok {
			t.Errorf("role %s missing: %v", id, roles)
		}
	}
	if roles["bot"].Human || roles["bot"].Kind != workflowmodel.RoleBot || roles["contributor"].Kind != workflowmodel.RoleAuthorKind {
		t.Fatalf("roles %+v", roles)
	}
	if m.Metadata.Annotations[AnnotationSource] != "spec:pasted" {
		t.Fatalf("annotations %v", m.Metadata.Annotations)
	}
	if len(m.Spec.Coverage.Findings) != 1 || m.Spec.Coverage.Findings[0].Code != CodeDerivationUnspecified {
		t.Fatalf("findings %+v", m.Spec.Coverage.Findings)
	}
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatalf("skeleton must compile: %v", err)
	}
}

func TestExtractFallbacks(t *testing.T) {
	// Headings as steps when there is no list; a condition tied by mention.
	byHeading := "# Flow\n\n## Propose\nWrite it up.\n\n## Review it\nReviewers comment. When review is done it is accepted.\n\n## Ship\nDone.\n"
	m := Extract("doc.md", "flow", scope, Split(byHeading))
	ids := stateIDs(m)
	if strings.Join(ids, " ") != "propose review-it ship" || m.Metadata.Name != "flow" {
		t.Fatalf("%v %q", ids, m.Metadata.Name)
	}
	if len(m.Spec.Guards) != 1 || m.Spec.Transitions[1].GuardRef != "condition-1" {
		t.Fatalf("guards %+v transitions %+v", m.Spec.Guards, m.Spec.Transitions)
	}
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatal(err)
	}

	// Nothing at all: request -> done with a finding.
	m = Extract("pasted", "", scope, Split("We like to ship when ready."))
	if strings.Join(stateIDs(m), " ") != "request done" || m.Metadata.Name != "document" {
		t.Fatalf("%v %q", stateIDs(m), m.Metadata.Name)
	}
	if !hasFinding(m, CodeNoSteps) || !hasFinding(m, CodeConditionUnplaced) {
		t.Fatalf("findings %+v", m.Spec.Coverage.Findings)
	}
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatal(err)
	}

	// One step gets a synthetic Done; duplicate labels get unique ids; a
	// gate with no role named adds maintainer.
	m = Extract("pasted", "", scope, Split("- Approve\n"))
	if strings.Join(stateIDs(m), " ") != "approve done" || m.Spec.Gates[0].ReleasedBy != "maintainer" || len(m.Spec.Roles) != 1 {
		t.Fatalf("%v %+v %+v", stateIDs(m), m.Spec.Gates, m.Spec.Roles)
	}
	m = Extract("pasted", "", scope, Split("- Review\n- Review\n- **Done**: finished.\n"))
	if strings.Join(stateIDs(m), " ") != "review review-2 done" {
		t.Fatalf("%v", stateIDs(m))
	}
	// Conditions in a section with no steps and no mention are skipped; a
	// condition on the terminal step is skipped.
	m = Extract("pasted", "", scope, Split("# A\n\n1. Start\n2. Finish when happy\n\n# B\n\nIf it rains, wait.\n"))
	if len(m.Spec.Guards) != 0 {
		t.Fatalf("guards %+v", m.Spec.Guards)
	}
	if _, _, err := workflowmodel.Compile(m); err != nil {
		t.Fatal(err)
	}
	// Empty sections slice still yields a model.
	m = Extract("pasted", "", scope, nil)
	if m.Metadata.Name != "document" || len(m.Spec.States) != 2 {
		t.Fatalf("%+v", m)
	}
}

func TestHelpers(t *testing.T) {
	if got := truncate("a very long label that goes on and on and on and on and on and on and on and on", 30); !strings.HasSuffix(got, "…") || len(got) > 34 {
		t.Fatalf("%q", got)
	}
	if got := truncate(strings.Repeat("x", 40), 30); got != strings.Repeat("x", 30)+"…" {
		t.Fatalf("%q", got)
	}
	if cleanLabel("  **Triage** (within a week): maintainers label it. ") != "Triage" {
		t.Fatalf("%q", cleanLabel("  **Triage** (within a week): maintainers label it. "))
	}
	if cleanLabel("") != "" || cleanLabel(": leading") != ": leading" {
		t.Fatal("edge labels")
	}
	seen := map[string]bool{}
	if uniqueID(seen, "") != "step" || uniqueID(seen, "") != "step-2" {
		t.Fatal("uniqueID")
	}
	if hintFor("something else").Column != "work" || hintFor("Deploy to prod").Owner != workflowmodel.OwnerExternalService {
		t.Fatal("hint defaults")
	}
	if !containsWord("the toc votes", "toc") || containsWord("protocol", "toc") {
		t.Fatal("containsWord")
	}
	cands := []candidate{{Label: "Request"}, {Label: "Implement the fix"}, {Label: "Ship"}}
	if stateMentioned("after implementing, ship it", cands, true) != 2 || stateMentioned("on request, go back to implement the fix", cands, false) != 1 {
		t.Fatal("stateMentioned")
	}
	if stateMentioned("nothing here", cands, true) != -1 {
		t.Fatal("no mention")
	}
	if firstStepIn(cands, Section{Heading: "x"}) != -1 {
		t.Fatal("firstStepIn")
	}
}

func TestMarkEnforcesGuarantees(t *testing.T) {
	secs := Split(sample)
	m := &workflowmodel.Model{}
	m.Metadata.Name = "llm-name"
	m.Spec.Pipelines = []workflowmodel.Pipeline{{ID: "p"}}
	m.Spec.Sources = []workflowmodel.Source{{Path: "x"}}
	m.Spec.Agents = []workflowmodel.AgentGrant{{Profile: "a"}}
	m.Spec.States = []workflowmodel.State{
		{ID: "a", Origin: workflowmodel.OriginHuman, Pipelines: []string{"p"}, Lifecycle: workflowmodel.StateLifecycle{Citations: []workflowmodel.Citation{{Path: "spec:Steps#L5-L14", Line: 99}, {Path: "spec:Bogus#L1-L2"}, {Path: ".github/workflows/ci.yml"}}}},
		{ID: "par", Type: workflowmodel.StateParallel, Regions: []workflowmodel.State{{ID: "r"}}},
	}
	m.Spec.Events = []workflowmodel.Event{{ID: "e", Citations: []workflowmodel.Citation{{Path: "spec:Notes#L15-L18", Line: 16}}}}
	m.Spec.Gates = []workflowmodel.Gate{{ID: "g"}}
	m.Spec.Guards = []workflowmodel.Guard{{ID: "c", Source: "nowhere"}, {ID: "d", Source: "spec:Notes#L15-L18"}}
	replaced := Mark(m, "https://x/spec.md", "", scope, secs)
	if replaced != 3 { // par, its region, and the gate
		t.Fatalf("replaced %d", replaced)
	}
	if m.APIVersion != workflowmodel.APIVersion || m.Kind != workflowmodel.KindWorkflowModel || m.Metadata.Name != "llm-name" || m.Spec.Scope.Repository != scope.Repository {
		t.Fatalf("identity %+v", m)
	}
	if m.Spec.Meta.Version != "1" || m.Spec.Meta.Source != workflowmodel.SourceSpec || m.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed || m.Spec.Derivation.Mode != workflowmodel.DerivationBuiltin {
		t.Fatalf("meta %+v", m.Spec.Meta)
	}
	if m.Spec.Pipelines != nil || m.Spec.Sources != nil || m.Spec.Agents != nil || m.Spec.States[0].Pipelines != nil {
		t.Fatal("ingest-owned fields must be stripped")
	}
	a := m.Spec.States[0]
	if a.Origin != workflowmodel.OriginIngested || a.Lifecycle.Confidence != workflowmodel.ConfidenceInferred {
		t.Fatalf("state a %+v", a)
	}
	if len(a.Lifecycle.Citations) != 1 || a.Lifecycle.Citations[0].Path != "spec:Steps#L5-L14" || a.Lifecycle.Citations[0].Line != 5 {
		t.Fatalf("citations %+v", a.Lifecycle.Citations)
	}
	whole := "spec:https://x/spec.md#L1-L18"
	r := m.Spec.States[1].Regions[0]
	if r.Lifecycle.Confidence != workflowmodel.ConfidenceInferred || r.Lifecycle.Citations[0].Path != whole {
		t.Fatalf("region %+v", r)
	}
	if m.Spec.Events[0].Confidence != workflowmodel.ConfidenceInferred || m.Spec.Events[0].Citations[0].Line != 16 {
		t.Fatalf("event %+v", m.Spec.Events[0])
	}
	if m.Spec.Gates[0].Confidence != workflowmodel.ConfidenceInferred || m.Spec.Gates[0].Citations[0].Path != whole {
		t.Fatalf("gate %+v", m.Spec.Gates[0])
	}
	if m.Spec.Guards[0].Source != whole || m.Spec.Guards[1].Source != "spec:Notes#L15-L18" {
		t.Fatalf("guards %+v", m.Spec.Guards)
	}
	if m.Metadata.Annotations[AnnotationSource] != "spec:https://x/spec.md" || m.Spec.Coverage.Files != 1 {
		t.Fatalf("%+v", m.Metadata)
	}
	// Single-section specs cite that section; empty names fall back.
	one := &workflowmodel.Model{}
	Mark(one, "pasted", "", scope, Split("just text"))
	if one.Metadata.Name != "document" {
		t.Fatal(one.Metadata.Name)
	}
	Mark(one, "pasted", "", scope, nil)
	if one.Metadata.Name != "document" {
		t.Fatal(one.Metadata.Name)
	}
}

func TestSchemaAndPrompt(t *testing.T) {
	schema := Schema()
	for _, want := range []string{
		"spec.states: object[]",
		"spec.states.lifecycle.confidence: one of inferred",
		"spec.states.regions: State[] (same shape as above)",
		"spec.states.owner: one of human | registered-agent | external-service | external-observation",
		"spec.transitions.gateRef: string",
		"spec.guards.params: map[string]string",
		"spec.derivation.rules.when.op: one of is | not | has | lacks | true | false",
		"spec.roles.human: bool",
		"metadata.annotations: map[string]string",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema lacks %q\n%s", want, schema)
		}
	}
	for _, banned := range []string{"spec.pipelines", "spec.sources", "spec.agents", "spec.coverage"} {
		if strings.Contains(schema, banned) {
			t.Errorf("schema must omit %s", banned)
		}
	}
	vocab := Vocabulary()
	if !strings.Contains(vocab, "pr.review (scalar)") || !strings.Contains(vocab, "labels (set)") {
		t.Fatalf("%s", vocab)
	}
	user, err := UserPrompt(promptData{Origin: "o", Spec: "SPEC", Skeleton: "{}", Schema: "S", Vocabulary: "V", Sections: Split(sample), Rules: RefusalRules, Diagnostics: []string{"state-no-exit x"}, Name: "n", Forge: "f", Repository: "r"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- spec:Steps#L5-L14", "state-no-exit x", "did not compile", "name \"n\", scope.forge \"f\", scope.repository \"r\"", workflowmodel.CodeGateNoOwner} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !strings.Contains(SystemPrompt, `"confidence": "inferred"`) {
		t.Fatal("system prompt must demand inferred confidence")
	}
}

// goodAnswer is an LLM answer that compiles after marking: it cites real
// sections and declares a gate released by a role.
func goodAnswer(t *testing.T) string {
	t.Helper()
	m := Extract("pasted", "", scope, Split(sample))
	m.Spec.States[0].Label = "Request a change"
	m.Metadata.Name = "ignored-by-mark"
	for i := range m.Spec.States {
		m.Spec.States[i].Lifecycle.Confidence = "" // the LLM "forgot"; Mark restores
	}
	m.Spec.Coverage = workflowmodel.Coverage{}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIngestDeterministicOnly(t *testing.T) {
	res, err := Ingest(context.Background(), sample, Options{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if res.LLM.Used || res.Source != "spec:pasted" || !res.Compiled || res.Profile == nil || len(res.Sections) != 3 {
		t.Fatalf("%+v", res.LLM)
	}
	if _, ok := res.Model.Metadata.Annotations[AnnotationLLMAssisted]; ok {
		t.Fatal("no LLM annotation without the LLM")
	}
	warnings := 0
	for _, d := range res.Diagnostics {
		if d.Code == workflowmodel.CodeInferred {
			warnings++
		}
	}
	if warnings == 0 {
		t.Fatal("inferred elements must be reported")
	}
	if _, err := Ingest(context.Background(), "  \n", Options{}); !errors.Is(err, ErrEmpty) {
		t.Fatal(err)
	}
	if _, err := Ingest(context.Background(), strings.Repeat("x", MaxBytes+1), Options{}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestIngestLLMAccepted(t *testing.T) {
	var systems, users []string
	c := CompleterFunc(func(_ context.Context, system, user string) (string, error) {
		systems = append(systems, system)
		users = append(users, user)
		return "Here you go:\n```json\n" + goodAnswer(t) + "\n```\nThanks!", nil
	})
	res, err := Ingest(context.Background(), sample, Options{Origin: "acme/widgets:docs/process.md", Name: "process", Scope: scope, Completer: c, ModelID: "gw/model-x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LLM.Used || !res.LLM.Accepted || res.LLM.Attempts != 1 || res.LLM.Model != "gw/model-x" || res.LLM.Error != "" {
		t.Fatalf("%+v", res.LLM)
	}
	if !res.Compiled || res.Model.Metadata.Name != "process" || res.Model.Spec.States[0].Label != "Request a change" {
		t.Fatalf("compiled %v name %q", res.Compiled, res.Model.Metadata.Name)
	}
	if res.Model.Metadata.Annotations[AnnotationLLMAssisted] != "LLM-assisted (model gw/model-x)" || res.Model.Metadata.Annotations[AnnotationSource] != "spec:acme/widgets:docs/process.md" {
		t.Fatalf("%v", res.Model.Metadata.Annotations)
	}
	for _, s := range res.Model.Spec.States {
		if s.Lifecycle.Confidence != workflowmodel.ConfidenceInferred {
			t.Fatalf("%s not inferred after LLM", s.ID)
		}
	}
	if len(systems) != 1 || systems[0] != SystemPrompt || !strings.Contains(users[0], "# Specification (acme/widgets:docs/process.md)") || !strings.Contains(users[0], `"name": "process"`) {
		t.Fatalf("prompt wiring: %d calls", len(systems))
	}
}

func TestIngestLLMRetryThenAccept(t *testing.T) {
	calls := 0
	c := CompleterFunc(func(_ context.Context, _, user string) (string, error) {
		calls++
		if calls == 1 {
			// Compiles structurally but a gate names no role: a refusal.
			m := Extract("pasted", "", scope, Split(sample))
			m.Spec.Roles = nil
			b, _ := json.Marshal(m)
			return string(b), nil
		}
		if !strings.Contains(user, "did not compile") || !strings.Contains(user, workflowmodel.CodeUnknownRole) && !strings.Contains(user, workflowmodel.CodeGateNoOwner) {
			t.Errorf("retry prompt must quote diagnostics:\n%s", user[len(user)-800:])
		}
		return goodAnswer(t), nil
	})
	res, err := Ingest(context.Background(), sample, Options{Scope: scope, Completer: c, ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LLM.Accepted || res.LLM.Attempts != 2 || !res.Compiled {
		t.Fatalf("%+v", res.LLM)
	}
}

func TestIngestLLMFallsBack(t *testing.T) {
	cases := map[string]struct {
		answers []string
		err     error
		wantErr string
	}{
		"bad json":       {answers: []string{"{not json", "```\n{\"spec\": }\n```"}, wantErr: "not a Model"},
		"no json":        {answers: []string{"I cannot do that.", ""}, wantErr: "no JSON object"},
		"unknown field":  {answers: []string{`{"apiVersion":"x","spec":{"states":[],"magic":1}}`, `{"bogus":true}`}, wantErr: "not a Model"},
		"never compiles": {answers: []string{`{"spec":{"initial":"a","states":[{"id":"a","label":"A","owner":"human"}]}}`, `{"spec":{"initial":"a","states":[{"id":"a","label":"A","owner":"human"}]}}`}, wantErr: workflowmodel.CodeNoColumn},
		"gateway error":  {err: errors.New("503 upstream"), wantErr: "gateway: 503 upstream"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			c := CompleterFunc(func(context.Context, string, string) (string, error) {
				calls++
				if tc.err != nil {
					return "", tc.err
				}
				return tc.answers[calls-1], nil
			})
			res, err := Ingest(context.Background(), sample, Options{Scope: scope, Completer: c, ModelID: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if !res.LLM.Used || res.LLM.Accepted || !strings.Contains(res.LLM.Error, tc.wantErr) {
				t.Fatalf("%+v", res.LLM)
			}
			wantAttempts := MaxAttempts
			if tc.err != nil {
				wantAttempts = 1
			}
			if res.LLM.Attempts != wantAttempts || calls != wantAttempts {
				t.Fatalf("attempts %d calls %d", res.LLM.Attempts, calls)
			}
			// The skeleton is returned, compiled, with the fallback finding
			// and no LLM annotation.
			if !res.Compiled || !hasFinding(res.Model, CodeLLMFellBack) {
				t.Fatalf("compiled %v findings %+v", res.Compiled, res.Model.Spec.Coverage.Findings)
			}
			if _, ok := res.Model.Metadata.Annotations[AnnotationLLMAssisted]; ok {
				t.Fatal("fallback must not claim LLM assistance")
			}
		})
	}
}

func TestValidateCitationReplacement(t *testing.T) {
	secs := Split(sample)
	m := Extract("pasted", "", scope, secs)
	m.Spec.States[0].Lifecycle.Citations = []workflowmodel.Citation{{Path: "spec:Made up#L1-L3"}}
	b, _ := json.Marshal(m)
	got, diags, err := Validate(string(b), "pasted", "", scope, secs)
	if err != nil || diags != nil {
		t.Fatal(err, diags)
	}
	if !hasFinding(got, CodeLLMCitationReplaced) || got.Spec.States[0].Lifecycle.Citations[0].Path != "spec:pasted#L1-L18" {
		t.Fatalf("%+v %+v", got.Spec.Coverage.Findings, got.Spec.States[0].Lifecycle.Citations)
	}
	// A non-compiling answer returns its diagnostics for the retry prompt.
	m.Spec.Columns = nil
	b, _ = json.Marshal(m)
	_, diags, err = Validate(string(b), "pasted", "", scope, secs)
	if err == nil || len(diags) == 0 {
		t.Fatal("expected diagnostics")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"a\":1}\n```":     `{"a":1}`,
		"text {\"a\":{\"b\":2}} tail": `{"a":{"b":2}}`,
		"```\n{\"a\":1}":              `{"a":1}`,
		"no braces":                   "",
		"}{":                          "",
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func stateIDs(m *workflowmodel.Model) []string {
	var ids []string
	for _, s := range m.Spec.States {
		ids = append(ids, s.ID)
	}
	return ids
}

func hasFinding(m *workflowmodel.Model, code string) bool {
	for _, f := range m.Spec.Coverage.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
