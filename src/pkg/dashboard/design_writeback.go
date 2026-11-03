package dashboard

import (
	"context"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

// These fields are stamped only from the verified owner request, not from the
// design label or the fact that a Spektacular run exists.
const (
	designWritebackRequestedBy  = "design_writeback_requested_by"
	designWritebackRequestedRef = "design_writeback_requested_ref"
)

func designWritebackContext(ctx context.Context, epic *beads.Bead) context.Context {
	if epic == nil || epic.Meta(planning.MetaDesignVia) != planning.DesignViaSpektacular {
		return ctx
	}
	ref := issueWorkRef(issueFromEpic(epic))
	if epic.Meta(designWritebackRequestedRef) != ref.Key() {
		return ctx
	}
	return worksource.WithSolicitedWriteback(ctx, ref, epic.Meta(designWritebackRequestedBy))
}
