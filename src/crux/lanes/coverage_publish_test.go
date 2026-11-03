package lanes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
)

func TestPublicationReservationFailuresNeverReturnSuccess(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	invalid := b.Config()
	invalid.Namespace = "bad.*"
	if opened, err := Open(ctx, invalid); !errors.Is(err, ErrInvalid) || opened != nil {
		t.Fatal("invalid configuration reached broker initialization", err)
	}
	if _, err := b.Replay(ctx, "outsider", "shared", 0, 1); !errors.Is(err, ErrForbidden) {
		t.Fatal("unregistered project replayed lane data", err)
	}
	in := api.LanePublish{ID: "reserved", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("value")}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	key := "shared.atlas." + in.ID
	res := publishReservation{Hash: digest(body), Created: b.now().UTC(), Generation: strings.Repeat("a", receiptBytes*2)}
	if err := b.verifyReservation(ctx, key+".missing", res); !errors.Is(err, ErrStale) {
		t.Fatal("missing reservation accepted", err)
	}
	put := func(value publishReservation) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.dedup.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	put(res)
	actual := b.dedup
	b.dedup = &faultResponseKV{KeyValue: actual, getErr: errFixtureBroker}
	if out, err := b.Publish(ctx, "atlas", "shared", in); !errors.Is(err, ErrUnavailable) || out.Sequence != 0 {
		t.Fatal("unreadable reservation produced a publication", out, err)
	}
	if err := b.verifyReservation(ctx, key, res); !errors.Is(err, ErrUnavailable) {
		t.Fatal("reservation verification hid broker failure", err)
	}
	b.dedup = actual
	if _, err := b.dedup.Put(ctx, key, []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	if err := b.verifyReservation(ctx, key, res); !errors.Is(err, ErrUnavailable) {
		t.Fatal("malformed active reservation accepted", err)
	}
	invalidGeneration := res
	invalidGeneration.Generation = "invalid"
	put(invalidGeneration)
	if out, err := b.Publish(ctx, "atlas", "shared", in); !errors.Is(err, ErrConflict) || out.Sequence != 0 {
		t.Fatal("invalid reservation generation accepted", out, err)
	}
	expired := res
	expired.Created = expired.Created.Add(-b.cfg.Retention)
	put(expired)
	if out, err := b.Publish(ctx, "atlas", "shared", in); !errors.Is(err, ErrCursor) || out.Sequence != 0 {
		t.Fatal("expired unpublished reservation accepted", out, err)
	}
	if err := b.verifyReservation(ctx, key, expired); !errors.Is(err, ErrStale) {
		t.Fatal("expired active generation accepted", err)
	}
	put(res)
	subject := b.subject("shared") + digest([]byte(key))
	if _, err := b.js.Publish(ctx, subject, []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	if out, err := b.Publish(ctx, "atlas", "shared", in); !errors.Is(err, ErrUnavailable) || out.Sequence != 0 {
		t.Fatal("malformed persisted envelope accepted", out, err)
	}
	envelope := api.LaneEnvelope{LanePublish: in, Project: "beacon", Lane: "shared", CreatedAt: res.Created}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.js.Publish(ctx, subject, data); err != nil {
		t.Fatal(err)
	}
	if out, err := b.Publish(ctx, "atlas", "shared", in); !errors.Is(err, ErrConflict) || out.Sequence != 0 {
		t.Fatal("foreign persisted envelope reported as duplicate success", out, err)
	}
	envelope.Project = "atlas"
	data, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.js.Publish(ctx, subject, data); err != nil {
		t.Fatal(err)
	}
	put(expired)
	if out, err := b.Publish(ctx, "atlas", "shared", in); !errors.Is(err, ErrStale) || out.Sequence != 0 {
		t.Fatal("retained envelope bypassed expired active reservation", out, err)
	}
}
