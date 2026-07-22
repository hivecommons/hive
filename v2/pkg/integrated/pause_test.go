package integrated

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSetPausedWaitsForActiveRunLease(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Config{Repository: "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, pauseErr := SetPaused(context.Background(), stateDir, true)
		done <- pauseErr
	}()
	select {
	case err := <-done:
		t.Fatalf("pause returned before the active run released its lease: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	if requested, err := PauseRequested(stateDir); err != nil || !requested {
		t.Fatalf("active run was not given an immediate pause request: requested=%t err=%v", requested, err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pause did not complete after the active run released its lease")
	}
	config, err := store.Load()
	if err != nil || !config.Paused {
		t.Fatalf("pause state was not durable: paused=%t err=%v", config.Paused, err)
	}
	if requested, err := PauseRequested(stateDir); err != nil || requested {
		t.Fatalf("completed pause left a transient request marker: requested=%t err=%v", requested, err)
	}
}

func TestPauseRequestCancelsActiveProductionContext(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchPauseRequest(ctx, stateDir, cancel)
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableStateFile(filepath.Join(store.Dir(), pauseRequestFile), []byte("pause\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("active production context did not observe the pause request")
	}
}
