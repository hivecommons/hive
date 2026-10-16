package github

import "strings"

// The un-park sweep and the dashboard's "your turn" decision must agree on
// which labels park an issue on a person. Ownership reads the operator's
// project.issue_filter.hard_suppress_labels, so these helpers do too: a label
// counts when it is one of the built-in names Hive applies itself, or when the
// configured filter places it in the needs_human, needs_direction or
// needs_decision bucket. needs_spec is deliberately outside the un-park
// commands and never counts here.

// isParkingLabel reports whether label parks an issue on a person, under the
// built-in names or the operator's renamed ones.
func (c *Client) isParkingLabel(label string) bool {
	if isIssueParkingLabel(label) {
		return true
	}
	switch c.configuredParkingBucket(label) {
	case issueNeedsHumanLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel:
		return true
	}
	return false
}

// hasParkingLabel reports whether any of labels parks the issue on a person.
func (c *Client) hasParkingLabel(labels []string) bool {
	for _, label := range labels {
		if c.isParkingLabel(label) {
			return true
		}
	}
	return false
}

// hasNeedsHumanLabel reports whether labels carry `needs-human` or a label the
// operator configured for the needs_human bucket.
func (c *Client) hasNeedsHumanLabel(labels []string) bool {
	if hasIssueNeedsHumanLabel(labels) {
		return true
	}
	for _, label := range labels {
		if c.configuredParkingBucket(label) == issueNeedsHumanLabel {
			return true
		}
	}
	return false
}

// parkingLabelsPresent returns the issue's own labels that park it, spelled
// as the issue carries them so a removal hits the exact label.
func (c *Client) parkingLabelsPresent(labels []string) []string {
	var out []string
	for _, label := range labels {
		if strings.TrimSpace(label) != "" && c.isParkingLabel(label) {
			out = append(out, label)
		}
	}
	return out
}

// configuredParkingBucket returns the hard-suppress bucket the configured
// issue filter places label in, or "" when no filter classifies it.
func (c *Client) configuredParkingBucket(label string) string {
	label = strings.TrimSpace(label)
	if c == nil || label == "" {
		return ""
	}
	classifier, ok := c.getIssueFilter().(HardSuppressClassifier)
	if !ok {
		return ""
	}
	return classifier.HardSuppressIssueBucket([]string{label})
}
