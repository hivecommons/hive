package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPodmanKickAcceptanceRequiresContainerID(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	workspace := t.TempDir()
	bin := fakePodman(t, `while [ "$1" != "--cidfile" ]; do shift; done
shift
printf 'container-id' > "$1"
while [ ! -f "$GATE" ]; do sleep 0.05; done
exit 7`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := (PodmanLauncher{Binary: bin}).Run(ctx, LaunchSpec{
			Image: "image", Workspace: workspace, Env: map[string]string{"GATE": gate},
			OnStarted: func() { close(started) },
		})
		finished <- err
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("launcher returned before reporting accepted container: %v", err)
	case <-ctx.Done():
		t.Fatal("container ID was not observed")
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err == nil {
		t.Fatal("expected post-acceptance process failure")
	}
}

func TestPodmanKickRejectionIsNotAcceptance(t *testing.T) {
	bin := fakePodman(t, `exit 125`)
	started := false
	_, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Image: "image", Workspace: t.TempDir(), OnStarted: func() { started = true },
	})
	if err == nil || started {
		t.Fatalf("rejected run: err=%v started=%v", err, started)
	}
}

func TestOpenShellKickAcceptanceRequiresExecResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *fakeOpenShell
		want bool
	}{
		{"executed", &fakeOpenShell{}, true},
		{"not ready", &fakeOpenShell{waitErr: errors.New("not ready")}, false},
		{"execution rejected", &fakeOpenShell{runErr: errors.New("rejected")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := false
			spec := testSpec()
			spec.OnStarted = func() { started = true }
			_, _ = newTestOpenShell(tc.fake).Run(context.Background(), spec)
			if started != tc.want {
				t.Fatalf("started=%v, want %v", started, tc.want)
			}
		})
	}
}
