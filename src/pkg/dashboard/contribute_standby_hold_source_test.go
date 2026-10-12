package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// readHoldSourceLedgerApplied returns the `applied` section of Hive's local
// hold-source record: item key → source names.
func readHoldSourceLedgerApplied(t *testing.T) map[string]map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(ghpkg.HoldSourcesPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading hold-source record: %v", err)
	}
	var ledger struct {
		Applied map[string]map[string]json.RawMessage `json:"applied"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatalf("parsing hold-source record: %v", err)
	}
	return ledger.Applied
}

// A donated standby hold leaves no marker on GitHub, so applyDonatedHold must
// write the standby_donation source into Hive's local record once the label
// landed (hivecommons/hive#11564), and none when the label call fails.
func TestApplyDonatedHold_RecordsStandbyDonationSource(t *testing.T) {
	old := ghpkg.HoldSourcesPath
	ghpkg.HoldSourcesPath = filepath.Join(t.TempDir(), ghpkg.HoldSourcesFile)
	t.Cleanup(func() { ghpkg.HoldSourcesPath = old })

	fail := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/widgets/issues/7/labels" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"hold"}]`))
	}))
	t.Cleanup(api.Close)
	client := ghpkg.NewClientForTest(api.URL, "acme", []string{"widgets"}, slog.Default())
	hub := &ContributeWSHub{
		server: &Server{deps: &Dependencies{Ctx: context.Background(), GHClient: client}},
		logger: slog.Default(),
	}

	fail = true
	if err := hub.applyDonatedHold("https://github.com/acme/widgets/pull/7"); err == nil {
		t.Fatal("applyDonatedHold must report the failed label call")
	}
	if applied := readHoldSourceLedgerApplied(t); len(applied) != 0 {
		t.Fatalf("a hold whose label failed must record no source, got %v", applied)
	}

	fail = false
	if err := hub.applyDonatedHold("https://github.com/acme/widgets/pull/7"); err != nil {
		t.Fatalf("applyDonatedHold: %v", err)
	}
	applied := readHoldSourceLedgerApplied(t)
	if _, ok := applied["pr:acme/widgets#7"][ghpkg.HoldSourceStandbyDonation]; !ok || len(applied) != 1 {
		t.Fatalf("hold-source record = %v, want pr:acme/widgets#7 -> %s", applied, ghpkg.HoldSourceStandbyDonation)
	}
}
