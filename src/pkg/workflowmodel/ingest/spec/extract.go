package spec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Finding codes added by the deterministic stage.
const (
	// CodeNoSteps says no step list or headings were found, so a minimal
	// request -> done skeleton was produced.
	CodeNoSteps = "spec-no-steps"
	// CodeDerivationUnspecified says the spec does not say how item state
	// is derived from forge facts; builtin derivation is assumed.
	CodeDerivationUnspecified = "spec-derivation-unspecified"
	// CodeConditionUnplaced says a conditional sentence could not be tied
	// to a step and was recorded on the section's first transition.
	CodeConditionUnplaced = "spec-condition-unplaced"
)

// Annotation keys set on ingested models.
const (
	AnnotationSource      = "workflow.hivecommons.dev/source"
	AnnotationLLMAssisted = "workflow.hivecommons.dev/llm-assisted"
)

const stageStep = 10

// columnHint maps a keyword found in a step to a column hint and the
// owner that usually acts there.
type columnHint struct {
	Column string
	Owner  workflowmodel.Owner
}

// hintTable is scanned in order; the first keyword present wins.
var hintTable = []struct {
	Words []string
	Hint  columnHint
}{
	{[]string{"triage", "prioriti"}, columnHint{"triage", workflowmodel.OwnerHuman}},
	{[]string{"intake", "request", "submit", "file", "open an issue", "propose", "proposal"}, columnHint{"intake", workflowmodel.OwnerHuman}},
	{[]string{"approve", "approval", "lgtm", "sign-off", "sign off", "vote"}, columnHint{"approval", workflowmodel.OwnerHuman}},
	{[]string{"review", "feedback", "comment"}, columnHint{"review", workflowmodel.OwnerHuman}},
	{[]string{"implement", "develop", "draft", "write", "fix", "design", "prototype"}, columnHint{"work", workflowmodel.OwnerHuman}},
	{[]string{"ci ", "ci.", "pipeline", "build", "test", "lint", "check"}, columnHint{"ci", workflowmodel.OwnerExternalService}},
	{[]string{"merge"}, columnHint{"merge", workflowmodel.OwnerExternalObservation}},
	{[]string{"publish", "release", "tag", "announce"}, columnHint{"release", workflowmodel.OwnerExternalObservation}},
	{[]string{"deploy", "roll out", "rollout", "ship"}, columnHint{"deploy", workflowmodel.OwnerExternalService}},
	{[]string{"close", "done", "complete", "archive", "finish", "resolved"}, columnHint{"done", workflowmodel.OwnerExternalObservation}},
}

// roleTable maps role nouns to role definitions.
var roleTable = []struct {
	Words []string
	Role  workflowmodel.RoleDef
}{
	{[]string{"maintainer", "maintainers"}, workflowmodel.RoleDef{ID: "maintainer", Label: "Maintainer", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "maintain"}, Human: true}},
	{[]string{"reviewer", "reviewers"}, workflowmodel.RoleDef{ID: "reviewer", Label: "Reviewer", Kind: workflowmodel.RoleFrozenSet, Params: map[string]string{"frozenAt": "review-requested"}, Human: true}},
	{[]string{"approver", "approvers"}, workflowmodel.RoleDef{ID: "approver", Label: "Approver", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "write"}, Human: true}},
	{[]string{"toc", "technical oversight committee"}, workflowmodel.RoleDef{ID: "toc", Label: "TOC", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "admin"}, Human: true}},
	{[]string{"steering committee", "steering"}, workflowmodel.RoleDef{ID: "steering-committee", Label: "Steering committee", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "admin"}, Human: true}},
	{[]string{"chair", "chairs"}, workflowmodel.RoleDef{ID: "chair", Label: "Chair", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "admin"}, Human: true}},
	{[]string{"owner", "owners", "code owner", "codeowners"}, workflowmodel.RoleDef{ID: "owner", Label: "Owner", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "admin"}, Human: true}},
	{[]string{"committer", "committers"}, workflowmodel.RoleDef{ID: "committer", Label: "Committer", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "write"}, Human: true}},
	{[]string{"release manager", "release managers"}, workflowmodel.RoleDef{ID: "release-manager", Label: "Release manager", Kind: workflowmodel.RolePermission, Params: map[string]string{"permission": "maintain"}, Human: true}},
	{[]string{"author", "authors", "submitter", "requester"}, workflowmodel.RoleDef{ID: "author", Label: "Author", Kind: workflowmodel.RoleAuthorKind, Params: map[string]string{"kind": "human"}, Human: true}},
	{[]string{"contributor", "contributors"}, workflowmodel.RoleDef{ID: "contributor", Label: "Contributor", Kind: workflowmodel.RoleAuthorKind, Params: map[string]string{"kind": "human"}, Human: true}},
	{[]string{"bot", "bots", "automation"}, workflowmodel.RoleDef{ID: "bot", Label: "Bot", Kind: workflowmodel.RoleBot, Params: map[string]string{"login": ""}}},
}

// gateWords mark a step whose exit a person must release.
var gateWords = []string{"approve", "approval", "review", "lgtm", "sign-off", "sign off", "vote", "accept"}

// gateReleaserPreference is the order roles are tried for a gate.
var gateReleaserPreference = []string{"reviewer", "approver", "maintainer", "toc", "steering-committee", "chair", "owner", "committer", "release-manager"}

var (
	listItem     = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*+])\s+(.+?)\s*$`)
	conditional  = regexp.MustCompile(`(?i)\b(if|when|after|unless|once|until)\b`)
	sentenceEnd  = regexp.MustCompile(`[.!?]\s+|[.!?]$`)
	backWords    = regexp.MustCompile(`(?i)\b(back|return|returned|reject|rejected|request changes|changes requested|reopen)\b`)
	labelCutoff  = regexp.MustCompile(`[:.;](\s|$)|\s[(—–-]`)
	emphasis     = strings.NewReplacer("**", "", "__", "", "`", "", "*", "")
	wordBoundary = regexp.MustCompile(`[^a-z0-9]+`)
)

const maxLabelLen = 60

// candidate is one step pulled from the spec.
type candidate struct {
	Label string
	// Text is the whole list item, for role and condition detection.
	Text    string
	Section Section
	Line    int
}

// block is a paragraph or one list item with the line it starts on.
type block struct {
	Text string
	Line int
	// Item indexes the candidate this block is, or -1 for prose.
	Item int
}

// Extract runs the deterministic stage: a compiled skeleton model built
// from the sections without any LLM. The skeleton always compiles.
func Extract(origin, name string, scope workflowmodel.Scope, secs []Section) *workflowmodel.Model {
	cands := steps(secs)
	var findings []workflowmodel.Finding
	if len(cands) == 0 {
		cands = headingSteps(secs)
	}
	if len(cands) == 0 {
		first := firstSection(secs)
		cands = []candidate{{Label: "Request", Section: first, Line: first.StartLine}, {Label: "Done", Section: first, Line: first.StartLine}}
		findings = append(findings, workflowmodel.Finding{Path: first.Ref(), Code: CodeNoSteps, Severity: workflowmodel.SeverityWarning, Detail: "no step list or headings found; produced a request -> done skeleton"})
	}
	if len(cands) == 1 {
		c := cands[0]
		cands = append(cands, candidate{Label: "Done", Section: c.Section, Line: c.Line})
	}

	m := &workflowmodel.Model{APIVersion: workflowmodel.APIVersion, Kind: workflowmodel.KindWorkflowModel}
	m.Metadata.Name = name
	if m.Metadata.Name == "" {
		m.Metadata.Name = workflowmodel.Slug(firstSection(secs).Heading)
	}
	m.Spec.Scope = scope
	m.Spec.Meta = workflowmodel.Meta{Version: "1", Description: "Draft ingested from written spec " + origin, Source: workflowmodel.SourceSpec, Provenance: workflowmodel.ProvenanceIngestedUnreviewed}
	m.Spec.Derivation = workflowmodel.Derivation{Mode: workflowmodel.DerivationBuiltin}
	findings = append(findings, workflowmodel.Finding{Path: "spec:" + origin, Code: CodeDerivationUnspecified, Severity: workflowmodel.SeverityWarning, Detail: "the spec does not say how item state is derived from forge facts; builtin derivation assumed"})

	// Roles from nouns anywhere in the text.
	all := strings.ToLower(joinText(secs))
	roles := map[string]bool{}
	for _, r := range roleTable {
		for _, w := range r.Words {
			if containsWord(all, w) {
				if !roles[r.Role.ID] {
					roles[r.Role.ID] = true
					m.Spec.Roles = append(m.Spec.Roles, cloneRole(r.Role))
				}
				break
			}
		}
	}

	// States and columns, one column per step, last step terminal.
	ids := map[string]bool{}
	var stateIDs []string
	for i, c := range cands {
		id := uniqueID(ids, workflowmodel.Slug(c.Label))
		stateIDs = append(stateIDs, id)
		hint := hintFor(c.Label)
		last := i == len(cands)-1
		st := workflowmodel.State{ID: id, Label: c.Label, Owner: hint.Owner, Origin: workflowmodel.OriginIngested}
		if last {
			st.Type = workflowmodel.StateFinal
			st.Owner = workflowmodel.OwnerExternalObservation
		}
		st.Lifecycle = workflowmodel.StateLifecycle{Column: id, Confidence: workflowmodel.ConfidenceInferred, Citations: []workflowmodel.Citation{cite(c)}}
		if !last {
			st.Lifecycle.Number = (i + 1) * stageStep
		}
		m.Spec.States = append(m.Spec.States, st)
		m.Spec.Columns = append(m.Spec.Columns, workflowmodel.Column{ID: id, Label: c.Label, Hint: hint.Column, States: []string{id}, Terminal: last})
	}
	m.Spec.Initial = stateIDs[0]

	// Chain transitions; gates on steps a person must release.
	for i := 0; i+1 < len(cands); i++ {
		tr := workflowmodel.Transition{From: stateIDs[i], To: stateIDs[i+1], On: "advance-to-" + stateIDs[i+1], Note: "next step in " + cands[i].Section.Heading}
		if hasAny(strings.ToLower(cands[i].Label), gateWords) {
			g := workflowmodel.Gate{ID: "gate-" + stateIDs[i], ReleasedBy: releaser(m, roles, cands[i].Text), Confidence: workflowmodel.ConfidenceInferred, Citations: []workflowmodel.Citation{cite(cands[i])}}
			m.Spec.Gates = append(m.Spec.Gates, g)
			tr.GateRef = g.ID
		}
		m.Spec.Transitions = append(m.Spec.Transitions, tr)
	}

	// Conditional sentences become manual guards on the transition out
	// of the step they mention, and back edges when they speak of
	// returning to an earlier step.
	guardN := 0
	for _, sec := range secs {
		for _, blk := range blocks(sec, cands) {
			for _, sent := range sentences(blk.Text) {
				if !conditional.MatchString(sent) {
					continue
				}
				from := blk.Item
				placed := from >= 0
				if !placed {
					from = stateMentioned(sent, cands, true)
					placed = from >= 0
				}
				if !placed {
					from = firstStepIn(cands, sec)
				}
				if from < 0 || from == len(stateIDs)-1 {
					continue
				}
				guardN++
				g := workflowmodel.Guard{ID: fmt.Sprintf("condition-%d", guardN), Kind: workflowmodel.GuardManual, Params: map[string]string{"condition": strings.TrimSpace(sent)}, Source: sec.Ref()}
				m.Spec.Guards = append(m.Spec.Guards, g)
				if !placed {
					findings = append(findings, workflowmodel.Finding{Path: sec.Ref(), Node: stateIDs[from], Code: CodeConditionUnplaced, Severity: workflowmodel.SeverityWarning, Detail: "condition names no step; attached to " + stateIDs[from] + ": " + truncate(sent, maxLabelLen)})
				}
				if back := stateMentioned(sent, cands, false); back >= 0 && back < from && backWords.MatchString(sent) {
					m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: stateIDs[from], To: stateIDs[back], On: "return-to-" + stateIDs[back], GuardRef: g.ID, Note: truncate(sent, maxLabelLen)})
					continue
				}
				for i := range m.Spec.Transitions {
					t := &m.Spec.Transitions[i]
					if t.From == stateIDs[from] && t.GuardRef == "" && t.To == stateIDs[from+1] {
						t.GuardRef = g.ID
						break
					}
				}
			}
		}
	}

	m.Metadata.Annotations = map[string]string{AnnotationSource: "spec:" + origin}
	m.Spec.Coverage = workflowmodel.Coverage{Files: 1, Parsed: 1, Findings: findings}
	return m
}

// steps collects list items across sections in order.
func steps(secs []Section) []candidate {
	var out []candidate
	for _, s := range secs {
		for i, line := range s.Lines {
			m := listItem.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			label := cleanLabel(m[1])
			if label == "" {
				continue
			}
			out = append(out, candidate{Label: label, Text: strings.TrimSpace(m[1]), Section: s, Line: s.StartLine + i})
		}
	}
	return out
}

// headingSteps uses sub-headings as steps when there is no list.
func headingSteps(secs []Section) []candidate {
	var out []candidate
	for _, s := range secs {
		if s.Level >= 2 {
			out = append(out, candidate{Label: cleanLabel(s.Heading), Text: s.Heading + " " + strings.Join(s.Lines, " "), Section: s, Line: s.StartLine})
		}
	}
	return out
}

func firstSection(secs []Section) Section {
	if len(secs) == 0 {
		return Section{Heading: "Document", StartLine: 1, EndLine: 1}
	}
	return secs[0]
}

func firstStepIn(cands []candidate, sec Section) int {
	for i, c := range cands {
		if c.Section.Heading == sec.Heading && c.Section.StartLine == sec.StartLine {
			return i
		}
	}
	return -1
}

// stateMentioned finds the step whose label words appear in the sentence.
// With preferFirst the earliest mention wins; otherwise the latest one.
func stateMentioned(sent string, cands []candidate, preferFirst bool) int {
	low := strings.ToLower(sent)
	best, bestPos := -1, -1
	for i, c := range cands {
		key := strings.ToLower(c.Label)
		pos := strings.Index(low, key)
		if pos < 0 {
			// Fall back to the leading word of the label when it is distinctive.
			words := strings.Fields(wordBoundary.ReplaceAllString(key, " "))
			if len(words) == 0 || len(words[0]) < 5 || !containsWord(low, words[0]) {
				continue
			}
			pos = strings.Index(low, words[0])
		}
		if best < 0 || (preferFirst && pos < bestPos) || (!preferFirst && pos > bestPos) {
			best, bestPos = i, pos
		}
	}
	return best
}

// blocks splits a section into list items (tied to their candidate) and
// prose paragraphs, each with its starting line.
func blocks(sec Section, cands []candidate) []block {
	var out []block
	var para []string
	paraLine := 0
	flush := func() {
		if len(para) > 0 {
			out = append(out, block{Text: strings.Join(para, " "), Line: paraLine, Item: -1})
			para = nil
		}
	}
	for i, line := range sec.Lines {
		n := sec.StartLine + i
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if listItem.MatchString(line) {
			flush()
			item := -1
			for ci, c := range cands {
				if c.Line == n && c.Section.StartLine == sec.StartLine {
					item = ci
				}
			}
			out = append(out, block{Text: strings.TrimSpace(listItem.FindStringSubmatch(line)[1]), Line: n, Item: item})
			continue
		}
		if len(para) == 0 {
			paraLine = n
		}
		para = append(para, strings.TrimSpace(line))
	}
	flush()
	return out
}

func sentences(text string) []string {
	var out []string
	for _, s := range sentenceEnd.Split(text, -1) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func cleanLabel(s string) string {
	s = strings.TrimSpace(emphasis.Replace(s))
	if loc := labelCutoff.FindStringIndex(s); loc != nil && loc[0] > 0 {
		s = s[:loc[0]]
	}
	return truncate(strings.TrimSpace(s), maxLabelLen)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], " ")
	if cut < n/2 {
		cut = n
	}
	return strings.TrimRight(s[:cut], " ,;:") + "…"
}

func hintFor(label string) columnHint {
	low := strings.ReplaceAll(strings.ToLower(label), "pull request", "pr")
	for _, h := range hintTable {
		if hasAny(low, h.Words) {
			return h.Hint
		}
	}
	return columnHint{"work", workflowmodel.OwnerHuman}
}

func hasAny(low string, words []string) bool {
	for _, w := range words {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

func containsWord(low, w string) bool {
	padded := " " + wordBoundary.ReplaceAllString(low, " ") + " "
	return strings.Contains(padded, " "+w+" ")
}

func uniqueID(seen map[string]bool, id string) string {
	if id == "" {
		id = "step"
	}
	base, n := id, 2
	for seen[id] {
		id = fmt.Sprintf("%s-%d", base, n)
		n++
	}
	seen[id] = true
	return id
}

func releaser(m *workflowmodel.Model, roles map[string]bool, text string) string {
	low := strings.ToLower(text)
	for _, r := range roleTable {
		if !roles[r.Role.ID] || !r.Role.Human {
			continue
		}
		for _, w := range r.Words {
			if containsWord(low, w) {
				return r.Role.ID
			}
		}
	}
	for _, id := range gateReleaserPreference {
		if roles[id] {
			return id
		}
	}
	for _, r := range roleTable {
		if r.Role.ID == "maintainer" {
			roles["maintainer"] = true
			m.Spec.Roles = append(m.Spec.Roles, cloneRole(r.Role))
		}
	}
	return "maintainer"
}

func cloneRole(r workflowmodel.RoleDef) workflowmodel.RoleDef {
	p := map[string]string{}
	for k, v := range r.Params {
		p[k] = v
	}
	r.Params = p
	return r
}

func cite(c candidate) workflowmodel.Citation {
	return workflowmodel.Citation{Path: c.Section.Ref(), Line: c.Line, Text: truncate(c.Label, maxLabelLen)}
}

func joinText(secs []Section) string {
	var b strings.Builder
	for _, s := range secs {
		b.WriteString(s.Heading)
		b.WriteByte('\n')
		b.WriteString(strings.Join(s.Lines, "\n"))
		b.WriteByte('\n')
	}
	return b.String()
}
