package laneparticipant

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/nats-io/nats-server/v2/server"
)

func fixture(t *testing.T) (*Participant, *api.Client, *lanes.Broker) {
	t.Helper()
	dir := filepath.Join(".", fmt.Sprintf(".lane-fixture-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(time.Second * 10) {
		ns.Shutdown()
		t.Fatal("NATS not ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	cfg := lanes.Config{URL: ns.ClientURL(),
		Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}, {Project: "hive", TokenEnv: "HIVE"}},
		Lanes: []api.Lane{{ID: "commons", Owner: "atlas", Publishers: []string{"atlas", "hive"}, Subscribers: []string{"atlas"},
			Groups: map[string][]string{"hive-adapter": {"hive"}}}}}
	cfg.Defaults()
	cfg.RetryDelay, cfg.MaxRetryDelay = lanes.MinRetryDelay, lanes.MinRetryDelay
	b, err := lanes.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	tokens := map[string]string{"ATLAS": strings.Repeat("a", 32), "HIVE": strings.Repeat("h", 32)}
	auth, err := lanes.Bind(cfg, func(env string) string { return tokens[env] }, "admin")
	if err != nil {
		t.Fatal(err)
	}
	s := serve.New(nil)
	s.Token, s.Lanes, s.LaneAuth = "admin", b, auth
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	atlas, err := api.NewClient(hs.URL, tokens["ATLAS"])
	if err != nil {
		t.Fatal(err)
	}
	hive, err := api.NewClient(hs.URL, tokens["HIVE"])
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(hive, "commons", "hive-adapter", nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, atlas, b
}

func waitRetry(t *testing.T, b *lanes.Broker) {
	t.Helper()
	state, err := b.InspectGroup(context.Background(), "hive", "commons", "hive-adapter")
	if err != nil || len(state.Retries) == 0 {
		t.Fatalf("missing durable retry: %+v %v", state, err)
	}
	timer := time.NewTimer(time.Until(state.Retries[0].AvailableAt))
	defer timer.Stop()
	<-timer.C
}

func TestHiveIsAnOptInPeerAndSnapshotHandler(t *testing.T) {
	p, atlas, _ := fixture(t)
	ctx := context.Background()
	change := governor.ModeChange{Timestamp: time.Now().UTC(), From: governor.ModeIdle, To: governor.ModeSurge, Reason: "depth"}
	p.ObserveMode(ctx, change)
	if err := p.PublishModeChange(ctx, change); err != nil {
		t.Fatal(err)
	}
	page, err := atlas.ReplayLane(ctx, "commons", 0, 8)
	if err != nil || len(page.Events) != 1 || page.Events[0].Envelope.Project != "hive" || page.Events[0].Envelope.Schema != ModeChangeSchema {
		t.Fatalf("Hive event %+v %v", page, err)
	}
	if err := p.RegisterSnapshot([]string{"atlas"}, func() governor.State { return governor.State{Mode: governor.ModeIdle} }); err != nil {
		t.Fatal(err)
	}
	work := api.LanePublish{ID: "snapshot", Kind: api.LaneWork, Schema: SnapshotRequestSchema, Payload: []byte(`{}`)}
	if _, err := atlas.PublishLane(ctx, "commons", work); err != nil {
		t.Fatal(err)
	}
	handled, err := p.HandleOnce(ctx)
	if err != nil || !handled {
		t.Fatalf("snapshot work: %v %v", handled, err)
	}
	if handled, err := p.HandleOnce(ctx); err != nil || handled {
		t.Fatalf("acked work redelivered: %v %v", handled, err)
	}
	page, err = atlas.ReplayLane(ctx, "commons", page.Cursor, 8)
	if err != nil || len(page.Events) != 1 || page.Events[0].Envelope.Schema != SnapshotSchema {
		t.Fatalf("snapshot response %+v %v", page, err)
	}
}

func TestHandlerAllowlistFailureAndCancellation(t *testing.T) {
	p, atlas, b := fixture(t)
	ctx := context.Background()
	work := api.LanePublish{ID: "job", Kind: api.LaneWork, Schema: "tool-execution-v1", Payload: []byte(`{"prompt":"not executable"}`)}
	if _, err := atlas.PublishLane(ctx, "commons", work); err != nil {
		t.Fatal(err)
	}
	if handled, err := p.HandleOnce(ctx); !handled || !errors.Is(err, ErrUnregistered) {
		t.Fatalf("unknown schema: %v %v", handled, err)
	}
	waitRetry(t, b)
	calls := 0
	if err := p.Register(work.Schema, []string{"not-atlas"}, func(context.Context, api.LaneEnvelope) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleOnce(ctx); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("publisher allowlist: %v", err)
	}
	if calls != 0 {
		t.Fatal("unauthorized project ran a handler")
	}
	waitRetry(t, b)
	delete(p.handlers, work.Schema)
	want := errors.New("handler refused")
	if err := p.Register(work.Schema, []string{"atlas"}, func(context.Context, api.LaneEnvelope) error { return want }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleOnce(ctx); !errors.Is(err, want) {
		t.Fatalf("handler failure hidden: %v", err)
	}
	waitRetry(t, b)
	c, err := b.Claim(ctx, "hive", "commons", "hive-adapter")
	if err != nil || c.Attempt < 3 {
		t.Fatalf("nack did not redeliver: %+v %v", c, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.Run(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation: %v", err)
	}
	p.ObserveMode(canceled, governor.ModeChange{})
}

func TestRegistrationAndSnapshotValidation(t *testing.T) {
	p, atlas, _ := fixture(t)
	if _, err := New(nil, "", "", nil); err == nil {
		t.Fatal("invalid participant accepted")
	}
	for _, projects := range [][]string{nil, {""}} {
		if err := p.Register("schema", projects, func(context.Context, api.LaneEnvelope) error { return nil }); !errors.Is(err, ErrUnregistered) {
			t.Fatal("invalid registration", err)
		}
	}
	if err := p.RegisterSnapshot([]string{"atlas"}, nil); !errors.Is(err, ErrUnregistered) {
		t.Fatal(err)
	}
	if err := p.RegisterSnapshot([]string{"atlas"}, func() governor.State { return governor.State{} }); err != nil {
		t.Fatal(err)
	}
	if err := p.RegisterSnapshot([]string{"atlas"}, func() governor.State { return governor.State{} }); err == nil {
		t.Fatal("duplicate handler accepted")
	}
	if _, err := atlas.PublishLane(context.Background(), "commons", api.LanePublish{ID: "bad", Kind: api.LaneWork, Schema: SnapshotRequestSchema, Payload: []byte(`{"prompt":"no"}`)}); err != nil {
		t.Fatal(err)
	}
	if handled, err := p.HandleOnce(context.Background()); !handled || err == nil {
		t.Fatal("arbitrary snapshot request accepted")
	}
}

func TestRejectedSnapshotCannotStarveValidWorkAcrossParticipantRestart(t *testing.T) {
	for _, kind := range []string{"malformed", "unsupported", "unauthorized-publisher"} {
		t.Run(kind, func(t *testing.T) {
			p, atlas, b := fixture(t)
			read := func() governor.State { return governor.State{Mode: governor.ModeIdle} }
			if err := p.RegisterSnapshot([]string{"atlas"}, read); err != nil {
				t.Fatal(err)
			}
			work := api.LanePublish{ID: "poison", Kind: api.LaneWork, Schema: SnapshotRequestSchema, Payload: []byte(`{}`)}
			publisher := atlas
			switch kind {
			case "malformed":
				work.Payload = []byte(`{"prompt":"not permitted"}`)
			case "unsupported":
				work.Schema = "unsupported-v1"
			case "unauthorized-publisher":
				publisher = p.client
			}
			if _, err := publisher.PublishLane(context.Background(), "commons", work); err != nil {
				t.Fatal(err)
			}
			if handled, err := p.HandleOnce(context.Background()); !handled || err == nil {
				t.Fatal("poison item was not rejected")
			}
			work.ID, work.Schema, work.Payload = "valid", SnapshotRequestSchema, []byte(`{}`)
			if _, err := atlas.PublishLane(context.Background(), "commons", work); err != nil {
				t.Fatal(err)
			}
			// Poll after the persisted delay: a delay alone must not hide starvation.
			waitRetry(t, b)
			if handled, err := p.HandleOnce(context.Background()); !handled || err == nil {
				t.Fatal("expected explicit retry turn")
			}
			waitRetry(t, b)
			restarted, err := New(p.client, "commons", "hive-adapter", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.RegisterSnapshot([]string{"atlas"}, read); err != nil {
				t.Fatal(err)
			}
			if handled, err := restarted.HandleOnce(context.Background()); !handled || err != nil {
				t.Fatalf("valid snapshot starved: %v %v", handled, err)
			}
			status, err := b.InspectGroup(context.Background(), "hive", "commons", "hive-adapter")
			if err != nil || len(status.Retries) != 1 || status.Retries[0].Attempt != 2 {
				t.Fatalf("rejected work disappeared: %+v %v", status, err)
			}
			page, err := atlas.ReplayLane(context.Background(), "commons", 0, 8)
			if err != nil || len(page.Events) != 1 || page.Events[0].Envelope.Schema != SnapshotSchema {
				t.Fatalf("missing valid result: %+v %v", page, err)
			}
		})
	}
}
