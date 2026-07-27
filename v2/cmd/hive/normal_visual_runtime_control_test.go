package main

import (
	"context"
	"sync"
	"testing"
)

type runtimeStopTestManager struct {
	mu    sync.Mutex
	calls int
}

func (manager *runtimeStopTestManager) ShutdownSpecialistChildren(context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.calls++
	return nil
}

func TestNormalVisualRuntimeStopperStopsBeforeReleasingAndIsIdempotent(t *testing.T) {
	runContext, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-runContext.Done()
	}()
	manager := &runtimeStopTestManager{}
	releases := 0
	stopper := &normalVisualRuntimeStopper{
		cancel:  cancelRun,
		done:    done,
		manager: manager,
		releaseOwnership: func() {
			select {
			case <-done:
			default:
				t.Fatal("ownership released before the normal Visual service stopped")
			}
			releases++
		},
	}
	if err := stopper.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stopper.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	shutdownCalls := manager.calls
	manager.mu.Unlock()
	if shutdownCalls != 1 || releases != 1 {
		t.Fatalf("shutdownCalls=%d releases=%d", shutdownCalls, releases)
	}
}
