package spoke

import (
	"os"
	"testing"
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
	grown := OpenFDCount()
	for _, f := range files {
		f.Close()
	}
	if grown < base+extra {
		t.Fatalf("OpenFDCount() = %d after opening %d more (baseline %d) — gauge does not track real descriptors", grown, extra, base)
	}
}

// TestFDSoftLimitPositiveOnUnix: the rlimit companion must be readable where
// the daemon actually runs.
func TestFDSoftLimitPositiveOnUnix(t *testing.T) {
	if got := FDSoftLimit(); got == 0 {
		t.Fatalf("FDSoftLimit() = 0 on a unix build — the ulimit-relative gauge would read UNKNOWN")
	}
}
