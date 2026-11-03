package lanes

import (
	"testing"

	"github.com/hivecommons/hive/crux/api"
)

func TestPublisherResponseDoesNotWaitForProcessExit(t *testing.T) {
	const publishedSequence = 1
	p := &publisherProcess{
		results:  make(chan publisherProcessResult, 1),
		finished: make(chan struct{}),
	}
	p.results <- publisherProcessResult{Published: api.LanePublished{Sequence: publishedSequence}}
	if result := processResponse(t, p); result.Published.Sequence != publishedSequence {
		t.Fatal("publication response was lost before process exit", result)
	}
	select {
	case <-p.finished:
		t.Fatal("response required process exit")
	default:
	}
	close(p.finished)
	waitPublisherProcess(t, p)
}
