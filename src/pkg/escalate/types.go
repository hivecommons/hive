package escalate

import (
	"context"
	"strings"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityDecision Severity = "decision"
	SeverityPage     Severity = "page"
)

type Event struct {
	Severity Severity
	Title    string
	Body     string
	Link     string
}

type Sink interface {
	Name() string
	Deliver(ctx context.Context, ev Event) error
}

func ParseSeverity(s string) (Severity, bool) {
	switch Severity(strings.ToLower(strings.TrimSpace(s))) {
	case SeverityInfo:
		return SeverityInfo, true
	case SeverityDecision:
		return SeverityDecision, true
	case SeverityPage:
		return SeverityPage, true
	default:
		return "", false
	}
}

func SeverityAtLeast(got, min Severity) bool {
	return severityRank(got) >= severityRank(min)
}

func severityRank(s Severity) int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityDecision:
		return 2
	case SeverityPage:
		return 3
	default:
		return 0
	}
}

func (e Event) valid() bool {
	return severityRank(e.Severity) > 0 && strings.TrimSpace(e.Title) != ""
}
