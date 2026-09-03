package tracing

import (
	"context"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/timeline"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	AttrGenAISystem            = "gen_ai.system"
	AttrGenAIRequestModel      = "gen_ai.request.model"
	AttrGenAIUsageInputTokens  = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"

	AttrHiveAgent        = "hive.agent"
	AttrHiveLane         = "hive.lane"
	AttrHiveACMMLevel    = "hive.acmm_level"
	AttrHiveGovernorMode = "hive.governor.mode"
	// AttrHiveComponent is the coarse package/component a span belongs to,
	// derived from the span-name prefix before the first dot ("governor",
	// "agent", "pr", ...). It is the phase-1 reach dimension for #3973: a PR's
	// changed files map to components, and (hive.commit, hive.component) on
	// span data answers "did that component ever run on the merged commit".
	AttrHiveComponent = "hive.component"

	attrEventKind = "hive.timeline.kind"
	attrEventID   = "event.id"
	attrIssueRef  = "issue.ref"
	attrPRNumber  = "pr.number"
	attrPRURL     = "pr.url"
)

const (
	eventAttrSystem       = "gen_ai.system"
	eventAttrModel        = "model"
	eventAttrInputTokens  = "input_tokens"
	eventAttrOutputTokens = "output_tokens"
	eventAttrLane         = "lane"
	eventAttrACMMLevel    = "acmm_level"
	eventAttrGovMode      = "governor_mode"
	eventAttrPRNumber     = "pr"
	eventAttrPRURL        = "pr_url"
)

var timelineSpanNames = map[timeline.Kind]string{
	timeline.KindEnumerated: "governor.enumerate",
	timeline.KindClassified: "governor.classify",
	timeline.KindKicked:     "agent.kick",
	timeline.KindPROpened:   "pr.opened",
	timeline.KindMerged:     "pr.merged",
	timeline.KindBlocked:    "issue.blocked",
}

// TimelineSpanName returns the canonical span name for a lifecycle event kind.
func TimelineSpanName(e timeline.Event) string {
	if name, ok := timelineSpanNames[e.Kind]; ok {
		return name
	}
	return "hive.timeline"
}

// TimelineSpanAttributes maps hive lifecycle events to OTLP attributes. Agent
// and model fields use OTel GenAI semantic convention attribute names where the
// event carries model/backend usage data; hive-specific context uses hive.*.
func TimelineSpanAttributes(e timeline.Event) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(attrEventKind, string(e.Kind))}
	if e.ID != "" {
		attrs = append(attrs, attribute.String(attrEventID, e.ID))
	}
	if e.IssueRef != "" {
		attrs = append(attrs, attribute.String(attrIssueRef, e.IssueRef))
	}
	if e.Agent != "" {
		attrs = append(attrs, attribute.String(AttrHiveAgent, e.Agent))
	}
	for k, v := range e.Attrs {
		switch k {
		case eventAttrSystem:
			attrs = append(attrs, attribute.String(AttrGenAISystem, v))
		case eventAttrModel:
			attrs = append(attrs, attribute.String(AttrGenAIRequestModel, v))
		case eventAttrInputTokens:
			if n, ok := parseIntAttr(v); ok {
				attrs = append(attrs, attribute.Int(AttrGenAIUsageInputTokens, n))
			}
		case eventAttrOutputTokens:
			if n, ok := parseIntAttr(v); ok {
				attrs = append(attrs, attribute.Int(AttrGenAIUsageOutputTokens, n))
			}
		case eventAttrLane:
			attrs = append(attrs, attribute.String(AttrHiveLane, v))
		case eventAttrACMMLevel:
			if n, ok := parseIntAttr(v); ok {
				attrs = append(attrs, attribute.Int(AttrHiveACMMLevel, n))
			}
		case eventAttrGovMode:
			attrs = append(attrs, attribute.String(AttrHiveGovernorMode, v))
		case eventAttrPRNumber:
			attrs = append(attrs, attribute.String(attrPRNumber, v))
		case eventAttrPRURL:
			attrs = append(attrs, attribute.String(attrPRURL, v))
		}
	}
	return attrs
}

// StartTimelineSpan starts a span for an existing dashboard lifecycle event.
func StartTimelineSpan(ctx context.Context, e timeline.Event) (context.Context, trace.Span) {
	return StartSpan(ctx, TimelineSpanName(e), TimelineSpanAttributes(e)...)
}

// AgentKickAttributes returns common attributes for governor→agent kick spans.
func AgentKickAttributes(agent, backend, model, lane, governorMode string, acmmLevel int) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(AttrHiveAgent, agent)}
	if backend != "" {
		attrs = append(attrs, attribute.String(AttrGenAISystem, backend))
	}
	if model != "" {
		attrs = append(attrs, attribute.String(AttrGenAIRequestModel, model))
	}
	if lane != "" {
		attrs = append(attrs, attribute.String(AttrHiveLane, lane))
	}
	if acmmLevel > 0 {
		attrs = append(attrs, attribute.Int(AttrHiveACMMLevel, acmmLevel))
	}
	if governorMode != "" {
		attrs = append(attrs, attribute.String(AttrHiveGovernorMode, governorMode))
	}
	return attrs
}

func parseIntAttr(v string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	return n, err == nil
}
