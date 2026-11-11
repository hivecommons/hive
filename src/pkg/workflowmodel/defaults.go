package workflowmodel

import (
	"fmt"
	"sort"
	"strings"
)

// State ids of the hive-default process graph. These are the columns the
// Activity Monitor draws today (agent-now.js STAGE_TRACKER_PATH =
// [0,10,20,30,50,80] + the merged cap); the ids are stable so a bound
// model keeps meaning the same thing to the dashboard.
const (
	StateOpen         = "open"
	StateActionable   = "actionable"
	StateClaimed      = "claimed"
	StatePRInProgress = "pr-in-progress"
	StateVerify       = "verify"
	StateReview       = "review"
	StateCI           = "ci"
	StateMergeQueue   = "merge-queue"
	StateMerged       = "merged"
	StateMainCI       = "main-ci"
	StateRelease      = "release"
	StateDone         = "done"
)

// Guard ids of the hive-default process graph.
const (
	GuardIDRequiredChecks = "required-checks-satisfied"
	GuardIDReviewPolicy   = "review-policy-satisfied"
)

// DeployStateID is the id of the deploy state for an environment.
func DeployStateID(env string) string { return "deploy-" + slug(env) }

// HiveDefault is the process graph every hive runs today, as a model.
// Binding no pipelines into it changes nothing; that is the compatibility
// promise of #17620.
func HiveDefault(scope Scope) *Model {
	m := New("hive-default", scope)
	m.Spec.Initial = StateOpen
	m.Spec.States = []State{
		{ID: StateOpen, Label: "Open", Owner: OwnerHuman, Origin: OriginHiveDefault},
		{ID: StateActionable, Label: "Actionable", Owner: OwnerHuman, Origin: OriginHiveDefault},
		{ID: StateClaimed, Label: "Claimed", Owner: OwnerRegisteredAgent, Origin: OriginHiveDefault},
		{ID: StatePRInProgress, Label: "PR in progress", Owner: OwnerRegisteredAgent, Origin: OriginHiveDefault},
		{ID: StateVerify, Label: "CI / Review", Owner: OwnerExternalService, Type: StateParallel, Origin: OriginHiveDefault, Regions: []State{
			{ID: StateReview, Label: "Review", Owner: OwnerHuman, GuardRef: GuardIDReviewPolicy, Origin: OriginHiveDefault},
			{ID: StateCI, Label: "CI", Owner: OwnerExternalService, GuardRef: GuardIDRequiredChecks, Origin: OriginHiveDefault},
		}},
		{ID: StateMergeQueue, Label: "Merge queue", Owner: OwnerExternalService, Origin: OriginHiveDefault},
		{ID: StateMerged, Label: "Merged", Owner: OwnerExternalObservation, Type: StateFinal, Origin: OriginHiveDefault},
	}
	m.Spec.Transitions = []Transition{
		{From: StateOpen, To: StateActionable, On: "triaged"},
		{From: StateActionable, To: StateClaimed, On: "claimed"},
		{From: StateClaimed, To: StatePRInProgress, On: "pr-opened"},
		{From: StatePRInProgress, To: StateVerify, On: "ready-for-review"},
		{From: StateVerify, To: StateMergeQueue, On: "verified", GuardRef: GuardIDRequiredChecks},
		{From: StateMergeQueue, To: StateMerged, On: "merged"},
	}
	m.Spec.Guards = []Guard{
		{ID: GuardIDReviewPolicy, Kind: GuardReviewPolicy},
		{ID: GuardIDRequiredChecks, Kind: GuardRequiredChecks},
	}
	m.Spec.Lanes = []Lane{
		{ID: "implement", Label: "Implement", StateRef: StateClaimed, Actor: OwnerRegisteredAgent, Effects: []string{"branch", "pr"}},
		{ID: "review", Label: "Review", StateRef: StateReview, Actor: OwnerRegisteredAgent, Effects: []string{"review"}},
		{ID: "repair", Label: "Repair", StateRef: StateCI, Actor: OwnerRegisteredAgent, Effects: []string{"push"}},
	}
	return m
}

// Bind composes ingested pipelines into the hive-default process graph:
// PR-verify pipelines fill the CI region; push-verify, release and deploy
// pipelines add post-merge states, in that order, each only when present.
// Scheduled and manual pipelines are kept in Spec.Pipelines but bind to no
// state. Sources and coverage are recorded as given.
func Bind(scope Scope, pipelines []Pipeline, sources []Source, cov Coverage) *Model {
	m := HiveDefault(scope)
	m.Metadata.Name = slug(scope.Repository)
	pipelines = DedupeIDs(pipelines)
	m.Spec.Pipelines = pipelines
	m.Spec.Sources = append([]Source(nil), sources...)
	m.Spec.Coverage = cov
	if len(pipelines) == 0 {
		return m
	}
	m.Metadata.Annotations = map[string]string{"workflow.hivecommons.dev/bound": "true"}

	byRole := map[Role][]string{}
	envs := map[string][]string{}
	for _, p := range pipelines {
		for _, r := range BindingRoles(p) {
			byRole[r] = append(byRole[r], p.ID)
		}
		if p.Role == RoleDeploy {
			for _, n := range p.Nodes {
				switch {
				case n.Environment == "":
				case IsExpression(n.Environment):
					// A computed environment name is one deploy state, not a
					// column per possible value; the finding keeps it visible.
					m.Spec.Coverage.Findings = append(m.Spec.Coverage.Findings, Finding{Path: p.Path, Node: n.ID, Code: CodeEnvironmentDynamic, Detail: n.Environment, Severity: SeverityWarning})
				default:
					envs[n.Environment] = appendUnique(envs[n.Environment], p.ID)
				}
			}
		}
	}
	if ci := m.FindState(StateCI); ci != nil && len(byRole[RolePRVerify]) > 0 {
		ci.Pipelines = byRole[RolePRVerify]
		ci.Origin = OriginIngested
	}

	var post []State
	if ids := byRole[RolePushVerify]; len(ids) > 0 {
		post = append(post, State{ID: StateMainCI, Label: "Main CI", Owner: OwnerExternalService, Pipelines: ids, Origin: OriginIngested})
	}
	if ids := byRole[RoleRelease]; len(ids) > 0 {
		post = append(post, State{ID: StateRelease, Label: "Release", Owner: OwnerExternalService, Pipelines: ids, Origin: OriginIngested})
	}
	envNames := make([]string, 0, len(envs))
	for e := range envs {
		envNames = append(envNames, e)
	}
	sort.Strings(envNames)
	for _, e := range envNames {
		post = append(post, State{ID: DeployStateID(e), Label: "Deploy: " + e, Owner: OwnerExternalService, GuardRef: envGuardID(e), Pipelines: envs[e], Origin: OriginIngested})
		m.Spec.Guards = append(m.Spec.Guards, Guard{ID: envGuardID(e), Kind: GuardEnvironmentApproval, Params: map[string]string{"environment": e}, Source: string(SystemGitHubActions)})
	}
	if deployNoEnv := withoutEnv(pipelines, byRole[RoleDeploy]); len(deployNoEnv) > 0 {
		post = append(post, State{ID: "deploy", Label: "Deploy", Owner: OwnerExternalService, Pipelines: deployNoEnv, Origin: OriginIngested})
	}
	if len(post) == 0 {
		return m
	}
	// Merged stops being final; the ingested post-merge chain follows it.
	merged := m.FindState(StateMerged)
	merged.Type = StateSequential
	prev := StateMerged
	for _, s := range post {
		m.Spec.States = append(m.Spec.States, s)
		m.Spec.Transitions = append(m.Spec.Transitions, Transition{From: prev, To: s.ID, On: "pipeline-succeeded", GuardRef: s.GuardRef})
		prev = s.ID
	}
	m.Spec.States = append(m.Spec.States, State{ID: StateDone, Label: "Done", Owner: OwnerExternalObservation, Type: StateFinal, Origin: OriginIngested})
	m.Spec.Transitions = append(m.Spec.Transitions, Transition{From: prev, To: StateDone, On: "pipeline-succeeded"})
	return m
}

// BindingRoles is every state role a pipeline binds to: its primary Role
// plus the secondary ones its triggers imply, because one workflow often
// serves several (`on: [pull_request, push]` verifies PRs and the default
// branch; a PaC pipeline shared by pull-request, push and tag runs does
// all three). Order: primary first, then push-verify, then release.
func BindingRoles(p Pipeline) []Role {
	roles := []Role{p.Role}
	add := func(r Role) {
		for _, x := range roles {
			if x == r {
				return
			}
		}
		roles = append(roles, r)
	}
	for _, t := range p.Triggers {
		switch t.Event {
		case "push":
			if len(positive(t.Tags)) > 0 {
				add(RoleRelease)
			}
			if len(positive(t.Tags)) == 0 || len(positive(t.Branches)) > 0 {
				add(RolePushVerify)
			}
		case "release":
			add(RoleRelease)
		}
	}
	return roles
}

// positive drops "!"-prefixed (ignore) filters.
func positive(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !strings.HasPrefix(x, "!") {
			out = append(out, x)
		}
	}
	return out
}

func withoutEnv(all []Pipeline, ids []string) []string {
	var out []string
	for _, id := range ids {
		for _, p := range all {
			if p.ID != id {
				continue
			}
			hasEnv := false
			for _, n := range p.Nodes {
				if n.Environment != "" && !IsExpression(n.Environment) {
					hasEnv = true
					break
				}
			}
			if !hasEnv {
				out = append(out, id)
			}
		}
	}
	return out
}

// CodeEnvironmentDynamic is the finding for a deploy environment whose
// name is an expression.
const CodeEnvironmentDynamic = "environment-dynamic"

// IsExpression reports whether s is a GitHub Actions `${{ }}` or Tekton
// `$( )` expression rather than a literal.
func IsExpression(s string) bool {
	return strings.Contains(s, "${{") || strings.Contains(s, "$(")
}

// DedupeIDs makes pipeline ids unique in order: a second pipeline whose id
// collides (for example `_stale.yml` and `stale.yml` both slug to the same
// id) gets a "-2", "-3" suffix. Deterministic for a given input order.
func DedupeIDs(pipelines []Pipeline) []Pipeline {
	out := append([]Pipeline(nil), pipelines...)
	seen := map[string]int{}
	for i := range out {
		base := out[i].ID
		seen[base]++
		if seen[base] == 1 {
			continue
		}
		for {
			candidate := fmt.Sprintf("%s-%d", base, seen[base])
			if seen[candidate] == 0 {
				seen[candidate] = 1
				out[i].ID = candidate
				break
			}
			seen[base]++
		}
	}
	return out
}

func envGuardID(env string) string { return "environment-approved-" + slug(env) }

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// slug lowercases s and keeps [a-z0-9-], collapsing runs of other
// characters to one hyphen; empty input yields "x".
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if out == "" {
		return "x"
	}
	return out
}

// Slug is slug, exported for ingesters that need stable ids from names.
func Slug(s string) string { return slug(s) }

// PipelineID is the stable id of a pipeline from its system and path.
func PipelineID(system System, path string) string {
	return fmt.Sprintf("%s-%s", shortSystem(system), slug(strings.TrimSuffix(strings.TrimSuffix(path, ".yml"), ".yaml")))
}

func shortSystem(s System) string {
	switch s {
	case SystemGitHubActions:
		return "gha"
	case SystemTekton:
		return "tkn"
	}
	return slug(string(s))
}
