package planning

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	MetaPlanSpecRevision = "spec_revision"
	MetaSpecRevision     = "current_spec_revision"
	MetaRunWaitingOn     = "waiting_on"
	MetaRunWaitingReason = "waiting_reason"

	WaitingReasonStalePlan = "stale_plan"
)

// RecordPlanSpecRevisionFromReceipt records the spec revision a finalized plan
// consumed. #8301 will eventually replace current-spec lookup with the
// document_status/updated_at status verb; until then the receipt input_revision
// is the provenance carrier.
func RecordPlanSpecRevisionFromReceipt(store *beads.Store, epicID string, receipt outputschema.StageReceipt) error {
	if strings.TrimSpace(receipt.Stage) != worksource.RunStagePlan {
		return nil
	}
	switch receipt.ResultClass {
	case outputschema.ReceiptResultCompleted, outputschema.ReceiptResultNoChange:
	default:
		return nil
	}
	rev := strings.TrimSpace(receipt.InputRevision)
	if rev == "" {
		return fmt.Errorf("planning: plan receipt for epic %s has no input_revision", epicID)
	}
	if err := store.SetMetadata(epicID, MetaPlanSpecRevision, rev); err != nil {
		return fmt.Errorf("planning: recording plan spec revision on epic %s: %w", epicID, err)
	}
	return nil
}

// GuardImplementStage refuses implementation when the approved plan was built
// from a different spec revision than the current spec artifact. It only reads
// bead metadata and writes the existing waiting_on human hold metadata; no new
// store, CRD, DSL, or credential path is introduced.
func GuardImplementStage(store *beads.Store, epicID string) (bool, error) {
	epic, err := loadEpic(store, epicID)
	if err != nil {
		return false, err
	}
	planRevision := strings.TrimSpace(epic.Meta(MetaPlanSpecRevision))
	specRevision := strings.TrimSpace(epic.Meta(MetaSpecRevision))
	if planRevision == "" || specRevision == "" || planRevision == specRevision {
		_ = clearStalePlanHold(store, epicID)
		return true, nil
	}
	if err := store.Update(epicID, func(b *beads.Bead) {
		if b.Metadata == nil {
			b.Metadata = map[string]interface{}{}
		}
		b.Metadata[MetaRunWaitingOn] = worksource.RunWaitingOnHuman
		b.Metadata[MetaRunWaitingReason] = WaitingReasonStalePlan
	}); err != nil {
		return false, fmt.Errorf("planning: marking stale plan hold on epic %s: %w", epicID, err)
	}
	return false, nil
}

func clearStalePlanHold(store *beads.Store, epicID string) error {
	return store.Update(epicID, func(b *beads.Bead) {
		if b.Metadata == nil || b.Metadata[MetaRunWaitingReason] != WaitingReasonStalePlan {
			return
		}
		delete(b.Metadata, MetaRunWaitingReason)
		if b.Metadata[MetaRunWaitingOn] == worksource.RunWaitingOnHuman {
			delete(b.Metadata, MetaRunWaitingOn)
		}
	})
}
