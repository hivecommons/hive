package meetings

import (
	"context"
	"errors"
	"time"
)

// Deleting meetings (the dashboard's 🗑 Delete and "Clear finished"): the
// owner removes a finished meeting or a stale platform join request for
// good. Hive drops the record, its transcript and its side documents (the
// live loop's state and notes), and erases the meeting upstream with
// meeting.delete so Vexa forgets it too. Knowledge facts and drafts already
// made from the meeting live elsewhere and are not touched.

// ErrStillRunning is returned when deleting a meeting hive is still in
// (joining, lobby or live): leave it first.
var ErrStillRunning = errors.New("meetings: the meeting is still running; leave the meeting first")

// DeleteResult is a deleted meeting. UpstreamError is set when the Quorum
// call failed with anything but not_found: the record is removed anyway,
// and the error says what Vexa may still hold.
type DeleteResult struct {
	Meeting       Meeting `json:"meeting"`
	UpstreamError string  `json:"upstream_error,omitempty"`
}

// Deletable reports whether the owner may delete mt: a meeting in a final
// state, or a platform join request still waiting (deleting it declines
// it).
func Deletable(mt Meeting) bool {
	return mt.State.Terminal() || isPendingRequest(mt)
}

// Finished reports whether mt is in a final state ("Clear finished"):
// pending join requests are not finished.
func Finished(mt Meeting) bool { return mt.State.Terminal() }

func isPendingRequest(mt Meeting) bool {
	return mt.State == StateRequested && mt.RequestedBy != nil
}

// upstreamErased reports whether the meeting was already erased upstream:
// a hand-off deletes it, and the retention sweeper purges only after it
// did.
func upstreamErased(st State) bool { return st == StateHandedOff || st == StatePurged }

// Delete removes one meeting for good. A pending join request is declined
// first (meeting.leave). Quorum errors are best effort: not_found means
// Vexa already purged it; any other error is reported in the result while
// the record is still removed.
func (m *Manager) Delete(ctx context.Context, id string) (DeleteResult, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return DeleteResult{}, err
	}
	if !Deletable(mt) {
		return DeleteResult{Meeting: mt}, ErrStillRunning
	}
	res := DeleteResult{Meeting: mt}
	upstream := func(err error) {
		if err != nil && !IsCode(err, CodeNotFound) && res.UpstreamError == "" {
			res.UpstreamError = err.Error()
		}
	}
	if mt.NativeID != "" {
		if isPendingRequest(mt) {
			_, lerr := m.opts.Quorum.Leave(ctx, mt.NativeID)
			upstream(lerr)
		}
		if !upstreamErased(mt.State) {
			upstream(m.opts.Quorum.Delete(ctx, mt.NativeID))
		}
	}
	m.stopWatch(id)
	m.clearLobby(id)
	m.mu.Lock()
	delete(m.consentAt, id)
	delete(m.consentTries, id)
	m.mu.Unlock()
	if err := m.opts.Store.Remove(id); err != nil {
		return res, err
	}
	m.log.Info("[meetings] meeting deleted", "meeting", id, "state", string(mt.State), "upstream_error", res.UpstreamError)
	return res, nil
}

// finishedAt is when a meeting stopped: its end, else its last change.
func finishedAt(mt Meeting) time.Time {
	if mt.EndedAt != nil {
		return *mt.EndedAt
	}
	return mt.UpdatedAt
}

// ClearFinished deletes every meeting in a final state that finished
// before cutoff, one Delete each. It returns the deleted meetings; a
// meeting that could not be removed is skipped (logged).
func (m *Manager) ClearFinished(ctx context.Context, cutoff time.Time) ([]DeleteResult, error) {
	list, err := m.opts.Store.List()
	if err != nil {
		return nil, err
	}
	var out []DeleteResult
	for _, mt := range list {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !Finished(mt) || !finishedAt(mt).Before(cutoff) {
			continue
		}
		res, err := m.Delete(ctx, mt.ID)
		if err != nil {
			m.log.Warn("[meetings] clearing a finished meeting failed", "meeting", mt.ID, "error", err)
			continue
		}
		out = append(out, res)
	}
	return out, nil
}
