package tekton

import (
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Pipelines-as-Code annotation and label keys read from PipelineRun
// metadata.
const (
	pacPrefix              = "pipelinesascode.tekton.dev/"
	pacOnEvent             = pacPrefix + "on-event"
	pacOnTargetBranch      = pacPrefix + "on-target-branch"
	pacOnCELExpression     = pacPrefix + "on-cel-expression"
	pacOnPathChange        = pacPrefix + "on-path-change"
	pacEventTypeLabel      = pacPrefix + "event-type"
	tagRefPrefix           = "refs/tags/"
	tagGlobMarker          = "v*"
	eventPullRequest       = "pull_request"
	eventPush              = "push"
	eventIncoming          = "incoming"
	eventCEL               = "cel"
	eventWebhook           = "webhook"
	celPullRequestFragment = `event == "pull_request"`
	celPushFragment        = `event == "push"`
)

// pacTriggers derives triggers from a PipelineRun's Pipelines-as-Code
// annotations and the event-type label PaC stamps on runs.
func pacTriggers(doc map[string]any) []workflowmodel.Trigger {
	md := mapAt(doc, "metadata")
	ann := stringMap(mapAt(md, "annotations"))
	labels := stringMap(mapAt(md, "labels"))

	events := parsePaCList(ann[pacOnEvent])
	if et := labels[pacEventTypeLabel]; et != "" && !contains(events, et) {
		events = append(events, et)
	}
	branches := parsePaCList(ann[pacOnTargetBranch])
	paths := parsePaCList(ann[pacOnPathChange])

	var out []workflowmodel.Trigger
	for _, e := range events {
		out = append(out, workflowmodel.Trigger{Event: e, Branches: branches, Paths: paths})
	}
	if cel := strings.TrimSpace(ann[pacOnCELExpression]); cel != "" {
		out = append(out, workflowmodel.Trigger{Event: eventCEL, Types: []string{cel}})
		for _, f := range celFragments {
			if strings.Contains(cel, f.fragment) && !contains(events, f.event) {
				events = append(events, f.event)
				out = append(out, workflowmodel.Trigger{Event: f.event})
			}
		}
	}
	return out
}

// celFragments are the CEL sub-expressions that pin an on-cel-expression
// to a concrete event, checked in this order.
var celFragments = []struct{ fragment, event string }{
	{celPullRequestFragment, eventPullRequest},
	{celPushFragment, eventPush},
}

// parsePaCList parses the PaC bracketed list syntax: "[a, b]" or "a".
// Brackets and quotes are stripped, entries are trimmed, empties dropped.
func parsePaCList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// roleFor derives the role from triggers: pull_request gates PRs; a push
// on a tag-looking branch is a release; a plain push verifies the branch;
// incoming is manual; anything else (webhook only, none) is other.
func roleFor(triggers []workflowmodel.Trigger) workflowmodel.Role {
	var push *workflowmodel.Trigger
	hasIncoming := false
	for i := range triggers {
		switch triggers[i].Event {
		case eventPullRequest:
			return workflowmodel.RolePRVerify
		case eventPush:
			if push == nil {
				push = &triggers[i]
			}
		case eventIncoming:
			hasIncoming = true
		}
	}
	switch {
	case push != nil && anyTagBranch(push.Branches):
		return workflowmodel.RoleRelease
	case push != nil:
		return workflowmodel.RolePushVerify
	case hasIncoming:
		return workflowmodel.RoleManual
	}
	return workflowmodel.RoleOther
}

func anyTagBranch(branches []string) bool {
	for _, b := range branches {
		if strings.HasPrefix(b, tagRefPrefix) || strings.Contains(b, tagGlobMarker) {
			return true
		}
	}
	return false
}

func stringMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
