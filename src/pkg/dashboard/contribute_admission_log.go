package dashboard

import (
	"strconv"
	"strings"
	"time"
)

// admissionSkipLogLimit bounds the per-item memory behind
// admissionSkipChanged. Entries are dropped when their item is admitted, but an
// issue that closes while withheld is never admitted again, so the map is
// cleared outright past this size; the cost is one repeat log per item.
const admissionSkipLogLimit = 4096

// logKey is what an admission refusal says about its item, so the skip log
// can tell a repeat from a change. Volatile detail that does not change the
// answer (the dependency record's generation) is left out.
func (d contributorAdmissionDecision) logKey() string {
	parts := []string{d.reason}
	switch d.reason {
	case contributorAdmissionReasonOpenPRClaim, contributorAdmissionReasonMergedClaimStale:
		parts = append(parts, d.claim.PRURL, strconv.FormatBool(d.claim.MergedPR), d.claim.Source)
	case contributorAdmissionReasonIssueClaim:
		parts = append(parts, d.issueClaim.Identity, d.issueClaim.ExpiresAt.UTC().Format(time.RFC3339))
	case contributorAdmissionReasonIssueChurn:
		parts = append(parts, d.churn.PRNumbers()...)
	case contributorAdmissionReasonWorkflowBlocked, contributorAdmissionReasonLabelSkipped:
		parts = append(parts, d.skippedLabel)
	case contributorAdmissionReasonDependencyBlocked, contributorAdmissionReasonDependencyUnknown:
		parts = append(parts, d.convergence.Reason, d.convergence.ObservedRecord)
		parts = append(parts, d.convergence.Blockers...)
	}
	return strings.Join(parts, "\x00")
}

// admissionSkipChanged reports whether selectTask should log this refusal:
// true the first time an item is withheld for a reason, and again only when
// the reason or its detail changes. selectTask runs for every connected relay
// every 20-40s, and logging each pass buried the Operations feed in the same
// handful of lines.
func (h *ContributeWSHub) admissionSkipChanged(itemKey string, d contributorAdmissionDecision) bool {
	key := d.logKey()
	h.admissionSkipMu.Lock()
	defer h.admissionSkipMu.Unlock()
	if h.admissionSkipLogged[itemKey] == key {
		return false
	}
	if h.admissionSkipLogged == nil || len(h.admissionSkipLogged) >= admissionSkipLogLimit {
		h.admissionSkipLogged = make(map[string]string)
	}
	h.admissionSkipLogged[itemKey] = key
	return true
}

// admissionSkipCleared forgets an item's last logged refusal once it is
// admitted, so withholding it again later is logged again.
func (h *ContributeWSHub) admissionSkipCleared(itemKey string) {
	h.admissionSkipMu.Lock()
	defer h.admissionSkipMu.Unlock()
	delete(h.admissionSkipLogged, itemKey)
}
