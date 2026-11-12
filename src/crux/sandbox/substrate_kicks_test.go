package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

func substrateStates(t *testing.T, kicks *SubstrateKicks, launchName string) ([]SubstrateActorInfo, string) {
	t.Helper()
	kicks.mu.Lock()
	defer kicks.mu.Unlock()
	k, ok := kicks.byLaunch[launchName]
	if !ok {
		t.Fatalf("no kick recorded for %q", launchName)
	}
	return append([]SubstrateActorInfo(nil), k.history...), string(k.out)
}

func TestSubstrateLauncherRecordsKickForTerminal(t *testing.T) {
	fake := &fakeSubstrate{
		spiffeID: "spiffe://ate.local/ns/ate-workers/sa/worker",
		runResp:  substratebridge.RunResponse{Stdout: "step 1\n", Stderr: "warn\n", ExitCode: 5},
	}
	l := newTestSubstrate(fake)
	if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	history, out := substrateStates(t, l.kicks, "deps-sandbox")
	if out != "step 1\nwarn\n" {
		t.Errorf("recorded output = %q", out)
	}
	var states []string
	for _, h := range history {
		states = append(states, h.State)
	}
	if got := strings.Join(states, ","); got != "CREATED,RESUMING,RUNNING,RUNNING,DELETING" {
		t.Errorf("states = %s", got)
	}
	final := history[len(history)-1]
	if final.Name != "hive-kick-deps-1700000000" || final.Atespace != "hive" || final.SpiffeID != fake.spiffeID ||
		!final.Exited || final.ExitCode != 5 || !final.Done || final.Suspended {
		t.Errorf("final = %+v", final)
	}
	if info, found, err := l.kicks.LatestActor(context.Background(), "deps-sandbox"); err != nil || !found || info != final {
		t.Errorf("LatestActor = %+v %v %v", info, found, err)
	}
	if _, found, _ := l.kicks.LatestActor(context.Background(), "other-sandbox"); found {
		t.Error("an agent without a kick must not be found")
	}
}

func TestSubstrateLauncherRecordsSuspendAndFailure(t *testing.T) {
	fake := &fakeSubstrate{}
	l := newTestSubstrate(fake)
	l.Options.SnapshotOnIdle = true
	if _, err := l.Run(context.Background(), substrateTestSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	history, _ := substrateStates(t, l.kicks, "deps-sandbox")
	if final := history[len(history)-1]; final.State != SubstrateStateSuspended || !final.Suspended || !final.Done {
		t.Errorf("snapshot_on_idle final = %+v", final)
	}

	fake = &fakeSubstrate{waitReadyErr: errors.New("no bridge")}
	l = newTestSubstrate(fake)
	if _, err := l.Run(context.Background(), substrateTestSpec()); err == nil {
		t.Fatal("expected bridge error")
	}
	history, _ = substrateStates(t, l.kicks, "deps-sandbox")
	if final := history[len(history)-1]; final.State != SubstrateStateDeleting || !final.Done || final.Exited {
		t.Errorf("failed kick final = %+v", final)
	}

	// A kick that fails before the actor exists records nothing.
	fake = &fakeSubstrate{actorErr: errors.New("quota")}
	l = newTestSubstrate(fake)
	_, _ = l.Run(context.Background(), substrateTestSpec())
	if _, found, _ := l.kicks.LatestActor(context.Background(), "deps-sandbox"); found {
		t.Error("no actor was created; nothing must be recorded")
	}
}

func TestSubstrateKicksStreamRunFollowsStatesAndOutput(t *testing.T) {
	kicks := &SubstrateKicks{}
	k := kicks.begin("deps-sandbox", SubstrateActorInfo{Name: "a1", State: SubstrateStateCreated})
	kicks.update(k, func(i *SubstrateActorInfo) { i.State = SubstrateStateResuming })

	type event struct{ state, out string }
	var events []event
	var buf strings.Builder
	done := make(chan SubstrateActorInfo)
	go func() {
		final, err := kicks.StreamRun(context.Background(), "a1", time.Millisecond, &buf, func(i SubstrateActorInfo) {
			events = append(events, event{state: i.State, out: buf.String()})
		})
		if err != nil {
			t.Errorf("StreamRun: %v", err)
		}
		done <- final
	}()
	kicks.observe(k, &substrateapi.Actor{Status: &substrateapi.ActorStatus{
		State:            substrateapi.ActorState_ACTOR_STATE_RUNNING,
		WorkerAssignment: &substrateapi.WorkerAssignment{WorkerSpiffeId: "spiffe://x/w"},
	}})
	w := kicks.output(k)
	_, _ = w.Write([]byte("line 1\n"))
	_, _ = w.Write([]byte("line 2\n"))
	kicks.update(k, func(i *SubstrateActorInfo) { i.Exited, i.ExitCode = true, 0 })
	kicks.update(k, func(i *SubstrateActorInfo) { i.State, i.Suspended, i.Done = SubstrateStateSuspended, true, true })

	final := <-done
	if buf.String() != "line 1\nline 2\n" {
		t.Fatalf("streamed = %q", buf.String())
	}
	if final.State != SubstrateStateSuspended || !final.Done || final.SpiffeID != "spiffe://x/w" {
		t.Fatalf("final = %+v", final)
	}
	// The first state is the one LatestActor already reported; RESUMING
	// may have been recorded before the stream started and is replayed.
	var states []string
	for _, e := range events {
		states = append(states, e.state)
	}
	if got := strings.Join(states, ","); !strings.Contains(got, "RUNNING") || !strings.HasSuffix(got, "SUSPENDED") {
		t.Fatalf("states = %s", got)
	}
	if last := events[len(events)-1]; last.out != "line 1\nline 2\n" {
		t.Fatalf("SUSPENDED must be reported after the output, saw output %q", last.out)
	}
}

func TestSubstrateKicksStreamRunUnknownActorAndCancel(t *testing.T) {
	kicks := &SubstrateKicks{}
	final, err := kicks.StreamRun(context.Background(), "gone", time.Millisecond, &strings.Builder{}, nil)
	if err != nil || !final.Done || final.Name != "gone" {
		t.Fatalf("unknown actor: %+v %v", final, err)
	}
	kicks.begin("deps-sandbox", SubstrateActorInfo{Name: "a1", State: SubstrateStateRunning})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := kicks.StreamRun(ctx, "a1", time.Millisecond, &strings.Builder{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream: %v", err)
	}
}

func TestSubstrateKickOutputIsCapped(t *testing.T) {
	kicks := &SubstrateKicks{}
	k := kicks.begin("x", SubstrateActorInfo{Name: "a"})
	w := kicks.output(k)
	big := make([]byte, substratebridge.MaxOutput+10)
	if n, err := w.Write(big); err != nil || n != len(big) {
		t.Fatalf("Write = %d %v", n, err)
	}
	if n, _ := w.Write([]byte("more")); n != 4 {
		t.Fatal("a full buffer must still accept writes")
	}
	if len(k.out) != substratebridge.MaxOutput {
		t.Fatalf("recorded %d bytes", len(k.out))
	}
	if kicks.output(nil) != nil {
		t.Fatal("no kick, no writer")
	}
	kicks.update(nil, func(*SubstrateActorInfo) { t.Fatal("must not run") })
}

func TestSubstrateDecodeRunStreamedAndLegacy(t *testing.T) {
	var live strings.Builder
	stream := `{"stdout":"a","stderr":"","exit_code":0,"partial":true}
{"stdout":"","stderr":"b","exit_code":0,"partial":true}
{"stdout":"","stderr":"","exit_code":2,"error":"boom"}
`
	out, err := substrateDecodeRun(strings.NewReader(stream), &live)
	if err != nil || out.Stdout != "a" || out.Stderr != "b" || out.ExitCode != 2 || out.Error != "boom" || live.String() != "ab" {
		t.Fatalf("streamed: %+v %v live=%q", out, err, live.String())
	}
	// A bridge that predates streaming answers with one complete response.
	live.Reset()
	out, err = substrateDecodeRun(strings.NewReader(`{"stdout":"all","stderr":"err","exit_code":1}`), &live)
	if err != nil || out.Stdout != "all" || out.Stderr != "err" || out.ExitCode != 1 || live.String() != "allerr" {
		t.Fatalf("legacy: %+v %v live=%q", out, err, live.String())
	}
	// A stream cut before the final frame is an error, not exit code 0.
	if _, err := substrateDecodeRun(strings.NewReader(`{"stdout":"a","partial":true}`), nil); err == nil || !strings.Contains(err.Error(), "decoding run response") {
		t.Fatalf("truncated: %v", err)
	}
}
