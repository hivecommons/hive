package lanes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type Broker struct {
	cfg      Config
	nc       *nats.Conn
	js       jetstream.JetStream
	state    jetstream.KeyValue
	dedup    jetstream.KeyValue
	streams  map[string]jetstream.Stream
	registry map[string]api.Lane
	now      func() time.Time
}

// Open owns a bounded NATS connection. The caller owns Close. Existing broker
// resources must exactly match configuration; Open never rewrites retention or
// membership underneath another running service.
func Open(ctx context.Context, cfg Config, options ...nats.Option) (*Broker, error) {
	cfg = cfg.clone()
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options = append(options, nats.Name("crux-lanes"), nats.Timeout(cfg.Timeout),
		nats.MaxReconnects(-1), nats.ReconnectWait(time.Second), nats.ReconnectBufSize(0))
	nc, err := nats.Connect(cfg.URL, options...)
	if err != nil {
		return nil, ErrUnavailable
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, ErrUnavailable
	}
	b := &Broker{cfg: cfg, nc: nc, js: js, streams: map[string]jetstream.Stream{}, registry: map[string]api.Lane{}, now: time.Now}
	if err := b.initialize(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	return b, nil
}

func (b *Broker) Close()         { b.nc.Close() }
func (b *Broker) Config() Config { return b.cfg.clone() }

func backend(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func (b *Broker) initialize(ctx context.Context) error {
	var err error
	b.state, err = b.bucket(ctx, b.cfg.Namespace+"_state", 0)
	if err != nil {
		return err
	}
	b.dedup, err = b.bucket(ctx, b.cfg.Namespace+"_dedup", b.cfg.Retention)
	if err != nil {
		return err
	}
	// Registry and bounds are an immutable namespace-wide generation. Changing
	// membership requires an explicit new namespace, not racing deployments.
	snapshot := struct {
		Lanes                                                            []api.Lane
		Retention, Dedup, Lease, AckRetention, RetryDelay, MaxRetryDelay time.Duration
		MaxPayload, MaxLedger, MaxPending, Replicas                      int
		MaxMessages, MaxBytes, MaxKVBytes                                int64
		StateFormat                                                      int
	}{b.cfg.Lanes, b.cfg.Retention, b.cfg.Dedup, b.cfg.Lease, b.cfg.AckRetention, b.cfg.RetryDelay, b.cfg.MaxRetryDelay,
		b.cfg.MaxPayload, b.cfg.MaxLedger, b.cfg.MaxPending, b.cfg.Replicas,
		b.cfg.MaxMessages, b.cfg.MaxBytes, b.cfg.MaxKVBytes, groupStateFormat}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return ErrCapacity
	}
	if _, err = b.state.Create(ctx, "registry", data); err != nil {
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return backend(err)
		}
		entry, err := b.state.Get(ctx, "registry")
		if err != nil {
			return backend(err)
		}
		if !bytes.Equal(entry.Value(), data) {
			return fmt.Errorf("%w: namespace registry differs", ErrConflict)
		}
	}
	for _, l := range b.cfg.Lanes {
		sc := jetstream.StreamConfig{Name: b.streamName(l.ID), Subjects: []string{b.subject(l.ID) + "*"},
			Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy,
			Discard: jetstream.DiscardNew, MaxAge: b.cfg.Retention,
			MaxMsgs: b.cfg.MaxMessages, MaxBytes: b.cfg.MaxBytes,
			MaxMsgSize: int32(base64.StdEncoding.EncodedLen(b.cfg.MaxPayload) + envelopeOverhead), Replicas: b.cfg.Replicas,
			Duplicates: b.cfg.Dedup, DenyDelete: true, DenyPurge: true}
		s, err := b.js.CreateStream(ctx, sc)
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			s, err = b.js.Stream(ctx, sc.Name)
			if err == nil {
				info, e := s.Info(ctx)
				if e != nil {
					return backend(e)
				}
				got := info.Config
				if got.MaxAge != sc.MaxAge || got.MaxMsgs != sc.MaxMsgs || got.MaxBytes != sc.MaxBytes ||
					got.Storage != sc.Storage || got.Retention != sc.Retention || got.Discard != sc.Discard ||
					got.Replicas != sc.Replicas || got.MaxMsgSize != sc.MaxMsgSize || got.Duplicates != sc.Duplicates ||
					!got.DenyDelete || !got.DenyPurge || len(got.Subjects) != 1 || got.Subjects[0] != sc.Subjects[0] {
					return fmt.Errorf("%w: stream configuration differs", ErrConflict)
				}
			}
		}
		if err != nil {
			return backend(err)
		}
		b.streams[l.ID], b.registry[l.ID] = s, l
	}
	return nil
}

const envelopeOverhead = 8 << 10

func (b *Broker) bucket(ctx context.Context, name string, ttl time.Duration) (jetstream.KeyValue, error) {
	kv, err := b.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: name, Storage: jetstream.FileStorage, History: 1, TTL: ttl,
		MaxBytes: b.cfg.MaxKVBytes, Replicas: b.cfg.Replicas})
	if errors.Is(err, jetstream.ErrBucketExists) {
		kv, err = b.js.KeyValue(ctx, name)
	}
	if err != nil {
		return nil, backend(err)
	}
	s, err := b.js.Stream(ctx, "KV_"+name)
	if err != nil {
		return nil, backend(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		return nil, backend(err)
	}
	if info.Config.MaxAge != ttl || info.Config.MaxBytes != b.cfg.MaxKVBytes ||
		info.Config.Storage != jetstream.FileStorage || info.Config.Replicas != b.cfg.Replicas ||
		info.Config.MaxMsgsPerSubject != 1 {
		return nil, fmt.Errorf("%w: key-value configuration differs", ErrConflict)
	}
	return kv, nil
}

func (b *Broker) streamName(lane string) string {
	return "crux_lane_" + digest([]byte(b.cfg.Namespace+"\x00"+lane))
}
func (b *Broker) subject(lane string) string { return b.cfg.Namespace + ".lane." + lane + "." }
func digest(data []byte) string              { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

type publishReservation struct {
	Hash       string    `json:"hash"`
	Created    time.Time `json:"created"`
	Generation string    `json:"generation"`
}

func (b *Broker) Publish(ctx context.Context, project, lane string, in api.LanePublish) (api.LanePublished, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	if _, err := b.authorize(project, lane, "publish", ""); err != nil {
		return api.LanePublished{}, err
	}
	if !ValidID(in.ID) || !ValidID(in.Schema) || (in.Kind != api.LaneEvent && in.Kind != api.LaneWork) ||
		len(in.Payload) == 0 || len(in.Payload) > b.cfg.MaxPayload || len(in.Provenance) > MaxProvenanceLength {
		return api.LanePublished{}, ErrInvalid
	}
	if in.Provenance != "" {
		u, err := url.Parse(in.Provenance)
		if err != nil || !u.IsAbs() || u.User != nil {
			return api.LanePublished{}, ErrInvalid
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return api.LanePublished{}, err
	}
	key := lane + "." + project + "." + in.ID
	generation, err := newReceipt()
	if err != nil {
		return api.LanePublished{}, err
	}
	res := publishReservation{Hash: digest(body), Created: b.now().UTC(), Generation: generation}
	data, err := json.Marshal(res)
	if err != nil {
		return api.LanePublished{}, err
	}
	if _, err := b.dedup.Create(ctx, key, data); err != nil {
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return api.LanePublished{}, backend(err)
		}
		entry, err := b.dedup.Get(ctx, key)
		if err != nil {
			return api.LanePublished{}, backend(err)
		}
		if err := json.Unmarshal(entry.Value(), &res); err != nil {
			return api.LanePublished{}, backend(err)
		}
		if res.Hash != digest(body) {
			return api.LanePublished{}, ErrConflict
		}
	}
	if !validReceipt(res.Generation) {
		return api.LanePublished{}, ErrConflict
	}
	subject := b.subject(lane) + digest([]byte(key))
	stream := b.streams[lane]
	if msg, err := stream.GetLastMsgForSubject(ctx, subject); err == nil {
		var envelope api.LaneEnvelope
		if err := json.Unmarshal(msg.Data, &envelope); err != nil {
			return api.LanePublished{}, backend(err)
		}
		existing, err := json.Marshal(envelope.LanePublish)
		if err != nil {
			return api.LanePublished{}, err
		}
		if !bytes.Equal(existing, body) || envelope.Project != project || envelope.Lane != lane {
			return api.LanePublished{}, ErrConflict
		}
		if err := b.verifyReservation(ctx, key, res); err != nil {
			return api.LanePublished{}, err
		}
		return api.LanePublished{Sequence: msg.Sequence, Duplicate: true}, nil
	} else if !errors.Is(err, jetstream.ErrMsgNotFound) {
		return api.LanePublished{}, backend(err)
	}
	if b.now().Sub(res.Created) >= b.cfg.Retention {
		return api.LanePublished{}, ErrCursor
	}
	envelope := api.LaneEnvelope{LanePublish: in, Lane: lane, Project: project, CreatedAt: res.Created}
	data, err = json.Marshal(envelope)
	if err != nil {
		return api.LanePublished{}, err
	}
	if err := b.verifyReservation(ctx, key, res); err != nil {
		return api.LanePublished{}, err
	}
	// Subject CAS closes the crash-between-reservation-and-publish gap even
	// after JetStream's finite message-ID deduplication window has elapsed.
	msg := &nats.Msg{Subject: subject, Data: data, Header: nats.Header{reservationHeader: []string{res.Generation}}}
	ack, err := b.js.PublishMsg(ctx, msg,
		jetstream.WithMsgID(key+"."+res.Generation), jetstream.WithExpectLastSequencePerSubject(0))
	if err != nil {
		// A concurrent identical publisher may have won subject CAS. Only a
		// persisted, verified envelope is accepted as a successful duplicate.
		msg, readErr := stream.GetLastMsgForSubject(ctx, subject)
		if readErr == nil {
			var got api.LaneEnvelope
			if e := json.Unmarshal(msg.Data, &got); e != nil {
				return api.LanePublished{}, backend(e)
			}
			encoded, e := json.Marshal(got.LanePublish)
			if e != nil {
				return api.LanePublished{}, e
			}
			if bytes.Equal(encoded, body) && got.Project == project && got.Lane == lane && msg.Header.Get(reservationHeader) == res.Generation {
				if e := b.verifyReservation(ctx, key, res); e != nil {
					return api.LanePublished{}, e
				}
				return api.LanePublished{Sequence: msg.Sequence, Duplicate: true}, nil
			}
			return api.LanePublished{}, ErrConflict
		}
		return api.LanePublished{}, backend(err)
	}
	// JetStream deduplicates message IDs before evaluating subject CAS. Never
	// equate a broker duplicate ACK with this request's content or generation.
	persisted, err := stream.GetMsg(ctx, ack.Sequence)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return api.LanePublished{}, ErrCursor
	}
	if err != nil {
		return api.LanePublished{}, backend(err)
	}
	if persisted.Subject != subject || persisted.Header.Get(reservationHeader) != res.Generation || !bytes.Equal(persisted.Data, data) {
		return api.LanePublished{}, ErrConflict
	}
	if err := b.verifyReservation(ctx, key, res); err != nil {
		return api.LanePublished{}, err
	}
	return api.LanePublished{Sequence: ack.Sequence, Duplicate: ack.Duplicate}, nil
}

const reservationHeader = "Crux-Reservation-Generation"

func (b *Broker) verifyReservation(ctx context.Context, key string, expected publishReservation) error {
	entry, err := b.dedup.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
		return ErrStale
	}
	if err != nil {
		return backend(err)
	}
	var current publishReservation
	if err := json.Unmarshal(entry.Value(), &current); err != nil {
		return backend(err)
	}
	if current.Generation != expected.Generation || current.Hash != expected.Hash || !current.Created.Equal(expected.Created) {
		return ErrConflict
	}
	if b.now().Sub(current.Created) >= b.cfg.Retention {
		return ErrStale
	}
	return nil
}

func (b *Broker) Replay(ctx context.Context, project, lane string, cursor uint64, limit int) (api.LaneReplay, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	if _, err := b.authorize(project, lane, "subscribe", ""); err != nil {
		return api.LaneReplay{}, err
	}
	if limit < 1 || limit > MaxReplayBatch {
		return api.LaneReplay{}, ErrInvalid
	}
	return b.scan(ctx, lane, cursor, limit, api.LaneEvent)
}

func (b *Broker) scan(ctx context.Context, lane string, cursor uint64, limit int, kind string) (api.LaneReplay, error) {
	s := b.streams[lane]
	info, err := s.Info(ctx)
	if err != nil {
		return api.LaneReplay{}, backend(err)
	}
	first, last := info.State.FirstSeq, info.State.LastSeq
	if cursor > last || (cursor != 0 && first > 0 && cursor < first-1) {
		return api.LaneReplay{}, ErrCursor
	}
	next := cursor + 1
	if cursor == 0 && first > next {
		next = first
	}
	out := api.LaneReplay{Events: []api.LaneRecord{}, Cursor: cursor}
	for count := 0; next <= last && count < MaxScanBatch && len(out.Events) < limit; count++ {
		msg, err := s.GetMsg(ctx, next)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return api.LaneReplay{}, ErrCursor
		}
		if err != nil {
			return api.LaneReplay{}, backend(err)
		}
		var envelope api.LaneEnvelope
		if err := json.Unmarshal(msg.Data, &envelope); err != nil {
			return api.LaneReplay{}, backend(err)
		}
		if envelope.Lane != lane {
			return api.LaneReplay{}, ErrConflict
		}
		out.Cursor = next
		if envelope.Kind == kind {
			out.Events = append(out.Events, api.LaneRecord{Sequence: next, Envelope: envelope})
		}
		next++
	}
	return out, nil
}
