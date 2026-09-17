package proxy

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// TestProxyGrantsPRReviewOnConverse pins the premise that
// Manager.AuthorizeReviewRequest is aligned to (hivecommons/hive#7485).
//
// The review-request relay grants a review to an ADVISORY+converse agent
// BECAUSE this proxy rule already does — the relay is the audited path, so it
// must never be stricter than the unaudited one. If someone drops the
// Capability grant from the `/pulls/{n}/reviews` rule, that justification is
// gone and the relay silently becomes the more permissive route. Fail here
// rather than let the two drift apart unnoticed.
func TestProxyGrantsPRReviewOnConverse(t *testing.T) {
	const path = "/repos/projectbluefin/common/pulls/971/reviews"
	converse := agent.AgentCapabilities{Converse: true}

	if !AllowedByModeCaps(agent.ModeAdvisory, converse, "POST", path) {
		t.Errorf("AllowedByModeCaps(ADVISORY, converse, POST, %s) = false, want true — "+
			"pkg/agent.Manager.AuthorizeReviewRequest relies on this grant existing", path)
	}

	// And converse must remain the thing that grants it: an advisory agent
	// WITHOUT converse is still refused, so the rule is not simply open.
	if AllowedByModeCaps(agent.ModeAdvisory, agent.AgentCapabilities{}, "POST", path) {
		t.Errorf("AllowedByModeCaps(ADVISORY, no caps, POST, %s) = true, want false — "+
			"review access must come from converse, not from ADVISORY alone", path)
	}
}

// TestProxyKeepsMergeOffConverse guards the boundary that makes the relay
// change safe to ship at ACMM L5: converse is conversation-only. If converse
// ever started granting a merge, AuthorizeReviewRequest's "conversation-only"
// reasoning would be wrong.
func TestProxyKeepsMergeOffConverse(t *testing.T) {
	const mergePath = "/repos/projectbluefin/common/pulls/971/merge"
	converse := agent.AgentCapabilities{Converse: true}

	if AllowedByModeCaps(agent.ModeAdvisory, converse, "PUT", mergePath) {
		t.Errorf("AllowedByModeCaps(ADVISORY, converse, PUT, %s) = true, want false — "+
			"converse must never grant merge", mergePath)
	}
}
