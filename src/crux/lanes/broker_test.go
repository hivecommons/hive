package lanes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats-server/v2/server"
)

const (
	testWait    = 10 * time.Second
	fixturePoll = 25 * time.Millisecond
)

func testServer(t *testing.T) (*server.Server, *server.Options) {
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
	opts := &server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true}
	s := startServer(t, opts)
	opts.Port = s.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	return s, opts
}

func startServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(testWait) {
		s.Shutdown()
		t.Fatal("broker not ready")
	}
	return s
}

func testConfig(url string) Config {
	c := Config{URL: url, Principals: []Principal{{Project: "atlas", TokenEnv: "ATLAS_TOKEN"}, {Project: "beacon", TokenEnv: "BEACON_TOKEN"}},
		Lanes: []api.Lane{{ID: "shared", Owner: "atlas", Publishers: []string{"atlas", "beacon"}, Subscribers: []string{"atlas", "beacon"},
			Groups:     map[string][]string{"workers": {"atlas", "beacon"}, "other": {"beacon"}},
			Recoverers: map[string][]string{"workers": {"atlas", "beacon"}}}}}
	c.Defaults()
	return c
}

func testBroker(t *testing.T, change func(*Config)) *Broker {
	t.Helper()
	s, _ := testServer(t)
	cfg := testConfig(s.ClientURL())
	if change != nil {
		change(&cfg)
	}
	b, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func publish(t *testing.T, b *Broker, id, kind string) api.LanePublished {
	t.Helper()
	out, err := b.Publish(context.Background(), "atlas", "shared", api.LanePublish{ID: id, Kind: kind, Schema: "sample-v1", Payload: []byte(`{"value":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func claim(t *testing.T, b *Broker, project, group string) api.LaneClaim {
	t.Helper()
	out, err := b.Claim(context.Background(), project, "shared", group)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func receipt(c api.LaneClaim) api.LaneReceipt {
	return api.LaneReceipt{Sequence: c.Sequence, Receipt: c.Receipt}
}

func TestPublishFanoutReplayAndIdempotency(t *testing.T) {
	b := testBroker(t, nil)
	first := publish(t, b, "first", api.LaneEvent)
	duplicate := publish(t, b, "first", api.LaneEvent)
	if !duplicate.Duplicate || duplicate.Sequence != first.Sequence {
		t.Fatalf("duplicate %+v", duplicate)
	}
	publish(t, b, "job", api.LaneWork)
	last := publish(t, b, "last", api.LaneEvent)
	for _, project := range []string{"atlas", "beacon"} {
		page, err := b.Replay(context.Background(), project, "shared", 0, 1)
		if err != nil || len(page.Events) != 1 || page.Events[0].Envelope.Project != "atlas" {
			t.Fatalf("%s: %+v %v", project, page, err)
		}
		page, err = b.Replay(context.Background(), project, "shared", page.Cursor, MaxReplayBatch)
		if err != nil || len(page.Events) != 1 || page.Cursor != last.Sequence {
			t.Fatalf("cursor %+v %v", page, err)
		}
	}
	_, err := b.Publish(context.Background(), "atlas", "shared", api.LanePublish{ID: "first", Kind: api.LaneEvent, Schema: "sample-v1", Payload: []byte("different")})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched payload: %v", err)
	}
	_, err = b.Replay(context.Background(), "atlas", "shared", last.Sequence+1, 1)
	if !errors.Is(err, ErrCursor) {
		t.Fatalf("future cursor: %v", err)
	}
}

func TestWorkCompetitionGroupsFencingAndAcknowledgements(t *testing.T) {
	b := testBroker(t, nil)
	publish(t, b, "job", api.LaneWork)
	a := claim(t, b, "atlas", "workers")
	if _, err := b.Claim(context.Background(), "beacon", "shared", "workers"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("competing claim: %v", err)
	}
	other := claim(t, b, "beacon", "other")
	if other.Sequence != a.Sequence {
		t.Fatal("other delivery group did not get its own work")
	}
	if err := b.Ack(context.Background(), "beacon", "shared", "workers", receipt(a)); !errors.Is(err, ErrStale) {
		t.Fatalf("cross-principal ack: %v", err)
	}
	if err := b.Ack(context.Background(), "atlas", "shared", "other", receipt(a)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("forged group: %v", err)
	}
	renewed, err := b.Renew(context.Background(), "atlas", "shared", "workers", receipt(a))
	if err != nil || renewed.Before(a.Expires) {
		t.Fatalf("renew: %v %v", renewed, err)
	}
	if err := b.Nack(context.Background(), "atlas", "shared", "workers", receipt(a)); err != nil {
		t.Fatal(err)
	}
	if err := b.Nack(context.Background(), "atlas", "shared", "workers", receipt(a)); err != nil {
		t.Fatal("duplicate nack", err)
	}
	makeRetriesDue(t, b)
	reassigned := claim(t, b, "beacon", "workers")
	if reassigned.Attempt != 2 || reassigned.Receipt == a.Receipt {
		t.Fatalf("redelivery %+v", reassigned)
	}
	for _, operation := range []string{"ack", "nack", "renew"} {
		_, err := b.receipt(context.Background(), "atlas", "shared", "workers", operation, receipt(a))
		if !errors.Is(err, ErrStale) {
			t.Fatalf("stale %s: %v", operation, err)
		}
	}
	for range 2 {
		if err := b.Ack(context.Background(), "beacon", "shared", "workers", receipt(reassigned)); err != nil {
			t.Fatal("idempotent ack", err)
		}
	}
	if _, err := b.Claim(context.Background(), "beacon", "shared", "workers"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("acked redelivered: %v", err)
	}
}

func TestExpiryCapacityCancellationAndIsolation(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.MaxPending, c.MaxLedger = 1, 1 })
	publish(t, b, "job", api.LaneWork)
	publish(t, b, "next", api.LaneWork)
	c := claim(t, b, "atlas", "workers")
	if _, err := b.Claim(context.Background(), "atlas", "shared", "workers"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("pending bound: %v", err)
	}
	future := b.now().Add(b.cfg.Lease + time.Second)
	b.now = func() time.Time { return future }
	if err := b.Ack(context.Background(), "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrStale) {
		t.Fatalf("expired ack: %v", err)
	}
	next := claim(t, b, "beacon", "workers")
	if next.Sequence != c.Sequence || next.Attempt != 2 {
		t.Fatal("expiry did not redeliver")
	}
	if err := b.Ack(context.Background(), "beacon", "shared", "workers", receipt(next)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(context.Background(), "atlas", "shared", "workers"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("ack ledger bound: %v", err)
	}
	future = future.Add(b.cfg.AckRetention + time.Second)
	if c := claim(t, b, "atlas", "workers"); c.Sequence <= next.Sequence {
		t.Fatal("ledger pruning did not allow next work")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Replay(ctx, "atlas", "shared", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := b.Publish(context.Background(), "outsider", "shared", api.LanePublish{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthenticated: %v", err)
	}
	if _, err := b.Claim(context.Background(), "atlas", "shared", "*"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wildcard: %v", err)
	}
}

func TestConcurrentPublishAndClaims(t *testing.T) {
	b := testBroker(t, nil)
	const writers = 8
	var wg sync.WaitGroup
	results := make(chan api.LanePublished, writers)
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := b.Publish(context.Background(), "atlas", "shared", api.LanePublish{ID: "same", Kind: api.LaneWork, Schema: "sample-v1", Payload: []byte("work")})
			if err != nil {
				errs <- err
			} else {
				results <- out
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var seq uint64
	for out := range results {
		if seq != 0 && seq != out.Sequence {
			t.Fatal("duplicate stream messages")
		}
		seq = out.Sequence
	}
	claims := make(chan api.LaneClaim, writers)
	errs = make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := b.Claim(context.Background(), "atlas", "shared", "workers")
			if err == nil {
				claims <- c
			} else if !errors.Is(err, ErrEmpty) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("owners: %d", len(claims))
	}
}

func TestServiceAndBrokerRestart(t *testing.T) {
	s, opts := testServer(t)
	cfg := testConfig(s.ClientURL())
	b, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	published := publish(t, b, "event", api.LaneEvent)
	publish(t, b, "work", api.LaneWork)
	c := claim(t, b, "atlas", "workers")
	b.Close()
	s.Shutdown()
	s.WaitForShutdown()
	s = startServer(t, opts)
	defer func() { s.Shutdown(); s.WaitForShutdown() }()
	b, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	duplicate := publish(t, b, "event", api.LaneEvent)
	if duplicate.Sequence != published.Sequence || !duplicate.Duplicate {
		t.Fatal("restart lost dedup")
	}
	page, err := b.Replay(context.Background(), "beacon", "shared", 0, 1)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("restart lost replay: %+v %v", page, err)
	}
	if _, err := b.Claim(context.Background(), "beacon", "shared", "workers"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("restart lost lease: %v", err)
	}
	if err := b.Ack(context.Background(), "atlas", "shared", "workers", receipt(c)); err != nil {
		t.Fatal("restart lost receipt", err)
	}
}

func awaitCondition(t *testing.T, check func() (bool, error)) {
	t.Helper()
	awaitConditionWithin(t, testWait, check)
}

func awaitConditionWithin(t *testing.T, timeout time.Duration, check func() (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(fixturePoll)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		done, err := check()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("broker condition not reached")
		}
	}
}

func makeRetriesDue(t *testing.T, b *Broker) {
	t.Helper()
	state, _, err := b.readGroup(context.Background(), groupKey("shared", "workers"))
	if err != nil {
		t.Fatal(err)
	}
	now := b.now()
	for _, l := range state.Leases {
		if l.NextAttempt.After(now) {
			now = l.NextAttempt
		}
	}
	now = now.Add(time.Nanosecond)
	b.now = func() time.Time { return now }
}

func TestConfigurationAndInputBounds(t *testing.T) {
	cfg := testConfig("nats://127.0.0.1:4222")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "a.b", "*", ">", "a/b", strings.Repeat("a", MaxIDLength+1)} {
		if ValidID(id) {
			t.Fatalf("unsafe identifier %q", id)
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Namespace = "*" },
		func(c *Config) { c.Lease = MaxLease + 1 },
		func(c *Config) { c.URL = "nats://user:secret@localhost:4222" },
		func(c *Config) { c.Lanes[0].Owner = "outsider" },
	} {
		c := testConfig("nats://127.0.0.1:4222")
		mutate(&c)
		if !errors.Is(c.Validate(), ErrInvalid) {
			t.Fatal("invalid configuration accepted")
		}
	}
	b := testBroker(t, nil)
	for _, in := range []api.LanePublish{
		{ID: "x", Kind: "other", Schema: "v1", Payload: []byte("x")},
		{ID: "*", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("x")},
		{ID: "x", Kind: api.LaneEvent, Schema: "v1", Payload: make([]byte, b.cfg.MaxPayload+1)},
		{ID: "x", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("x"), Provenance: "relative/path"},
	} {
		if _, err := b.Publish(context.Background(), "atlas", "shared", in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid envelope: %v", err)
		}
	}
}
