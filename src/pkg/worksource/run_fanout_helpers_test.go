package worksource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunFanoutHelperCoverage(t *testing.T) {
	status := SpektacularRunStatus{RunKey: "acme/app#7", Waves: []RunWaveStatus{
		{Wave: 3, Repositories: []RunRepositoryStatus{{Repo: "later"}}},
		{Wave: 1, Repositories: []RunRepositoryStatus{{Repo: "acme/app", DocumentStatus: RunDocumentStatusFinal}}},
		{Wave: 2, Repositories: []RunRepositoryStatus{{Repo: "acme/api", DocumentStatus: "draft"}}},
	}}
	if got := firstRunWave(status); got != 1 {
		t.Fatalf("firstRunWave = %d", got)
	}
	if wave, ok := statusWave(status, 2); !ok || wave.Wave != 2 {
		t.Fatalf("statusWave = %+v %v", wave, ok)
	}
	if _, ok := statusWave(status, 99); ok {
		t.Fatal("missing wave reported ok")
	}
	if !previousRunWaveFinal(status, 2) {
		t.Fatal("wave 1 is final, so wave 2 should be unblocked")
	}
	if previousRunWaveFinal(status, 3) {
		t.Fatal("wave 2 is draft, so wave 3 should be blocked")
	}
	msg := runOverlapMessage([]RunRepoOverlap{
		{Repo: "acme/api", Hives: []string{"hive-b", "hive-a"}},
		{Repo: " ", Reason: "custom refusal"},
		{Repo: "", Hives: nil},
	})
	if !strings.Contains(msg, "custom refusal") || !strings.Contains(msg, "acme/api claimed by hive-b, hive-a") || !strings.Contains(msg, "repository has overlapping spoke claims") {
		t.Fatalf("overlap message = %q", msg)
	}
}

func TestRunFanoutFetchStatusErrors(t *testing.T) {
	if _, err := (RunFanoutRunner{}).FetchStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "status URL") {
		t.Fatalf("missing URL err = %v", err)
	}
	t.Run("http status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusTeapot)
		}))
		defer srv.Close()
		if _, err := (RunFanoutRunner{StatusURL: srv.URL}).FetchStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "418") {
			t.Fatalf("status err = %v", err)
		}
	})
	t.Run("decode", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer srv.Close()
		if _, err := (RunFanoutRunner{StatusURL: srv.URL}).FetchStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("decode err = %v", err)
		}
	})
}
