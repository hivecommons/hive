package ingest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// owners collects who approves and reviews from CODEOWNERS (GitHub) and
// OWNERS / OWNERS_ALIASES (Prow). Only the repository-root files are
// read: nested OWNERS scope approvers to subtrees, which the model does
// not express.
type owners struct {
	approvers, reviewers map[string]bool
	maintainers          map[string]bool
	aliases              map[string][]string
	rawApprovers         []string
	rawReviewers         []string
	cites                []workflowmodel.Citation
	maintainerCites      []workflowmodel.Citation
	prow                 bool
}

func (o *owners) init() {
	if o.approvers == nil {
		o.approvers = map[string]bool{}
		o.reviewers = map[string]bool{}
		o.maintainers = map[string]bool{}
		o.aliases = map[string][]string{}
	}
}

func (o *owners) empty() bool {
	return len(o.approvers) == 0 && len(o.reviewers) == 0 && len(o.maintainers) == 0
}

// maintainerHandle matches a GitHub handle written as @handle (not the
// local part of an e-mail address) or as a github.com/handle link in a
// MAINTAINERS file.
var maintainerHandle = regexp.MustCompile(`(?:^|[^\w.])(?:@|https?://github\.com/)([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))`)

// addMaintainers reads the handles named in a MAINTAINERS file (plain
// list, markdown table or prose). Only @handle and github.com/handle
// spellings count: names and e-mail addresses are not GitHub identities.
func (o *owners) addMaintainers(p string, data []byte) {
	o.init()
	first := 0
	for i, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || (strings.HasPrefix(s, "#") && !strings.Contains(s, "@")) {
			continue
		}
		for _, m := range maintainerHandle.FindAllStringSubmatch(line, -1) {
			o.maintainers[m[1]] = true
			if first == 0 {
				first = i + 1
			}
		}
	}
	if first > 0 {
		o.maintainerCites = append(o.maintainerCites, cite(p, first, "MAINTAINERS"))
	}
}

// addCodeowners reads every owner on every rule line as an approver:
// GitHub requests their review and, under require-code-owner-review,
// needs their approval.
func (o *owners) addCodeowners(p string, data []byte) {
	o.init()
	first := 0
	for i, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) < 2 {
			continue
		}
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "#") {
				break
			}
			o.approvers[strings.TrimPrefix(f, "@")] = true
			if first == 0 {
				first = i + 1
			}
		}
	}
	if first > 0 {
		o.cites = append(o.cites, cite(p, first, "CODEOWNERS"))
	}
}

// prowOwners is the subset of a Prow OWNERS file the model reads.
type prowOwners struct {
	Approvers []string `yaml:"approvers"`
	Reviewers []string `yaml:"reviewers"`
	Filters   map[string]struct {
		Approvers []string `yaml:"approvers"`
		Reviewers []string `yaml:"reviewers"`
	} `yaml:"filters"`
}

// addOwners reads the root OWNERS file's approvers and reviewers (the
// top-level lists plus the catch-all ".*" filter).
func (o *owners) addOwners(p string, data []byte) {
	o.init()
	var po prowOwners
	if err := yaml.Unmarshal(data, &po); err != nil {
		return
	}
	o.rawApprovers = append(o.rawApprovers, po.Approvers...)
	o.rawReviewers = append(o.rawReviewers, po.Reviewers...)
	if f, ok := po.Filters[".*"]; ok {
		o.rawApprovers = append(o.rawApprovers, f.Approvers...)
		o.rawReviewers = append(o.rawReviewers, f.Reviewers...)
	}
	if len(o.rawApprovers)+len(o.rawReviewers) > 0 {
		o.prow = true
		o.cites = append(o.cites, cite(p, keyLine(data, "approvers", "reviewers", "filters"), "OWNERS"))
	}
}

// addAliases reads OWNERS_ALIASES so alias names in OWNERS expand to
// their members.
func (o *owners) addAliases(p string, data []byte) {
	o.init()
	var a struct {
		Aliases map[string][]string `yaml:"aliases"`
	}
	if err := yaml.Unmarshal(data, &a); err != nil {
		return
	}
	for k, v := range a.Aliases {
		o.aliases[k] = v
	}
	if len(a.Aliases) > 0 {
		o.cites = append(o.cites, cite(p, keyLine(data, "aliases"), "OWNERS_ALIASES"))
	}
}

// resolve expands aliases into the approver and reviewer sets.
func (o *owners) resolve() {
	if o.approvers == nil {
		return
	}
	expand := func(raw []string, into map[string]bool) {
		for _, n := range raw {
			if members, ok := o.aliases[n]; ok {
				for _, m := range members {
					into[strings.TrimPrefix(m, "@")] = true
				}
				continue
			}
			into[strings.TrimPrefix(n, "@")] = true
		}
	}
	expand(o.rawApprovers, o.approvers)
	expand(o.rawReviewers, o.reviewers)
}

func members(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for m := range set {
		if m != "" {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// keyLine returns the 1-based line of the first top-level key among keys
// in a YAML document, or 1.
func keyLine(data []byte, keys ...string) int {
	root := yamlRoot(data)
	if root == nil || root.Kind != yaml.MappingNode {
		return 1
	}
	for _, k := range keys {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == k {
				return root.Content[i].Line
			}
		}
	}
	return 1
}

// applyOwners adds the approver and reviewer roles and, when there is an
// approver role, the review gate on the edges from CI / Review into the
// Merge queue. It returns the role ids it added.
func applyOwners(m *workflowmodel.Model, o owners) map[string]bool {
	roles := map[string]bool{}
	if o.empty() {
		return roles
	}
	source := citeSources(o.cites)
	if a := members(o.approvers); len(a) > 0 {
		m.Spec.Roles = append(m.Spec.Roles, workflowmodel.RoleDef{ID: RoleApprover, Label: "Approver", Kind: workflowmodel.RoleMembers, Params: map[string]string{"members": strings.Join(a, ","), "source": source}, Human: true})
		roles[RoleApprover] = true
	}
	if r := members(o.reviewers); len(r) > 0 {
		m.Spec.Roles = append(m.Spec.Roles, workflowmodel.RoleDef{ID: RoleReviewer, Label: "Reviewer", Kind: workflowmodel.RoleMembers, Params: map[string]string{"members": strings.Join(r, ","), "source": source}, Human: true})
		roles[RoleReviewer] = true
	}
	if roles[RoleApprover] {
		g := ensureReviewGate(m, RoleApprover)
		g.Citations = append(g.Citations, o.cites...)
		if o.prow {
			g.Reply = "/approve"
		}
	}
	if mt := members(o.maintainers); len(mt) > 0 {
		applyMaintainers(m, mt, o.maintainerCites)
		roles[hivedefault.RoleMaintainer] = true
		if !roles[RoleApprover] {
			g := ensureReviewGate(m, hivedefault.RoleMaintainer)
			g.Citations = append(g.Citations, o.maintainerCites...)
		}
	}
	return roles
}

// applyMaintainers turns the hive-default permission-based maintainer
// role into the members the MAINTAINERS file names (adding the role when
// the model has none), keeping the gates that already reference it.
func applyMaintainers(m *workflowmodel.Model, mt []string, cites []workflowmodel.Citation) {
	params := map[string]string{"members": strings.Join(mt, ","), "source": citeSources(cites)}
	for i := range m.Spec.Roles {
		if m.Spec.Roles[i].ID == hivedefault.RoleMaintainer {
			m.Spec.Roles[i].Kind = workflowmodel.RoleMembers
			m.Spec.Roles[i].Params = params
			m.Spec.Roles[i].Human = true
			return
		}
	}
	m.Spec.Roles = append(m.Spec.Roles, workflowmodel.RoleDef{ID: hivedefault.RoleMaintainer, Label: "Maintainer", Kind: workflowmodel.RoleMembers, Params: params, Human: true})
}

// citeSources joins citations as the "path:line,..." source string roles
// and checks carry.
func citeSources(cites []workflowmodel.Citation) string {
	sources := make([]string, 0, len(cites))
	for _, c := range cites {
		sources = append(sources, fmt.Sprintf("%s:%d", c.Path, c.Line))
	}
	return strings.Join(sources, ",")
}

// ensureReviewGate returns the review gate, adding it (released by role,
// condition pr.review is approved) and attaching it to the merge edges
// when absent.
func ensureReviewGate(m *workflowmodel.Model, role string) *workflowmodel.Gate {
	for i := range m.Spec.Gates {
		if m.Spec.Gates[i].ID == GateReview {
			return &m.Spec.Gates[i]
		}
	}
	m.Spec.Gates = append(m.Spec.Gates, workflowmodel.Gate{
		ID: GateReview, ReleasedBy: role,
		Conditions: []workflowmodel.Predicate{{Fact: "pr.review", Op: workflowmodel.OpIs, Value: "approved"}},
		Confidence: workflowmodel.ConfidenceInferred,
	})
	for _, i := range mergeTransitions(m) {
		if m.Spec.Transitions[i].GateRef == "" {
			m.Spec.Transitions[i].GateRef = GateReview
		}
	}
	return &m.Spec.Gates[len(m.Spec.Gates)-1]
}
