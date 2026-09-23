//go:build integration

package wavefront_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/worksource/wavefront"
)

// TestLiveCrustifyWavefrontSmoke exercises a real Wavefront/Crustify graph when
// the scheduled smoke provides one. It skips locally and on forks unless the
// workflow supplies WAVEFRONT_SMOKE_GRAPH and WAVEFRONT_SMOKE_REPO.
func TestLiveCrustifyWavefrontSmoke(t *testing.T) {
	graphPath := strings.TrimSpace(os.Getenv("WAVEFRONT_SMOKE_GRAPH"))
	repo := strings.TrimSpace(os.Getenv("WAVEFRONT_SMOKE_REPO"))
	if graphPath == "" || repo == "" {
		t.Skip("set WAVEFRONT_SMOKE_GRAPH and WAVEFRONT_SMOKE_REPO to run the live Crustify/Wavefront smoke")
	}
	src, err := wavefront.New(wavefront.Options{Repo: repo, Path: graphPath, ReceiptsDir: t.TempDir(), Now: func() time.Time { return time.Date(2026, 9, 23, 16, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatalf("open live wavefront graph: %v", err)
	}
	ctx := context.Background()
	graph, ready, err := src.Ready(ctx)
	if err != nil {
		t.Fatalf("list ready nodes: %v", err)
	}
	if graph.Name == "" || graph.Revision == "" || len(graph.Nodes) == 0 {
		t.Fatalf("graph identity is incomplete: %+v", graph)
	}
	if len(ready) == 0 {
		t.Fatalf("graph %s@%s has no ready nodes to smoke", graph.Name, graph.Revision)
	}
	runID := wavefront.ExternalID(graph.Name, ready[0].ID)
	if _, err := src.Complete(ctx, runID, graph.Revision, nil, time.Now().UTC()); err != nil {
		t.Fatalf("complete first ready node %s: %v", runID, err)
	}
	if _, _, err := src.Verify(ctx, runID, graph.Revision); err != nil {
		t.Fatalf("verify completed node %s: %v", runID, err)
	}
	if _, err := src.Complete(ctx, runID, graph.Revision+"-stale", nil, time.Now().UTC()); !errors.Is(err, wavefront.ErrStaleRevision) {
		t.Fatalf("stale revision completion = %v, want ErrStaleRevision", err)
	}
}
