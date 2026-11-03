package lanes

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	receiptBytes   = 32
	maxCASAttempts = 32
	maxStateBytes  = 512 << 10
)

type lease struct {
	Receipt     string    `json:"receipt"`
	Project     string    `json:"project"`
	Expires     time.Time `json:"expires"`
	Attempt     uint64    `json:"attempt"`
	Done        bool      `json:"done"`
	Released    bool      `json:"released"`
	AckedAt     time.Time `json:"acked_at,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitempty"`
	Expired     bool      `json:"expired,omitempty"`
	RetiredAt   time.Time `json:"retired_at,omitempty"`
}

type groupState struct {
	Cursor         uint64                    `json:"cursor"`
	Leases         map[uint64]lease          `json:"leases"`
	RetryTurn      bool                      `json:"retry_turn,omitempty"`
	Recoveries     map[string]recoveryRecord `json:"recoveries,omitempty"`
	MissingThrough uint64                    `json:"missing_through,omitempty"`
	RetryCursor    uint64                    `json:"retry_cursor,omitempty"`
	Slots          int                       `json:"slots"`
	Padding        string                    `json:"padding"`
}

func (b *Broker) readGroup(ctx context.Context, key string) (groupState, uint64, error) {
	entry, err := b.state.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return groupState{Leases: map[uint64]lease{}}, 0, nil
	}
	if err != nil {
		return groupState{}, 0, backend(err)
	}
	if len(entry.Value()) > maxStateBytes {
		return groupState{}, 0, ErrCapacity
	}
	var state groupState
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return groupState{}, 0, backend(err)
	}
	if state.Leases == nil {
		return groupState{}, 0, ErrConflict
	}
	state.Padding = ""
	if err := validateGroup(state); err != nil {
		return groupState{}, 0, err
	}
	reserved, err := groupReservation(state.Slots)
	if err != nil {
		return groupState{}, 0, err
	}
	if reserved > maxStateBytes || state.Slots > b.cfg.MaxLedger ||
		len(entry.Value()) > reserved || len(entry.Value()) < reserved-revisionHeaderGrowth(^uint64(0)) {
		return groupState{}, 0, ErrConflict
	}
	return state, entry.Revision(), nil
}

func (b *Broker) saveGroup(ctx context.Context, key string, state groupState, revision uint64) error {
	_, err := b.writeGroup(ctx, key, state, revision)
	return err
}

func (b *Broker) writeGroup(ctx context.Context, key string, state groupState, revision uint64) (uint64, error) {
	data, err := encodeGroup(state, revision)
	if err != nil {
		return 0, err
	}
	if max(state.Slots, len(state.Leases), minGroupSlots) > b.cfg.MaxLedger {
		return 0, ErrCapacity
	}
	var next uint64
	if revision == 0 {
		next, err = b.state.Create(ctx, key, data)
	} else {
		next, err = b.state.Update(ctx, key, data, revision)
	}
	if errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		return 0, ErrConflict
	}
	return next, backend(err)
}

func groupKey(lane, group string) string { return "group." + lane + "." + group }

// Claim's successful KV compare-and-set is the sole ownership linearization
// point. A response lost afterwards leaves a lease, not a second owner. Event
// and work log records remain in the stream for other delivery groups.
func (b *Broker) Claim(ctx context.Context, project, lane, group string) (api.LaneClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	if _, err := b.authorize(project, lane, "work", group); err != nil {
		return api.LaneClaim{}, err
	}
	key := groupKey(lane, group)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return api.LaneClaim{}, err
		}
		state, revision, err := b.readGroup(ctx, key)
		if err != nil {
			return api.LaneClaim{}, err
		}
		now := b.now().UTC()
		b.pruneGroup(&state, now)
		pending := 0
		ready := []uint64{}
		for seq, l := range state.Leases {
			if !l.Done && !l.Expired {
				if l.Expires.After(now) && !l.Released {
					pending++
				}
				if !l.Expires.After(now) && !l.NextAttempt.After(now) {
					ready = append(ready, seq)
				}
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
		var record api.LaneRecord
		var previous lease
		retried := false
		canClaim := pending < b.cfg.MaxPending
		canFresh := canClaim && len(state.Leases) < b.cfg.MaxLedger
		if canFresh {
			reserved, err := groupReservation(max(state.Slots, len(state.Leases)+1))
			if err != nil {
				return api.LaneClaim{}, err
			}
			canFresh = reserved <= maxStateBytes
		}
		if !canClaim || (!canFresh && len(ready) == 0) {
			return api.LaneClaim{}, ErrCapacity
		}
		freshTurn := canFresh && (len(ready) == 0 || !state.RetryTurn)
		if freshTurn && len(ready) != 0 {
			// Persist the next retry turn before attempting value growth.
			// If global bucket admission fails (or its response is lost),
			// the next call can still redeliver already accepted work.
			state.RetryTurn = true
			revision, err = b.writeGroup(ctx, key, state, revision)
			if errors.Is(err, ErrConflict) {
				continue
			} else if err != nil {
				return api.LaneClaim{}, err
			}
		}
		if freshTurn {
			page, err := b.scan(ctx, lane, state.Cursor, 1, api.LaneWork)
			if err != nil {
				return api.LaneClaim{}, err
			}
			state.Cursor = page.Cursor
			if len(page.Events) != 0 {
				record = page.Events[0]
			}
		}
		if record.Sequence == 0 && len(ready) != 0 {
			// Rotate among already-claimed deliveries as well as alternating
			// retries with fresh work. A repeatedly ready low sequence cannot
			// monopolize a group's retry turns across process restarts.
			index := sort.Search(len(ready), func(i int) bool { return ready[i] > state.RetryCursor })
			if index == len(ready) {
				index = 0
			}
			seq := ready[index]
			msg, err := b.streams[lane].GetMsg(ctx, seq)
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				return api.LaneClaim{}, ErrCursor
			}
			if err != nil {
				return api.LaneClaim{}, backend(err)
			}
			record.Sequence = seq
			if err := json.Unmarshal(msg.Data, &record.Envelope); err != nil {
				return api.LaneClaim{}, backend(err)
			}
			if record.Envelope.Kind != api.LaneWork || record.Envelope.Lane != lane {
				return api.LaneClaim{}, ErrConflict
			}
			previous = state.Leases[seq]
			if previous.Attempt == ^uint64(0) {
				return api.LaneClaim{}, ErrCapacity
			}
			state.RetryCursor = seq
			retried = true
		}
		if record.Sequence == 0 {
			if err := b.saveGroup(ctx, key, state, revision); errors.Is(err, ErrConflict) {
				continue
			} else if err != nil {
				return api.LaneClaim{}, err
			}
			return api.LaneClaim{}, ErrEmpty
		}
		receipt, err := newReceipt()
		if err != nil {
			return api.LaneClaim{}, err
		}
		l := lease{Receipt: receipt, Project: project,
			Expires: now.Add(b.cfg.Lease), Attempt: previous.Attempt + 1}
		state.Leases[record.Sequence] = l
		state.RetryTurn = !retried
		if err := b.saveGroup(ctx, key, state, revision); errors.Is(err, ErrConflict) {
			continue
		} else if err != nil {
			return api.LaneClaim{}, err
		}
		return api.LaneClaim{LaneRecord: record, Receipt: l.Receipt, Expires: l.Expires, Attempt: l.Attempt}, nil
	}
	return api.LaneClaim{}, ErrConflict
}

// Receipt operations bind lane, group, authenticated project, sequence and
// random ownership generation. No NATS ACK subject is exposed to clients.
func (b *Broker) receipt(ctx context.Context, project, lane, group, operation string, in api.LaneReceipt) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	if _, err := b.authorize(project, lane, "work", group); err != nil {
		return time.Time{}, err
	}
	if !validReceipt(in.Receipt) || in.Sequence == 0 {
		return time.Time{}, ErrInvalid
	}
	key := groupKey(lane, group)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		state, revision, err := b.readGroup(ctx, key)
		if err != nil {
			return time.Time{}, err
		}
		l, ok := state.Leases[in.Sequence]
		if !ok || subtle.ConstantTimeCompare([]byte(l.Receipt), []byte(in.Receipt)) != 1 || l.Project != project {
			return time.Time{}, ErrStale
		}
		now := b.now().UTC()
		if l.Expired {
			return time.Time{}, ErrStale
		}
		if l.Done {
			if operation == "ack" && now.Sub(l.AckedAt) < b.cfg.AckRetention {
				return l.Expires, nil
			}
			return time.Time{}, ErrStale
		}
		if l.Released && operation == "nack" {
			return l.Expires, nil
		}
		if l.Released || !l.Expires.After(now) {
			return time.Time{}, ErrStale
		}
		switch operation {
		case "ack":
			l.Done, l.AckedAt = true, now
		case "nack":
			l.Released, l.Expires = true, now
			l.NextAttempt = now.Add(b.retryDelay(l.Attempt))
		case "renew":
			l.Expires = now.Add(b.cfg.Lease)
		default:
			return time.Time{}, ErrInvalid
		}

		state.Leases[in.Sequence] = l
		if err := b.saveGroup(ctx, key, state, revision); errors.Is(err, ErrConflict) {
			continue
		} else if err != nil {
			return time.Time{}, err
		}
		return l.Expires, nil
	}
	return time.Time{}, ErrConflict
}

func newReceipt() (string, error) {
	data := make([]byte, receiptBytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func validReceipt(receipt string) bool {
	data, err := hex.DecodeString(receipt)
	return err == nil && len(data) == receiptBytes
}

func (b *Broker) retryDelay(attempt uint64) time.Duration {
	delay := b.cfg.RetryDelay
	for n := uint64(1); n < attempt && delay < b.cfg.MaxRetryDelay; n++ {
		if delay > b.cfg.MaxRetryDelay/2 {
			return b.cfg.MaxRetryDelay
		}
		delay *= 2
	}
	return delay
}

func (b *Broker) Ack(ctx context.Context, project, lane, group string, in api.LaneReceipt) error {
	_, err := b.receipt(ctx, project, lane, group, "ack", in)
	return err
}
func (b *Broker) Nack(ctx context.Context, project, lane, group string, in api.LaneReceipt) error {
	_, err := b.receipt(ctx, project, lane, group, "nack", in)
	return err
}
func (b *Broker) Renew(ctx context.Context, project, lane, group string, in api.LaneReceipt) (time.Time, error) {
	return b.receipt(ctx, project, lane, group, "renew", in)
}
