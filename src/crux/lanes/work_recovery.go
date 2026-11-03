package lanes

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go/jetstream"
)

const maxRecoveryHistory = 64

type recoveryRecord struct {
	RequestHash string            `json:"request_hash"`
	Result      api.LaneRecovered `json:"result"`
}

func (b *Broker) pruneGroup(state *groupState, now time.Time) {
	for seq, l := range state.Leases {
		when := l.AckedAt
		if l.Expired {
			when = l.RetiredAt
		}
		if (l.Done || l.Expired) && now.Sub(when) >= b.cfg.AckRetention {
			delete(state.Leases, seq)
		}
	}
	for id, record := range state.Recoveries {
		if now.Sub(record.Result.RecoveredAt) >= b.cfg.AckRetention {
			delete(state.Recoveries, id)
		}
	}
	if state.Recoveries == nil {
		state.Recoveries = map[string]recoveryRecord{}
	}
}

// InspectGroup exposes only this principal's recoverable receipts. Missing
// delivery identities are public within the group, never ownership tokens.
func (b *Broker) InspectGroup(ctx context.Context, project, lane, group string) (api.LaneGroupState, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	if _, err := b.authorize(project, lane, "work", group); err != nil {
		return api.LaneGroupState{}, err
	}
	state, revision, err := b.readGroup(ctx, groupKey(lane, group))
	if err != nil {
		return api.LaneGroupState{}, err
	}
	info, err := b.streams[lane].Info(ctx)
	if err != nil {
		return api.LaneGroupState{}, backend(err)
	}
	out := api.LaneGroupState{Revision: revision, Cursor: state.Cursor,
		FirstRetained: info.State.FirstSeq, LastSequence: info.State.LastSeq,
		Recoverable: []api.LaneReceipt{}, Recoveries: []api.LaneRecovered{}, Retries: []api.LaneRetry{},
		Missing: []api.LaneExpiration{}}
	out.MissingThrough = state.MissingThrough
	out.Slots = state.Slots
	now := b.now().UTC()
	for seq, l := range state.Leases {
		if l.Project == project && !l.Done && !l.Expired && l.Released {
			out.Retries = append(out.Retries, api.LaneRetry{Sequence: seq, Attempt: l.Attempt, AvailableAt: l.NextAttempt})
		}
		if !l.Done && !l.Expired && !l.Expires.After(now) && seq < out.FirstRetained {
			out.Missing = append(out.Missing, api.LaneExpiration{Sequence: seq, Project: l.Project, Attempt: l.Attempt})
			if l.Project == project {
				out.Recoverable = append(out.Recoverable, api.LaneReceipt{Sequence: seq, Receipt: l.Receipt})
			}
		}
	}
	for _, r := range state.Recoveries {
		if now.Sub(r.Result.RecoveredAt) < b.cfg.AckRetention {
			out.Recoveries = append(out.Recoveries, r.Result)
		}
	}
	sort.Slice(out.Recoverable, func(i, j int) bool { return out.Recoverable[i].Sequence < out.Recoverable[j].Sequence })
	sort.Slice(out.Missing, func(i, j int) bool { return out.Missing[i].Sequence < out.Missing[j].Sequence })
	sort.Slice(out.Retries, func(i, j int) bool { return out.Retries[i].Sequence < out.Retries[j].Sequence })
	sort.Slice(out.Recoveries, func(i, j int) bool { return out.Recoveries[i].ID < out.Recoveries[j].ID })
	return out, nil
}

// RecoverGroup is an explicit gap acceptance, not an ACK or cursor reset.
// The caller's supplied KV revision fences claims, renewals and ACKs. A
// persisted request/result makes response-loss retry safe across restarts.
func (b *Broker) RecoverGroup(ctx context.Context, project, lane, group string, in api.LaneRecover) (api.LaneRecovered, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	if _, err := b.authorize(project, lane, "recover", group); err != nil {
		return api.LaneRecovered{}, err
	}
	if !ValidID(in.ID) || in.Revision == 0 || len(in.Expired)+len(in.Missing) > MaxRecoveryBatch {
		return api.LaneRecovered{}, ErrInvalid
	}
	request, err := json.Marshal(in)
	if err != nil {
		return api.LaneRecovered{}, err
	}
	key := groupKey(lane, group)
	state, revision, err := b.readGroup(ctx, key)
	if err != nil {
		return api.LaneRecovered{}, err
	}
	if previous, ok := state.Recoveries[in.ID]; ok {
		if previous.Result.Project != project || previous.RequestHash != digest(request) {
			return api.LaneRecovered{}, ErrConflict
		}
		if b.now().Sub(previous.Result.RecoveredAt) >= b.cfg.AckRetention {
			return api.LaneRecovered{}, ErrStale
		}
		return previous.Result, nil
	}
	if revision != in.Revision {
		return api.LaneRecovered{}, ErrConflict
	}
	now := b.now().UTC()
	b.pruneGroup(&state, now)
	if len(state.Recoveries) >= maxRecoveryHistory {
		return api.LaneRecovered{}, ErrCapacity
	}
	info, err := b.streams[lane].Info(ctx)
	if err != nil {
		return api.LaneRecovered{}, backend(err)
	}
	first := info.State.FirstSeq
	out := api.LaneRecovered{ID: in.ID, Project: project, Cursor: state.Cursor,
		Retired: []uint64{}, Expirations: []api.LaneExpiration{}, RecoveredAt: now}
	if state.Cursor != 0 && first > 0 && state.Cursor < first-1 {
		if in.CursorThrough != first-1 {
			return api.LaneRecovered{}, ErrCursor
		}
		out.GapFrom, out.GapThrough = state.Cursor+1, first-1
		state.Cursor, out.Cursor = first-1, first-1
	} else if in.CursorThrough != state.Cursor {
		// Never advance over retained, unclaimed records.
		return api.LaneRecovered{}, ErrCursor
	}
	seen := map[uint64]bool{}
	for _, receipt := range in.Expired {
		if !validReceipt(receipt.Receipt) || receipt.Sequence == 0 || seen[receipt.Sequence] {
			return api.LaneRecovered{}, ErrInvalid
		}
		seen[receipt.Sequence] = true
		l, ok := state.Leases[receipt.Sequence]
		if !ok || l.Project != project || subtle.ConstantTimeCompare([]byte(l.Receipt), []byte(receipt.Receipt)) != 1 {
			return api.LaneRecovered{}, ErrStale
		}
		expiration, err := b.retireMissing(ctx, &state, lane, receipt.Sequence, first, now)
		if err != nil {
			return api.LaneRecovered{}, err
		}
		out.Retired = append(out.Retired, receipt.Sequence)
		out.Expirations = append(out.Expirations, expiration)
	}
	for _, seq := range in.Missing {
		if seq == 0 || seen[seq] {
			return api.LaneRecovered{}, ErrInvalid
		}
		seen[seq] = true
		expiration, err := b.retireMissing(ctx, &state, lane, seq, first, now)
		if err != nil {
			return api.LaneRecovered{}, err
		}
		out.Retired = append(out.Retired, seq)
		out.Expirations = append(out.Expirations, expiration)
	}
	if len(out.Retired) == 0 && out.GapThrough == 0 {
		return api.LaneRecovered{}, ErrInvalid
	}
	// The prefix checkpoint audits log expiry; it does not ACK or suppress
	// unlisted deliveries. Explicit expiration records release ledger slots.
	if first > 0 && first-1 > state.MissingThrough {
		state.MissingThrough = first - 1
	}
	out.MissingThrough = state.MissingThrough
	sort.Slice(out.Retired, func(i, j int) bool { return out.Retired[i] < out.Retired[j] })
	sort.Slice(out.Expirations, func(i, j int) bool { return out.Expirations[i].Sequence < out.Expirations[j].Sequence })
	state.Recoveries[in.ID] = recoveryRecord{RequestHash: digest(request), Result: out}
	if err := b.saveGroup(ctx, key, state, revision); err != nil {
		return api.LaneRecovered{}, err
	}
	return out, nil
}

func (b *Broker) retireMissing(ctx context.Context, state *groupState, lane string, seq, first uint64, now time.Time) (api.LaneExpiration, error) {
	l, ok := state.Leases[seq]
	if !ok || l.Done || l.Expired || l.Expires.After(now) {
		return api.LaneExpiration{}, ErrStale
	}
	if seq >= first {
		return api.LaneExpiration{}, ErrCursor
	}
	if _, err := b.streams[lane].GetMsg(ctx, seq); !errors.Is(err, jetstream.ErrMsgNotFound) {
		if err != nil {
			return api.LaneExpiration{}, backend(err)
		}
		return api.LaneExpiration{}, ErrCursor
	}
	delete(state.Leases, seq)
	return api.LaneExpiration{Sequence: seq, Project: l.Project, Attempt: l.Attempt}, nil
}
