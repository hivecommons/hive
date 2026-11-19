package dashboard

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#18516: the owner-only project.remote_write_identity
// control on Settings → Repos → Reference repos.

const remoteWriteIdentityPath = "/api/config/reference-repos/write-identity"

func TestRemoteWriteIdentity_GetDefaultsToBot(t *testing.T) {
	s := covApiServer(t)
	got := decodeReferenceRepos(t, doOwnerGet(s, referenceReposPath).Body.Bytes())
	if got.WriteIdentity != config.RemoteWriteIdentityBot {
		t.Fatalf("a hive without the key must read as bot, got %q", got.WriteIdentity)
	}
	if len(got.WriteIdentities) != 3 || got.WriteIdentities[0] != "bot" || got.WriteIdentities[1] != "bot_only" || got.WriteIdentities[2] != "owner" {
		t.Fatalf("write_identities = %v", got.WriteIdentities)
	}
}

func TestRemoteWriteIdentity_PutOwnerOnlyValidatesChecksAndAudits(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	if rec := doPutNoRole(s, remoteWriteIdentityPath, `{"write_identity":"owner"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: expected 403, got %d", rec.Code)
	}
	for _, body := range []map[string]any{{}, {"write_identity": "me"}, {"write_identity": "app"}} {
		if rec := doPut(s, remoteWriteIdentityPath, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %v: expected 400, got %d", body, rec.Code)
		}
	}
	if cfg.Project.RemoteWriteIdentity != "" {
		t.Fatalf("a refused PUT changed the config: %q", cfg.Project.RemoteWriteIdentity)
	}

	// The dry run refuses a value boot would stop on; nothing is stored.
	var checked []string
	changed := 0
	s.deps.CheckRemoteWriteIdentity = func(_ context.Context, setting string) error {
		checked = append(checked, setting)
		if setting == config.RemoteWriteIdentityBotOnly {
			return errors.New("project.reference_repos acme/site: write: [issues] has no credential")
		}
		return nil
	}
	s.deps.RemoteWriteIdentityChanged = func() { changed++ }
	rec := doPut(s, remoteWriteIdentityPath, map[string]any{"write_identity": "bot-only"})
	if rec.Code != http.StatusBadRequest || cfg.Project.RemoteWriteIdentity != "" || changed != 0 {
		t.Fatalf("refused dry run must store nothing: code=%d value=%q changed=%d", rec.Code, cfg.Project.RemoteWriteIdentity, changed)
	}

	rec = doPut(s, remoteWriteIdentityPath, map[string]any{"write_identity": "Owner"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT owner: %d %s", rec.Code, rec.Body.String())
	}
	if cfg.Project.RemoteWriteIdentity != config.RemoteWriteIdentityOwner || changed != 1 {
		t.Fatalf("value=%q changed=%d", cfg.Project.RemoteWriteIdentity, changed)
	}
	if got := decodeReferenceRepos(t, rec.Body.Bytes()); got.WriteIdentity != config.RemoteWriteIdentityOwner {
		t.Fatalf("response write_identity = %q", got.WriteIdentity)
	}
	if len(checked) != 2 || checked[1] != config.RemoteWriteIdentityOwner {
		t.Fatalf("dry run saw %v, want the normalized values", checked)
	}
	if s.audit != nil {
		found := false
		for _, entry := range s.audit.Recent(50) {
			if entry.Action == "config_remote_write_identity" {
				found = true
				if !auditEntryIsSensitive(entry) {
					t.Error("config_remote_write_identity audit must be marked sensitive")
				}
			}
		}
		if !found {
			t.Error("missing config_remote_write_identity audit")
		}
	}

	// Setting the same value again is a no-op: no re-resolve.
	if rec := doPut(s, remoteWriteIdentityPath, map[string]any{"write_identity": "owner"}); rec.Code != http.StatusOK || changed != 1 {
		t.Fatalf("same value: code=%d changed=%d", rec.Code, changed)
	}
}
