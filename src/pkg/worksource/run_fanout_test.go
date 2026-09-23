package worksource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type fakeFanoutLeases struct {
	created []RunImplementationLease
}

func (f *fakeFanoutLeases) CreateImplementationLease(_ context.Context, lease RunImplementationLease) error {
	f.created = append(f.created, lease)
	return nil
}

type fakeFanoutOverlaps struct {
	overlaps []RunRepoOverlap
	repos    []string
}

func (f *fakeFanoutOverlaps) Overlaps(_ context.Context, repos []string) ([]RunRepoOverlap, error) {
	f.repos = append([]string(nil), repos...)
	return f.overlaps, nil
}

type fakeFanoutAudit struct {
	reasons []string
}

func (f *fakeFanoutAudit) RecordRunFanoutRefusal(_ context.Context, _ string, reason string) {
	f.reasons = append(f.reasons, reason)
}

func fanoutStatusServer(t *testing.T, status SpektacularRunStatus) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(status); err != nil {
			t.Fatalf("encode status: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunFanoutCreatesWaveOneAndBlocksWaveTwoUntilFinal(t *testing.T) {
	status := SpektacularRunStatus{
		RunKey: "run-8314",
		Waves: []RunWaveStatus{
			{Wave: 1, Repositories: []RunRepositoryStatus{
				{Repo: "acme/api", Role: "service", DocumentStatus: "draft"},
				{Repo: "acme/ui", Role: "consumer", DocumentStatus: "draft"},
			}},
			{Wave: 2, Repositories: []RunRepositoryStatus{{Repo: "acme/docs", Role: "docs"}}},
		},
	}
	leases := &fakeFanoutLeases{}
	runner := RunFanoutRunner{StatusURL: fanoutStatusServer(t, status), Leases: leases}
	got, err := runner.FanOutWave(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("FanOutWave wave1: %v", err)
	}
	if len(got.Created) != 2 || len(leases.created) != 2 {
		t.Fatalf("wave1 created = %+v, leases = %+v", got.Created, leases.created)
	}
	for _, lease := range got.Created {
		if lease.RunKey != "run-8314" || lease.Wave != 1 || lease.Stage != RunStageImplement {
			t.Fatalf("lease shape = %+v", lease)
		}
	}

	leases.created = nil
	got, err = runner.FanOutWave(context.Background(), "", 2)
	if err != nil {
		t.Fatalf("FanOutWave wave2: %v", err)
	}
	if !got.Blocked || len(got.Created) != 0 || len(leases.created) != 0 {
		t.Fatalf("wave2 before final = %+v, leases = %+v", got, leases.created)
	}

	status.Waves[0].Repositories[0].DocumentStatus = RunDocumentStatusFinal
	status.Waves[0].Repositories[1].DocumentStatus = RunDocumentStatusFinal
	runner.StatusURL = fanoutStatusServer(t, status)
	got, err = runner.FanOutWave(context.Background(), "", 2)
	if err != nil {
		t.Fatalf("FanOutWave wave2 after final: %v", err)
	}
	if got.Blocked || len(got.Created) != 1 || got.Created[0].Repo != "acme/docs" {
		t.Fatalf("wave2 after final = %+v", got)
	}
}

func TestRunFanoutRefusesAndAuditsOverlap(t *testing.T) {
	leases := &fakeFanoutLeases{}
	overlaps := &fakeFanoutOverlaps{overlaps: []RunRepoOverlap{{Repo: "acme/api", Hives: []string{"h-a", "h-b"}}}}
	audit := &fakeFanoutAudit{}
	runner := RunFanoutRunner{
		StatusURL: fanoutStatusServer(t, SpektacularRunStatus{
			RunKey: "run-overlap",
			Waves:  []RunWaveStatus{{Wave: 1, Repositories: []RunRepositoryStatus{{Repo: "acme/api"}}}},
		}),
		Leases:   leases,
		Overlaps: overlaps,
		Audit:    audit,
	}
	got, err := runner.FanOutWave(context.Background(), "", 1)
	if err == nil || !strings.Contains(err.Error(), "constellation overlap") {
		t.Fatalf("FanOutWave error = %v, want overlap refusal", err)
	}
	if !got.Refused || len(leases.created) != 0 || len(audit.reasons) != 1 {
		t.Fatalf("overlap result = %+v leases=%+v audit=%+v", got, leases.created, audit.reasons)
	}
	if !reflect.DeepEqual(overlaps.repos, []string{"acme/api"}) {
		t.Fatalf("overlap repos = %v", overlaps.repos)
	}
}

func TestRunFanoutOverlapCheckUsesOnlyLeaseCandidates(t *testing.T) {
	leases := &fakeFanoutLeases{}
	overlaps := &fakeFanoutOverlaps{}
	runner := RunFanoutRunner{
		StatusURL: fanoutStatusServer(t, SpektacularRunStatus{
			RunKey: "run-filtered-overlap",
			Waves: []RunWaveStatus{{Wave: 1, Repositories: []RunRepositoryStatus{
				{Repo: "acme/done", DocumentStatus: RunDocumentStatusFinal},
				{Repo: "acme/next", DocumentStatus: "draft"},
			}}},
		}),
		Leases:   leases,
		Overlaps: overlaps,
	}
	got, err := runner.FanOutWave(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("FanOutWave: %v", err)
	}
	if len(got.Created) != 1 || got.Created[0].Repo != "acme/next" {
		t.Fatalf("created = %+v", got.Created)
	}
	if !reflect.DeepEqual(overlaps.repos, []string{"acme/next"}) {
		t.Fatalf("overlap repos = %v, want only lease candidates", overlaps.repos)
	}
}

func TestRunFanoutForwardFixLeavesOtherReposAlone(t *testing.T) {
	leases := &fakeFanoutLeases{}
	runner := RunFanoutRunner{
		StatusURL: fanoutStatusServer(t, SpektacularRunStatus{
			RunKey: "run-forward-fix",
			Waves: []RunWaveStatus{
				{Wave: 1, Repositories: []RunRepositoryStatus{
					{Repo: "acme/api", DocumentStatus: RunDocumentStatusFinal},
					{Repo: "acme/ui", Failed: true, DocumentStatus: "failed"},
				}},
			},
		}),
		Leases: leases,
	}
	got, err := runner.FanOutWave(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("FanOutWave: %v", err)
	}
	if !got.ForwardFix || got.WaitingOn != RunWaitingOnHuman {
		t.Fatalf("forward-fix state = %+v", got)
	}
	if len(leases.created) != 0 {
		t.Fatalf("merged/final and failed repos should not get fresh leases: %+v", leases.created)
	}
}

func TestRunFanoutSurfacesLeaseErrors(t *testing.T) {
	errBoom := errors.New("boom")
	runner := RunFanoutRunner{
		StatusURL: fanoutStatusServer(t, SpektacularRunStatus{
			RunKey: "run-error",
			Waves:  []RunWaveStatus{{Wave: 1, Repositories: []RunRepositoryStatus{{Repo: "acme/api"}}}},
		}),
		Leases: fanoutLeaseFunc(func(context.Context, RunImplementationLease) error { return errBoom }),
	}
	if _, err := runner.FanOutWave(context.Background(), "", 1); !errors.Is(err, errBoom) {
		t.Fatalf("FanOutWave error = %v, want %v", err, errBoom)
	}
}

type fanoutLeaseFunc func(context.Context, RunImplementationLease) error

func (f fanoutLeaseFunc) CreateImplementationLease(ctx context.Context, lease RunImplementationLease) error {
	return f(ctx, lease)
}
