package hub

import (
	"github.com/hivecommons/hive/pkg/crux"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func QuotaExhaustedProcessCount(statuses map[string]*crux.Process) int {
	return spoke.QuotaExhaustedProcessCount(statuses)
}

func QuotaExhaustedAgentReason(count int) string { return spoke.QuotaExhaustedAgentReason(count) }
