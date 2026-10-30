// Package issueshape is the Hive-side name for the shape validators that
// agent-authored GitHub issue and comment content must pass. The validators
// live in the Crux ring as pkg/crux/issueshape (stdlib only) because the
// proxy enforces them on an agent's raw POST /repos/{owner}/{repo}/issues;
// this package re-exports them so the issue-request watcher (pkg/github) and
// pkg/classify keep their import path: Crux owns, Hive aliases
// (hivecommons/hive#13536).
package issueshape

import cruxissueshape "github.com/hivecommons/hive/crux/issueshape"

var (
	UnsubstitutedTemplatePlaceholder = cruxissueshape.UnsubstitutedTemplatePlaceholder
	BodyHasMisEscapedNewlines        = cruxissueshape.BodyHasMisEscapedNewlines
	UnchosenOptionList               = cruxissueshape.UnchosenOptionList
	TitleFor                         = cruxissueshape.TitleFor
	HasAgentPrefix                   = cruxissueshape.HasAgentPrefix
	AgentPrefix                      = cruxissueshape.AgentPrefix
	StripAgentPrefix                 = cruxissueshape.StripAgentPrefix
	SameTitle                        = cruxissueshape.SameTitle
)
