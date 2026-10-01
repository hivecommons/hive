package spoke

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// TestOpenFDCountReportsLiveDescriptors pins the gauge to reality: it must
// report a positive count on the platforms we build on (Linux containers in
// production, macOS dev machines), and the count must move when descriptors
// are opened. A gauge that silently reads 0 would report UNKNOWN forever and
// reintroduce exactly the blindness #3875 showed (92,962 leaked FDs found
// only by manual /proc inspection).
func TestOpenFDCountReportsLiveDescriptors(t *testing.T) {
	base := OpenFDCount()
	if base <= 0 {
		t.Fatalf("OpenFDCount() = %d, want > 0 — a live process always holds descriptors", base)
	}

	const extra = 8
	files := make([]*os.File, 0, extra)
	for i := 0; i < extra; i++ {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		files = append(files, f)
	}
	// OpenFDCount() reads the whole process's /proc/self/fd (or /dev/fd)
	// listing, not just this goroutine's descriptors: an unrelated goroutine
	// elsewhere in the binary closing a descriptor between our baseline read
	// and this one can transiently dip the count below base+extra even
	// though the 8 files opened above are still held open. Poll instead of
	// reading once so that transient dip doesn't flake this assertion.
	var grown int
	testutil.EventuallyEveryFunc(t, 2*time.Second, 10*time.Millisecond, func() bool {
		grown = OpenFDCount()
		return grown >= base+extra
	}, func() string {
		return fmt.Sprintf("OpenFDCount() = %d after opening %d more (baseline %d) — gauge does not track real descriptors", grown, extra, base)
	})
	for _, f := range files {
		f.Close()
	}
}

// TestFDSoftLimitPositiveOnUnix: the rlimit companion must be readable where
// the daemon actually runs.
func TestFDSoftLimitPositiveOnUnix(t *testing.T) {
	if got := FDSoftLimit(); got == 0 {
		t.Fatalf("FDSoftLimit() = 0 on a unix build — the ulimit-relative gauge would read UNKNOWN")
	}
}
