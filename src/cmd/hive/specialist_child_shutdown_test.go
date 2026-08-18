package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingSpecialistChildShutdowner struct {
	calls       int
	deadline    time.Time
	hadDeadline bool
	events      *[]string
	err         error
	waitForDone bool
}

func (shutdowner *recordingSpecialistChildShutdowner) ShutdownSpecialistChildren(ctx context.Context) error {
	shutdowner.calls++
	shutdowner.deadline, shutdowner.hadDeadline = ctx.Deadline()
	if shutdowner.events != nil {
		*shutdowner.events = append(*shutdowner.events, "shutdown")
	}
	if shutdowner.waitForDone {
		<-ctx.Done()
		return ctx.Err()
	}
	return shutdowner.err
}

func TestOrdinaryMainShutdownUsesBoundedChildOnlyCleanup(t *testing.T) {
	shutdowner := &recordingSpecialistChildShutdowner{}
	started := time.Now()
	if err := shutdownOrdinarySpecialistChildren(shutdowner, nil); err != nil {
		t.Fatal(err)
	}
	if shutdowner.calls != 1 {
		t.Fatalf("specialist child shutdown calls = %d, want 1", shutdowner.calls)
	}
	if !shutdowner.hadDeadline {
		t.Fatal("specialist child shutdown did not receive a bounded context")
	}
	minimum := started.Add(ordinarySpecialistChildShutdownTimeout - time.Second)
	maximum := time.Now().Add(ordinarySpecialistChildShutdownTimeout + time.Second)
	if shutdowner.deadline.Before(minimum) || shutdowner.deadline.After(maximum) {
		t.Fatalf("specialist child shutdown deadline = %v, want a %s bound", shutdowner.deadline, ordinarySpecialistChildShutdownTimeout)
	}
}

func TestOrdinaryRuntimeReapsChildrenBeforeReleasingOwnership(t *testing.T) {
	events := []string{}
	shutdowner := &recordingSpecialistChildShutdowner{events: &events}
	applicationCtx, cancel := context.WithCancel(context.Background())
	if err := shutdownOrdinaryVisualRuntime(cancel, shutdowner, func() {
		select {
		case <-applicationCtx.Done():
		default:
			t.Fatal("ownership released before application intake was canceled")
		}
		events = append(events, "release")
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != "shutdown" || events[1] != "release" {
		t.Fatalf("ordinary runtime cleanup order = %v, want [shutdown release]", events)
	}
}

func TestOrdinaryRuntimeRetainsOwnershipWhenHungChildIsNotProvenDead(t *testing.T) {
	helper := os.Getenv("HIVE_TEST_HUNG_CHILD_SHUTDOWN") == "child"
	stateDir := t.TempDir()
	if helper {
		stateDir = os.Getenv("HIVE_TEST_HUNG_CHILD_STATE_DIR")
	}
	ready := filepath.Join(stateDir, "shutdown-ready")
	if helper {
		lease, err := claimNormalVisualDaemonLease(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		ordinarySpecialistChildShutdownTimeout = 20 * time.Millisecond
		_, cancel := context.WithCancel(context.Background())
		defer shutdownOrdinaryVisualRuntime(cancel, &recordingSpecialistChildShutdowner{waitForDone: true}, func() {
			releaseDaemonLease(lease)
		}, nil)
		if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestOrdinaryRuntimeRetainsOwnershipWhenHungChildIsNotProvenDead$")
	command.Env = make([]string, 0, len(os.Environ())+2)
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if name != "HIVE_TEST_HUNG_CHILD_SHUTDOWN" && name != "HIVE_TEST_HUNG_CHILD_STATE_DIR" {
			command.Env = append(command.Env, pair)
		}
	}
	command.Env = append(command.Env, "HIVE_TEST_HUNG_CHILD_SHUTDOWN=child", "HIVE_TEST_HUNG_CHILD_STATE_DIR="+stateDir)
	var childOutput bytes.Buffer
	command.Stdout, command.Stderr = &childOutput, &childOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown helper did not acquire the ordinary runtime lease")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-waitDone:
		t.Fatalf("owner process exited instead of halting its deferred hung-child shutdown: %v\n%s", err, childOutput.String())
	default:
	}
	if lease, err := claimNormalVisualDaemonLease(stateDir); err == nil {
		releaseDaemonLease(lease)
		t.Fatal("hung-child deferred shutdown returned and released the OS ownership lease")
	}
	if command.ProcessState != nil && command.ProcessState.Exited() {
		t.Fatal("owner process exited while its exact specialist child remained unverified")
	}
}
