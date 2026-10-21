package hivectl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestRunningContributorDiscovery(t *testing.T) {
	oldRead, oldRun, oldSignal := readRelayPIDFile, runRelayCommand, signalRelayPID
	t.Cleanup(func() { readRelayPIDFile, runRelayCommand, signalRelayPID = oldRead, oldRun, oldSignal })
	store := NewProfileStore(t.TempDir())
	for _, runtime := range []string{"", "docker", "podman"} {
		r := ContributorRelay{PID: 123, Mode: "local", TmuxSession: "hive-pi-1234", LogFile: "/private/relay.log", EnvFile: "/private/contributor.env", StartedAt: "today"}
		if runtime != "" {
			r.ContainerRuntime = runtime
			r.ContainerName = "hive-contributor-test"
			r.Mode = "container"
		}
		data, _ := json.Marshal(r)
		readRelayPIDFile = func(string) ([]byte, error) { return data, nil }
		signalRelayPID = func(pid int, sig syscall.Signal) error {
			if runtime != "" || pid != 123 || sig != 0 {
				t.Fatal("discovery sent a signal or checked container PID on host")
			}
			return nil
		}
		runRelayCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != runtime || !reflect.DeepEqual(args, []string{"inspect", "-f", "{{.State.Running}}", r.ContainerName}) {
				t.Fatalf("wrong inspection: %s %q", name, args)
			}
			return []byte("true\n"), nil
		}
		got, err := RunningContributor(context.Background(), store)
		if err != nil || got == nil || *got != r {
			t.Fatalf("discovery: %#v %v", got, err)
		}
		if runtime == "" {
			signalRelayPID = func(int, syscall.Signal) error { return syscall.ESRCH }
		} else {
			runRelayCommand = func(context.Context, string, ...string) ([]byte, error) { return []byte("false"), nil }
		}
		got, err = RunningContributor(context.Background(), store)
		if err != nil || got != nil {
			t.Fatalf("stale contributor: %#v %v", got, err)
		}
	}
	readRelayPIDFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if got, err := RunningContributor(context.Background(), store); got != nil || err != nil {
		t.Fatalf("missing: %v %v", got, err)
	}
	readRelayPIDFile = func(string) ([]byte, error) { return []byte(`{"container_name":"x","container_runtime":"sh"}`), nil }
	if _, err := RunningContributor(context.Background(), store); err == nil {
		t.Fatal("arbitrary runtime accepted")
	}
}

func TestStopRecordedContributor(t *testing.T) {
	oldRun, oldSignal := runRelayCommand, signalRelayPID
	t.Cleanup(func() { runRelayCommand, signalRelayPID = oldRun, oldSignal })
	signalRelayPID = func(pid int, sig syscall.Signal) error {
		if pid != 123 || sig != syscall.SIGTERM {
			t.Fatal("wrong stop signal")
		}
		return nil
	}
	if err := StopContributor(context.Background(), ContributorRelay{PID: 123}); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"docker", "podman"} {
		runRelayCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != runtime || !reflect.DeepEqual(args, []string{"stop", "hive-contributor-test"}) {
				t.Fatalf("wrong stop: %s %q", name, args)
			}
			return nil, nil
		}
		if err := StopContributor(context.Background(), ContributorRelay{ContainerRuntime: runtime, ContainerName: "hive-contributor-test"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunningContributorRejectsUnreadableRecordsAndDefaultsToDocker(t *testing.T) {
	oldRead, oldRun := readRelayPIDFile, runRelayCommand
	t.Cleanup(func() { readRelayPIDFile, runRelayCommand = oldRead, oldRun })
	store := NewProfileStore(t.TempDir())

	readRelayPIDFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
	if got, err := RunningContributor(context.Background(), store); got != nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable record: %v %v", got, err)
	}
	readRelayPIDFile = func(string) ([]byte, error) { return []byte("{"), nil }
	if got, err := RunningContributor(context.Background(), store); got != nil || err == nil || !strings.Contains(err.Error(), "parse contributor PID record") {
		t.Fatalf("corrupt record: %v %v", got, err)
	}

	// Records written before container_runtime existed were always docker.
	readRelayPIDFile = func(string) ([]byte, error) { return []byte(`{"container_name":"hive-contributor-old"}`), nil }
	runRelayCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" {
			t.Fatalf("legacy container record inspected with %q", name)
		}
		return []byte("true"), nil
	}
	got, err := RunningContributor(context.Background(), store)
	if err != nil || got == nil || got.ContainerRuntime != "docker" {
		t.Fatalf("legacy container record: %#v %v", got, err)
	}
	runRelayCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no such container") }
	if got, err := RunningContributor(context.Background(), store); got != nil || err != nil {
		t.Fatalf("vanished container must read as not running: %v %v", got, err)
	}
}

func TestStopContributorRefusesUnsafeTargets(t *testing.T) {
	oldRun, oldSignal := runRelayCommand, signalRelayPID
	t.Cleanup(func() { runRelayCommand, signalRelayPID = oldRun, oldSignal })
	runRelayCommand = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "podman" {
			t.Fatalf("ran %q for an unsafe target", name)
		}
		return []byte("container is locked"), errors.New("exit status 125")
	}
	signalRelayPID = func(pid int, _ syscall.Signal) error {
		t.Fatalf("signaled pid %d for an unsafe target", pid)
		return nil
	}
	for _, r := range []ContributorRelay{
		{ContainerName: "hive-contributor-test", ContainerRuntime: "sh"},
		{ContainerName: "hive-contributor-test"},
		{PID: 0},
		{PID: -1},
	} {
		if err := StopContributor(context.Background(), r); err == nil {
			t.Fatalf("stopped unsafe target %#v", r)
		}
	}
	err := StopContributor(context.Background(), ContributorRelay{ContainerName: "hive-contributor-test", ContainerRuntime: "podman"})
	if err == nil || !strings.Contains(err.Error(), "container is locked") {
		t.Fatalf("runtime stop failure not surfaced with its output: %v", err)
	}
}
