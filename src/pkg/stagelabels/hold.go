package stagelabels

import (
	"strconv"
	"strings"
	"time"
)

// HoldProvenance records who set the `hold` label on an item and why, so the
// stage tracker row, the 085 entry comment and the decision popover can say
// "Held by hive · reporter trust (issue #15017 by @Karibhou, contributor)"
// instead of a bare "hold +1" (hivecommons/hive#15020). The stage-label
// pass reads it from the newest `hold` label event's actor and, for the
// hive's own holds, from the notice comment the gate left.
type HoldProvenance struct {
	// By is "hive" when the hive's own GitHub App set the label, else the
	// lower-cased login of the person (or other app) who did; "" unknown.
	By string `json:"by,omitempty"`
	// Source names the hive gate behind a hive-set hold: HoldSourceReporterTrust
	// for the reporter-trust gate (#9665), HoldSourceLabel for a hold the
	// hive can only attribute to the label event.
	Source string `json:"source,omitempty"`
	// Reason is the plain-words cause ("reporter trust (issue #N by @X,
	// contributor)"), "" when only the label is known.
	Reason string `json:"reason,omitempty"`
	// Since is when the current hold was applied (the label event time, or
	// the notice time when no event was readable).
	Since time.Time `json:"since,omitempty"`
	// Issue / Reporter / Association detail a reporter-trust hold: the
	// linked issue the gate judged, its reporter and their association.
	Issue       int    `json:"issue,omitempty"`
	Reporter    string `json:"reporter,omitempty"`
	Association string `json:"association,omitempty"`
}

// Hold sources.
const (
	HoldByHive              = "hive"
	HoldSourceReporterTrust = "reporter-trust"
	HoldSourceLabel         = "label"
)

// SelfImposed reports whether the hive itself set this hold.
func (h *HoldProvenance) SelfImposed() bool {
	return h != nil && strings.EqualFold(strings.TrimSpace(h.By), HoldByHive)
}

// ReporterTrust reports whether the hold came from the reporter-trust gate.
func (h *HoldProvenance) ReporterTrust() bool {
	return h != nil && strings.EqualFold(strings.TrimSpace(h.Source), HoldSourceReporterTrust)
}

// Pill is the tracker pill text: "Held by hive · reporter trust (issue #N
// by @X, contributor)", "Held by @octocat", or "" when nothing is known.
func (h *HoldProvenance) Pill() string {
	if h == nil || strings.TrimSpace(h.By) == "" {
		return ""
	}
	who := "@" + strings.TrimSpace(h.By)
	if h.SelfImposed() {
		who = HoldByHive
	}
	s := "Held by " + who
	if reason := strings.TrimSpace(h.Reason); reason != "" {
		s += " · " + reason
	}
	return s
}

// Clause is the sentence fragment the 085 comment and the popover use for
// "the `hold` label was set by hive: reporter trust (…)"; "" when unknown.
func (h *HoldProvenance) Clause() string {
	if h == nil || strings.TrimSpace(h.By) == "" {
		return ""
	}
	who := "@" + strings.TrimSpace(h.By)
	if h.SelfImposed() {
		who = "the hive itself"
	}
	s := "the `hold` label was set by " + who
	if reason := strings.TrimSpace(h.Reason); reason != "" {
		s += ": " + reason
	}
	return s
}

// ReporterTrustReason renders the gate's finding the way the pill and the
// comment print it: "reporter trust (issue #N by @X, contributor)".
func ReporterTrustReason(issue int, reporter, association string) string {
	var parts []string
	if issue > 0 {
		parts = append(parts, "issue #"+strconv.Itoa(issue))
	}
	if reporter = strings.TrimSpace(reporter); reporter != "" {
		parts = append(parts, "by @"+reporter)
	}
	detail := strings.Join(parts, " ")
	if assoc := strings.ToLower(strings.TrimSpace(association)); assoc != "" {
		if detail != "" {
			detail += ", "
		}
		detail += strings.ReplaceAll(assoc, "_", " ")
	}
	if detail == "" {
		return "reporter trust"
	}
	return "reporter trust (" + detail + ")"
}

// Equal reports whether two provenances say the same thing (Since compared
// to the second, so a re-read of the same label event is a no-op).
func (h *HoldProvenance) Equal(o *HoldProvenance) bool {
	if h == nil || o == nil {
		return h == o
	}
	return h.By == o.By && h.Source == o.Source && h.Reason == o.Reason && h.Issue == o.Issue &&
		h.Reporter == o.Reporter && h.Association == o.Association && h.Since.Truncate(time.Second).Equal(o.Since.Truncate(time.Second))
}
