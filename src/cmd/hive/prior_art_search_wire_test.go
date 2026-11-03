package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/priorart"
	"github.com/hivecommons/hive/pkg/search"
)

func priorArtSearchFixture(t *testing.T) search.Backend {
	t.Helper()
	b, err := search.NewEmbedded(search.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	err = b.Index(context.Background(), []search.Document{{
		ID: "pr/41/finding/0-0", Repo: "org/a", Kind: search.KindFinding, At: time.Now(),
		URL: "https://github.com/org/a/pull/41", Visibility: search.Public,
		Title:  "Retry webhook delivery on 502",
		Text:   "reviewer: webhook.retry_backoff now retries webhook delivery when the relay answers 502",
		Fields: map[string]string{"number": "41"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasPriorArtSource(r priorart.Report, name string) bool {
	for _, s := range r.Sources {
		if s == name {
			return true
		}
	}
	return false
}

// The search source joins the gate only when search.enabled is set (#14639).
func TestNewPriorArtSearcherSearchSourceFollowsSearchEnabled(t *testing.T) {
	backend := priorArtSearchFixture(t)
	issue := priorart.Issue{Repo: "org/a", Number: 7, Title: "Retry webhook delivery on 502",
		Body: "`webhook.retry_backoff` should retry webhook delivery when the relay answers 502."}
	on := true
	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		backend func() search.Backend
		want    bool
	}{
		{"disabled", &config.Config{}, func() search.Backend { return backend }, false},
		{"enabled without resolver", &config.Config{Search: config.SearchConfig{Enabled: &on}}, nil, false},
		{"enabled", &config.Config{Search: config.SearchConfig{Enabled: &on}}, func() search.Backend { return backend }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPriorArtSearcher(tc.cfg, nil, nil, nil, tc.backend, slog.Default())
			r := s.Find(context.Background(), issue)
			if got := hasPriorArtSource(r, search.PriorArtSourceName); got != tc.want {
				t.Fatalf("search source answered = %v, want %v (sources %v)", got, tc.want, r.Sources)
			}
			found := false
			for _, f := range r.Findings {
				if f.Kind == priorart.KindReview && f.Ref == "#41" {
					found = true
				}
			}
			if found != tc.want {
				t.Fatalf("past fix finding = %v, want %v (findings %+v)", found, tc.want, r.Findings)
			}
		})
	}
}

func TestBootPriorArtSearchBackend(t *testing.T) {
	b := &boot{}
	if got := b.priorArtSearchBackend(); got != nil {
		t.Fatalf("backend before indexer start = %v, want nil", got)
	}
	backend := priorArtSearchFixture(t)
	b.searchIndexer.Store(&search.Indexer{Backend: backend})
	if got := b.priorArtSearchBackend(); got != backend {
		t.Fatalf("backend = %v, want the indexer's", got)
	}
}
