//go:build linux

package integrated

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestLegacyProductionRunOwnerRejectsReusedThreadID(t *testing.T) {
	threadID := make(chan int, 2)
	release := make(chan struct{})
	for range 2 {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			threadID <- syscall.Gettid()
			<-release
		}()
	}
	defer close(release)
	tid := 0
	for range 2 {
		candidate := <-threadID
		if candidate != os.Getpid() {
			tid = candidate
		}
	}
	if tid == 0 {
		t.Fatal("locked goroutines unexpectedly shared the process leader")
	}
	if leader, known := legacyProductionRunProcessLeader(tid); !known || leader {
		t.Fatalf("thread identity = leader %t, known %t; want known non-leader", leader, known)
	}
	if legacyProductionRunOwnerMatches(tid, time.Now().Add(-time.Hour)) {
		t.Fatal("reused thread ID was accepted as a live legacy process owner")
	}
}
