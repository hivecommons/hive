package lanes

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	groupStateFormat = 3
	minGroupSlots    = 1
)

var completionBudget = sync.OnceValues(buildCompletionBudget)

// Reserve the exact serialized maximum of every allowed recovery record and
// every field of every delivery, including terminal/retry timestamps. Padding
// allocates that headroom in the KV bucket at admission, not just in memory.
// An empty poll reserves its first delivery slot. Slot high-water marks never
// shrink: pruning and recovery make slots reusable without growing the value.
// Completion responses occupy already reserved history.
func buildCompletionBudget() (int, int, error) {
	worst, worstLease, err := maximumGroupShape()
	if err != nil {
		return 0, 0, err
	}
	entry, err := json.Marshal(map[uint64]lease{^uint64(0): worstLease})
	if err != nil {
		return 0, 0, err
	}
	leaseBytes := len(entry) - len("{}") + len(",")
	base, err := json.Marshal(worst)
	maxHeaderGrowth := revisionHeaderGrowth(^uint64(0))
	return len(base) + maxHeaderGrowth, leaseBytes, err
}

func maximumGroupShape() (groupState, lease, error) {
	longest, err := time.Parse(time.RFC3339Nano, "9999-12-31T23:59:59.999999999Z")
	if err != nil {
		return groupState{}, lease{}, err
	}
	maximum := ^uint64(0)
	project := strings.Repeat("p", MaxIDLength)
	worstLease := lease{Receipt: strings.Repeat("f", receiptBytes*2), Project: project,
		Expires: longest, Attempt: maximum, AckedAt: longest, NextAttempt: longest,
		Expired: true, RetiredAt: longest}
	worst := groupState{Cursor: maximum, MissingThrough: maximum, RetryCursor: maximum,
		RetryTurn: true, Slots: MaxLedgerEntries, Leases: map[uint64]lease{}, Recoveries: map[string]recoveryRecord{}}
	for n := 0; n < maxRecoveryHistory; n++ {
		suffix := strconv.Itoa(n)
		id := strings.Repeat("r", MaxIDLength-len(suffix)) + suffix
		result := api.LaneRecovered{ID: id, Project: project, Cursor: maximum,
			GapFrom: maximum, GapThrough: maximum, MissingThrough: maximum, RecoveredAt: longest}
		for item := 0; item < MaxRecoveryBatch; item++ {
			seq := maximum - uint64(MaxRecoveryBatch-1-item)
			result.Retired = append(result.Retired, seq)
			result.Expirations = append(result.Expirations, api.LaneExpiration{
				Sequence: seq, Project: project, Attempt: maximum})
		}
		worst.Recoveries[id] = recoveryRecord{RequestHash: strings.Repeat("f", receiptBytes*2), Result: result}
	}
	return worst, worstLease, nil
}

func revisionHeaderGrowth(revision uint64) int {
	return len(strconv.FormatUint(revision, 10)) - len("0")
}

func groupReservation(slots int) (int, error) {
	base, delivery, err := completionBudget()
	if err != nil {
		return 0, err
	}
	if slots < 0 || slots > MaxLedgerEntries {
		return 0, ErrCapacity
	}
	return base + slots*delivery, nil
}

func encodeGroup(state groupState, revision uint64) ([]byte, error) {
	if state.Slots < 0 {
		return nil, ErrConflict
	}
	state.Slots = max(state.Slots, len(state.Leases), minGroupSlots)
	state.Padding = ""
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateBytes {
		return nil, ErrCapacity
	}
	if err := validateGroup(state); err != nil {
		return nil, err
	}
	reserved, err := groupReservation(state.Slots)
	if err != nil {
		return nil, err
	}
	if reserved > maxStateBytes {
		return nil, ErrCapacity
	}
	// The pinned KV driver stores the decimal expected-revision CAS header
	// on Create and Update. Compensate its growth byte-for-byte in padding,
	// so aggregate bucket capacity cannot strand an existing group's ACK.
	valueBytes := reserved - revisionHeaderGrowth(revision)
	if len(data) > valueBytes {
		return nil, ErrConflict
	}
	state.Padding = strings.Repeat(" ", valueBytes-len(data))
	return json.Marshal(state)
}

func validateGroup(state groupState) error {
	if state.Leases == nil {
		return ErrConflict
	}
	if state.Slots < minGroupSlots || state.Slots < len(state.Leases) {
		return ErrConflict
	}
	if state.Slots > MaxLedgerEntries || len(state.Recoveries) > maxRecoveryHistory {
		return ErrCapacity
	}
	for seq, l := range state.Leases {
		if seq == 0 || !ValidID(l.Project) || !validReceipt(l.Receipt) ||
			l.Attempt == 0 || l.Expires.IsZero() || (l.Done && l.AckedAt.IsZero()) ||
			(l.Released && l.NextAttempt.IsZero()) || (l.Expired && l.RetiredAt.IsZero()) {
			return ErrConflict
		}
		for _, stamp := range []time.Time{l.Expires, l.AckedAt, l.NextAttempt, l.RetiredAt} {
			if stamp.Location() != time.UTC {
				return ErrConflict
			}
		}
	}
	for id, record := range state.Recoveries {
		r := record.Result
		if !ValidID(id) || r.ID != id || !validReceipt(record.RequestHash) || !ValidID(r.Project) ||
			r.RecoveredAt.IsZero() || r.RecoveredAt.Location() != time.UTC || len(r.Retired) > MaxRecoveryBatch ||
			len(r.Expirations) != len(r.Retired) {
			return ErrConflict
		}
		for index, expiration := range r.Expirations {
			if expiration.Sequence == 0 || expiration.Sequence != r.Retired[index] ||
				!ValidID(expiration.Project) || expiration.Attempt == 0 ||
				(index > 0 && r.Retired[index-1] >= expiration.Sequence) {
				return ErrConflict
			}
		}
	}
	return nil
}
