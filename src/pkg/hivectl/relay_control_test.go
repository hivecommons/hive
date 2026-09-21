package hivectl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestSignalRunningRelay(t *testing.T) {
	tests := []struct {
		name        string
		pidFile     string
		readErr     error
		killErrs    []error
		commandErr  error
		commandOut  string
		wantRunning bool
		wantTarget  string
		wantErr     string
		wantCommand []string
		wantSignals []syscall.Signal
	}{
		{
			name:    "missing pid file means no relay running",
			readErr: os.ErrNotExist,
		},
		{
			name:    "read failure is surfaced",
			readErr: errors.New("permission denied"),
			wantErr: "read relay pid file",
		},
		{
			name:    "malformed pid file is surfaced",
			pidFile: "{",
			wantErr: "parse relay pid file",
		},
		{
			name:    "invalid pid is refused",
			pidFile: `{"pid":0}`,
			wantErr: "invalid pid",
		},
		{
			name:        "native relay is signaled",
			pidFile:     `{"pid":4242}`,
			wantRunning: true,
			wantTarget:  "pid 4242",
			wantSignals: []syscall.Signal{0, syscall.SIGUSR1},
		},
		{
			name:        "stale native pid reports no relay",
			pidFile:     `{"pid":4242}`,
			killErrs:    []error{syscall.ESRCH},
			wantSignals: []syscall.Signal{0},
		},
		{
			name:        "native pid check failure is surfaced",
			pidFile:     `{"pid":4242}`,
			killErrs:    []error{syscall.EPERM},
			wantErr:     "check relay pid",
			wantSignals: []syscall.Signal{0},
		},
		{
			name:        "native signal failure is surfaced",
			pidFile:     `{"pid":4242}`,
			killErrs:    []error{nil, syscall.EPERM},
			wantErr:     "signal relay pid",
			wantSignals: []syscall.Signal{0, syscall.SIGUSR1},
		},
		{
			name:        "stale pid between check and signal reports no relay",
			pidFile:     `{"pid":4242}`,
			killErrs:    []error{nil, syscall.ESRCH},
			wantSignals: []syscall.Signal{0, syscall.SIGUSR1},
		},
		{
			name:        "container relay is signaled with recorded runtime",
			pidFile:     `{"container_runtime":"podman","container_name":"hive-contributor-abc"}`,
			wantRunning: true,
			wantTarget:  "podman container hive-contributor-abc",
			wantCommand: []string{"podman", "kill", "--signal", "USR1", "hive-contributor-abc"},
		},
		{
			name:        "container relay defaults to docker runtime",
			pidFile:     `{"container_name":"hive-contributor-abc"}`,
			wantRunning: true,
			wantTarget:  "docker container hive-contributor-abc",
			wantCommand: []string{"docker", "kill", "--signal", "USR1", "hive-contributor-abc"},
		},
		{
			name:       "container signal failure includes output",
			pidFile:    `{"container_runtime":"podman","container_name":"hive-contributor-abc"}`,
			commandErr: errors.New("exit 125"),
			commandOut: "no such container",
			wantErr:    "no such container",
			wantCommand: []string{
				"podman", "kill", "--signal", "USR1", "hive-contributor-abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewProfileStore(t.TempDir())
			origRead, origRun, origSignal := readRelayPIDFile, runRelayCommand, signalRelayPID
			t.Cleanup(func() {
				readRelayPIDFile, runRelayCommand, signalRelayPID = origRead, origRun, origSignal
			})

			readRelayPIDFile = func(path string) ([]byte, error) {
				if path != store.RelayPIDPath() {
					t.Fatalf("read path = %q, want %q", path, store.RelayPIDPath())
				}
				return []byte(tt.pidFile), tt.readErr
			}

			var gotCommand []string
			runRelayCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
				gotCommand = append([]string{name}, args...)
				return []byte(tt.commandOut), tt.commandErr
			}

			var gotSignals []syscall.Signal
			signalRelayPID = func(pid int, sig syscall.Signal) error {
				if pid != 4242 {
					t.Fatalf("pid = %d, want 4242", pid)
				}
				gotSignals = append(gotSignals, sig)
				idx := len(gotSignals) - 1
				if idx < len(tt.killErrs) {
					return tt.killErrs[idx]
				}
				return nil
			}

			got, err := SignalRunningRelay(context.Background(), store)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SignalRunningRelay: %v", err)
			}
			if got.Running != tt.wantRunning || got.Target != tt.wantTarget {
				t.Fatalf("result = %+v, want running=%v target=%q", got, tt.wantRunning, tt.wantTarget)
			}
			if fmt.Sprint(gotCommand) != fmt.Sprint(tt.wantCommand) {
				t.Fatalf("command = %v, want %v", gotCommand, tt.wantCommand)
			}
			if fmt.Sprint(gotSignals) != fmt.Sprint(tt.wantSignals) {
				t.Fatalf("signals = %v, want %v", gotSignals, tt.wantSignals)
			}
		})
	}
}
