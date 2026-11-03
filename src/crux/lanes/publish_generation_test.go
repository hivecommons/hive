package lanes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	processConfigEnv     = "CRUX_LANES_TEST_PROCESS_CONFIG"
	processPayloadEnv    = "CRUX_LANES_TEST_PROCESS_PAYLOAD"
	processDelayEnv      = "CRUX_LANES_TEST_PROCESS_DELAY"
	processWorkActionEnv = "CRUX_LANES_TEST_PROCESS_WORK_ACTION"
	processProjectEnv    = "CRUX_LANES_TEST_PROCESS_PROJECT"
	processReady         = "LANE_READY"
	processResult        = "LANE_RESULT "
)

type delayedProcessPublisher struct{ jetstream.JetStream }

func (d *delayedProcessPublisher) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	fmt.Fprintln(os.Stdout, processReady)
	if _, err := bufio.NewReader(os.Stdin).ReadByte(); err != nil {
		return nil, err
	}
	return d.JetStream.PublishMsg(ctx, msg, opts...)
}

type publisherProcessResult struct {
	Published     api.LanePublished `json:"published"`
	Error         string            `json:"error,omitempty"`
	ClaimSequence uint64            `json:"claim_sequence,omitempty"`
	Attempt       uint64            `json:"attempt,omitempty"`
	AvailableAt   time.Time         `json:"available_at,omitempty"`
}

// The same CI test executable acts as two isolated Crux publisher processes;
// pipes pause A after reservation validation but before broker publication.
func TestLanePublisherProcessHelper(t *testing.T) {
	raw := os.Getenv(processConfigEnv)
	if raw == "" {
		return
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	b, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if os.Getenv(processDelayEnv) == "yes" {
		b.js = &delayedProcessPublisher{JetStream: b.js}
	}
	result := publisherProcessResult{}
	if action := os.Getenv(processWorkActionEnv); action != "" {
		project := os.Getenv(processProjectEnv)
		var claim api.LaneClaim
		claim, err = b.Claim(context.Background(), project, "shared", "workers")
		if err == nil {
			result.ClaimSequence, result.Attempt = claim.Sequence, claim.Attempt
			switch action {
			case "nack":
				err = b.Nack(context.Background(), project, "shared", "workers", receipt(claim))
				if err == nil {
					var status api.LaneGroupState
					status, err = b.InspectGroup(context.Background(), project, "shared", "workers")
					for _, retry := range status.Retries {
						if retry.Sequence == claim.Sequence {
							result.AvailableAt = retry.AvailableAt
						}
					}
				}
			case "ack":
				err = b.Ack(context.Background(), project, "shared", "workers", receipt(claim))
			default:
				err = ErrInvalid
			}
		}
	} else {
		result.Published, err = b.Publish(context.Background(), "atlas", "shared", api.LanePublish{ID: "shared-key", Kind: api.LaneEvent, Schema: "v1", Payload: []byte(os.Getenv(processPayloadEnv))})
	}
	switch {
	case errors.Is(err, ErrConflict):
		result.Error = "conflict"
	case errors.Is(err, ErrStale):
		result.Error = "stale"
	case err != nil:
		result.Error = err.Error()
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, processResult+string(data))
}

type publisherProcess struct {
	pid      int
	input    io.WriteCloser
	ready    chan struct{}
	results  chan publisherProcessResult
	finished chan struct{}
	waitErr  error
}

func startPublisherProcess(t *testing.T, cfg Config, payload string, delay bool, extraEnv ...string) *publisherProcess {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLanePublisherProcessHelper$", "-test.timeout="+testWait.String())
	cmd.Env = append(os.Environ(), processConfigEnv+"="+string(data), processPayloadEnv+"="+payload, processDelayEnv+"=no", processWorkActionEnv+"=")
	cmd.Env = append(cmd.Env, extraEnv...)
	if delay {
		cmd.Env = append(cmd.Env, processDelayEnv+"=yes")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		input.Close()
		t.Fatal(err)
	}
	p := &publisherProcess{pid: cmd.Process.Pid, input: input, ready: make(chan struct{}, 1), results: make(chan publisherProcessResult, 1), finished: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			line := scanner.Text()
			if line == processReady {
				p.ready <- struct{}{}
			}
			if strings.HasPrefix(line, processResult) {
				var result publisherProcessResult
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, processResult)), &result); err != nil {
					p.waitErr = err
				} else {
					p.results <- result
				}
			}
		}
		scanErr := scanner.Err()
		p.waitErr = errors.Join(p.waitErr, scanErr, cmd.Wait())
		close(p.finished)
	}()
	t.Cleanup(func() { cancel(); input.Close(); <-p.finished })
	return p
}

func processOutcome(t *testing.T, p *publisherProcess) publisherProcessResult {
	t.Helper()
	result := processResponse(t, p)
	waitPublisherProcess(t, p)
	return result
}

func waitPublisherProcess(t *testing.T, p *publisherProcess) {
	t.Helper()
	select {
	case <-p.finished:
	case <-time.After(testWait):
		t.Fatal("publisher process did not exit after its result")
	}
	if p.waitErr != nil {
		t.Fatalf("publisher process failed after result: %v", p.waitErr)
	}
}

func processResponse(t *testing.T, p *publisherProcess) publisherProcessResult {
	t.Helper()
	select {
	case result := <-p.results:
		return result
	case <-p.finished:
		if p.waitErr != nil {
			t.Fatalf("publisher process failed: %v", p.waitErr)
		}
		select {
		case result := <-p.results:
			return result
		default:
			t.Fatalf("publisher process failed: %v", p.waitErr)
		}
	case <-time.After(testWait):
		t.Fatal("publisher process did not finish")
	}
	return publisherProcessResult{}
}

func TestTwoCruxProcessesFenceDelayedExpiredPublicationGeneration(t *testing.T) {
	b := testBroker(t, shortRetention)
	a := startPublisherProcess(t, b.Config(), "payload-A", true)
	select {
	case <-a.ready:
	case <-a.finished:
		t.Fatalf("A failed before pause: %v", a.waitErr)
	case <-time.After(testWait):
		t.Fatal("A never reserved")
	}
	key := "shared.atlas.shared-key"
	entry, err := b.dedup.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var first publishReservation
	if err := json.Unmarshal(entry.Value(), &first); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, func() (bool, error) {
		_, err := b.dedup.Get(context.Background(), key)
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return true, nil
		}
		return false, err
	})
	second := startPublisherProcess(t, b.Config(), "payload-B", false)
	if a.pid == second.pid {
		t.Fatal("publishers were not separate processes")
	}
	// Read the durable response before waiting for process exit: the race
	// detector's exit delay alone exceeds this stream's one-second retention.
	winner := processResponse(t, second)
	if winner.Error != "" || winner.Published.Sequence == 0 {
		t.Fatalf("B did not publish: %+v", winner)
	}
	if _, err := a.input.Write([]byte("r")); err != nil {
		t.Fatal(err)
	}
	loser := processResponse(t, a)
	if loser.Error != "conflict" && loser.Error != "stale" {
		t.Fatalf("delayed A falsely succeeded or only timed out: %+v", loser)
	}
	if loser.Published.Sequence != 0 {
		t.Fatal("A received success-shaped result for B")
	}
	record, err := b.streams["shared"].GetMsg(context.Background(), winner.Published.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	var envelope api.LaneEnvelope
	if err := json.Unmarshal(record.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope.Payload) != "payload-B" || record.Header.Get(reservationHeader) == first.Generation {
		t.Fatal("A replaced B or shared expired generation")
	}
	waitPublisherProcess(t, second)
	waitPublisherProcess(t, a)
}

type wrongAckPublisher struct {
	jetstream.JetStream
	sequence uint64
}

func (w wrongAckPublisher) PublishMsg(context.Context, *nats.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return &jetstream.PubAck{Sequence: w.sequence, Duplicate: true}, nil
}

func TestDuplicateAckMustResolveToMatchingRetainedEnvelope(t *testing.T) {
	b := testBroker(t, nil)
	other := publish(t, b, "other", api.LaneEvent)
	b.js = wrongAckPublisher{JetStream: b.js, sequence: other.Sequence}
	in := api.LanePublish{ID: "not-other", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("unpublished")}
	if out, err := b.Publish(context.Background(), "atlas", "shared", in); !errors.Is(err, ErrConflict) || out.Sequence != 0 {
		t.Fatalf("wrong duplicate ACK accepted: %+v %v", out, err)
	}
	b.js = wrongAckPublisher{JetStream: b.js, sequence: other.Sequence + 1}
	if out, err := b.Publish(context.Background(), "atlas", "shared", in); !errors.Is(err, ErrCursor) || out.Sequence != 0 {
		t.Fatalf("missing duplicate metadata accepted: %+v %v", out, err)
	}
}

type replaceAfterAckPublisher struct {
	jetstream.JetStream
	kv jetstream.KeyValue
}

func (r replaceAfterAckPublisher) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := r.JetStream.PublishMsg(ctx, msg, opts...)
	if err != nil {
		return nil, err
	}
	entry, err := r.kv.Get(ctx, "shared.atlas.normal")
	if err != nil {
		return nil, err
	}
	var res publishReservation
	if err := json.Unmarshal(entry.Value(), &res); err != nil {
		return nil, err
	}
	res.Generation, err = newReceipt()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if _, err := r.kv.Update(ctx, entry.Key(), data, entry.Revision()); err != nil {
		return nil, err
	}
	return ack, nil
}

func TestNormalAckAlsoFencesReplacedReservation(t *testing.T) {
	b := testBroker(t, nil)
	b.js = replaceAfterAckPublisher{JetStream: b.js, kv: b.dedup}
	out, err := b.Publish(context.Background(), "atlas", "shared", api.LanePublish{ID: "normal", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("persisted-but-fenced")})
	if !errors.Is(err, ErrConflict) || out.Sequence != 0 {
		t.Fatalf("replaced reservation got successful normal ACK: %+v %v", out, err)
	}
}
